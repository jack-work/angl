//go:build windows

package daemon

import (
	"bytes"
	"strings"
	"sync"
)

// RingBuffer holds the last N lines of process output and broadcasts new
// lines to live subscribers (for tail).
type RingBuffer struct {
	mu      sync.Mutex
	lines   []string
	head    int
	count   int
	cap     int
	subs    map[uint64]chan string
	nextSub uint64
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		lines: make([]string, capacity),
		cap:   capacity,
		subs:  make(map[uint64]chan string),
	}
}

func (r *RingBuffer) Push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	idx := (r.head + r.count) % r.cap
	if r.count == r.cap {
		r.head = (r.head + 1) % r.cap
	} else {
		r.count++
	}
	r.lines[idx] = line

	for id, ch := range r.subs {
		select {
		case ch <- line:
		default:
			delete(r.subs, id)
			close(ch)
		}
	}
}

func (r *RingBuffer) History(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n > r.count {
		n = r.count
	}
	result := make([]string, n)
	start := (r.head + r.count - n) % r.cap
	for i := 0; i < n; i++ {
		result[i] = r.lines[(start+i)%r.cap]
	}
	return result
}

func (r *RingBuffer) Subscribe() (uint64, <-chan string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ch := make(chan string, 64)
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	return id, ch
}

func (r *RingBuffer) Unsubscribe(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ch, ok := r.subs[id]; ok {
		delete(r.subs, id)
		close(ch)
	}
}

// LineWriter splits a raw byte stream into lines and pushes each to a
// RingBuffer. Attach it as cmd.Stdout / cmd.Stderr.
type LineWriter struct {
	buf  []byte
	ring *RingBuffer
}

func NewLineWriter(ring *RingBuffer) *LineWriter {
	return &LineWriter{ring: ring}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:idx]), "\r")
		w.buf = w.buf[idx+1:]
		w.ring.Push(line)
	}
	return len(p), nil
}

func (w *LineWriter) Flush() {
	if len(w.buf) > 0 {
		w.ring.Push(strings.TrimRight(string(w.buf), "\r"))
		w.buf = nil
	}
}
