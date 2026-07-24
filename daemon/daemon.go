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
	mu         sync.RWMutex
	ctx        context.Context
	config     Config
	configPath string
	processes  map[string]*Process
	logger     *log.Logger
	logFile    *os.File
	inventory  inventoryStream
}

func New(configPath string) (*Daemon, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	logDir := filepath.Join(DefaultConfigDir(), "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	logPath := filepath.Join(logDir, "angld.log")
	rotateLog(logPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}

	logger := log.New(io.MultiWriter(logFile, os.Stderr), "[angld] ", log.LstdFlags)

	return &Daemon{
		config:     cfg,
		configPath: configPath,
		processes:  make(map[string]*Process),
		logger:     logger,
		logFile:    logFile,
	}, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	d.logger.Printf("starting (%d angls)", len(d.config.Angls))
	if d.logFile != nil {
		defer d.logFile.Close()
	}

	d.mu.Lock()
	for name, def := range d.config.Angls {
		p := NewProcess(name, def, d.logger)
		d.processes[name] = p
		if def.IsEnabled() {
			p.Start(ctx)
		}
	}
	d.mu.Unlock()

	if err := d.initInventoryStream(); err != nil {
		return err
	}
	go d.watchInventory(ctx.Done())

	srv := NewServer(d)
	err := srv.Run(ctx)

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

func (d *Daemon) ExecAngl(name string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.processes[name]
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}
	if err := p.Exec(d.ctx); err != nil {
		return err
	}
	d.logger.Printf("executed %s", name)
	return nil
}

func (d *Daemon) StopAngl(name string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.processes[name]
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}
	if err := p.StopRunning(); err != nil {
		return err
	}
	d.logger.Printf("stopped %s", name)
	return nil
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
			p.Stop()
			delete(d.processes, name)
			result.Removed = append(result.Removed, name)
		}
	}

	for name, def := range cfg.Angls {
		if p, ok := d.processes[name]; ok {
			oldDef := d.config.Angls[name]
			if defChanged(oldDef, def) {
				p.Stop()
				np := NewProcess(name, def, d.logger)
				d.processes[name] = np
				if def.IsEnabled() {
					np.Start(d.ctx)
				}
				result.Restarted = append(result.Restarted, name)
			} else {
				result.Unchanged = append(result.Unchanged, name)
			}
		} else {
			np := NewProcess(name, def, d.logger)
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

	def, ok := d.config.Angls[name]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("unknown angl %q", name)
	}

	enabled := true
	def.Enabled = &enabled
	d.config.Angls[name] = def

	if err := SaveConfig(d.configPath, d.config); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("save config: %w", err)
	}
	p := d.processes[name]
	d.mu.Unlock()

	if p != nil {
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

	def, ok := d.config.Angls[name]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("unknown angl %q", name)
	}

	disabled := false
	def.Enabled = &disabled
	d.config.Angls[name] = def

	if err := SaveConfig(d.configPath, d.config); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("save config: %w", err)
	}
	p := d.processes[name]
	d.mu.Unlock()

	if p != nil {
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
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := def.Validate(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.config.Angls[name]; ok {
		return fmt.Errorf("angl %q already exists", name)
	}

	disabled := false
	def.Enabled = &disabled
	d.config.Angls[name] = def
	if err := SaveConfig(d.configPath, d.config); err != nil {
		delete(d.config.Angls, name)
		return fmt.Errorf("save config: %w", err)
	}

	d.processes[name] = NewProcess(name, def, d.logger)
	d.logger.Printf("registered %s", name)
	return nil
}

func (d *Daemon) Delete(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	def, ok := d.config.Angls[name]
	if !ok {
		return fmt.Errorf("unknown angl %q", name)
	}

	delete(d.config.Angls, name)
	if err := SaveConfig(d.configPath, d.config); err != nil {
		d.config.Angls[name] = def
		return fmt.Errorf("save config: %w", err)
	}

	if p := d.processes[name]; p != nil {
		p.Stop()
		delete(d.processes, name)
	}
	d.logger.Printf("deleted %s", name)
	return nil
}

type ReloadResult struct {
	Added     []string `json:"added,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Restarted []string `json:"restarted,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
}

func defChanged(a, b AnglDef) bool {
	if a.Command != b.Command || a.Interval != b.Interval || a.IsEnabled() != b.IsEnabled() ||
		a.MaxRestarts != b.MaxRestarts || a.KillExisting != b.KillExisting ||
		a.Charge != b.Charge || !endpointsEqual(a.Endpoint, b.Endpoint) {
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

func endpointsEqual(a, b *EndpointDef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.HTTP == b.HTTP
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
