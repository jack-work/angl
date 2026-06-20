//go:build windows

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Daemon  DaemonConfig       `json:"daemon"`
	Links   LinksConfig        `json:"links,omitempty"`
	Orchard OrchardConfig      `json:"orchard,omitempty"`
	Angls   map[string]AnglDef `json:"angls"`
}

type DaemonConfig struct {
	HTTPPort int    `json:"http_port,omitempty"`
	WebPort  int    `json:"web_port,omitempty"` // unified web UI port (default 4343)
	WebDir   string `json:"web_dir,omitempty"`  // path to web/dist
	LogLines int    `json:"log_lines,omitempty"`
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
	Tags         []string     `json:"tags,omitempty"`
	CreatedAt    string       `json:"created_at,omitempty"`
}

type LinksConfig struct {
	SchedgURL string `json:"schedg_url,omitempty"`
	AnglURL   string `json:"angl_url,omitempty"`
}

type OrchardConfig struct {
	URL           string `json:"url,omitempty"`           // e.g. https://forintracommuseonly.localhost:50772
	EnvironmentID string `json:"environment_id,omitempty"` // Power Platform env GUID
	Tenant        string `json:"tenant,omitempty"`         // msft, aurora, custom
	TenantID      string `json:"tenant_id,omitempty"`      // custom tenant GUID (when tenant=custom)
	Environment   string `json:"environment,omitempty"`    // test or prod (for token scope)
	Username      string `json:"username,omitempty"`       // UPN for token acquisition
	TokensUtilExe string `json:"tokens_util_exe,omitempty"` // path to TokensUtil.exe
}

type EndpointDef struct {
	HTTP   string `json:"http,omitempty"`
	Pipe   string `json:"pipe,omitempty"`
	Health string `json:"health,omitempty"`
}

func (a AnglDef) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

func (a AnglDef) Port() int {
	if a.Endpoint == nil || a.Endpoint.HTTP == "" {
		return 0
	}
	url := a.Endpoint.HTTP
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == ':' {
			port := 0
			for j := i + 1; j < len(url); j++ {
				if url[j] >= '0' && url[j] <= '9' {
					port = port*10 + int(url[j]-'0')
				} else {
					break
				}
			}
			return port
		}
	}
	return 0
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
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Angls == nil {
		cfg.Angls = make(map[string]AnglDef)
	}
	if cfg.Daemon.HTTPPort == 0 {
		cfg.Daemon.HTTPPort = 3333
	}
	if cfg.Daemon.WebPort == 0 {
		cfg.Daemon.WebPort = 4343
	}
	if cfg.Daemon.LogLines == 0 {
		cfg.Daemon.LogLines = 1000
	}
	return cfg, nil
}

func SaveConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func DefaultTransientPath() string {
	return filepath.Join(DefaultConfigDir(), "transient.json")
}

func LoadTransient(path string) (map[string]AnglDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]AnglDef), nil
		}
		return nil, err
	}
	var m map[string]AnglDef
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = make(map[string]AnglDef)
	}
	return m, nil
}

func SaveTransient(path string, m map[string]AnglDef) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}
