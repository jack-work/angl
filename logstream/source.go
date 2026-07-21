package logstream

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
)

type pollRequest struct{ initial bool }

type pollResponse struct {
	index int
	lines []Line
	err   error
}

type sourceState struct {
	source  Source
	opts    Options
	file    *os.File
	offset  int64
	info    os.FileInfo
	parser  lineParser
	lastErr string
}

func runSource(ctx context.Context, index int, source Source, opts Options, requests <-chan pollRequest, responses chan<- pollResponse) {
	state := sourceState{source: source, opts: opts, parser: newLineParser(opts.MaxLineBytes)}
	defer func() {
		if state.file != nil {
			_ = state.file.Close()
		}
	}()
	for {
		select {
		case request := <-requests:
			lines, err := state.poll(request.initial)
			if err != nil {
				key := err.Error()
				if key == state.lastErr {
					err = nil
				} else {
					state.lastErr = key
				}
			} else {
				state.lastErr = ""
			}
			select {
			case responses <- pollResponse{index: index, lines: lines, err: err}:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *sourceState) poll(initial bool) ([]Line, error) {
	if s.file == nil {
		return s.open(initial)
	}

	pathInfo, err := os.Stat(s.source.Path)
	if err != nil {
		// Keep reading an already-open renamed file while its replacement is
		// temporarily absent. Report the path failure, but do not discard data.
		lines, readErr := s.readAvailable(s.opts.MaxReadBytesPerPoll, s.opts.MaxLinesPerPoll)
		if readErr != nil {
			return lines, readErr
		}
		return lines, sourceErr(s.source, "stat", err)
	}
	currentInfo, err := s.file.Stat()
	if err != nil {
		return nil, sourceErr(s.source, "stat open file", err)
	}

	if !os.SameFile(currentInfo, pathInfo) {
		// Drain bytes already present in the renamed file before switching.
		lines, err := s.readAvailable(s.opts.MaxReadBytesPerPoll, s.opts.MaxLinesPerPoll)
		if err != nil {
			return lines, err
		}
		if len(lines) == s.opts.MaxLinesPerPoll || currentInfo.Size() > s.offset {
			return lines, nil
		}
		if line, ok := s.parser.flush(s.source); ok {
			lines = append(lines, line)
		}
		_ = s.file.Close()
		s.file = nil
		opened, openErr := s.open(false)
		return append(lines, opened...), openErr
	}

	if pathInfo.Size() < s.offset {
		s.offset = 0
		s.parser.reset()
		if _, err := s.file.Seek(0, io.SeekStart); err != nil {
			return nil, sourceErr(s.source, "seek after truncation", err)
		}
	}
	s.info = pathInfo
	return s.readAvailable(s.opts.MaxReadBytesPerPoll, s.opts.MaxLinesPerPoll)
}

func (s *sourceState) open(initial bool) ([]Line, error) {
	file, err := openRead(s.source.Path)
	if err != nil {
		return nil, sourceErr(s.source, "open", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, sourceErr(s.source, "stat", err)
	}
	s.file, s.info = file, info
	s.parser.reset()

	if initial && s.opts.TailLines > 0 {
		lines, err := snapshotFile(file, info.Size(), s.source, s.opts.TailLines, s.opts)
		if err != nil {
			_ = file.Close()
			s.file = nil
			return nil, sourceErr(s.source, "read initial tail", err)
		}
		s.offset = info.Size()
		if _, err := file.Seek(s.offset, io.SeekStart); err != nil {
			return lines, sourceErr(s.source, "seek", err)
		}
		return lines, nil
	}

	if initial { // TailLines == 0 means follow new data only.
		s.offset = info.Size()
	} else { // A newly appeared or rotated file is new data; read from start.
		s.offset = 0
	}
	_, err = file.Seek(s.offset, io.SeekStart)
	if err != nil {
		return nil, sourceErr(s.source, "seek", err)
	}
	if initial {
		return nil, nil
	}
	return s.readAvailable(s.opts.MaxReadBytesPerPoll, s.opts.MaxLinesPerPoll)
}

func (s *sourceState) readAvailable(byteLimit, lineLimit int) ([]Line, error) {
	bufferSize := min(s.opts.ReadBufferBytes, byteLimit)
	buffer := make([]byte, bufferSize)
	var lines []Line
	remaining := byteLimit
	for remaining > 0 && len(lines) < lineLimit {
		want := min(len(buffer), remaining)
		n, err := s.file.Read(buffer[:want])
		if n > 0 {
			previousOffset := s.offset
			consumed, parsed := s.parser.consume(buffer[:n], lineLimit-len(lines), s.source)
			lines = append(lines, parsed...)
			s.offset += int64(consumed)
			remaining -= consumed
			if consumed < n {
				if _, seekErr := s.file.Seek(previousOffset+int64(consumed), io.SeekStart); seekErr != nil {
					return lines, sourceErr(s.source, "rewind", seekErr)
				}
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return lines, sourceErr(s.source, "read", err)
		}
		if errors.Is(err, io.EOF) || n == 0 {
			break
		}
	}
	if info, err := s.file.Stat(); err == nil {
		s.info = info
	}
	return lines, nil
}

// snapshot opens and scans a stable file handle. It is intentionally
// read-only; concurrent appends beyond the size observed at open are excluded.
func snapshot(ctx context.Context, source Source, n int, opts Options) ([]Line, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openRead(source.Path)
	if err != nil {
		return nil, sourceErr(source, "open", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, sourceErr(source, "stat", err)
	}
	lines, err := snapshotFileContext(ctx, file, info.Size(), source, n, opts)
	if err != nil {
		return nil, sourceErr(source, "read", err)
	}
	return lines, nil
}

func snapshotFile(file *os.File, size int64, source Source, n int, opts Options) ([]Line, error) {
	return snapshotFileContext(context.Background(), file, size, source, n, opts)
}

func snapshotFileContext(ctx context.Context, file *os.File, size int64, source Source, n int, opts Options) ([]Line, error) {
	if n == 0 {
		return nil, nil
	}
	start, err := reverseTailStart(ctx, file, size, n, opts)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, size-start), opts.ReadBufferBytes)
	parser := newLineParser(opts.MaxLineBytes)
	ring := newLineRing(n)
	buffer := make([]byte, opts.ReadBufferBytes)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			_, lines := parser.consume(buffer[:count], int(^uint(0)>>1), source)
			for _, line := range lines {
				ring.add(line)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if line, ok := parser.flush(source); ok {
		ring.add(line)
	}
	return ring.lines(), nil
}

func reverseTailStart(ctx context.Context, file *os.File, size int64, lines int, opts Options) (int64, error) {
	if size == 0 || lines <= 0 {
		return size, nil
	}
	const blockSize int64 = 64 << 10
	position := size
	found := 0
	// A trailing newline terminates a line but does not require one extra
	// historical record, so skip it during delimiter counting.
	skipTrailing := true
	for position > 0 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		readSize := min(blockSize, position)
		if opts.MaxHistoryBytes > 0 && size-(position-readSize) > opts.MaxHistoryBytes {
			readSize = opts.MaxHistoryBytes - (size - position)
			if readSize <= 0 {
				return position, nil
			}
		}
		position -= readSize
		buffer := make([]byte, readSize)
		if _, err := file.ReadAt(buffer, position); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := len(buffer) - 1; i >= 0; i-- {
			if buffer[i] != '\n' {
				continue
			}
			if skipTrailing && position+int64(i) == size-1 {
				skipTrailing = false
				continue
			}
			skipTrailing = false
			found++
			if found == lines {
				return position + int64(i) + 1, nil
			}
		}
		if opts.MaxHistoryBytes > 0 && size-position >= opts.MaxHistoryBytes {
			return position, nil
		}
	}
	return 0, nil
}
