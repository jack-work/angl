package logcodec

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testRecord() Record {
	return Record{
		TimeUnixNano:         "1784596530123456789",
		Time:                 "2026-07-21T01:15:30.123456789Z",
		ObservedTimeUnixNano: "1784596530123456789",
		SeverityText:         "WARN",
		SeverityNumber:       SeverityWarn,
		Body:                 "slow\nrequest\t\x1b[31mnot child red\x1b[0m",
		Attributes: map[string]any{
			"angl.name": "api\nspoof",
			"stream":    "stderr",
		},
		Resource: Resource{Attributes: map[string]any{"service.name": "angl"}},
	}
}

func TestRenderCleanTerminalLine(t *testing.T) {
	got := Render(testRecord(), RenderOptions{Location: time.UTC})
	want := "01:15:30.123 WARN  [api spoof/stderr]  slow request not child red"
	if got != want {
		t.Fatalf("Render()\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "\x1b") || strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("unsafe terminal output: %q", got)
	}
}

func TestRenderColorAndOptions(t *testing.T) {
	record := testRecord()
	got := Render(record, RenderOptions{Color: true, HideMetadata: true, TimestampLayout: time.RFC3339Nano})
	if !strings.Contains(got, "\x1b[33mWARN \x1b[0m") || strings.Contains(got, "[api") {
		t.Fatalf("colored render = %q", got)
	}
}

func TestRendererSupportsFragmentedFollowStream(t *testing.T) {
	var encoded bytes.Buffer
	if err := Convert(&encoded, strings.NewReader("one\nERROR two\nthree"), Options{
		Clock: fixedClock, Metadata: Metadata{Angl: "worker", Stream: Stdout},
	}); err != nil {
		t.Fatal(err)
	}

	var terminal bytes.Buffer
	renderer := NewRenderer(&terminal, RenderOptions{Location: time.UTC})
	data := encoded.Bytes()
	for start := 0; start < len(data); {
		end := start + 7
		if end > len(data) {
			end = len(data)
		}
		if n, err := renderer.Write(data[start:end]); err != nil || n != end-start {
			t.Fatalf("Write = %d, %v", n, err)
		}
		start = end
	}
	if err := renderer.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(terminal.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("rendered lines = %q", terminal.String())
	}
	if !strings.Contains(lines[0], "INFO  [worker/stdout]  one") ||
		!strings.Contains(lines[1], "ERROR [worker/stdout]  ERROR two") {
		t.Fatalf("rendered lines = %#v", lines)
	}
}

func TestParseRecordUnknownFieldsAndNilAttributes(t *testing.T) {
	record, err := ParseRecord([]byte(`{"time":"2026-07-20T00:00:00Z","body":"ok","future":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if record.Body != "ok" || record.Attributes == nil {
		t.Fatalf("record = %#v", record)
	}
}

func TestRendererRejectsMalformedRecord(t *testing.T) {
	var out bytes.Buffer
	renderer := NewRenderer(&out, RenderOptions{})
	n, err := renderer.Write([]byte("not json\nnext"))
	if err == nil || n != len("not json\n") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func BenchmarkRender(b *testing.B) {
	record := testRecord()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Render(record, RenderOptions{Location: time.UTC})
	}
}
