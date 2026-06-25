//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/jack-work/angl/daemon"
)

const version = "0.5.0"

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
	case "install-orchard":
		err = cmdInstallOrchard(os.Args[2:])
	case "vpn":
		err = cmdVPN(os.Args[2:])
	case "exec":
		err = cmdExec(os.Args[2:])
	case "message":
		err = cmdMessage(os.Args[2:])
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
		fmt.Println(string(result))
		return nil
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
	fmt.Fprintln(w, "NAME\tSTATE\tPID\tUPTIME\tLIFETIME\tRESTARTS\tCHARGE")
	for _, s := range statuses {
		pid := "-"
		if s.PID > 0 {
			pid = fmt.Sprintf("%d", s.PID)
		}
		uptime := "-"
		if s.Uptime != "" {
			uptime = s.Uptime
		}
		lifetime := "-"
		if s.Lifetime != "" {
			lifetime = s.Lifetime
		}
		restarts := fmt.Sprintf("%d", s.Restarts)
		if s.MaxRestarts > 0 {
			restarts = fmt.Sprintf("%d/%d", s.Restarts, s.MaxRestarts)
		}
		charge := s.Charge
		if len(charge) > 50 {
			charge = charge[:50] + "..."
		}
		if charge == "" {
			charge = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.State, pid, uptime, lifetime, restarts, charge)
	}
	return w.Flush()
}

// --- Tail ---

func cmdTail(name string) error {
	port, err := daemonHTTPPort()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://localhost:%d/angls/%s/tail?history=100", port, name)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("(%d) %s", resp.StatusCode, resp.Status)
	}

	fmt.Fprintf(os.Stderr, "tailing %s (ctrl+c to stop)\n", name)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			fmt.Println(line[6:])
		}
	}
	return scanner.Err()
}

// --- Message ---

func cmdMessage(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: angl message <name> [--queue|--interrupt] <prompt...>")
	}
	name := args[0]
	rest := args[1:]
	mode := "wake" // default
	var promptParts []string
	for _, a := range rest {
		switch a {
		case "--queue":
			mode = "queue"
		case "--interrupt":
			mode = "interrupt"
		default:
			promptParts = append(promptParts, a)
		}
	}
	if len(promptParts) == 0 {
		return fmt.Errorf("prompt required")
	}
	prompt := strings.Join(promptParts, " ")
	from := defaultFrom()

	params := map[string]string{"name": name, "prompt": prompt, "from": from, "mode": mode}
	result, err := rpcCallRaw("message", params)
	if err != nil {
		return err
	}

	var parsed interface{}
	if json.Unmarshal(result, &parsed) == nil {
		pretty, _ := json.MarshalIndent(parsed, "", "  ")
		fmt.Println(string(pretty))
	} else {
		fmt.Println(string(result))
	}
	return nil
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

	// Try named pipe first.
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

func daemonHTTPPort() (int, error) {
	cfg, err := daemon.LoadConfig(daemon.DefaultConfigPath())
	if err != nil {
		return 3333, nil
	}
	return cfg.Daemon.HTTPPort, nil
}

func defaultFrom() string {
	u, err := user.Current()
	if err != nil {
		return "angl-cli"
	}
	name := u.Username
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// --- Register ---

func cmdRegister(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: angl register <name> [--interval <dur>] [--charge <desc>] -- <command> [args...]")
	}

	name := args[0]
	rest := args[1:]

	var interval, charge string
	var maxRestarts int
	var tags []string
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
		case "--tag":
			if i+1 >= len(rest) {
				return fmt.Errorf("--tag requires a value")
			}
			i++
			tags = append(tags, rest[i])
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
	if len(tags) > 0 {
		params["tags"] = tags
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
    --interval <dur>        Run periodically (e.g. 45m)
    --max-restarts <n>      Give up after N consecutive failures (0=unlimited)
    --charge <desc>         Description
    --tag <value>           Add a tag (repeatable, e.g. --tag schedg:orchard)

Agent:
  exec <name> [flags]                          Run one tick: drain messages, check work queue
    --work-queue <schedg>                      Named schedg to lease from
    --cwd <dir>                                Working directory for pi
    --runbook <path>                           Runbook to include in work prompts

Interaction:
  tail <name>               Stream stdout/stderr (ctrl+c to stop)
  message <name> <prompt>   Send a message to an angl's schedg queue

Other:
  version                   Print version
  help                      Print this help

Config: ~/.config/angl/config.json
Transient: ~/.config/angl/transient.json
`)
}
