package logstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadLastUsesReverseSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(file, "line-%06d payload payload payload\n", i)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	lines, err := ReadLast(context.Background(), []Source{{Path: path}}, 3, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := lines[len(lines)-1].Text; got != "line-199999 payload payload payload" {
		t.Fatalf("last = %q", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("reverse tail too slow: %v", elapsed)
	}
}

func TestStreamReturnsBeforeLargeInitialTailIsDrained(t *testing.T) {
	var content strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintln(&content, i)
	}
	path := writeFile(t, t.TempDir(), "startup.log", content.String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer := mustTailer(t, []Source{{Path: path}}, Options{TailLines: 5000, EventBuffer: 1, MaxLinesPerPoll: 5000})
	returned := make(chan (<-chan Event), 1)
	go func() { returned <- tailer.Stream(ctx) }()
	select {
	case events := <-returned:
		if got := receiveLines(t, events, 1)[0].Text; got != "0" {
			t.Fatalf("first line = %q", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Stream blocked on initial event delivery")
	}
}

func TestReadLastMultipleSourcesStableOrderAndTags(t *testing.T) {
	dir := t.TempDir()
	first := writeFile(t, dir, "first.log", "a\nb\nc\n")
	second := writeFile(t, dir, "second.log", "x\r\ny\r\n")

	lines, err := ReadLast(context.Background(), []Source{
		{Name: "alpha", Path: first},
		{Name: "beta", Path: second},
	}, 2, Options{MaxLineBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, lines, []string{"alpha:b", "alpha:c", "beta:x", "beta:y"})
	for i, line := range lines {
		if line.Sequence != uint64(i+1) {
			t.Fatalf("sequence[%d] = %d", i, line.Sequence)
		}
	}
}

func TestReadLastIncludesFinalPartialAndClipsLongLines(t *testing.T) {
	path := writeFile(t, t.TempDir(), "long.log", "short\nabcdefghij")
	lines, err := ReadLast(context.Background(), []Source{{Path: path}}, 2, Options{MaxLineBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{lines[0].Text, lines[1].Text}; fmt.Sprint(got) != "[short abcde]" {
		t.Fatalf("lines = %q", got)
	}
	if lines[0].Truncated || !lines[1].Truncated {
		t.Fatalf("truncation flags = %v, %v", lines[0].Truncated, lines[1].Truncated)
	}
	if !lines[0].Terminated || lines[1].Terminated {
		t.Fatalf("terminators = %v, %v", lines[0].Terminated, lines[1].Terminated)
	}
	if lines[0].Source != "long.log" {
		t.Fatalf("default source = %q", lines[0].Source)
	}
}

func TestStreamInitialTailThenConcurrentAppendHasStableMerge(t *testing.T) {
	dir := t.TempDir()
	one := writeFile(t, dir, "one.log", "old-one\n")
	two := writeFile(t, dir, "two.log", "old-two\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tailer := mustTailer(t, []Source{{Name: "one", Path: one}, {Name: "two", Path: two}}, Options{
		TailLines: 1, PollInterval: 10 * time.Millisecond,
	})
	events := tailer.Stream(ctx)
	initial := receiveLines(t, events, 2)
	assertLines(t, initial, []string{"one:old-one", "two:old-two"})

	appendFile(t, one, "new-one\n")
	appendFile(t, two, "new-two\n")
	appended := receiveLines(t, events, 2)
	assertLines(t, appended, []string{"one:new-one", "two:new-two"})
	if appended[0].Sequence != 3 || appended[1].Sequence != 4 {
		t.Fatalf("sequences = %d, %d", appended[0].Sequence, appended[1].Sequence)
	}
}

func TestStreamZeroTailStartsAtEOFAndWaitsWithoutBusyLoop(t *testing.T) {
	path := writeFile(t, t.TempDir(), "quiet.log", "ignored\n")
	ctx, cancel := context.WithCancel(context.Background())
	tailer := mustTailer(t, []Source{{Path: path}}, Options{PollInterval: 80 * time.Millisecond})
	events := tailer.Stream(ctx)
	select {
	case event := <-events:
		t.Fatalf("unexpected initial event: %+v", event)
	case <-time.After(35 * time.Millisecond):
	}
	appendFile(t, path, "seen\n")
	lines := receiveLines(t, events, 1)
	if lines[0].Text != "seen" {
		t.Fatalf("text = %q", lines[0].Text)
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("event channel remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close channel")
	}
}

func TestStreamDoesNotEmitPartialUntilCompleted(t *testing.T) {
	path := writeFile(t, t.TempDir(), "partial.log", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := mustTailer(t, []Source{{Path: path}}, Options{PollInterval: 10 * time.Millisecond}).Stream(ctx)
	appendFile(t, path, "part")
	select {
	case event := <-events:
		t.Fatalf("partial line emitted: %+v", event)
	case <-time.After(40 * time.Millisecond):
	}
	appendFile(t, path, "ial\n")
	if got := receiveLines(t, events, 1)[0].Text; got != "partial" {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamDetectsTruncation(t *testing.T) {
	path := writeFile(t, t.TempDir(), "truncate.log", "old-data-long\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := mustTailer(t, []Source{{Path: path}}, Options{TailLines: 1, PollInterval: 10 * time.Millisecond}).Stream(ctx)
	_ = receiveLines(t, events, 1)
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := receiveLines(t, events, 1)[0].Text; got != "new" {
		t.Fatalf("after truncation = %q", got)
	}
}

func TestStreamDetectsTruncateAndRegrowPastOffset(t *testing.T) {
	path := writeFile(t, t.TempDir(), "regrow.log", "old-one\nold-two\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := mustTailer(t, []Source{{Path: path}}, Options{TailLines: 1, PollInterval: 10 * time.Millisecond}).Stream(ctx)
	_ = receiveLines(t, events, 1)
	// Rewrite with a distinct prefix and grow beyond the previous offset before
	// the next poll. Size-only truncation detection would skip this content.
	if err := os.WriteFile(path, []byte("new-one\nnew-two\nnew-three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := receiveLines(t, events, 3)
	assertLines(t, lines, []string{"regrow.log:new-one", "regrow.log:new-two", "regrow.log:new-three"})
}

func TestStreamDetectsRotationAndDrainsRenamedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "rotate.log", "before\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := mustTailer(t, []Source{{Name: "service", Path: path}}, Options{TailLines: 1, PollInterval: 15 * time.Millisecond}).Stream(ctx)
	_ = receiveLines(t, events, 1)

	rotated := filepath.Join(dir, "rotate.log.1")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	appendFile(t, rotated, "late-old\n")
	writeFile(t, dir, "rotate.log", "new-file\n")
	lines := receiveLines(t, events, 2)
	assertLines(t, lines, []string{"service:late-old", "service:new-file"})
}

func TestStreamRecoversWhenFileAppearsAndDeduplicatesErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.log")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := mustTailer(t, []Source{{Name: "later", Path: path}}, Options{PollInterval: 10 * time.Millisecond}).Stream(ctx)

	first := receiveEvent(t, events)
	if first.Err == nil || !errors.Is(first.Err, os.ErrNotExist) {
		t.Fatalf("first event = %+v", first)
	}
	select {
	case event := <-events:
		t.Fatalf("unchanged error repeated: %+v", event)
	case <-time.After(45 * time.Millisecond):
	}
	writeFile(t, dir, "later.log", "arrived\n")
	if got := receiveLines(t, events, 1)[0].Text; got != "arrived" {
		t.Fatalf("recovered line = %q", got)
	}
}

func TestStreamBackpressureIsBoundedAndCancellationUnblocks(t *testing.T) {
	var content strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintln(&content, i)
	}
	path := writeFile(t, t.TempDir(), "many.log", content.String())
	ctx, cancel := context.WithCancel(context.Background())
	events := mustTailer(t, []Source{{Path: path}}, Options{
		TailLines: 5000, EventBuffer: 1, MaxLinesPerPoll: 5000, MaxLineBytes: 16,
	}).Stream(ctx)
	time.Sleep(25 * time.Millisecond)
	cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("blocked producer did not honor cancellation")
		}
	}
}

func TestReadLastCancellationAndValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadLast(ctx, []Source{{Path: "unused"}}, 1, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	cases := []struct {
		sources []Source
		opts    Options
	}{
		{nil, Options{}},
		{[]Source{{Path: ""}}, Options{}},
		{[]Source{{Path: "x"}}, Options{MaxLineBytes: -1}},
		{[]Source{{Path: "x"}}, Options{PollInterval: -1}},
	}
	for _, tc := range cases {
		if _, err := New(tc.sources, tc.opts); err == nil {
			t.Fatalf("New(%+v, %+v) succeeded", tc.sources, tc.opts)
		}
	}
}

func TestReadLastDoesNotModifyFile(t *testing.T) {
	path := writeFile(t, t.TempDir(), "readonly.log", "one\ntwo\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ReadLast(context.Background(), []Source{{Path: path}}, 1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("file metadata changed: before=%v/%v after=%v/%v", before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
}

func TestParserTracksNewlineTermination(t *testing.T) {
	parser := newLineParser(32)
	_, lines := parser.consume([]byte("one\ntwo"), 10, Source{Name: "x"})
	if len(lines) != 1 || !lines[0].Terminated {
		t.Fatalf("terminated line = %#v", lines)
	}
	partial, ok := parser.flush(Source{Name: "x"})
	if !ok || partial.Terminated || partial.Text != "two" {
		t.Fatalf("partial line = %#v, %v", partial, ok)
	}
}

func TestParserMemoryIsCappedForHugeLine(t *testing.T) {
	parser := newLineParser(8)
	data := []byte(strings.Repeat("x", 1<<20) + "\n")
	_, lines := parser.consume(data, 1, Source{Name: "x", Path: "x"})
	if cap(parser.buf) > 8 || len(lines) != 1 || len(lines[0].Text) != 8 || !lines[0].Truncated {
		t.Fatalf("cap=%d line=%+v", cap(parser.buf), lines)
	}
}

func TestNoGoroutineLeakAcrossCancellation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	path := writeFile(t, t.TempDir(), "leak.log", "")
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch := mustTailer(t, []Source{{Path: path}, {Path: path}}, Options{PollInterval: time.Hour}).Stream(ctx)
		cancel()
		for range ch {
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("goroutines: baseline=%d after=%d", baseline, got)
	}
}

func BenchmarkReadLast100Lines(b *testing.B) {
	dir := b.TempDir()
	var content strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&content, "line %05d: %s\n", i, strings.Repeat("x", 80))
	}
	path := filepath.Join(dir, "bench.log")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(content.Len()))
	for i := 0; i < b.N; i++ {
		if _, err := ReadLast(context.Background(), []Source{{Path: path}}, 100, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLineParserLongLine(b *testing.B) {
	data := []byte(strings.Repeat("x", 1<<20) + "\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		parser := newLineParser(1024)
		if _, lines := parser.consume(data, 1, Source{}); len(lines) != 1 {
			b.Fatal("missing line")
		}
	}
}

func mustTailer(t *testing.T, sources []Source, opts Options) *Tailer {
	t.Helper()
	tailer, err := New(sources, opts)
	if err != nil {
		t.Fatal(err)
	}
	return tailer
}

func writeFile(t testing.TB, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendFile(t testing.TB, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("event channel closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func receiveLines(t *testing.T, events <-chan Event, count int) []Line {
	t.Helper()
	lines := make([]Line, 0, count)
	for len(lines) < count {
		event := receiveEvent(t, events)
		if event.Err != nil {
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
		lines = append(lines, event.Line)
	}
	return lines
}

func assertLines(t *testing.T, lines []Line, want []string) {
	t.Helper()
	got := make([]string, len(lines))
	for i, line := range lines {
		got[i] = line.Source + ":" + line.Text
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
}

var _ sync.Locker
