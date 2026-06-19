//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/jack-work/schedg"
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

// SchedgDir returns the directory for per-angl SQLite queues.
func SchedgDir() string {
	return filepath.Join(DefaultConfigDir(), "schedg")
}

// schedgPath returns the SQLite DB path for an angl's message queue.
func schedgPath(name string) string {
	return filepath.Join(SchedgDir(), name+".db")
}

// enqueueMessage opens the per-angl schedg, inserts the task, saves, and closes.
// No long-lived DB handle -- the bridge owns the state file between ticks.
func (d *Daemon) enqueueMessage(name, title, desc string, priority int64) (string, error) {
	os.MkdirAll(SchedgDir(), 0755)
	path := schedgPath(name)

	db, err := schedg.Init(context.Background(), schedg.Options{
		Driver: "sqlite",
		Path:   path,
		Name:   anglSchedgName(name),
	})
	if err != nil {
		return "", fmt.Errorf("init schedg for %s: %w", name, err)
	}
	defer db.Close()

	// Register with the schedg config so the web UI can see it.
	d.registerSchedg(name, path)

	id, err := db.Add(context.Background(), title, schedg.TaskOpts{
		Description: desc,
		Priority:    priority,
	})
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}
	if err := db.Save(); err != nil {
		return "", fmt.Errorf("save: %w", err)
	}
	return id, nil
}

// initSchedg creates the per-angl schedg DB (if it doesn't exist) and registers
// it with the schedg config so the web UI can see it.
func (d *Daemon) initSchedg(name string) {
	os.MkdirAll(SchedgDir(), 0755)
	path := schedgPath(name)
	db, err := schedg.Init(context.Background(), schedg.Options{
		Driver: "sqlite",
		Path:   path,
		Name:   anglSchedgName(name),
	})
	if err != nil {
		d.logger.Printf("[%s] warning: init schedg: %v", name, err)
		return
	}
	db.Close()
	d.registerSchedg(name, path)
}

// anglSchedgName returns the schedg config name for an angl's message queue.
func anglSchedgName(anglName string) string {
	return "angl:" + anglName
}

// registerSchedg adds the per-angl SQLite DB to the schedg config (idempotent).
func (d *Daemon) registerSchedg(anglName, dbPath string) {
	name := anglSchedgName(anglName)
	if err := schedg.RegisterConfig(schedg.ConfigDB{
		Name:   name,
		Driver: "sqlite",
		Path:   dbPath,
	}); err != nil {
		d.logger.Printf("[%s] warning: register schedg config: %v", anglName, err)
	}
}

// unregisterSchedg removes the per-angl DB from the schedg config.
func (d *Daemon) unregisterSchedg(anglName string) {
	name := anglSchedgName(anglName)
	if err := schedg.UnregisterConfig(name); err != nil {
		d.logger.Printf("[%s] warning: unregister schedg config: %v", anglName, err)
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	d.ctx = ctx
	d.logger.Printf("starting (port %d, %d angls)", d.config.Daemon.HTTPPort, len(d.config.Angls))

	// Register any existing per-angl schedg DBs with the schedg config.
	if entries, err := os.ReadDir(SchedgDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".db" {
				name := e.Name()[:len(e.Name())-3]
				d.registerSchedg(name, filepath.Join(SchedgDir(), e.Name()))
			}
		}
	}

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
		d.initSchedg(name)
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

	// Initialize the per-angl schedg message queue and register it
	// with the schedg config so the web UI can see it immediately.
	d.initSchedg(name)

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

	// Remove from schedg config so the web UI no longer shows it.
	d.unregisterSchedg(name)

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

func (d *Daemon) Message(name, prompt, from string) (json.RawMessage, error) {
	d.mu.RLock()
	_, ok := d.processes[name]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown angl %q", name)
	}

	title := prompt
	if len(title) > 120 {
		title = title[:120]
	}
	desc := prompt
	if from != "" {
		desc = fmt.Sprintf("From: %s\n\n%s", from, prompt)
	}

	id, err := d.enqueueMessage(name, title, desc, 1)
	if err != nil {
		return nil, err
	}

	d.logger.Printf("[%s] message enqueued (task %s from %s)", name, id, from)

	// Wake the process so it picks up the message immediately.
	d.mu.RLock()
	if p, ok := d.processes[name]; ok {
		p.Wake()
	}
	d.mu.RUnlock()

	result := map[string]string{"status": "enqueued", "task_id": id}
	raw, _ := json.Marshal(result)
	return raw, nil
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
