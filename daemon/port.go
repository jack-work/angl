//go:build windows

package daemon

import (
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var reListeningPid = regexp.MustCompile(`\s+LISTENING\s+(\d+)\s*$`)

func IsPortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func KillPortHolder(port int) error {
	pid, err := findPidOnPort(port)
	if err != nil || pid == "" {
		return nil
	}
	out, err := exec.Command("taskkill", "/F", "/PID", pid).CombinedOutput()
	if err != nil {
		return fmt.Errorf("kill PID %s: %w\n%s", pid, err, out)
	}
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if !IsPortInUse(port) {
			return nil
		}
	}
	return fmt.Errorf("port %d still in use after killing PID %s", port, pid)
}

func findPidOnPort(port int) (string, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return "", err
	}
	target := fmt.Sprintf(":%d ", port)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, target) && strings.Contains(line, "LISTENING") {
			if m := reListeningPid.FindStringSubmatch(line); len(m) >= 2 {
				return m[1], nil
			}
		}
	}
	return "", nil
}
