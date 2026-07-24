//go:build windows

package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAnglDefValidate(t *testing.T) {
	tests := []struct {
		name    string
		def     AnglDef
		wantErr bool
	}{
		{name: "persistent", def: AnglDef{Command: "tool.exe"}},
		{name: "heartbeat", def: AnglDef{Command: "tool.exe", Interval: "15m"}},
		{name: "missing command", def: AnglDef{}, wantErr: true},
		{name: "bad interval", def: AnglDef{Command: "tool.exe", Interval: "tomorrow"}, wantErr: true},
		{name: "zero interval", def: AnglDef{Command: "tool.exe", Interval: "0s"}, wantErr: true},
		{name: "negative restarts", def: AnglDef{Command: "tool.exe", MaxRestarts: -1}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.def.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"api", "my-api", "sync.daily", "worker_2"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "../escape", "has space", "slash/name"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) unexpectedly succeeded", name)
		}
	}
}

func TestPort(t *testing.T) {
	tests := []struct {
		url  string
		want int
	}{{
		url: "http://localhost:8080/health", want: 8080,
	}, {
		url: "https://[::1]:443", want: 443,
	}, {
		url: "http://localhost", want: 0,
	}, {
		url: "not a URL", want: 0,
	}}

	for _, tt := range tests {
		def := AnglDef{Endpoint: &EndpointDef{HTTP: tt.url}}
		if got := def.Port(); got != tt.want {
			t.Errorf("Port(%q) = %d, want %d", tt.url, got, tt.want)
		}
	}
}

func TestLoadConfigValidatesDefinitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"angls":{"bad":{"command":"x","interval":"nope"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig unexpectedly accepted an invalid interval")
	}
}

func TestSaveConfigReplacesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first := Config{Angls: map[string]AnglDef{"one": {Command: "one.exe"}}}
	second := Config{Angls: map[string]AnglDef{"two": {Command: "two.exe"}}}
	if err := SaveConfig(path, first); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Angls["two"]; !ok || len(got.Angls) != 1 {
		t.Fatalf("saved config = %#v, want only two", got.Angls)
	}
}

func TestRegisterAndDeleteUseConfigAsOnlyRegistry(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	daemon := &Daemon{
		ctx:        context.Background(),
		config:     Config{Angls: make(map[string]AnglDef)},
		configPath: configPath,
		processes:  make(map[string]*Process),
		logger:     log.New(io.Discard, "", 0),
	}
	if err := daemon.Register("worker", AnglDef{Command: "worker.exe", Charge: "test"}); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Angls["worker"].IsEnabled() {
		t.Fatal("newly registered angl should be disabled until explicitly enabled")
	}
	if got := daemon.processes["worker"].Status().State; got != StateDisabled {
		t.Fatalf("registered state = %s, want disabled", got)
	}
	if err := daemon.Delete("worker"); err != nil {
		t.Fatal(err)
	}
	stored, err = LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Angls) != 0 || len(daemon.processes) != 0 {
		t.Fatalf("delete left config/processes behind: %#v %#v", stored.Angls, daemon.processes)
	}
}

func TestExecWakesCurrentBackoffWait(t *testing.T) {
	process := NewProcess("waiting", AnglDef{Command: "tool.exe"}, log.New(io.Discard, "", 0))
	process.mu.Lock()
	wake := process.beginBackoffLocked(time.Now(), time.Hour)
	process.mu.Unlock()

	waited := make(chan bool, 1)
	go func() { waited <- process.wait(context.Background(), time.Hour, wake) }()

	if err := process.Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-waited:
		if !completed {
			t.Fatal("forced wait reported cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("exec did not wake backoff")
	}
}

func TestExecRejectsStartingAndRunning(t *testing.T) {
	for _, state := range []ProcessState{StateStarting, StateRunning} {
		process := NewProcess("busy", AnglDef{Command: "tool.exe"}, log.New(io.Discard, "", 0))
		process.state = state
		if err := process.Exec(context.Background()); err == nil {
			t.Fatalf("Exec unexpectedly accepted %s", state)
		}
	}
}

func TestExecRunsDisabledAndFailedAngls(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
		state   ProcessState
		final   ProcessState
	}{
		{name: "disabled", enabled: false, state: StateDisabled, final: StateDisabled},
		{name: "failed", enabled: true, state: StateFailed, final: StateStopped},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enabled := tt.enabled
			process := NewProcess(tt.name, AnglDef{
				Command: "cmd.exe",
				Args:    []string{"/d", "/c", "ping -n 30 127.0.0.1 >nul"},
				Enabled: &enabled,
			}, log.New(io.Discard, "", 0))
			process.state = tt.state
			if err := process.Exec(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitForProcessState(t, process, StateRunning)
			if process.Status().Enabled != tt.enabled {
				t.Fatal("exec mutated durable enabled state")
			}
			if err := process.StopRunning(); err != nil {
				t.Fatal(err)
			}
			if got := process.Status().State; got != tt.final {
				t.Fatalf("state after stop = %s, want %s", got, tt.final)
			}
		})
	}
}

func waitForProcessState(t *testing.T, process *Process, want ProcessState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if process.Status().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", process.Status().State, want)
}

func TestConcurrentExecStartsOnlyOneLoop(t *testing.T) {
	process := NewProcess("once", AnglDef{Command: "cmd.exe", Args: []string{"/d", "/c", "ping -n 30 127.0.0.1 >nul"}}, log.New(io.Discard, "", 0))
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- process.Exec(context.Background())
		}()
	}
	close(start)
	accepted := 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted execs = %d, want 1", accepted)
	}
	process.Stop()
}

func TestStopRunningRejectsEveryNonRunningState(t *testing.T) {
	for _, state := range []ProcessState{StateDisabled, StateStopped, StateStarting, StateBackoff, StateFailed} {
		process := NewProcess("quiet", AnglDef{Command: "tool.exe"}, log.New(io.Discard, "", 0))
		process.state = state
		if err := process.StopRunning(); err == nil {
			t.Fatalf("StopRunning unexpectedly accepted %s", state)
		}
	}

	process := NewProcess("live", AnglDef{Command: "tool.exe"}, log.New(io.Discard, "", 0))
	process.state = StateRunning
	process.done = make(chan struct{})
	close(process.done)
	if err := process.StopRunning(); err != nil {
		t.Fatalf("StopRunning rejected running process: %v", err)
	}
}

func TestStartFailureHonorsLimitAndStaysFailed(t *testing.T) {
	process := NewProcess("missing", AnglDef{
		Command:     filepath.Join(t.TempDir(), "does-not-exist.exe"),
		MaxRestarts: 1,
	}, log.New(io.Discard, "", 0))
	process.Start(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := process.Status()
		if status.State == StateFailed {
			if status.Restarts != 1 {
				t.Fatalf("restarts = %d, want 1", status.Restarts)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", process.Status().State, StateFailed)
}

func TestDefChangedIncludesSupervisorSettings(t *testing.T) {
	base := AnglDef{Command: "tool.exe"}
	changed := base
	changed.MaxRestarts = 3
	if !defChanged(base, changed) {
		t.Fatal("max_restarts change was ignored")
	}

	changed = base
	changed.Endpoint = &EndpointDef{HTTP: "http://localhost:8080"}
	if !defChanged(base, changed) {
		t.Fatal("endpoint change was ignored")
	}
}
