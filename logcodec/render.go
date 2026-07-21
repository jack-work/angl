package logcodec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// RenderOptions controls the compact human view of a canonical record.
type RenderOptions struct {
	Color           bool
	TimestampLayout string
	Location        *time.Location
	HideMetadata    bool
}

// ParseRecord parses one canonical JSON record. Unknown fields are ignored so
// newer producers remain readable by older terminal clients.
func ParseRecord(line []byte) (Record, error) {
	var record Record
	if err := json.Unmarshal(bytes.TrimSpace(line), &record); err != nil {
		return Record{}, fmt.Errorf("logcodec: parse record: %w", err)
	}
	if record.Attributes == nil {
		record.Attributes = map[string]any{}
	}
	return record, nil
}

// Render formats a record as a single terminal-safe line without a trailing
// newline. Newline/control bytes in body or metadata cannot corrupt the view.
func Render(record Record, opts RenderOptions) string {
	layout := opts.TimestampLayout
	if layout == "" {
		layout = "15:04:05.000"
	}
	timestamp := record.Time
	if t, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
		if opts.Location != nil {
			t = t.In(opts.Location)
		}
		timestamp = t.Format(layout)
	}

	level := record.SeverityText
	if level == "" {
		level = SeverityName(record.SeverityNumber)
	}
	level = fmt.Sprintf("%-5s", clean(level))
	if opts.Color {
		level = colorize(record.SeverityNumber, level)
	}

	var b strings.Builder
	b.WriteString(clean(timestamp))
	b.WriteByte(' ')
	b.WriteString(level)
	if !opts.HideMetadata {
		name, _ := record.Attributes["angl.name"].(string)
		stream, _ := record.Attributes["stream"].(string)
		label := clean(name)
		if stream != "" {
			if label != "" {
				label += "/"
			}
			label += clean(stream)
		}
		if label != "" {
			b.WriteString(" [")
			b.WriteString(label)
			b.WriteByte(']')
		}
	}
	b.WriteString("  ")
	b.WriteString(clean(record.Body))
	return b.String()
}

func colorize(severity int, text string) string {
	code := "37"
	switch {
	case severity >= SeverityFatal:
		code = "1;31"
	case severity >= SeverityError:
		code = "31"
	case severity >= SeverityWarn:
		code = "33"
	case severity >= SeverityInfo:
		code = "32"
	case severity >= SeverityDebug:
		code = "36"
	case severity >= SeverityTrace:
		code = "2;37"
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func clean(s string) string {
	s = stripANSI(s)
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r == utf8.RuneError && size == 1 {
			r = '�'
		}
		if unicode.IsControl(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = unicode.IsSpace(r)
	}
	return strings.TrimSpace(b.String())
}

// stripANSI removes CSI terminal escape sequences from untrusted child output.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) {
				c := s[i]
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Renderer is an io.WriteCloser that accepts arbitrarily fragmented canonical
// JSONL and writes terminal lines. Like Encoder, it has no line-size limit.
type Renderer struct {
	mu      sync.Mutex
	out     io.Writer
	opts    RenderOptions
	pending []byte
	closed  bool
	err     error
}

func NewRenderer(out io.Writer, opts RenderOptions) *Renderer {
	return &Renderer{out: out, opts: opts}
}

func (r *Renderer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, ErrClosed
	}
	if r.err != nil {
		return 0, r.err
	}
	consumed := 0
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			r.pending = append(r.pending, p...)
			return consumed + len(p), nil
		}
		r.pending = append(r.pending, p[:i]...)
		consumed += i + 1
		if err := r.emit(r.pending); err != nil {
			r.err = err
			return consumed, err
		}
		r.pending = r.pending[:0]
		p = p[i+1:]
	}
	return consumed, nil
}

func (r *Renderer) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if len(r.pending) == 0 {
		return nil
	}
	if err := r.emit(r.pending); err != nil {
		r.err = err
		return err
	}
	r.pending = r.pending[:0]
	return nil
}

func (r *Renderer) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.err
	}
	r.closed = true
	if r.err == nil && len(r.pending) > 0 {
		r.err = r.emit(r.pending)
		r.pending = r.pending[:0]
	}
	return r.err
}

func (r *Renderer) emit(line []byte) error {
	record, err := ParseRecord(line)
	if err != nil {
		return err
	}
	encoded := append([]byte(Render(record, r.opts)), '\n')
	n, err := r.out.Write(encoded)
	if err == nil && n != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("logcodec: write terminal line: %w", err)
	}
	return nil
}
