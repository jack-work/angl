// Package logcodec adapts existing plain process logs into stable,
// OpenTelemetry-inspired JSON Lines at observation time. It deliberately has
// no dependency on the angl daemon or its on-disk format: callers can apply it
// to both history readers and live/follow byte streams.
package logcodec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Stream identifies the child-process pipe from which a record came.
type Stream string

const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// OpenTelemetry severity numbers are ranges; these constants use the first
// number in each named range.
const (
	SeverityTrace = 1
	SeverityDebug = 5
	SeverityInfo  = 9
	SeverityWarn  = 13
	SeverityError = 17
	SeverityFatal = 21
)

var ErrClosed = errors.New("logcodec: adapter or renderer is closed")

// Metadata describes an angl and the process stream being observed.
type Metadata struct {
	Angl               string
	Stream             Stream
	Charge             string
	Command            string
	PID                int
	Attributes         map[string]any
	ResourceAttributes map[string]any
}

// Resource contains OTEL resource attributes shared by the emitting service.
type Resource struct {
	Attributes map[string]any `json:"attributes"`
}

// Record is the canonical JSONL schema. Field order is stable when marshaled.
// Nanosecond values are decimal strings, matching OTLP's lossless JSON encoding
// for uint64 and avoiding precision loss in JavaScript consumers. Time is an
// RFC3339 convenience field for jq, sed, awk, and humans.
type Record struct {
	TimeUnixNano         string         `json:"timeUnixNano"`
	Time                 string         `json:"time"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano"`
	SeverityText         string         `json:"severityText"`
	SeverityNumber       int            `json:"severityNumber"`
	Body                 string         `json:"body"`
	Attributes           map[string]any `json:"attributes"`
	Resource             Resource       `json:"resource"`
}

// Options controls conversion from process bytes to records.
type Options struct {
	Metadata Metadata

	// Clock is called exactly once per emitted record. It defaults to time.Now.
	Clock func() time.Time

	// ParseJSON recognizes JSON object logs, extracting common body, severity,
	// and timestamp fields. Safe original fields are copied to attributes using
	// the json.<field> namespace.
	ParseJSON bool

	// DefaultSeverity is used when no level can be inferred. Zero selects INFO
	// for stdout and ERROR for stderr.
	DefaultSeverity int
}

// Adapter is an io.WriteCloser for follow streams. It frames arbitrary writes
// into lines and emits one canonical JSON object plus '\n' for each line. It is
// safe for concurrent use and has no scanner token limit, so large lines stay
// intact. Close or Flush emits a final unterminated line.
type Adapter struct {
	mu      sync.Mutex
	out     io.Writer
	opts    Options
	pending []byte
	closed  bool
	err     error
}

func NewAdapter(out io.Writer, opts Options) *Adapter {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Metadata.Stream == "" {
		opts.Metadata.Stream = Stdout
	}
	return &Adapter{out: out, opts: opts}
}

// Convert adapts a finite history reader. For a live source, use NewAdapter and
// feed each follow chunk to Write so an incomplete final line remains pending.
func Convert(dst io.Writer, src io.Reader, opts Options) error {
	adapter := NewAdapter(dst, opts)
	if _, err := io.Copy(adapter, src); err != nil {
		return err
	}
	return adapter.Close()
}

func (a *Adapter) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return 0, ErrClosed
	}
	if a.err != nil {
		return 0, a.err
	}

	consumed := 0
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			a.pending = append(a.pending, p...)
			return consumed + len(p), nil
		}
		a.pending = append(a.pending, p[:i]...)
		line := a.pending
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		consumed += i + 1
		if err := a.emit(line); err != nil {
			a.err = err
			return consumed, err
		}
		a.pending = a.pending[:0]
		p = p[i+1:]
	}
	return consumed, nil
}

// Flush emits a final unterminated line. An empty buffer emits nothing.
func (a *Adapter) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	if len(a.pending) == 0 {
		return nil
	}
	line := a.pending
	if line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if err := a.emit(line); err != nil {
		a.err = err
		return err
	}
	a.pending = a.pending[:0]
	return nil
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return a.err
	}
	a.closed = true
	if a.err == nil && len(a.pending) > 0 {
		line := a.pending
		if line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		a.err = a.emit(line)
		a.pending = a.pending[:0]
	}
	return a.err
}

func (a *Adapter) emit(raw []byte) error {
	now := a.opts.Clock().UTC()
	body := validString(raw)
	severity := a.opts.DefaultSeverity
	if severity == 0 {
		severity = SeverityInfo
		if a.opts.Metadata.Stream == Stderr {
			severity = SeverityError
		}
	}
	eventTime := now
	var source map[string]any
	jsonLevel := false

	if a.opts.ParseJSON {
		if parsed, ok := parseJSONObject(raw); ok {
			source = parsed
			if value, found := first(parsed, "message", "msg", "body"); found {
				body = stringify(value)
			}
			if value, found := first(parsed, "severityText", "severity_text", "severity", "level", "loglevel"); found {
				if number, _, ok := ParseSeverity(stringify(value)); ok {
					severity = number
					jsonLevel = true
				}
			}
			if value, found := first(parsed, "time", "timestamp", "ts", "@timestamp"); found {
				if ts, ok := parseTimestamp(value); ok {
					eventTime = ts
				}
			}
		}
	}
	if !jsonLevel {
		if number, _, ok := ParseSeverity(body); ok {
			severity = number
		}
	}

	attrs := cloneAttributes(a.opts.Metadata.Attributes)
	attrs["stream"] = string(a.opts.Metadata.Stream)
	if a.opts.Metadata.Angl != "" {
		attrs["angl.name"] = a.opts.Metadata.Angl
	}
	if a.opts.Metadata.Charge != "" {
		attrs["angl.charge"] = a.opts.Metadata.Charge
	}
	if a.opts.Metadata.Command != "" {
		attrs["process.command"] = a.opts.Metadata.Command
	}
	if a.opts.Metadata.PID != 0 {
		attrs["process.pid"] = a.opts.Metadata.PID
	}
	preserveJSON(attrs, source)

	resource := cloneAttributes(a.opts.Metadata.ResourceAttributes)
	if _, exists := resource["service.name"]; !exists {
		resource["service.name"] = "angl"
	}

	record := Record{
		TimeUnixNano:         strconv.FormatInt(eventTime.UnixNano(), 10),
		Time:                 eventTime.Format(time.RFC3339Nano),
		ObservedTimeUnixNano: strconv.FormatInt(now.UnixNano(), 10),
		SeverityText:         SeverityName(severity),
		SeverityNumber:       severity,
		Body:                 body,
		Attributes:           attrs,
		Resource:             Resource{Attributes: resource},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("logcodec: marshal record: %w", err)
	}
	encoded = append(encoded, '\n')
	n, err := a.out.Write(encoded)
	if err == nil && n != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("logcodec: write record: %w", err)
	}
	return nil
}

func cloneAttributes(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src)+6)
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func preserveJSON(attrs map[string]any, source map[string]any) {
	for key, value := range source {
		if key == "" || !safeAttributeValue(value) {
			continue
		}
		attributeKey := "json." + key
		if _, exists := attrs[attributeKey]; !exists {
			attrs[attributeKey] = value
		}
	}
}

// OTEL attributes are scalars or homogeneous arrays of scalar values. Keep the
// adapter conservative: nested objects and mixed arrays remain in the body.
func safeAttributeValue(value any) bool {
	switch v := value.(type) {
	case nil, string, bool, json.Number:
		return true
	case []any:
		kind := ""
		for _, item := range v {
			var current string
			switch item.(type) {
			case string:
				current = "string"
			case bool:
				current = "bool"
			case json.Number:
				current = "number"
			default:
				return false
			}
			if kind != "" && kind != current {
				return false
			}
			kind = current
		}
		return true
	default:
		return false
	}
}

func validString(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

func parseJSONObject(raw []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil || value == nil {
		return nil, false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, false
	}
	return value, true
}

func first(m map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func stringify(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(b)
}

func parseTimestamp(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC(), true
			}
		}
	case json.Number:
		if n, err := strconv.ParseFloat(v.String(), 64); err == nil {
			return unixTimestamp(n), true
		}
	case float64:
		return unixTimestamp(v), true
	}
	return time.Time{}, false
}

func unixTimestamp(value float64) time.Time {
	if math.Abs(value) > 1e12 { // conventional JSON epoch milliseconds
		value /= 1000
	}
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(fraction*1e9)).UTC()
}
