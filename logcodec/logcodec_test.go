package logcodec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, 7, 20, 21, 15, 30, 123456789, time.FixedZone("EDT", -4*60*60))

func fixedClock() time.Time { return fixedTime }

func decodeRecords(t *testing.T, data []byte) []Record {
	t.Helper()
	var records []Record
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestAdapterPartialLinesCRLFAndFlush(t *testing.T) {
	var out bytes.Buffer
	adapter := NewAdapter(&out, Options{
		Clock: fixedClock,
		Metadata: Metadata{
			Angl: "api", Stream: Stdout, Charge: "serve", Command: "api.exe", PID: 42,
			Attributes:         map[string]any{"deployment.environment": "dev"},
			ResourceAttributes: map[string]any{"host.name": "stable"},
		},
	})
	for _, chunk := range []string{"hel", "lo\r", "\nWARN sec", "ond\nfinal\r"} {
		if n, err := adapter.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	records := decodeRecords(t, out.Bytes())
	if len(records) != 3 {
		t.Fatalf("got %d records: %s", len(records), out.String())
	}
	wantBodies := []string{"hello", "WARN second", "final"}
	wantSeverities := []int{SeverityInfo, SeverityWarn, SeverityInfo}
	for i, record := range records {
		if record.Body != wantBodies[i] || record.SeverityNumber != wantSeverities[i] {
			t.Errorf("record[%d] = body %q severity %d", i, record.Body, record.SeverityNumber)
		}
		if record.Time != "2026-07-21T01:15:30.123456789Z" || record.TimeUnixNano != "1784596530123456789" {
			t.Errorf("unexpected deterministic time: %#v", record)
		}
		if record.ObservedTimeUnixNano != record.TimeUnixNano {
			t.Errorf("observed time = %q, event time = %q", record.ObservedTimeUnixNano, record.TimeUnixNano)
		}
		if record.Attributes["angl.name"] != "api" || record.Attributes["stream"] != "stdout" {
			t.Errorf("missing angl metadata: %#v", record.Attributes)
		}
		if record.Resource.Attributes["service.name"] != "angl" || record.Resource.Attributes["host.name"] != "stable" {
			t.Errorf("resource = %#v", record.Resource)
		}
	}
	if _, err := adapter.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v", err)
	}
}

func TestConvertHistory(t *testing.T) {
	var out bytes.Buffer
	err := Convert(&out, strings.NewReader("one\ntwo-without-newline"), Options{Clock: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, out.Bytes())
	if len(records) != 2 || records[0].Body != "one" || records[1].Body != "two-without-newline" {
		t.Fatalf("records = %#v", records)
	}
}

func TestJSONExtractionAndSafeFieldPreservation(t *testing.T) {
	input := `{"time":"2026-07-20T12:00:00.123Z","level":"error","message":"database unavailable","attempt":3,"ready":false,"tags":["db","retry"],"nested":{"secret":"x"},"mixed":[1,"two"]}` + "\n"
	var out bytes.Buffer
	if err := Convert(&out, strings.NewReader(input), Options{Clock: fixedClock, ParseJSON: true}); err != nil {
		t.Fatal(err)
	}
	record := decodeRecords(t, out.Bytes())[0]
	if record.Body != "database unavailable" || record.SeverityNumber != SeverityError {
		t.Fatalf("body/severity = %q/%d", record.Body, record.SeverityNumber)
	}
	if record.Time != "2026-07-20T12:00:00.123Z" || record.ObservedTimeUnixNano != "1784596530123456789" {
		t.Fatalf("times = %s / %s", record.Time, record.ObservedTimeUnixNano)
	}
	for _, key := range []string{"json.time", "json.level", "json.message", "json.attempt", "json.ready", "json.tags"} {
		if _, ok := record.Attributes[key]; !ok {
			t.Errorf("safe JSON field %q not preserved: %#v", key, record.Attributes)
		}
	}
	for _, key := range []string{"json.nested", "json.mixed"} {
		if _, ok := record.Attributes[key]; ok {
			t.Errorf("unsafe JSON field %q preserved", key)
		}
	}
}

func TestMalformedJSONAndInvalidUTF8RemainUsable(t *testing.T) {
	var out bytes.Buffer
	adapter := NewAdapter(&out, Options{Clock: fixedClock, ParseJSON: true, Metadata: Metadata{Stream: Stderr}})
	_, err := adapter.Write(append([]byte(`{"message":`), 0xff, '\n'))
	if err != nil {
		t.Fatal(err)
	}
	record := decodeRecords(t, out.Bytes())[0]
	if !strings.Contains(record.Body, "�") || record.SeverityNumber != SeverityError {
		t.Fatalf("record = %#v", record)
	}
}

func TestLargeLine(t *testing.T) {
	body := strings.Repeat("x", 2<<20)
	var out bytes.Buffer
	if err := Convert(&out, strings.NewReader(body+"\n"), Options{Clock: fixedClock}); err != nil {
		t.Fatal(err)
	}
	record := decodeRecords(t, out.Bytes())[0]
	if record.Body != body {
		t.Fatalf("large body length = %d, want %d", len(record.Body), len(body))
	}
}

func TestEmptyLinesAreRecords(t *testing.T) {
	var out bytes.Buffer
	if err := Convert(&out, strings.NewReader("\n\r\n"), Options{Clock: fixedClock}); err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, out.Bytes())
	if len(records) != 2 || records[0].Body != "" || records[1].Body != "" {
		t.Fatalf("records = %#v", records)
	}
}

func TestAttributeInputsAreNotMutated(t *testing.T) {
	attrs := map[string]any{"stream": "caller", "angl.name": "caller"}
	resources := map[string]any{}
	var out bytes.Buffer
	if err := Convert(&out, strings.NewReader("ok\n"), Options{
		Clock:    fixedClock,
		Metadata: Metadata{Angl: "actual", Stream: Stderr, Attributes: attrs, ResourceAttributes: resources},
	}); err != nil {
		t.Fatal(err)
	}
	if attrs["stream"] != "caller" || attrs["angl.name"] != "caller" || len(resources) != 0 {
		t.Fatalf("caller maps mutated: %#v %#v", attrs, resources)
	}
}

func TestOutputErrorIsStickyAndReportsConsumedBytes(t *testing.T) {
	want := errors.New("disk full")
	adapter := NewAdapter(errorWriter{want}, Options{Clock: fixedClock})
	n, err := adapter.Write([]byte("first\nsecond"))
	if !errors.Is(err, want) || n != len("first\n") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, err = adapter.Write([]byte("later")); !errors.Is(err, want) || n != 0 {
		t.Fatalf("second Write = %d, %v", n, err)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestParseSeverity(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{"[DBG] request", SeverityDebug},
		{"2026-07-20 warning: slow", SeverityWarn},
		{"level=critical stopped", SeverityFatal},
		{"ordinary message", 0},
	}
	for _, tc := range tests {
		got, _, _ := ParseSeverity(tc.line)
		if got != tc.want {
			t.Errorf("ParseSeverity(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func FuzzAdapterNeverEmitsInvalidJSON(f *testing.F) {
	f.Add([]byte("hello\r\nwarn: oh no\npartial"))
	f.Add([]byte{0xff, '\n', 0x00})
	f.Fuzz(func(t *testing.T, input []byte) {
		var out bytes.Buffer
		if err := Convert(&out, bytes.NewReader(input), Options{Clock: fixedClock, ParseJSON: true}); err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), []byte{'\n'}) {
			if len(line) > 0 && !json.Valid(line) {
				t.Fatalf("invalid JSON: %q", line)
			}
		}
	})
}

func BenchmarkAdapterPlain(b *testing.B) {
	line := []byte("2026-07-20 INFO handled request route=/health latency=12ms\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		adapter := NewAdapter(io.Discard, Options{Clock: fixedClock, Metadata: Metadata{Angl: "api"}})
		_, _ = adapter.Write(line)
	}
}

func BenchmarkAdapterJSON(b *testing.B) {
	line := []byte(`{"time":"2026-07-20T12:00:00Z","level":"info","message":"handled request","status":200}` + "\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		adapter := NewAdapter(io.Discard, Options{Clock: fixedClock, ParseJSON: true})
		_, _ = adapter.Write(line)
	}
}

func BenchmarkAdapterLargeLine(b *testing.B) {
	line := append(bytes.Repeat([]byte("x"), 1<<20), '\n')
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		adapter := NewAdapter(io.Discard, Options{Clock: fixedClock})
		_, _ = adapter.Write(line)
	}
}
