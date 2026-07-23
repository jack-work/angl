//go:build windows

package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jack-work/angl/catalog"
	"github.com/jack-work/angl/daemon"
	"github.com/jedib0t/go-pretty/v6/text"
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

func TestRenderListTableFitsTerminalWidth(t *testing.T) {
	statuses := []daemon.ProcessStatus{
		{Name: "a-very-long-process-name", State: daemon.StateRunning, PID: 12345, Uptime: "12m3s", Command: `C:\\Program Files\\tool.exe`, Args: []string{"--message", strings.Repeat("long ", 30)}, Charge: strings.Repeat("charge ", 20)},
	}
	store := catalog.New()
	store.Labels[statuses[0].Name] = map[string]string{"stack": "dracarys", "role": "runtime"}
	for _, width := range []int{32, 40, 47, 48, 60, 71, 72, 80, 99, 100, 120, 129, 130, 160, 169, 170, 200} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			got := renderListTable(statuses, store, width)
			for lineNumber, line := range strings.Split(got, "\n") {
				if gotWidth := text.StringWidthWithoutEscSequences(line); gotWidth > width {
					t.Fatalf("line %d width = %d, want <= %d:\n%s", lineNumber+1, gotWidth, width, got)
				}
			}
		})
	}
}

func TestRenderListDetailWrapsFullVisibleValues(t *testing.T) {
	item := daemon.InventoryItem{
		ProcessStatus: daemon.ProcessStatus{
			Name: "detail", State: daemon.StateBackoff, Command: "tool.exe",
			Args:   []string{strings.Repeat("argument ", 12) + "FULL-COMMAND-TAIL"},
			Charge: strings.Repeat("charge ", 10) + "FULL-CHARGE-TAIL",
		},
		Metadata: map[string]string{"note": strings.Repeat("metadata ", 8) + "FULL-METADATA-TAIL"},
	}
	for _, width := range []int{48, 80, 120, 180} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			got := renderListDetail(item, width)
			lines := strings.Split(got, "\n")
			for lineNumber, line := range lines {
				if gotWidth := text.StringWidthWithoutEscSequences(line); gotWidth > width {
					t.Fatalf("line %d width = %d, want <= %d:\n%s", lineNumber+1, gotWidth, width, got)
				}
			}
			if strings.Contains(got, "...") {
				t.Fatalf("detail snipped a visible value at width %d:\n%s", width, got)
			}
			if len(lines) <= 5 {
				t.Fatalf("long visible values did not wrap at width %d:\n%s", width, got)
			}
		})
	}
}

func TestStatusMatchesCoreFieldsAndMetadata(t *testing.T) {
	status := daemon.ProcessStatus{Name: "api", State: daemon.StateRunning, Enabled: true, Interval: "1h"}
	for selectorText, want := range map[string]bool{
		"name=api,state=running,enabled=true,kind=heartbeat,stack=apps": true,
		"state=stopped":   false,
		"enabled=false":   false,
		"kind=persistent": false,
	} {
		selector, err := catalog.ParseSelector(selectorText)
		if err != nil {
			t.Fatal(err)
		}
		if got := statusMatches(selector, status, map[string]string{"stack": "apps"}); got != want {
			t.Errorf("%q = %v, want %v", selectorText, got, want)
		}
	}
}

func TestResolveObservationNames(t *testing.T) {
	statuses := []daemon.ProcessStatus{
		{Name: "runtime", State: daemon.StateRunning, Enabled: true},
		{Name: "loop", State: daemon.StateStopped, Enabled: false},
		{Name: "other", State: daemon.StateRunning, Enabled: true},
	}
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
	names, err := resolveObservationNames(nil, "", "dracarys", statuses, store)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names, ","), "loop,runtime"; got != want {
		t.Fatalf("view names = %q, want %q", got, want)
	}

	names, err = resolveObservationNames([]string{"other", "runtime"}, "state=running", "dracarys", statuses, store)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names, ","), "runtime"; got != want {
		t.Fatalf("intersection names = %q, want %q", got, want)
	}

	names, err = resolveObservationNames(nil, "", "", statuses, store)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names, ","), "loop,other,runtime"; got != want {
		t.Fatalf("all names = %q, want %q", got, want)
	}
}

func TestMetadataAttributes(t *testing.T) {
	got := metadataAttributes(map[string]string{"team": "orchard"})
	if got["angl.metadata.team"] != "orchard" {
		t.Fatalf("got %#v", got)
	}
}
