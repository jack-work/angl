//go:build windows

package daemon

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

type Config struct {
	Angls map[string]AnglDef `json:"angls"`
}

type AnglDef struct {
	Command      string       `json:"command"`
	Args         []string     `json:"args,omitempty"`
	Enabled      *bool        `json:"enabled,omitempty"`
	Endpoint     *EndpointDef `json:"endpoint,omitempty"`
	Interval     string       `json:"interval,omitempty"`
	MaxRestarts  int          `json:"max_restarts,omitempty"`
	KillExisting bool         `json:"kill_existing,omitempty"`
	Charge       string       `json:"charge,omitempty"`
	CreatedAt    string       `json:"created_at,omitempty"`
}

type EndpointDef struct {
	HTTP string `json:"http,omitempty"`
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid angl name %q (use letters, digits, '.', '_' or '-')", name)
	}
	return nil
}

func (a AnglDef) Validate() error {
	if strings.TrimSpace(a.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if a.MaxRestarts < 0 {
		return fmt.Errorf("max_restarts cannot be negative")
	}
	if a.Interval != "" {
		interval, err := time.ParseDuration(a.Interval)
		if err != nil || interval <= 0 {
			return fmt.Errorf("invalid interval %q", a.Interval)
		}
	}
	return nil
}

func (a AnglDef) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

func (a AnglDef) Port() int {
	if a.Endpoint == nil || a.Endpoint.HTTP == "" {
		return 0
	}
	parsed, err := url.Parse(a.Endpoint.HTTP)
	if err != nil || parsed.Port() == "" {
		return 0
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

func DefaultConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "angl")
}

func DefaultConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "config.json")
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{Angls: make(map[string]AnglDef)}, nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Angls == nil {
		cfg.Angls = make(map[string]AnglDef)
	}
	for name, def := range cfg.Angls {
		if err := ValidateName(name); err != nil {
			return Config{}, err
		}
		if err := def.Validate(); err != nil {
			return Config{}, fmt.Errorf("angl %q: %w", name, err)
		}
	}
	return cfg, nil
}

func SaveConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
