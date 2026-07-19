//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/jack-work/angl/daemon"
)

const version = "0.7.0"

const (
	detachedProcess = 0x00000008
	createNoWindow  = 0x08000000
	pipeName        = `\\.\pipe\angld`
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "list", "ls":
		jsonFlag := len(os.Args) > 2 && os.Args[2] == "--json"
		err = cmdList(jsonFlag)
	case "status":
		err = withName(os.Args[2:], func(name string) error {
			return rpcPrint("status", nameP(name))
		})
	case "start":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("start", nameP(name), "started %s", name)
		})
	case "stop":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("stop", nameP(name), "stopped %s", name)
		})
	case "restart":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("restart", nameP(name), "restarted %s", name)
		})
	case "reload":
		err = rpcPrint("reload", nil)
	case "enable":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("enable", nameP(name), "enabled %s", name)
		})
	case "disable":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("disable", nameP(name), "disabled %s", name)
		})
	case "register":
		err = cmdRegister(os.Args[2:])
	case "unregister":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("unregister", nameP(name), "unregistered %s", name)
		})
	case "tail":
		err = withName(os.Args[2:], cmdTail)
	case "install":
		err = cmdInstall()
	case "uninstall":
		err = cmdUninstall()
	case "version":
		fmt.Println("angl", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// --- Daemon ---

func cmdDaemon(args []string) error {
	detach := len(args) > 0 && args[0] == "--detach"

	if detach {
		if daemon.IsDaemonRunning() {
			return fmt.Errorf("daemon already running")
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "daemon")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: detachedProcess | createNoWindow,
		}
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("fork: %w", err)
		}
		fmt.Printf("angld started (pid %d)\n", cmd.Process.Pid)
		return nil
	}

	if daemon.IsDaemonRunning() {
		return fmt.Errorf("daemon already running (pipe %s is active)", pipeName)
	}

	cfgPath := daemon.DefaultConfigPath()
	d, err := daemon.New(cfgPath)
	if err != nil {
		return err
	}

	defer func() {
		if r := recover(); r != nil {
			panicLog := fmt.Sprintf("[angld] PANIC: %v\n", r)
			os.Stderr.WriteString(panicLog)
			logPath := daemon.DefaultConfigDir() + "/logs/angld-panic.log"
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
				f.WriteString(time.Now().Format(time.RFC3339) + " " + panicLog)
				f.Close()
			}
			os.Exit(1)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return d.Run(ctx)
}

// --- List ---

func cmdList(asJSON bool) error {
	result, err := rpcCallRaw("list", nil)
	if err != nil {
		return err
	}

	var statuses []daemon.ProcessStatus
	if err := json.Unmarshal(result, &statuses); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}
	if len(statuses) == 0 {
		fmt.Println("no angls configured")
		return nil
	}
	if asJSON {
		pretty, _ := json.MarshalIndent(statuses, "", "  ")
		fmt.Println(string(pretty))
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tPID\tUPTIME\tRESTARTS\tCOMMAND\tCHARGE")
	for _, status := range statuses {
		pid := "-"
		if status.PID > 0 {
			pid = strconv.Itoa(status.PID)
		}
		uptime := status.Uptime
		if uptime == "" {
			uptime = status.Lifetime
		}
		if uptime == "" {
			uptime = "-"
		}
		restarts := strconv.Itoa(status.Restarts)
		if status.MaxRestarts > 0 {
			restarts = fmt.Sprintf("%d/%d", status.Restarts, status.MaxRestarts)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			sanitizeCell(status.Name, 30), status.State, pid, uptime, restarts,
			sanitizeCell(formatCommand(status.Command, status.Args), 80),
			sanitizeCell(status.Charge, 60))
	}
	return w.Flush()
}

func formatCommand(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteCommandPart(command))
	for _, arg := range args {
		parts = append(parts, quoteCommandPart(arg))
	}
	return strings.Join(parts, " ")
}

func quoteCommandPart(part string) string {
	if part == "" || strings.ContainsAny(part, " \t\r\n\"") {
		return `"` + strings.ReplaceAll(part, `"`, `\"`) + `"`
	}
	return part
}

func sanitizeCell(s string, max int) string {
	s = strings.NewReplacer("\n", " ", "\r", "", "\t", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	if max > 3 && len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

// --- Tail ---

func cmdTail(name string) error {
	if err := daemon.ValidateName(name); err != nil {
		return err
	}
	path := daemon.LogPath(name)
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log %s: %w", path, err)
	}
	defer file.Close()

	history, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read log: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(history), "\r\n"), "\n")
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	if len(history) > 0 {
		fmt.Println(strings.Join(lines, "\n"))
	}

	fmt.Fprintf(os.Stderr, "tailing %s (ctrl+c to stop)\n", path)
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
		}
		if err == nil {
			continue
		}
		if err != io.EOF {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// --- JSON-RPC client ---

func rpcCallRaw(method string, params interface{}) (json.RawMessage, error) {
	req := daemon.RPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		ID:      1,
	}
	if params != nil {
		raw, _ := json.Marshal(params)
		req.Params = raw
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	timeout := 30 * time.Second
	conn, err := winio.DialPipe(pipeName, &timeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach daemon (is it running?): %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Minute))
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	var resp daemon.RPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}

	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func rpcPrint(method string, params interface{}) error {
	result, err := rpcCallRaw(method, params)
	if err != nil {
		return err
	}
	var v interface{}
	json.Unmarshal(result, &v)
	pretty, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(pretty))
	return nil
}

func rpcOKMsg(method string, params interface{}, format string, args ...interface{}) error {
	_, err := rpcCallRaw(method, params)
	if err != nil {
		return err
	}
	fmt.Printf(format+"\n", args...)
	return nil
}

// --- Install / Uninstall (Task Scheduler) ---

const taskName = `angld`

func taskXML(exe string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>angl process supervisor daemon</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>false</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <DisallowStartOnRemoteAppSession>false</DisallowStartOnRemoteAppSession>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>daemon</Arguments>
    </Exec>
  </Actions>
</Task>`, exe)
}

func cmdInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "angld-task-*.xml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	xmlContent := taskXML(exe)
	utf16 := utf16Encode(xmlContent)
	tmpFile.Write(utf16)
	tmpFile.Close()

	cmd := exec.Command("schtasks.exe",
		"/Create",
		"/TN", taskName,
		"/XML", tmpFile.Name(),
		"/F",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("schtasks create: %w", err)
	}

	fmt.Printf("installed logon task %q -> %s daemon\n", taskName, exe)
	fmt.Println("  - restarts on failure every 60s (999 attempts)")
	fmt.Println("  - runs on battery, no timeout, no idle requirement")

	if !daemon.IsDaemonRunning() {
		fmt.Println("starting daemon...")
		return cmdDaemon([]string{"--detach"})
	}
	fmt.Println("daemon already running")
	return nil
}

func cmdUninstall() error {
	cmd := exec.Command("schtasks.exe", "/Delete", "/TN", taskName, "/F")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("schtasks delete: %w", err)
	}
	fmt.Printf("removed logon task %q\n", taskName)
	return nil
}

func utf16Encode(s string) []byte {
	runes := []rune(s)
	buf := []byte{0xFF, 0xFE}
	for _, r := range runes {
		if r <= 0xFFFF {
			buf = append(buf, byte(r), byte(r>>8))
		} else {
			r -= 0x10000
			hi := 0xD800 + (r>>10)&0x3FF
			lo := 0xDC00 + r&0x3FF
			buf = append(buf, byte(hi), byte(hi>>8))
			buf = append(buf, byte(lo), byte(lo>>8))
		}
	}
	return buf
}

// --- Helpers ---

func withName(args []string, fn func(string) error) error {
	if len(args) < 1 {
		return fmt.Errorf("angl name required")
	}
	return fn(args[0])
}

func nameP(name string) map[string]string {
	return map[string]string{"name": name}
}

// --- Register ---

func cmdRegister(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: angl register <name> [--interval <dur>] [--charge <desc>] [--max-restarts <n>] -- <command> [args...]")
	}

	name := args[0]
	rest := args[1:]

	var interval, charge string
	var maxRestarts int
	var cmdStart int = -1

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--":
			cmdStart = i + 1
			goto done
		case "--interval":
			if i+1 >= len(rest) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			interval = rest[i]
		case "--max-restarts":
			if i+1 >= len(rest) {
				return fmt.Errorf("--max-restarts requires a value")
			}
			i++
			fmt.Sscanf(rest[i], "%d", &maxRestarts)
		case "--charge":
			if i+1 >= len(rest) {
				return fmt.Errorf("--charge requires a value")
			}
			i++
			charge = rest[i]
		default:
			cmdStart = i
			goto done
		}
	}
done:

	if cmdStart < 0 || cmdStart >= len(rest) {
		return fmt.Errorf("usage: angl register <name> [--interval <dur>] [--charge <desc>] -- <command> [args...]")
	}

	params := map[string]interface{}{
		"name":    name,
		"command": rest[cmdStart],
	}
	if cmdStart+1 < len(rest) {
		params["args"] = rest[cmdStart+1:]
	}
	if interval != "" {
		params["interval"] = interval
	}
	if maxRestarts > 0 {
		params["max_restarts"] = maxRestarts
	}
	if charge != "" {
		params["charge"] = charge
	}

	_, err := rpcCallRaw("register", params)
	if err != nil {
		return err
	}
	fmt.Printf("registered %s (start with: angl start %s)\n", name, name)
	return nil
}

func printUsage() {
	fmt.Print(`angl - process supervisor

Usage:
  angl <command> [args]

Daemon:
  daemon [--detach]         Run supervisor (--detach forks to background)

Process control:
  ls, list [--json]         List all angls with status
  status <name>             Detailed status of an angl
  start <name>              Start an angl
  stop <name>               Stop an angl
  restart <name>            Restart an angl

Configuration:
  reload                    Re-read config, reconcile running state
  enable <name>             Enable an angl (updates config + starts)
  disable <name>            Disable an angl (updates config + stops)

Transient:
  register <name> [flags] -- <cmd> [args...]   Register a transient angl
  unregister <name>                            Remove a transient angl

  Register flags:
    --interval <dur>        Run periodically (e.g. 45m, 1h)
    --max-restarts <n>      Give up after N consecutive failures (0=unlimited)
    --charge <desc>         Description

Observation:
  tail <name>               Stream stdout/stderr (ctrl+c to stop)

Startup:
  install                   Register logon task (Task Scheduler) + start daemon
  uninstall                 Remove logon task

Other:
  version                   Print version
  help                      Print this help

Config: ~/.config/angl/config.json
Transient: ~/.config/angl/transient.json

Semantics:
  Persistent process (no interval): runs continuously, restarts on crash
    with exponential backoff (2s -> 60s cap, resets after 2min healthy).

  Heartbeat process (with interval): runs once, sleeps for interval, repeats.
    Useful for periodic polling tasks.
`)
}
