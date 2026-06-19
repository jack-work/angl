# angl

Process supervisor for Windows. Keeps background processes running as headless daemons with automatic restart, named pipe IPC, and Task Scheduler integration.

Originally built for [copilot-api](https://github.com/ericc-ch/copilot-api), but works with any command.

## Install

```powershell
scoop bucket add angl https://github.com/jack-work/scoop-angl
scoop install angl
```

### Prerequisites

- Windows 10/11
- [Node.js](https://nodejs.org/) (if supervising npx-based tools)

## Configuration

Create `~/.config/angl/config.json`:

```json
{
  "angls": {
    "copilot-api": {
      "command": "npx",
      "args": ["copilot-api@latest", "start"],
      "port": 4141,
      "kill_existing": true
    },
    "oracle": {
      "command": "angl",
      "args": ["serve", "--port", "3080"],
      "port": 3080,
      "charge": "Claude Code oracle — a divine counsel",
      "station": "http://localhost:3080"
    }
  }
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `command` | *required* | Executable to run |
| `args` | `[]` | Command arguments |
| `port` | `0` | Port to monitor; enables conflict detection |
| `kill_existing` | `false` | Kill existing process on port before starting |
| `interval` | `""` | Run periodically instead of as a long-lived daemon |
| `charge` | `""` | Description — an angel's divine charge |
| `station` | `""` | Endpoint URL — an angel's post of duty |

## Usage

```powershell
# List configured angls
angl list

# Start an angl in the background (detached, no window)
angl start copilot-api

# Query status (one angl or all)
angl status copilot-api
angl status

# Tail live log stream via named pipe
angl logs copilot-api

# Gracefully stop (kills entire process tree)
angl stop copilot-api

# Register as a Windows logon task + start immediately
angl install copilot-api

# Remove scheduled task
angl uninstall copilot-api

# Print the station URL for an angl
angl endpoint oracle

# Start HTTP bridge for Claude Code
angl serve --port 3080
```

## How it works

- Each angl runs as a supervised child process with no visible window
- **Named pipe IPC** (`\\.\pipe\angl-<name>`) for `status`, `stop`, and `logs`
- `logs` streams live output via the pipe — connect from any terminal
- Auto-restarts on crash with exponential backoff (2s → 60s cap, resets after 2min healthy)
- Logs to `~/.config/angl/logs/<name>.log` with 10MB rotation
- `install` registers a Windows Task Scheduler logon trigger per angl
- `stop` uses `taskkill /t /f` to kill the entire process tree

## Claude Code bridge

`angl serve` starts an HTTP server that wraps `claude -p`:

- `POST /message` — accepts `{"prompt":"..."}`, returns Claude's JSON response
- `GET /health` — liveness check
- Maintains conversation context across requests via session ID

## Build from source

```powershell
go build -ldflags "-s -w" -o angl.exe .
```
