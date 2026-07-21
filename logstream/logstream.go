// Package logstream provides bounded, read-only tailing of one or more log
// files. It does not own, alter, or signal the processes which write them.
package logstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultPollInterval = 250 * time.Millisecond
	defaultMaxLineBytes = 1 << 20
	defaultReadBuffer   = 32 << 10
	defaultMaxRead      = 1 << 20
	defaultMaxLines     = 4096
	defaultEventBuffer  = 128
)

// Source identifies a log file. Name is copied onto every Line and defaults
// to the base name of Path when empty.
type Source struct {
	Name string
	Path string
}

// Options controls resource use and polling behavior.
type Options struct {
	// TailLines is the number of existing lines emitted when a source is first
	// opened. Zero starts at EOF.
	TailLines int
	// PollInterval controls how often all sources are checked. It must be
	// positive; the default is 250ms.
	PollInterval time.Duration
	// MaxLineBytes caps retained bytes per line. Longer lines are clipped and
	// marked Truncated. The default is 1MiB.
	MaxLineBytes int
	// ReadBufferBytes caps each filesystem read. The default is 32KiB.
	ReadBufferBytes int
	// MaxReadBytesPerPoll bounds bytes consumed from each source in one round.
	// The default is 1MiB.
	MaxReadBytesPerPoll int
	// MaxLinesPerPoll bounds lines emitted by each source in one round. The
	// default is 4096.
	MaxLinesPerPoll int
	// EventBuffer is the capacity of the returned event channel. The default
	// is 128. Backpressure stops further polling rather than growing memory.
	EventBuffer int
	// MaxHistoryBytes optionally caps reverse history discovery. Zero means no
	// byte cap: reading remains bounded to the suffix needed for TailLines.
	MaxHistoryBytes int64
}

// Line is one line read from a source. Text excludes the newline and a CR in
// a CRLF terminator. Terminated records whether a newline was present, allowing
// raw consumers to distinguish a final partial record. Sequence is globally
// monotonic within one Stream.
type Line struct {
	Source     string
	Path       string
	Text       string
	Sequence   uint64
	Truncated  bool
	Terminated bool
}

// Event carries either a Line or a non-fatal source error. When Err is not
// nil, Line is zero. Source errors are reported once per unchanged failure;
// polling and recovery continue.
type Event struct {
	Line Line
	Err  error
}

// SourceError identifies a filesystem operation which failed for a source.
type SourceError struct {
	Source string
	Path   string
	Op     string
	Err    error
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("logstream: %s %q (%s): %v", e.Op, e.Path, e.Source, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }

// Tailer is an immutable, reusable stream configuration.
type Tailer struct {
	sources []Source
	opts    Options
}

// New validates and copies a tailing configuration.
func New(sources []Source, opts Options) (*Tailer, error) {
	if len(sources) == 0 {
		return nil, errors.New("logstream: at least one source is required")
	}
	if opts.TailLines < 0 || opts.MaxLineBytes < 0 || opts.ReadBufferBytes < 0 ||
		opts.MaxReadBytesPerPoll < 0 || opts.MaxLinesPerPoll < 0 || opts.EventBuffer < 0 || opts.MaxHistoryBytes < 0 {
		return nil, errors.New("logstream: size and count options cannot be negative")
	}
	if opts.PollInterval < 0 {
		return nil, errors.New("logstream: poll interval cannot be negative")
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.MaxLineBytes == 0 {
		opts.MaxLineBytes = defaultMaxLineBytes
	}
	if opts.ReadBufferBytes == 0 {
		opts.ReadBufferBytes = defaultReadBuffer
	}
	if opts.MaxReadBytesPerPoll == 0 {
		opts.MaxReadBytesPerPoll = defaultMaxRead
	}
	if opts.MaxLinesPerPoll == 0 {
		opts.MaxLinesPerPoll = defaultMaxLines
	}
	if opts.EventBuffer == 0 {
		opts.EventBuffer = defaultEventBuffer
	}

	copied := make([]Source, len(sources))
	for i, source := range sources {
		if source.Path == "" {
			return nil, fmt.Errorf("logstream: source %d has an empty path", i)
		}
		if source.Name == "" {
			source.Name = filepath.Base(source.Path)
		}
		copied[i] = source
	}
	return &Tailer{sources: copied, opts: opts}, nil
}

// Stream starts following all configured sources. Existing tails and every
// polling round are merged in source configuration order, giving a stable
// order when multiple files become readable in the same round. The channel
// closes after ctx is canceled and in-flight filesystem calls return.
func (t *Tailer) Stream(ctx context.Context) <-chan Event {
	out := make(chan Event, t.opts.EventBuffer)
	ready := make(chan struct{})
	go t.run(ctx, out, ready)
	select {
	case <-ready:
	case <-ctx.Done():
	}
	return out
}

// ReadLast reads a bounded snapshot of the last n lines from each source.
// Files are read concurrently; results are merged in source order. Sequence
// starts at one. Unlike Stream, any source error is returned.
func ReadLast(ctx context.Context, sources []Source, n int, opts Options) ([]Line, error) {
	opts.TailLines = n
	t, err := New(sources, opts)
	if err != nil {
		return nil, err
	}
	return t.readLast(ctx)
}

func (t *Tailer) run(ctx context.Context, out chan<- Event, ready chan<- struct{}) {
	defer close(out)

	responses := make(chan pollResponse, len(t.sources))
	requests := make([]chan pollRequest, len(t.sources))
	for i, source := range t.sources {
		requests[i] = make(chan pollRequest)
		go runSource(ctx, i, source, t.opts, requests[i], responses)
	}

	var sequence uint64
	poll := func(initial bool, initialized chan<- struct{}) bool {
		for _, request := range requests {
			select {
			case request <- pollRequest{initial: initial}:
			case <-ctx.Done():
				if initialized != nil {
					close(initialized)
				}
				return false
			}
		}
		ordered := make([]pollResponse, len(requests))
		for range requests {
			select {
			case response := <-responses:
				ordered[response.index] = response
			case <-ctx.Done():
				if initialized != nil {
					close(initialized)
				}
				return false
			}
		}
		if initialized != nil {
			close(initialized)
		}
		for _, response := range ordered {
			if response.err != nil {
				select {
				case out <- Event{Err: response.err}:
				case <-ctx.Done():
					return false
				}
			}
			for _, line := range response.lines {
				sequence++
				line.Sequence = sequence
				select {
				case out <- Event{Line: line}:
				case <-ctx.Done():
					return false
				}
			}
		}
		return true
	}

	if !poll(true, ready) {
		return
	}
	ticker := time.NewTicker(t.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !poll(false, nil) {
				return
			}
		}
	}
}

func (t *Tailer) readLast(ctx context.Context) ([]Line, error) {
	type result struct {
		index int
		lines []Line
		err   error
	}
	results := make(chan result, len(t.sources))
	for i, source := range t.sources {
		go func(index int, source Source) {
			lines, err := snapshot(ctx, source, t.opts.TailLines, t.opts)
			select {
			case results <- result{index: index, lines: lines, err: err}:
			case <-ctx.Done():
			}
		}(i, source)
	}
	ordered := make([]result, len(t.sources))
	for range t.sources {
		select {
		case result := <-results:
			ordered[result.index] = result
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var lines []Line
	var sequence uint64
	for _, result := range ordered {
		if result.err != nil {
			return nil, result.err
		}
		for _, line := range result.lines {
			sequence++
			line.Sequence = sequence
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func sourceErr(source Source, op string, err error) error {
	return &SourceError{Source: source.Name, Path: source.Path, Op: op, Err: err}
}

// isNotExist is kept narrow so permission and malformed-path failures remain
// distinguishable to callers through errors.Is/errors.As.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
