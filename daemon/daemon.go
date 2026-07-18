//go:build windows

package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Daemon struct {
	mu            sync.RWMutex
	ctx           context.Context
	config        Config
	configPath    string
	processes     map[string]*Process
	transient     map[string]AnglDef
	transientPath string
	logger        *log.Logger
	logLines      int
}

func New(configPath string) (*Daemon, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	logDir := filepath.Join(DefaultConfigDir(), "logs")
	os.MkdirAll(logDir, 0755)

	logPath := filepath.Join(logDir, "angld.log")
	rotateLog(logPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}

	logger := log.New(io.MultiWriter(logFile, os.Stderr), "[angld] ", log.LstdFlags)

	transientPath := DefaultTransientPath()
	transient, err := LoadTransient(transientPath)
	if err != nil {
		logger.Printf("warning: load transient: %v", err)
		transient = make(map[string]AnglDef)
	}

	return &Daemon{
		config:        cfg,
		configPath:    configPath,
		processes:     make(map[string]*Process),
		transient:     transient,
		transientPath: transientPath,
		logger:        logger,
		logLines:      cfg.Daemon.LogLines,
	}, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	d.logger.Printf("starting (port %d, %d angls)", d.config.Daemon.HTTPPort, len(d.config.Angls))

	d.mu.Lock()
	for name, def := range d.config.Angls {
		p := NewProcess(name, def, d.logLines, d.logger)
		d.processes[name] = p
		if def.IsEnabled() {
			p.Start(ctx)
		}
	}
	for name, def := range d.transient {
		if _, exists := d.processes[name]; exists {
			d.logger.Printf("[%s] transient skipped (name conflict with config)", name)
			continue
		}
		p := NewProcess(name, def, d.logLines, d.logger)
		d.processes[name] = p
		d.logger.Printf("[%s] transient loaded (stopped)", name)
	}
	d.mu.Unlock()

	srv := NewServer(d)
	err := srv.Run(ctx, d.config.Daemon.HTTPPort)

	d.logger.Println("stopping all processes")
	d.mu.RLock()
	for _, p := range d.processes {
		p.Stop()
	}
	d.mu.RUnlock()

	d.logger.Println("shutdown complete")
	return err
}

func (d *Daemon) List() []ProcessStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()

	names := sortedKeys(d.processes)
	statuses := make([]ProcessStatus, 0, len(names))
	for _, name := range names {
		statuses = append(statuses, d.processes[name].Status())
	}
	return statuses
}

func (d *Daemon) StatusOf(name string) (ProcessStatus, error) {
	d.mu.RLock()
	p, ok := d.processes[name]
	d.mu.RUnlock()

	if !ok {
		return ProcessStatus{}, fmt.Errorf("unknown angl %q", name)
	}
	return p.Status(), nil
}

func (d *Daemon) StartAngl(name string) error {
	d.mu.RLock()
	p, ok := d.processes[name]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}
	p.Start(d.ctx)
	d.logger.Printf("started %s", name)
	return nil
}

func (d *Daemon) StopAngl(name string) error {
	d.mu.RLock()
	p, ok := d.processes[name]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}
	p.Stop()
	d.logger.Printf("stopped %s", name)
	return nil
}

func (d *Daemon) RestartAngl(name string) error {
	if err := d.StopAngl(name); err != nil {
		return err
	}
	return d.StartAngl(name)
}

func (d *Daemon) Reload() (ReloadResult, error) {
	cfg, err := LoadConfig(d.configPath)
	if err != nil {
		return ReloadResult{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	var result ReloadResult

	for name, p := range d.processes {
		if _, ok := cfg.Angls[name]; !ok {
			if _, isTransient := d.transient[name]; !isTransient {
				p.Stop()
				delete(d.processes, name)
				result.Removed = append(result.Removed, name)
			}
		}
	}

	for name, def := range cfg.Angls {
		if p, ok := d.processes[name]; ok {
			oldDef := d.config.Angls[name]
			if defChanged(oldDef, def) {
				p.Stop()
				np := NewProcess(name, def, d.logLines, d.logger)
				d.processes[name] = np
				if def.IsEnabled() {
					np.Start(d.ctx)
				}
				result.Restarted = append(result.Restarted, name)
			} else {
				result.Unchanged = append(result.Unchanged, name)
			}
		} else {
			np := NewProcess(name, def, d.logLines, d.logger)
			d.processes[name] = np
			if def.IsEnabled() {
				np.Start(d.ctx)
			}
			result.Added = append(result.Added, name)
		}
	}

	d.config = cfg
	d.logger.Printf("reload: +%v -%v ~%v", result.Added, result.Removed, result.Restarted)
	return result, nil
}

func (d *Daemon) Enable(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	def, ok := d.config.Angls[name]
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}

	enabled := true
	def.Enabled = &enabled
	d.config.Angls[name] = def

	if err := SaveConfig(d.configPath, d.config); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if p, ok := d.processes[name]; ok {
		p.mu.Lock()
		p.def = def
		p.mu.Unlock()
		p.Start(d.ctx)
	}

	d.logger.Printf("enabled %s", name)
	return nil
}

func (d *Daemon) Disable(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	def, ok := d.config.Angls[name]
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}

	disabled := false
	def.Enabled = &disabled
	d.config.Angls[name] = def

	if err := SaveConfig(d.configPath, d.config); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	if p, ok := d.processes[name]; ok {
		p.Stop()
		p.mu.Lock()
		p.def = def
		p.state = StateDisabled
		p.mu.Unlock()
	}

	d.logger.Printf("disabled %s", name)
	return nil
}

func (d *Daemon) Register(name string, def AnglDef) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.config.Angls[name]; ok {
		return fmt.Errorf("%q already exists in config", name)
	}
	if _, ok := d.transient[name]; ok {
		return fmt.Errorf("%q already registered as transient", name)
	}

	disabled := false
	def.Enabled = &disabled

	d.transient[name] = def
	if err := SaveTransient(d.transientPath, d.transient); err != nil {
		delete(d.transient, name)
		return fmt.Errorf("save transient: %w", err)
	}

	p := NewProcess(name, def, d.logLines, d.logger)
	d.processes[name] = p

	d.logger.Printf("registered transient %s", name)
	return nil
}

func (d *Daemon) Unregister(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.config.Angls[name]; ok {
		return fmt.Errorf("%q is in config (use disable instead)", name)
	}
	if _, ok := d.transient[name]; !ok {
		return fmt.Errorf("%q is not a transient angl", name)
	}

	if p, ok := d.processes[name]; ok {
		p.Stop()
		delete(d.processes, name)
	}

	delete(d.transient, name)
	if err := SaveTransient(d.transientPath, d.transient); err != nil {
		d.logger.Printf("warning: save transient: %v", err)
	}

	d.logger.Printf("unregistered %s", name)
	return nil
}

func (d *Daemon) TailOutput(name string) (*RingBuffer, error) {
	d.mu.RLock()
	p, ok := d.processes[name]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown angl %q", name)
	}
	return p.output, nil
}

type ReloadResult struct {
	Added     []string `json:"added,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Restarted []string `json:"restarted,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
}

func defChanged(a, b AnglDef) bool {
	if a.Command != b.Command || a.Interval != b.Interval || a.IsEnabled() != b.IsEnabled() {
		return true
	}
	if len(a.Args) != len(b.Args) {
		return true
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
