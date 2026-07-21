//go:build windows

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jack-work/angl/catalog"
	"github.com/jack-work/angl/daemon"
)

func TestFormatCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		want    string
	}{
		{
			name:    "command without arguments",
			command: "tomb.exe",
			want:    "tomb.exe",
		},
		{
			name:    "simple arguments",
			command: "figaro.exe",
			args:    []string{"send", "-f", "--id", "abc123"},
			want:    "figaro.exe send -f --id abc123",
		},
		{
			name:    "quotes whitespace and empty arguments",
			command: `C:\Program Files\tool.exe`,
			args:    []string{"--message", "hello world", ""},
			want:    `"C:\Program Files\tool.exe" --message "hello world" ""`,
		},
		{
			name:    "quotes embedded quotes",
			command: "tool.exe",
			args:    []string{`say "hello"`, `C:\path with space\`},
			want:    `tool.exe "say \"hello\"" "C:\path with space\\"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatCommand(tt.command, tt.args); got != tt.want {
				t.Fatalf("formatCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListedStatusIncludesFullCommandLine(t *testing.T) {
	status := daemon.ProcessStatus{
		Name:    "monitor",
		Command: `C:\Program Files\figaro.exe`,
		Args:    []string{"send", "--", "a deliberately long prompt that must not be truncated"},
	}

	got := newListedStatus(status, nil)
	want := `"C:\Program Files\figaro.exe" send -- "a deliberately long prompt that must not be truncated"`
	if got.CommandLine != want {
		t.Fatalf("CommandLine = %q, want %q", got.CommandLine, want)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"command", "args", "command_line"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("JSON missing %q: %s", field, data)
		}
	}
	var commandLine string
	if err := json.Unmarshal(fields["command_line"], &commandLine); err != nil {
		t.Fatal(err)
	}
	if commandLine != want {
		t.Fatalf("JSON command_line = %q, want %q", commandLine, want)
	}
}

func TestSnipTableCell(t *testing.T) {
	if got, want := snipTableCell("0123456789", 8), "01234..."; got != want {
		t.Fatalf("snipTableCell() = %q, want %q", got, want)
	}
}

func TestFormatLabels(t *testing.T) {
	labels := map[string]string{"role": "runtime", "stack": "dracarys"}
	if got, want := formatLabels(labels), "role=runtime,stack=dracarys"; got != want {
		t.Fatalf("formatLabels() = %q, want %q", got, want)
	}
	if got := formatLabels(nil); got != "-" {
		t.Fatalf("formatLabels(nil) = %q", got)
	}
}

func TestResolveObservationNames(t *testing.T) {
	statuses := []daemon.ProcessStatus{{Name: "runtime"}, {Name: "loop"}, {Name: "other"}}
	store := catalog.New()
	if err := store.Annotate("runtime", map[string]string{"stack": "dracarys"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Annotate("loop", map[string]string{"stack": "dracarys"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveView("dracarys", "stack=dracarys"); err != nil {
		t.Fatal(err)
	}
	names, err := resolveObservationNames([]string{"other"}, "", "dracarys", statuses, store)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names, ","), "loop,other,runtime"; got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
}

func TestMetadataAttributes(t *testing.T) {
	got := metadataAttributes(map[string]string{"team": "orchard"})
	if got["angl.metadata.team"] != "orchard" {
		t.Fatalf("got %#v", got)
	}
}
