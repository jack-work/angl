//go:build windows

package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	initialBackoff   = 2 * time.Second
	maxBackoff       = 60 * time.Second
	backoffFactor    = 2
	healthyThreshold = 2 * time.Minute
	createNoWindow   = 0x08000000
	maxLogSize       = 10 * 1024 * 1024
)

type ProcessState string

const (
	StateDisabled ProcessState = "disabled"
	StateStopped  ProcessState = "stopped"
	StateStarting ProcessState = "starting"
	StateRunning  ProcessState = "running"
	StateBackoff  ProcessState = "backoff"
	StateFailed   ProcessState = "failed"
)

type ProcessStatus struct {
	Name        string       `json:"name"`
	State       ProcessState `json:"state"`
	Enabled     bool         `json:"enabled"`
	Command     string       `json:"command"`
	Args        []string     `json:"args,omitempty"`
	PID         int          `json:"pid,omitempty"`
	Started     string       `json:"started,omitempty"`
	Uptime      string       `json:"uptime,omitempty"`
	LastExit    string       `json:"last_exit,omitempty"`
	NextRun     string       `json:"next_run,omitempty"`
	NextRunIn   string       `json:"next_run_in,omitempty"`
	Restarts    int          `json:"restarts"`
	MaxRestarts int          `json:"max_restarts,omitempty"`
	Interval    string       `json:"interval,omitempty"`
	CreatedAt   string       `json:"created_at,omitempty"`
	Lifetime    string       `json:"lifetime,omitempty"`
	Endpoint    *EndpointDef `json:"endpoint,omitempty"`
	Charge      string       `json:"charge,omitempty"`
}

type Process struct {
	mu       sync.Mutex
	name     string
	def      AnglDef
	state    ProcessState
	pid      int
	started  time.Time
	lastExit time.Time
	nextRun  time.Time
	restarts int
	output   *RingBuffer
	logger   *log.Logger

	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
}

func NewProcess(name string, def AnglDef, logLines int, logger *log.Logger) *Process {
	state := StateStopped
	if !def.IsEnabled() {
		state = StateDisabled
	}
	return &Process{
		name:   name,
		def:    def,
		state:  state,
		output: NewRingBuffer(logLines),
		logger: logger,
	}
}

func (p *Process) Start(parentCtx context.Context) {
	p.mu.Lock()
	if p.state == StateRunning || p.state == StateStarting || p.state == StateBackoff {
		p.mu.Unlock()
		return
	}
	p.restarts = 0
	ctx, cancel := context.WithCancel(parentCtx)
	p.cancel = cancel
	p.done = make(chan struct{})
	p.wake = make(chan struct{}, 1)
	p.mu.Unlock()

	go p.runLoop(ctx)
}

// Wake interrupts the current backoff/interval wait, causing the next tick
// to fire immediately. Safe to call from any goroutine. No-op if the
// process is not waiting.
func (p *Process) Wake() {
	p.mu.Lock()
	ch := p.wake
	p.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *Process) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// IsRunning returns true if the process is currently executing (a turn may be in-flight).
func (p *Process) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == StateRunning || p.state == StateStarting
}

func (p *Process) Status() ProcessStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := ProcessStatus{
		Name:        p.name,
		State:       p.state,
		Enabled:     p.def.IsEnabled(),
		Command:     p.def.Command,
		Args:        p.def.Args,
		PID:         p.pid,
		Restarts:    p.restarts,
		MaxRestarts: p.def.MaxRestarts,
		Interval:    p.def.Interval,
		Charge:      p.def.Charge,
		Endpoint:    p.def.Endpoint,
	}
	if !p.started.IsZero() && p.state == StateRunning {
		s.Uptime = time.Since(p.started).Round(time.Second).String()
		s.Started = p.started.Format(time.RFC3339)
	}
	if p.def.CreatedAt != "" {
		s.CreatedAt = p.def.CreatedAt
		if created, err := time.Parse(time.RFC3339, p.def.CreatedAt); err == nil {
			s.Lifetime = time.Since(created).Round(time.Second).String()
		}
	}
	if !p.lastExit.IsZero() {
		s.LastExit = p.lastExit.Format(time.RFC3339)
	}
	if !p.nextRun.IsZero() && p.state == StateBackoff {
		s.NextRun = p.nextRun.Format(time.RFC3339)
		remaining := time.Until(p.nextRun).Round(time.Second)
		if remaining > 0 {
			s.NextRunIn = remaining.String()
		} else {
			s.NextRunIn = "imminent"
		}
	}
	return s
}

func (p *Process) runLoop(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		p.state = StateStopped
		p.pid = 0
		d := p.done
		p.done = nil
		p.cancel = nil
		p.mu.Unlock()
		if d != nil {
			close(d)
		}
	}()

	backoff := initialBackoff

	for {
		if port := p.def.Port(); port > 0 && p.def.KillExisting && IsPortInUse(port) {
			p.logger.Printf("[%s] port %d in use, killing holder", p.name, port)
			KillPortHolder(port)
		}

		p.mu.Lock()
		p.state = StateStarting
		p.mu.Unlock()

		logFile := p.openLogFile()
		lw := NewLineWriter(p.output)

		var stdout io.Writer = lw
		if logFile != nil {
			stdout = io.MultiWriter(lw, logFile)
		}

		p.logger.Printf("[%s] exec: %s %v", p.name, p.def.Command, p.def.Args)

		cmd := exec.Command(p.def.Command, p.def.Args...)
		cmd.Stdout = stdout
		cmd.Stderr = stdout
		cmd.Env = os.Environ()
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}

		startTime := time.Now()
		if err := cmd.Start(); err != nil {
			p.logger.Printf("[%s] start failed: %v", p.name, err)
			lw.Flush()
			if logFile != nil {
				logFile.Close()
			}

			now := time.Now()
			p.mu.Lock()
			p.state = StateBackoff
			p.lastExit = now
			p.nextRun = now.Add(backoff)
			p.mu.Unlock()

			if !p.wait(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		p.mu.Lock()
		p.state = StateRunning
		p.pid = cmd.Process.Pid
		p.started = startTime
		p.mu.Unlock()

		p.logger.Printf("[%s] running (pid %d)", p.name, cmd.Process.Pid)

		exitCh := make(chan error, 1)
		go func() { exitCh <- cmd.Wait() }()

		select {
		case err := <-exitCh:
			lw.Flush()
			if logFile != nil {
				logFile.Close()
			}
			elapsed := time.Since(startTime)
			p.logger.Printf("[%s] exited after %v: %v", p.name, elapsed.Round(time.Second), err)

			p.mu.Lock()
			p.pid = 0
			p.mu.Unlock()

			if p.def.Interval != "" {
				interval, _ := time.ParseDuration(p.def.Interval)
				now := time.Now()
				p.mu.Lock()
				p.state = StateBackoff
				p.lastExit = now
				p.nextRun = now.Add(interval)
				p.mu.Unlock()
				p.logger.Printf("[%s] next run in %v", p.name, interval)
				if !p.wait(ctx, interval) {
					return
				}
				continue
			}

			if elapsed > healthyThreshold {
				backoff = initialBackoff
			}

			restartNow := time.Now()
			p.mu.Lock()
			p.restarts++
			p.lastExit = restartNow
			max := p.def.MaxRestarts
			count := p.restarts
			if max > 0 && count >= max {
				p.state = StateFailed
				p.mu.Unlock()
				p.logger.Printf("[%s] max restarts reached (%d/%d) -- giving up", p.name, count, max)
				return
			}
			p.state = StateBackoff
			p.nextRun = restartNow.Add(backoff)
			p.mu.Unlock()

			p.logger.Printf("[%s] restart in %v (%d/%s)", p.name, backoff, count, restartLimit(max))
			if !p.wait(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)

		case <-ctx.Done():
			p.logger.Printf("[%s] stopping (pid %d)", p.name, cmd.Process.Pid)
			killProcessTree(cmd)
			<-exitCh
			lw.Flush()
			if logFile != nil {
				logFile.Close()
			}
			return
		}
	}
}

// wait returns true if the duration elapsed or the process was woken,
// false if ctx was cancelled.
func (p *Process) wait(ctx context.Context, d time.Duration) bool {
	p.mu.Lock()
	wake := p.wake
	p.mu.Unlock()

	select {
	case <-time.After(d):
		return true
	case <-wake:
		p.logger.Printf("[%s] woken early", p.name)
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *Process) openLogFile() *os.File {
	logDir := filepath.Join(DefaultConfigDir(), "logs")
	os.MkdirAll(logDir, 0755)
	path := filepath.Join(logDir, p.name+".log")
	rotateLog(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		p.logger.Printf("[%s] warning: cannot open log file: %v", p.name, err)
		return nil
	}
	return f
}

func rotateLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxLogSize {
		return
	}
	os.Remove(path + ".prev")
	os.Rename(path, path+".prev")
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		exec.Command("taskkill", "/t", "/f", "/pid", fmt.Sprintf("%d", cmd.Process.Pid)).Run()
	}
}

func restartLimit(max int) string {
	if max <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", max)
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * backoffFactor
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}
