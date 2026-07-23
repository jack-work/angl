//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/jack-work/angl/catalog"
	"github.com/jack-work/angl/daemon"
	"github.com/jedib0t/go-pretty/v6/text"
	"golang.org/x/term"
)

const version = "0.9.1"

const (
	detachedProcess     = 0x00000008
	createNoWindow      = 0x08000000
	pipeName            = `\\.\pipe\angld`
	daemonStartupWait   = 5 * time.Second
	daemonDialRetryWait = 25 * time.Millisecond
)

var (
	dialDaemonPipe = func(timeout time.Duration) (net.Conn, error) {
		return winio.DialPipe(pipeName, &timeout)
	}
	launchDetachedDaemon = startDetachedDaemon
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
		err = cmdListArgs(os.Args[2:])
	case "listen", "watch":
		err = cmdListenArgs(os.Args[2:])
	case "status":
		err = withName(os.Args[2:], cmdStatus)
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
	case "sing":
		err = withName(os.Args[2:], func(name string) error {
			return rpcOKMsg("sing", nameP(name), "sang %s", name)
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
		err = cmdTailArgs(os.Args[2:])
	case "logs":
		err = cmdLogs(os.Args[2:])
	case "label":
		err = cmdLabel(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "view":
		err = cmdView(os.Args[2:])
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
		pid, err := startDetachedDaemon()
		if err != nil {
			return err
		}
		fmt.Printf("angld started (pid %d)\n", pid)
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

func cmdStatus(name string) error {
	result, err := rpcCallRaw("status", nameP(name))
	if err != nil {
		return err
	}
	var status daemon.ProcessStatus
	if err := json.Unmarshal(result, &status); err != nil {
		return err
	}
	store, _ := catalog.Load(catalog.DefaultPath())
	output := struct {
		daemon.ProcessStatus
		Metadata map[string]string `json:"metadata,omitempty"`
	}{ProcessStatus: status, Metadata: cloneLabels(store.Labels[name])}
	pretty, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(pretty))
	return nil
}

func cmdListArgs(args []string) error {
	asJSON := false
	selector := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "-l", "--selector":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", args[i])
			}
			i++
			selector = args[i]
		default:
			return fmt.Errorf("unknown list flag %q", args[i])
		}
	}
	return cmdList(asJSON, selector)
}

type listedStatus struct {
	daemon.ProcessStatus
	CommandLine string            `json:"command_line"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func cmdList(asJSON bool, selector string) error {
	result, err := rpcCallRaw("list", nil)
	if err != nil {
		return err
	}

	var statuses []daemon.ProcessStatus
	if err := json.Unmarshal(result, &statuses); err != nil {
		return fmt.Errorf("decode daemon response: %w", err)
	}
	store, _ := catalog.Load(catalog.DefaultPath())
	if selector != "" {
		parsed, err := catalog.ParseSelector(selector)
		if err != nil {
			return err
		}
		filtered := statuses[:0]
		for _, status := range statuses {
			if statusMatches(parsed, status, store.Labels[status.Name]) {
				filtered = append(filtered, status)
			}
		}
		statuses = filtered
	}
	return renderListedStatuses(statuses, store, asJSON)
}

func renderListedStatuses(statuses []daemon.ProcessStatus, store catalog.Store, asJSON bool) error {
	if len(statuses) == 0 {
		if asJSON {
			fmt.Println("[]")
		}
		return nil
	}
	if asJSON {
		listed := make([]listedStatus, 0, len(statuses))
		for _, status := range statuses {
			listed = append(listed, newListedStatus(status, store.Labels[status.Name]))
		}
		pretty, _ := json.MarshalIndent(listed, "", "  ")
		fmt.Println(string(pretty))
		return nil
	}

	width := terminalWidth()
	fmt.Fprintln(os.Stdout, renderListTable(statuses, store, width))
	return nil
}

func newListedStatus(status daemon.ProcessStatus, labels map[string]string) listedStatus {
	return listedStatus{
		ProcessStatus: status,
		CommandLine:   formatCommand(status.Command, status.Args),
		Metadata:      cloneLabels(labels),
	}
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return sanitizeCell(strings.Join(parts, ","), 80)
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
	return syscall.EscapeArg(part)
}

func snipTableCell(s string, max int) string {
	return text.Snip(s, max, "...")
}

func terminalWidth() int {
	if columns, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && columns > 0 {
		return columns
	}
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && width > 0 {
		return width
	}
	return 0
}

func statusSelectorLabels(status daemon.ProcessStatus, labels map[string]string) map[string]string {
	fields := cloneLabels(labels)
	fields["name"] = status.Name
	fields["state"] = string(status.State)
	fields["enabled"] = strconv.FormatBool(status.Enabled)
	if status.Interval == "" {
		fields["kind"] = "persistent"
	} else {
		fields["kind"] = "heartbeat"
	}
	return fields
}

func statusMatches(selector catalog.Selector, status daemon.ProcessStatus, labels map[string]string) bool {
	return selector.Matches(statusSelectorLabels(status, labels))
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

// --- JSON-RPC client ---

func startDetachedDaemon() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve executable: %w", err)
	}
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNoWindow,
	}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return 0, fmt.Errorf("release daemon process: %w", err)
	}
	return pid, nil
}

func connectDaemon(timeout time.Duration) (net.Conn, error) {
	probeTimeout := 100 * time.Millisecond
	if timeout < probeTimeout {
		probeTimeout = timeout
	}
	conn, initialErr := dialDaemonPipe(probeTimeout)
	if initialErr == nil {
		return conn, nil
	}

	if _, err := launchDetachedDaemon(); err != nil {
		return nil, fmt.Errorf("cannot reach daemon and automatic start failed: %w", err)
	}

	wait := daemonStartupWait
	if timeout < wait {
		wait = timeout
	}
	deadline := time.Now().Add(wait)
	lastErr := initialErr
	for time.Now().Before(deadline) {
		attemptTimeout := 100 * time.Millisecond
		conn, err := dialDaemonPipe(attemptTimeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(daemonDialRetryWait)
	}
	return nil, fmt.Errorf("cannot reach daemon after automatic start: %w", lastErr)
}

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
	conn, err := connectDaemon(timeout)
	if err != nil {
		return nil, err
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
  ls, list [--json] [-l <selector>]   List angls with metadata
  listen, watch                        Live interactive angl inventory
  status <name>             Detailed status of an angl
  start <name>              Start an angl
  stop <name>               Stop an angl
  restart <name>            Restart an angl
  sing <name>               Run a backoff angl now; advance its retry timer

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
  logs [name...] [flags]    Read or follow logs (no names selects all)
    -n, --tail <n>          History per angl (default 100)
    -f, --follow            Continue following
    -o, --output <format>   pretty (default), jsonl, or raw
    -l, --selector <expr>   Select by name/state/enabled/kind or metadata; repeat to AND
    --view <name>           Intersect with a saved selector view
    --no-color              Disable pretty severity color
  tail <name> [flags]       Alias for logs <name> --follow
  label set <name> key=value [key=value...]
  label unset <name> key [key...]
  label list <name>
  query [-l <selector>] [--json]
  view save <name> --selector <expr> [--force]
  view list | view show <name> [--json] | view delete <name>

Startup:
  install                   Register logon task (Task Scheduler) + start daemon
  uninstall                 Remove logon task

Other:
  version                   Print version
  help                      Print this help

Config: ~/.config/angl/config.json
Transient: ~/.config/angl/transient.json
Catalog: ~/.config/angl/catalog.json

Listen keys:
  Up/k, Down/j   Move selection
  Home/g, End/G  Jump to first/last angl
  Enter          Toggle full wrapped details for visible columns
  s              Sing when in backoff; stop otherwise
  d              Disable configured angl
  u              Unregister transient angl
  q, Esc, Ctrl-C Quit

Semantics:
  Persistent process (no interval): runs continuously, restarts on crash
    with exponential backoff (2s -> 60s cap, resets after 2min healthy).

  Heartbeat process (with interval): runs once, sleeps for interval, repeats.
    Useful for periodic polling tasks.
`)
}
