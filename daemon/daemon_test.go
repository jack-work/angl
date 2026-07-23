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

func TestSingWakesCurrentBackoffWait(t *testing.T) {
	process := NewProcess("singer", AnglDef{Command: "tool.exe"}, log.New(io.Discard, "", 0))
	process.mu.Lock()
	wake := process.beginBackoffLocked(time.Now(), time.Hour)
	process.mu.Unlock()

	waited := make(chan bool, 1)
	go func() { waited <- process.wait(context.Background(), time.Hour, wake) }()

	if err := process.Sing(); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-waited:
		if !completed {
			t.Fatal("sung wait reported cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("sing did not wake backoff")
	}
	if err := process.Sing(); err == nil {
		t.Fatal("second sing unexpectedly succeeded outside an active backoff wait")
	}
}

func TestSingRejectsNonBackoffState(t *testing.T) {
	process := NewProcess("quiet", AnglDef{Command: "tool.exe"}, log.New(io.Discard, "", 0))
	if err := process.Sing(); err == nil {
		t.Fatal("sing unexpectedly accepted a stopped angl")
	}
}

func TestSingUsesNormalBackoffProgression(t *testing.T) {
	if got, want := nextBackoff(initialBackoff), 4*time.Second; got != want {
		t.Fatalf("next backoff after a sung initial wait = %v, want %v", got, want)
	}
	if got := nextBackoff(maxBackoff); got != maxBackoff {
		t.Fatalf("capped next backoff = %v, want %v", got, maxBackoff)
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
