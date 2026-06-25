//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
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
	tokens        *TokenManager
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
		tokens:        NewTokenManager(cfg.Orchard, logger),
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
// Uses dot separator since colon is illegal in Windows filenames (NTFS
// treats it as an alternate data stream marker).
func anglSchedgName(anglName string) string {
	return "angl." + anglName
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

// MessageWithMode sends a message to an angl with the specified delivery mode:
//   - "queue": enqueue only, agent picks it up on next heartbeat
//   - "wake" (default): enqueue + wake (agent processes after current tick finishes)
//   - "interrupt": enqueue + wake + for conversation agents, cancel current turn
//     and send immediately via Orchard
func (d *Daemon) Message(name, prompt, from, mode string) (json.RawMessage, error) {
	d.mu.RLock()
	_, ok := d.processes[name]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown angl %q", name)
	}

	if mode == "" {
		mode = "schedg"
	}

	switch mode {
	case "interrupt":
		// Bypass schedg entirely. Send directly to the Orchard conversation
		// API. This is fire-and-forget -- the caller accepts the risk of
		// concurrent POSTs if a turn is already in-flight.
		convID := d.getConversationID(name)
		if convID == "" {
			return nil, fmt.Errorf("interrupt requires a conversation agent (no conversation: tag on %q)", name)
		}
		d.logger.Printf("[%s] interrupt: sending directly to orchard from=%s", name, from)
		go d.sendToOrchard(convID, prompt)
		result := map[string]interface{}{"status": "sent", "mode": "interrupt"}
		raw, _ := json.Marshal(result)
		return raw, nil

	default: // "schedg"
		// Enqueue to the per-angl schedg queue and wake the process. The
		// exec bridge drains the queue serially, so messages are delivered
		// one at a time without concurrent conversation POSTs.
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

		d.logger.Printf("[%s] schedg: enqueued task %s from=%s", name, id, from)

		// Wake the process so it drains immediately.
		d.mu.RLock()
		if p, ok := d.processes[name]; ok {
			p.Wake()
		}
		d.mu.RUnlock()

		result := map[string]interface{}{"status": "enqueued", "task_id": id, "mode": "schedg"}
		raw, _ := json.Marshal(result)
		return raw, nil
	}
}

func (d *Daemon) getConversationID(name string) string {
	// Check config angls
	if def, ok := d.config.Angls[name]; ok {
		for _, t := range def.Tags {
			if strings.HasPrefix(t, "conversation:") {
				return strings.TrimPrefix(t, "conversation:")
			}
		}
	}
	// Check transient
	if def, ok := d.transient[name]; ok {
		for _, t := range def.Tags {
			if strings.HasPrefix(t, "conversation:") {
				return strings.TrimPrefix(t, "conversation:")
			}
		}
	}
	return ""
}

func (d *Daemon) sendToOrchard(convID, message string) {
	token, err := d.tokens.Token()
	if err != nil {
		d.logger.Printf("interrupt: token error: %v", err)
		return
	}
	orchardURL := d.config.Orchard.URL
	envID := d.config.Orchard.EnvironmentID
	if orchardURL == "" || envID == "" {
		d.logger.Printf("interrupt: orchard not configured")
		return
	}

	url := fmt.Sprintf("%s/api/e/%s/conversation/%s", orchardURL, envID, convID)
	body, _ := json.Marshal(map[string]string{"message": message, "modelTier": "large"})

	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := orchardHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		d.logger.Printf("interrupt: orchard error: %v", err)
		return
	}
	resp.Body.Close()
	d.logger.Printf("interrupt: sent to orchard (status %d)", resp.StatusCode)
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

// CreateChat creates a new orchard conversation agent with a fresh conversation ID.
func (d *Daemon) CreateChat(name, charge string) (map[string]string, error) {
	if name == "" {
		return nil, fmt.Errorf("name required")
	}

	d.mu.RLock()
	_, exists := d.processes[name]
	d.mu.RUnlock()
	if exists {
		return map[string]string{"name": name, "status": "exists"}, nil
	}

	convID := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		time.Now().UnixNano()&0xFFFFFFFF,
		time.Now().UnixNano()>>32&0xFFFF,
		0x4000|(time.Now().UnixNano()>>48&0x0FFF),
		0x8000|(time.Now().UnixNano()>>60&0x3FFF),
		time.Now().UnixNano()&0xFFFFFFFFFFFF)

	if charge == "" {
		charge = "conversation agent"
	}

	exe, _ := os.Executable()
	def := AnglDef{
		Command:   exe,
		Args:      []string{"exec", name, "--cwd", filepath.Join(os.Getenv("USERPROFILE"), "dev", "orchard", "main")},
		Enabled:   func() *bool { b := false; return &b }(),
		Interval:  "1h",
		Charge:    charge,
		Tags:      []string{"conversation:" + convID},
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	if err := d.Register(name, def); err != nil {
		return nil, err
	}
	d.initSchedg(name)
	if err := d.StartAngl(name); err != nil {
		return nil, err
	}

	d.logger.Printf("[%s] created chat agent (conv %s)", name, convID)
	return map[string]string{
		"name":           name,
		"conversation_id": convID,
		"status":         "created",
	}, nil
}

// Dispatch creates a new author angl for a specific schedg task, starts it, and
// sends it a message to lease the task. Returns the angl name.
func (d *Daemon) Dispatch(queue, taskID, cwd, runbook string) (map[string]string, error) {
	name := fmt.Sprintf("%s-%s", queue, taskID)

	// Don't create if it already exists
	d.mu.RLock()
	_, exists := d.processes[name]
	d.mu.RUnlock()
	if exists {
		return map[string]string{"name": name, "status": "exists"}, nil
	}

	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Find the angl.exe path (ourselves)
	exe, _ := os.Executable()

	args := []string{"exec", name, "--work-queue", queue}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	if runbook != "" {
		args = append(args, "--runbook", runbook)
	}

	enabled := false
	def := AnglDef{
		Command:   exe,
		Args:      args,
		Enabled:   &enabled,
		Interval:  "30m",
		Charge:    fmt.Sprintf("author: %s #%s", queue, taskID),
		Tags:      []string{"schedg:" + queue},
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	if err := d.Register(name, def); err != nil {
		return nil, err
	}
	d.initSchedg(name)

	// Start it
	if err := d.StartAngl(name); err != nil {
		return nil, err
	}

	// Send it a message to lease the specific task
	prompt := fmt.Sprintf(
		"Lease task #%s from queue '%s' via: schedg next %s --caller %s\n"+
			"If the task is already in-flight for another caller, just take over monitoring.\n"+
			"Follow the runbook and execute the task end to end.",
		taskID, queue, queue, name)

	_, err := d.enqueueMessage(name, fmt.Sprintf("Dispatch: %s #%s", queue, taskID), prompt, 5)
	if err != nil {
		return nil, err
	}

	// Wake it
	d.mu.RLock()
	if p, ok := d.processes[name]; ok {
		p.Wake()
	}
	d.mu.RUnlock()

	d.logger.Printf("[%s] dispatched for %s #%s", name, queue, taskID)
	return map[string]string{"name": name, "status": "created"}, nil
}

// SchedgOp performs a lifecycle operation on a schedg task.
func (d *Daemon) SchedgOp(queue, id, op string) error {
	db, err := schedg.OpenByName(queue)
	if err != nil {
		return err
	}
	defer db.Close()

	switch op {
	case "complete":
		if err := db.Complete(id); err != nil {
			return err
		}
	case "cancel":
		if _, err := db.Cancel(id, ""); err != nil {
			return err
		}
	case "fail":
		if err := db.Fail(id, "manual"); err != nil {
			return err
		}
	case "requeue":
		if err := db.Requeue(id); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown op %q", op)
	}
	return db.Save()
}

// SchedgAdd creates a new task in a schedg queue.
func (d *Daemon) SchedgAdd(queue, title, desc string, priority int64) (string, error) {
	db, err := schedg.OpenByName(queue)
	if err != nil {
		return "", err
	}
	defer db.Close()

	id, err := db.Add(context.Background(), title, schedg.TaskOpts{
		Description: desc,
		Priority:    priority,
	})
	if err != nil {
		return "", err
	}
	if err := db.Save(); err != nil {
		return "", err
	}
	return id, nil
}

// Completions returns context-appropriate completion items for the command system.
func (d *Daemon) Completions(ctx string) interface{} {
	d.mu.RLock()
	defer d.mu.RUnlock()

	type item struct {
		Value  string `json:"value"`
		Label  string `json:"label"`
		Detail string `json:"detail,omitempty"`
	}

	switch ctx {
	case "angls":
		out := make([]item, 0, len(d.processes))
		for name, p := range d.processes {
			st := p.Status()
			out = append(out, item{Value: name, Label: name, Detail: st.Charge})
		}
		return out

	case "queues":
		queues, _ := schedg.ListQueues()
		out := make([]item, 0, len(queues))
		for _, q := range queues {
			out = append(out, item{Value: q.Name, Label: q.Name, Detail: q.Driver})
		}
		return out

	case "states":
		return []item{
			{Value: "running", Label: "running"},
			{Value: "backoff", Label: "backoff"},
			{Value: "stopped", Label: "stopped"},
			{Value: "disabled", Label: "disabled"},
			{Value: "failed", Label: "failed"},
		}

	case "themes":
		return []item{
			{Value: "crimson", Label: "crimson", Detail: "Warm parchment"},
			{Value: "azure", Label: "azure", Detail: "Cool celestial"},
		}

	case "views":
		return []item{
			{Value: "angls", Label: "angls", Detail: "Angl list"},
			{Value: "queues", Label: "queues", Detail: "Queue list"},
		}

	case "directions":
		return []item{
			{Value: "h", Label: "h", Detail: "Horizontal (below)"},
			{Value: "v", Label: "v", Detail: "Vertical (right)"},
		}

	default:
		return []item{}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
