//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// cmdVPN connects or checks VPN status.
//
// Usage:
//   angl vpn              -- show status
//   angl vpn connect      -- connect the first available VPN
//   angl vpn disconnect   -- disconnect
func cmdVPN(args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}

	switch action {
	case "status":
		conns, err := vpnConnections()
		if err != nil {
			return err
		}
		for _, c := range conns {
			fmt.Printf("%-25s %s\n", c.name, c.status)
		}
		return nil

	case "connect":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return vpnConnect(name)

	case "disconnect":
		return vpnDisconnect()

	default:
		return fmt.Errorf("usage: angl vpn [status|connect|disconnect]")
	}
}

type vpnConn struct {
	name   string
	status string
}

func vpnConnections() ([]vpnConn, error) {
	out, err := exec.Command("powershell", "-Command",
		`Get-VpnConnection | Select-Object -Property Name,ConnectionStatus | ForEach-Object { "$($_.Name)|$($_.ConnectionStatus)" }`).Output()
	if err != nil {
		return nil, fmt.Errorf("Get-VpnConnection: %w", err)
	}
	var conns []vpnConn
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 {
			conns = append(conns, vpnConn{name: parts[0], status: parts[1]})
		}
	}
	return conns, nil
}

func vpnIsConnected() bool {
	conns, err := vpnConnections()
	if err != nil {
		return false
	}
	for _, c := range conns {
		if strings.EqualFold(c.status, "Connected") {
			return true
		}
	}
	return false
}

func vpnConnect(name string) error {
	if vpnIsConnected() {
		fmt.Println("VPN already connected")
		return nil
	}

	if name == "" {
		// Pick the first available VPN
		conns, err := vpnConnections()
		if err != nil {
			return err
		}
		if len(conns) == 0 {
			return fmt.Errorf("no VPN connections configured")
		}
		// Prefer Azure VPN
		for _, c := range conns {
			if strings.Contains(strings.ToLower(c.name), "az") {
				name = c.name
				break
			}
		}
		if name == "" {
			name = conns[0].name
		}
	}

	fmt.Printf("Connecting to %s...\n", name)
	cmd := exec.Command("rasdial", name)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		// rasdial may not work for UWP VPN apps, try rasphone
		cmd2 := exec.Command("rasphone", "-d", name)
		if err2 := cmd2.Start(); err2 != nil {
			return fmt.Errorf("connect failed (rasdial: %v, rasphone: %v)", err, err2)
		}
		fmt.Println("VPN dialog opened. Waiting for connection...")
		for i := 0; i < 30; i++ {
			time.Sleep(2 * time.Second)
			if vpnIsConnected() {
				fmt.Println("Connected!")
				return nil
			}
		}
		return fmt.Errorf("timed out waiting for VPN connection")
	}

	fmt.Println("Connected!")
	return nil
}

func vpnDisconnect() error {
	conns, err := vpnConnections()
	if err != nil {
		return err
	}
	for _, c := range conns {
		if strings.EqualFold(c.status, "Connected") {
			exec.Command("rasdial", c.name, "/disconnect").Run()
			fmt.Printf("Disconnected %s\n", c.name)
		}
	}
	return nil
}
