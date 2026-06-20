//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// cmdInstallOrchard publishes the Orchard service from a worktree and
// restarts the orchard-service angl.
//
// Usage: angl install-orchard [<worktree-path>]
//
// Defaults to ~/dev/orchard/pi-strategy if no path is given.
func cmdInstallOrchard(args []string) error {
	home, _ := os.UserHomeDir()

	worktree := filepath.Join(home, "dev", "orchard", "pi-strategy")
	if len(args) > 0 && args[0] != "" {
		worktree = args[0]
	}

	project := filepath.Join(worktree, "src", "Orchard", "Orchard.Service")
	if _, err := os.Stat(project); err != nil {
		return fmt.Errorf("project not found at %s", project)
	}

	outDir := filepath.Join(home, "bin", "orchard-service")
	fmt.Printf("Publishing from %s\n", worktree)
	fmt.Printf("Output: %s\n", outDir)

	// dotnet publish
	cmd := exec.Command("dotnet", "publish", project, "-c", "Release", "-o", outDir)
	cmd.Dir = worktree
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	// Copy appsettings.local.json
	src := filepath.Join(worktree, "src", "Orchard", "Orchard.Service", "appsettings.local.json")
	dst := filepath.Join(outDir, "appsettings.local.json")
	if data, err := os.ReadFile(src); err == nil {
		os.WriteFile(dst, data, 0644)
		fmt.Println("Copied appsettings.local.json")
	}

	fmt.Println("Published. Restarting orchard-service...")

	// Restart the angl
	restart := exec.Command(os.Args[0], "restart", "orchard-service")
	restart.Stdout = os.Stdout
	restart.Stderr = os.Stderr
	if err := restart.Run(); err != nil {
		fmt.Printf("Warning: restart failed: %v (start manually with: angl start orchard-service)\n", err)
	}

	return nil
}
