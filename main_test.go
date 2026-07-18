//go:build windows

package main

import "testing"

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
