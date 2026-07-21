# angl

A small, Windows-only process supervisor for personal development services and periodic jobs.

Angl has two jobs:

1. **Persistent processes** — keep a command running and restart it after failure with exponential backoff.
2. **Heartbeat jobs** — run a command, wait for a configured interval after it exits, and run it again.

There is deliberately no web UI, browser asset pipeline, plug-in system, or remote API. One `angl.exe` runs a local daemon and CLI. The CLI talks to the daemon through the Windows named pipe `\\.\pipe\angld`.

## Build

Requirements: Windows and Go 1.26 or newer.

```powershell
go test ./...
go build -trimpath -ldflags "-s -w" -o angl.exe .
```

## Configure

Create `$HOME\.config\angl\config.json`:

```json
{
  "angls": {
    "my-api": {
      "command": "C:\\tools\\my-api.exe",
      "args": ["--port", "8080"],
      "max_restarts": 20,
      "charge": "Local API"
    },
    "daily-sync": {
      "command": "pwsh",
      "args": ["-NoProfile", "-File", "C:\\scripts\\sync.ps1"],
      "interval": "24h",
      "charge": "Refresh local data"
    }
  }
}
```

| Field | Default | Meaning |
|---|---:|---|
| `command` | required | Executable to launch. Use an explicit `.cmd` path when needed. |
| `args` | `[]` | Argument array; no shell parsing is performed. |
| `enabled` | `true` | Whether a config-defined process starts with the daemon. |
| `interval` | unset | Go duration such as `30m` or `24h`; when set, use heartbeat mode. |
| `max_restarts` | `0` | Persistent-mode consecutive failure limit; `0` means unlimited. |
| `charge` | unset | Human-readable description shown by `angl ls`. |
| `endpoint.http` | unset | URL whose explicit port identifies a possible port conflict. |
| `kill_existing` | `false` | Kill the process listening on `endpoint.http`'s port before launch. |

`kill_existing` is intentionally opt-in and blunt. Avoid it unless the supervised command exclusively owns that port.

## Run

```powershell
# Install the daemon as a logon task and start it now
.\angl.exe install

# Inspect and control processes
.\angl.exe ls
.\angl.exe status my-api
.\angl.exe start my-api
.\angl.exe stop my-api
.\angl.exe restart my-api
.\angl.exe tail my-api

# Reconcile edits to config.json
.\angl.exe reload

# Persist enabled=false/true for config-defined processes
.\angl.exe disable my-api
.\angl.exe enable my-api

# Remove the daemon's logon task
.\angl.exe uninstall
```

Logs live under `$HOME\.config\angl\logs`. Each process log rotates to `.prev` at 10 MiB.

## Observe logs and add metadata

`angl logs` is a read-only observation layer over the existing plain log files. It does not require a daemon restart and never signals the supervised processes.

```powershell
# Clean terminal snapshot; add -f to follow
angl logs orchard-ask-user -n 50

# Canonical OpenTelemetry-inspired NDJSON, one record per line
angl logs orchard-ask-user -o jsonl -n 200 |
  jq 'select(.severityNumber >= 17) | {time, body, attributes}'

# Follow several angls with source identity in every record
angl logs dracarys-runtime dracarys-loop -f -o jsonl |
  jq -r '[.time, .attributes["angl.name"], .severityText, .body] | @tsv'

# Raw child text for grep, sed, or awk
angl logs orchard-ask-user -o raw | Select-String "error"
```

The JSONL schema is deliberately record-oriented and friendly to `jq`, `sed`, and `awk`; it follows the OpenTelemetry Logs data model without wrapping each line in an OTLP export batch. Core fields include `timeUnixNano`, `time`, `observedTimeUnixNano`, `severityText`, `severityNumber`, `body`, `attributes`, and `resource`.

Metadata is stored atomically in `catalog.json`, separate from process definitions, so annotation does not reload or restart an angl:

```powershell
angl annotate dracarys-runtime --set stack=dracarys --set role=runtime
angl annotate dracarys-loop --set stack=dracarys --set role=loop
angl query -l "stack=dracarys" --json
angl ls -l "stack=dracarys"
angl logs -l "stack=dracarys" -f
```

Selectors are comma-separated AND expressions: `key=value`, `key!=value`, `key`, and `!key`.

Saved views are virtual/materialized at query time: they store a selector, then reevaluate it against current metadata whenever used.

```powershell
angl view save dracarys --selector "stack=dracarys"
angl view list
angl logs --view dracarys -f
angl view delete dracarys
```

Following is rotation- and truncation-aware, bounded in memory, and reads all log files read-only.

### Temporary registrations

A registration is stored in `transient.json` but does not auto-start after daemon restart:

```powershell
.\angl.exe register scratch-api --max-restarts 5 --charge "Temporary API" -- C:\tools\api.exe --port 9000
.\angl.exe start scratch-api
.\angl.exe unregister scratch-api
```

Move stable definitions into `config.json` so their startup intent is explicit and versionable.

## Design

```text
Windows Task Scheduler (logon trigger; restarts daemon on failure)
  └─ angl daemon
       ├─ named pipe \\.\pipe\angld (local CLI control)
       ├─ persistent child processes (restart/backoff)
       └─ heartbeat child processes (run/wait/repeat)
```

The design favors a small operational surface:

- **One binary and one daemon.** Task Scheduler keeps the supervisor alive; the supervisor keeps children alive.
- **Local-only control.** A named pipe avoids reserving a TCP port and exposing an unauthenticated HTTP service.
- **Direct execution.** Commands are executed without a shell, reducing quoting surprises and injection risk.
- **Whole-tree stop.** Windows `taskkill /T /F` stops descendants as well as the direct child.
- **Simple state.** Durable definitions are JSON; transient registrations are separate; logs are ordinary files.

### Intentional limitations

Angl is not a service manager or production orchestrator. It does not provide dependency ordering, health-check-driven restarts, resource limits, isolated identities, secrets management, event-log integration, or zero-downtime upgrades. Heartbeat intervals are measured after a run exits and do not provide calendar/cron semantics. Child shutdown is forceful rather than graceful.

Those omissions are useful boundaries. If one of them becomes essential, an existing service manager is generally a better answer than growing angl into another one.

## Alternatives

### Windows

- **Task Scheduler** — best built-in answer for periodic tasks and simple logon/boot jobs. It can restart failed tasks, but managing several continuously running developer processes is cumbersome.
- **Windows Services + `sc.exe` / PowerShell service cmdlets** — native and robust, but an arbitrary console program usually needs to implement the service protocol or be wrapped.
- **WinSW** — mature XML-configured wrapper that turns an arbitrary executable into a Windows service. Prefer it when you need real service semantics.
- **NSSM** — very easy service wrapper with restart and output redirection. Convenient, though less actively evolved than WinSW.
- **Process Compose** — cross-platform, declarative supervision with a terminal UI, dependencies, health checks, and richer orchestration. It is the closest fit when angl becomes too small.
- **PM2** — good if the workload is mostly Node.js and its ecosystem overhead is acceptable.

### Linux

- **systemd** — the default choice on most distributions for machine/user services, restart policies, dependencies, timers, logging, identities, and resource controls. Use `systemd --user` for developer-owned processes.
- **supervisord** — straightforward process supervision with a simple configuration model; useful where systemd is unavailable or unwanted.
- **s6 / runit / daemontools** — small, composable supervision suites with excellent process-lifecycle behavior and a steeper operational model.
- **Process Compose** — useful for a portable, developer-oriented stack without adopting container orchestration.
- **Docker Compose** — appropriate when process isolation and reproducible container environments matter; overkill for ordinary host executables.

**Rule of thumb:** use Task Scheduler for a few timed Windows jobs, WinSW/NSSM for a real Windows service, systemd on Linux, and Process Compose when you want an easy multi-process developer stack. Angl occupies the deliberately narrow space between ad-hoc background commands and those larger tools.
