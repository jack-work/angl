//go:build windows

package main

import (
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatCommand(tt.command, tt.args); got != tt.want {
				t.Fatalf("formatCommand() = %q, want %q", got, tt.want)
			}
		})
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
