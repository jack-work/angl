package logstream

// lineParser retains at most max bytes regardless of input line length.
type lineParser struct {
	max       int
	buf       []byte
	active    bool
	truncated bool
}

func newLineParser(max int) lineParser {
	return lineParser{max: max, buf: make([]byte, 0, min(max, 4096))}
}

func (p *lineParser) reset() {
	p.buf = p.buf[:0]
	p.active = false
	p.truncated = false
}

// consume returns the bytes consumed, allowing the caller to rewind when the
// per-poll line budget is reached midway through a read.
func (p *lineParser) consume(data []byte, lineLimit int, source Source) (int, []Line) {
	lines := make([]Line, 0, min(lineLimit, 16))
	for index, value := range data {
		p.active = true
		if value == '\n' {
			line := p.line(source)
			line.Terminated = true
			lines = append(lines, line)
			p.reset()
			if len(lines) == lineLimit {
				return index + 1, lines
			}
			continue
		}
		if len(p.buf) < p.max {
			p.buf = append(p.buf, value)
		} else {
			p.truncated = true
		}
	}
	return len(data), lines
}

func (p *lineParser) flush(source Source) (Line, bool) {
	if !p.active {
		return Line{}, false
	}
	line := p.line(source)
	p.reset()
	return line, true
}

func (p *lineParser) line(source Source) Line {
	text := p.buf
	if len(text) > 0 && text[len(text)-1] == '\r' {
		text = text[:len(text)-1]
	}
	return Line{Source: source.Name, Path: source.Path, Text: string(text), Truncated: p.truncated}
}

type lineRing struct {
	items []Line
	next  int
	full  bool
}

func newLineRing(capacity int) *lineRing {
	return &lineRing{items: make([]Line, 0, capacity)}
}

func (r *lineRing) add(line Line) {
	if len(r.items) < cap(r.items) {
		r.items = append(r.items, line)
		return
	}
	r.items[r.next] = line
	r.next = (r.next + 1) % len(r.items)
	r.full = true
}

func (r *lineRing) lines() []Line {
	if !r.full {
		return append([]Line(nil), r.items...)
	}
	result := make([]Line, 0, len(r.items))
	result = append(result, r.items[r.next:]...)
	result = append(result, r.items[:r.next]...)
	return result
}
