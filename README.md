# protomolecule

A scheduler and execution system for AI agent tasks. Replaces a polling loop with proper cron scheduling, dependency DAGs, retry handling, and observability. Dispatches `claude -p` subprocesses to run each task.

## Features

- **Cron scheduling** — standard 5-field cron expressions per task or workflow
- **Dependency DAGs** — tasks within a workflow can depend on each other; dependents run automatically when their parent succeeds
- **Retries** — configurable retry count and delay, per-task or global
- **Timeouts** — SIGTERM → 10s grace period → SIGKILL
- **Catchup** — optionally fire once for the most recently missed window after downtime
- **Live config reload** — edit `protomolecule.yaml` while the daemon is running; changes take effect within 500ms
- **Status API** — Unix socket IPC for querying daemon state from other processes
- **Live TUI dashboard** — terminal UI showing task status, next run times, and worker utilization
- **Workflow visualization** — generates Mermaid diagrams of the task DAG with color-coded status
- **macOS launchd** — `install-plist` command sets up a persistent background service

## Installation

```bash
# Build from source (from the repo root)
make build
# Binary is at bin/protomolecule

# Or build directly
go build -o bin/protomolecule .
```

Requires Go 1.21+.

## Quick Start

1. Create a `protomolecule.yaml` in your project root (see [Configuration](#configuration))
2. Start the daemon:
   ```bash
   ./bin/protomolecule server
   ```
3. In another terminal, check status:
   ```bash
   ./bin/protomolecule status
   ```
4. Watch the live dashboard:
   ```bash
   ./bin/protomolecule watch
   ```

## CLI Reference

```
protomolecule server                       Start daemon (foreground)
protomolecule run --task <id>          Force-run a specific task immediately
protomolecule status                       Show daemon status (human-readable)
protomolecule status --json                Show daemon status as JSON
protomolecule watch                        Live-updating TUI dashboard
protomolecule visualize                    Generate Mermaid workflow diagram
protomolecule visualize --workflow <id>    Visualize a specific workflow
protomolecule install-plist                Install macOS launchd plist
protomolecule version                      Print version
protomolecule help                         Print help

Global flags:
  --config <path>    Path to protomolecule.yaml (default: protomolecule.yaml)
  --debug            Enable debug logging (shows exact claude commands being run)
```

### `server`

Starts the scheduler in the foreground. Writes structured JSON logs to `<runtime-dir>/logs/protomolecule.log` (and also echoes them to your terminal when run interactively). Handles SIGINT/SIGTERM for graceful shutdown. See [Logs](#logs) for details.

```bash
protomolecule --config /path/to/protomolecule.yaml server
```

### `run --task <id>`

Force-runs a single task immediately, bypassing its schedule. Useful for testing or re-running a failed task. Exits with code 0 on success, 1 on failure.

```bash
protomolecule run --task daily-report
```

### `status`

Queries the running daemon via Unix socket and prints task state, next scheduled times, worker utilization, and uptime.

```bash
protomolecule status
protomolecule status --json   # machine-readable output
```

### `watch`

Opens a live-updating terminal dashboard (Bubbletea TUI) that polls the daemon every second. Press `q` or `Ctrl+C` to exit.

```bash
protomolecule watch
```

### `visualize`

Generates a Mermaid diagram of the workflow DAG and writes it to `output/workflows/protomolecule.md`. Tasks are color-coded by last run status.

```bash
protomolecule visualize
protomolecule visualize --workflow my-pipeline
```

### `install-plist` (macOS)

Interactive wizard that generates and installs a launchd plist to `~/Library/LaunchAgents/`, then loads it immediately. The daemon will start automatically on login. The wizard prompts for the launchd **label** (default `com.protomolecule.daemon`) — substitute your chosen label in the commands below.

```bash
protomolecule install-plist
```

### Managing the launchd service (macOS)

Replace `com.protomolecule.daemon` with the label you chose at install time.

```bash
# Restart (stop + start in one command)
launchctl kickstart -k gui/$(id -u)/com.protomolecule.daemon

# Stop
launchctl stop com.protomolecule.daemon

# Start
launchctl start com.protomolecule.daemon

# Check status
launchctl list com.protomolecule.daemon
```

Note: config file changes are picked up automatically via live reload — a restart is only needed after binary updates or plist changes.

## Configuration

Configuration lives in `protomolecule.yaml`. The default location is
`~/.protomolecule/protomolecule.yaml`; the `--config` flag overrides it.

### Directories

Protomolecule separates two distinct locations:

- **Runtime directory** — a fixed global location, `~/.protomolecule`,
  regardless of where the config file lives. State (`state/`), the IPC socket,
  logs (`logs/`), and workflow output (`output/`) all live here. Because the
  location is fixed, `status`, `watch`, and `logs` find the running daemon's
  socket and log file no matter what directory you invoke them from.
- **Working directory** — set with the `working_dir` config field. This is the
  directory in which `claude -p` runs for each task: the project your agents
  operate on. If unset, it defaults to the current directory (with a warning).

### Full Example

```yaml
timezone: "America/New_York"
working_dir: "~/code/my-project"  # where claude -p runs (default: current dir)
worker_slots: 3          # max concurrent tasks (default: 5)
max_retries: 2           # retry attempts on failure (default: 2)
retry_delay_seconds: 300 # seconds between retries (default: 30)
timeout_minutes: 30      # per-task timeout (default: 10)

# Workflows: groups of tasks with dependencies and a shared schedule
workflows:
  - id: morning-pipeline
    schedule: "0 8 * * 1-5"   # 8am weekdays
    catchup: true              # fire once for missed windows
    tasks:
      - id: fetch-data
        prompt: "Fetch the latest data from the API"
      - id: process-data
        depends_on: fetch-data
        prompt: "Process the fetched data and generate a report"
        timeout_minutes: 60    # override global timeout for this task
      - id: post-summary
        depends_on: process-data
        prompt: "Post the report summary to Slack"

# Standalone: independent tasks with their own schedules
standalone:
  - id: health-check
    schedule: "*/15 * * * *"  # every 15 minutes
    catchup: false
    requires_network: true    # skip if offline (default: true)
    prompt: "Check system health and alert if anything is wrong"

  - id: weekly-digest
    schedule: "0 9 * * 1"    # 9am Monday
    catchup: true
    prompt: "Generate and send the weekly digest email"
    max_retries: 3            # override global retries
    retry_delay_seconds: 60
```

### Global Settings

| Field | Default | Description |
|-------|---------|-------------|
| `timezone` | system local | IANA timezone name (e.g., `"America/New_York"`) |
| `working_dir` | current dir | Directory in which `claude -p` runs for each task (supports a leading `~`) |
| `worker_slots` | `5` | Max concurrent tasks |
| `max_retries` | `2` | Retry attempts on failure |
| `retry_delay_seconds` | `30` | Seconds between retry attempts |
| `timeout_minutes` | `10` | Per-task timeout |

### Workflow Fields

| Field | Required | Description |
|-------|----------|-------------|
| `id` | yes | Unique workflow identifier |
| `schedule` | yes | 5-field cron expression or `on-demand` |
| `catchup` | no | Fire once for most recently missed window (default: `false`) |
| `tasks` | yes | List of task configs |

### Task Fields

| Field | Required | Description |
|-------|----------|-------------|
| `id` | yes | Unique task identifier (across all workflows and standalone) |
| `prompt` | yes | Prompt text passed to `claude -p` |
| `depends_on` | no | ID of another task in the same workflow to wait for |
| `agent` | no | `--agent` flag value passed to `claude -p` |
| `requires_network` | no | Skip if offline (default: `true`) |
| `schedule` | no | Standalone only — 5-field cron expression or `on-demand` |
| `catchup` | no | Standalone only — fire once for missed windows (default: `false`) |
| `timeout_minutes` | no | Override global timeout |
| `max_retries` | no | Override global retry count |
| `retry_delay_seconds` | no | Override global retry delay |

### Schedule Expressions

Standard 5-field cron: `minute hour day-of-month month day-of-week`

```
"*/15 * * * *"    every 15 minutes
"0 8 * * *"       daily at 8am
"0 8 * * 1-5"     weekdays at 8am
"0 9 * * 1"       every Monday at 9am
"on-demand"       never runs on schedule (use run --task to trigger)
```

## State

Task state is persisted to `state/task-state.json`. This file tracks the last success time and retry count for each task. It is written atomically (tmp → fsync → rename) and automatically backs up and recovers from corruption.

The `state/` directory is created automatically. Don't edit the state file manually.

## Logs

The daemon writes structured JSON logs (one entry per line) directly to:

```
<runtime-dir>/logs/protomolecule.log
```

The runtime directory is `~/.protomolecule` by default (see [Directories](#directories)), so logs live at `~/.protomolecule/logs/protomolecule.log`. The daemon opens and writes this file itself, so it is populated the same way no matter how the daemon was started — foreground (`server`) or via launchd. Each `claude -p` subprocess's output is streamed line by line into this file, tagged with the task ID.

When you run `server` **interactively** (attached to a terminal), logs are also echoed to stderr so you can watch them live. Under launchd there is no terminal, so logs go only to the file — no duplication.

Read logs with the built-in `logs` subcommand (it parses and filters this file); `watch` and `status` read from the same file over the socket:

```bash
protomolecule logs                        # all entries
protomolecule logs --task <id> --run last # last run of one task
protomolecule logs --output               # raw claude output lines only
protomolecule logs --status failed        # failed task_end entries
```

**launchd stderr sink.** When installed via `install-plist`, the plist also redirects the process's stdout/stderr to a *separate* file, `<runtime-dir>/logs/protomolecule.stderr.log`. This only captures crashes/panics and any early startup errors emitted before the logger is initialized — the structured logs themselves go to `protomolecule.log`, not here.

## Architecture

```
Config (YAML) → Scheduler → WorkerPool → Executor (claude -p)
                    ↕              ↕
               StateStore    Results channel
              (JSON file)    (back to Scheduler)
```

The scheduler ticks every minute, evaluates which tasks are due, and dispatches them to the worker pool. Workers enforce timeouts and report results back. Dependent tasks in a workflow are dispatched automatically when their parent succeeds — not on the next tick.

## Development

```bash
# Run tests with race detector
go test -race -count=1 ./...

# Run integration tests (requires built binary)
go test -tags integration -v ./...

# Run a specific test
go test -run TestScheduler_IsDue -v ./...
```

Tests use dependency injection extensively — `FakeClock`, `FakeExecutor`, and `FakeNetworkChecker` allow full deterministic control without running real processes or waiting for real time.
