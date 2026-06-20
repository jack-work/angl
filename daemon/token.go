//go:build windows

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenManager acquires Orchard bearer tokens by shelling out to
// TokensUtil.exe which uses MSAL + WAM (Windows broker) for silent SSO.
// No manual login required as long as the user has a Windows session.
type TokenManager struct {
	mu      sync.Mutex
	config  OrchardConfig
	token   string
	expires time.Time
	logger  interface{ Printf(string, ...interface{}) }
}

func NewTokenManager(cfg OrchardConfig, logger interface{ Printf(string, ...interface{}) }) *TokenManager {
	return &TokenManager{config: cfg, logger: logger}
}

type tokensUtilResult struct {
	Token     string `json:"token"`
	ExpiresOn string `json:"expiresOn"`
}

// Token returns a valid bearer token, refreshing silently via WAM.
func (tm *TokenManager) Token() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.token != "" && time.Until(tm.expires) > 5*time.Minute {
		return tm.token, nil
	}

	exe := tm.resolveExe()
	if exe == "" {
		return "", fmt.Errorf("TokensUtil.exe not found; publish it with: cd orchard/main && dotnet publish src/Tools/TokensUtil -c Release")
	}

	env := tm.config.Environment
	if env == "" {
		env = "test"
	}
	tenant := tm.config.Tenant
	if tenant == "" {
		tenant = "custom"
	}

	args := []string{
		"orchard-user-token",
		"--no-clipboard",
		"--silent-only",
		"--environment", env,
		"--tenant", tenant,
	}
	if tm.config.Username != "" {
		args = append(args, "--username", tm.config.Username)
	}
	if tenant == "custom" && tm.config.TenantID != "" {
		args = append(args, "--custom-tenant-id", tm.config.TenantID)
	}

	cmd := exec.Command(exe, args...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		// Silent failed - try interactive (WAM will show a window)
		tm.logger.Printf("silent token failed (%s), trying interactive", stderr)
		args2 := make([]string, 0, len(args))
		for _, a := range args {
			if a != "--silent-only" {
				args2 = append(args2, a)
			}
		}
		cmd2 := exec.Command(exe, args2...)
		out, err = cmd2.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return "", fmt.Errorf("TokensUtil: %s", strings.TrimSpace(string(ee.Stderr)))
			}
			return "", fmt.Errorf("TokensUtil: %w", err)
		}
	}

	var result tokensUtilResult
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("parse TokensUtil output: %w", err)
	}

	tm.token = result.Token
	if t, err := time.Parse(time.RFC3339Nano, result.ExpiresOn); err == nil {
		tm.expires = t
	} else if t, err := time.Parse("2006-01-02T15:04:05.9999999-07:00", result.ExpiresOn); err == nil {
		tm.expires = t
	} else {
		tm.expires = time.Now().Add(50 * time.Minute)
	}

	tm.logger.Printf("orchard token acquired, expires %s", tm.expires.Format("15:04:05"))
	return tm.token, nil
}

// ForceLogin triggers an interactive token acquisition.
func (tm *TokenManager) ForceLogin() (string, error) {
	tm.mu.Lock()
	tm.token = ""
	tm.expires = time.Time{}
	tm.mu.Unlock()

	_, err := tm.Token()
	if err != nil {
		return "", err
	}
	return "authenticated", nil
}

func (tm *TokenManager) resolveExe() string {
	if tm.config.TokensUtilExe != "" {
		if _, err := os.Stat(tm.config.TokensUtilExe); err == nil {
			return tm.config.TokensUtilExe
		}
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "dev", "orchard", "main", "out", "bin", "Release-AnyCPU",
			"Microsoft.PowerApps.Authoring.Orchard.TokensUtil", "net9.0", "win-x64", "publish",
			"Microsoft.PowerApps.Authoring.Orchard.TokensUtil.exe"),
		filepath.Join(home, "bin", "TokensUtil.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}
