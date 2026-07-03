# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## What This Is

Protomolecule is a Go binary that serves as a scheduler and execution system for AI agent tasks. It replaces a launchd polling loop with proper cron scheduling, dependency DAGs, retry handling, and observability. It dispatches `claude -p` subprocesses per task.

## Commands

```bash
# Build
go build -o bin/protomolecule .
# or
make build

# Run all tests with race detector
go test -race -count=1 ./...

# Run a single test
go test -run TestScheduler_IsDue -v ./...

# Run tests matching a pattern
go test -run "TestScheduler_DST" -v ./...

# Integration tests (requires built binary)
go test -tags integration -v ./...
```

## Architecture

Single Go package (`package main`), flat file layout. All components communicate through injected interfaces for testability.

### Core Interfaces (types.go)

Three injectable interfaces drive the entire system:
- **`Clock`** — abstracts `time.Now()`. Tests use `FakeClock` to control time.
- **`Executor`** — runs a task, returns `ExecutorResult`. Production uses `ClaudeExecutor` (spawns `claude -p`). Tests use `FakeExecutor`.
- **`NetworkChecker`** — checks internet connectivity. Production dials `8.8.8.8:53`. Tests use `FakeNetworkChecker`.

### Data Flow

```
Config (YAML) → Scheduler → WorkerPool → Executor (claude -p)
                    ↕              ↕
               StateStore    Results channel
              (JSON file)    (back to Scheduler)
```

### Key Components

| File | Responsibility |
|------|---------------|
| `config.go` | YAML parsing, defaults, validation (Kahn's algorithm for DAG cycles), `ConfigWatcher` with fsnotify (500ms debounce) |
| `scheduler.go` | Tick loop (1min), `isDue` evaluation, catchup logic, `computeLastFiring`, cycle ID generation, retry scheduling, event-driven dependent dispatch |
| `worker.go` | Semaphore-based concurrency pool, timeout enforcement (SIGTERM → 10s → SIGKILL), results channel |
| `state.go` | `StateStore` with RWMutex, atomic file writes (tmp → fsync → rename), corruption recovery with backup |
| `executor.go` | `ClaudeExecutor` spawns `claude -p` with `--agent` flag, `FakeExecutor` for tests |
| `status.go` | Unix socket IPC server (`protomolecule.sock`), `StatusClient`, `FormatStatus` for human-readable output |
| `tui.go` | Bubbletea v2 live dashboard (`status --watch`), polls daemon every second |
| `visualize.go` | Mermaid diagram generation with color-coded task status |
| `logger.go` | Thin wrapper around `log/slog` for structured JSON logging |

### Schedule Evaluation

The `isDue` method in scheduler.go is the core scheduling brain:
1. Parses cron expression via `robfig/cron/v3` (5-field format: min hour dom month dow)
2. Calls `computeLastFiring` to find most recent scheduled time (32-day lookback)
3. Compares against `LastSuccess` in state store
4. If `catchup=true`: fires once for the most recent missed window
5. If `catchup=false`: only fires if current minute matches the cron expression

### Cycle IDs

Workflow runs are tracked by cycle ID format: `workflowID:2026-03-08T09:00:00Z`. Force runs use: `workflowID:force:2026-03-08T09:00:00Z`. Dependents inherit their parent's cycle ID for DAG tracking.

### Directories

- **Runtime dir** = the directory containing the config file (default
  `~/.protomolecule`, since the default config is
  `~/.protomolecule/protomolecule.yaml`). `state/`, the socket, `logs/`, and
  `output/` resolve under it via `runtimeDir(configPath)` in main.go. All
  subcommands derive paths this way, so `status`/`watch`/`logs` locate the
  daemon's socket regardless of cwd.
- **Working dir** = the `working_dir` config field, resolved by
  `Config.ResolveWorkingDir()` (expands a leading `~`, defaults to cwd). It sets
  `ClaudeExecutor.ProjectRoot`, i.e. the cwd for `claude -p`.

### Config Format

```yaml
timezone: "America/New_York"
working_dir: "~/code/my-project"   # cwd for claude -p (default: current dir)
worker_slots: 3
max_retries: 2
retry_delay_seconds: 300
timeout_minutes: 30

workflows:
  - id: my-pipeline
    schedule: "0 8 * * *"
    catchup: true
    tasks:
      - id: step-one
        prompt: "Do the first thing"
      - id: step-two
        depends_on: step-one
        prompt: "Do the second thing"

standalone:
  - id: check-something
    schedule: "*/15 * * * *"
    catchup: false
    prompt: "Check the thing"
```

### CLI Subcommands

- `protomolecule server` — start daemon (foreground)
- `protomolecule run --task <id>` — force-run one task
- `protomolecule status [--json]` — query running daemon
- `protomolecule watch` — live-updating TUI dashboard
- `protomolecule visualize [--workflow <id>]` — generate Mermaid diagram
- `protomolecule install-plist` — install macOS launchd plist

## Important: Bubbletea v2

This project uses **Bubbletea v2** (`charm.land/bubbletea/v2`), NOT v1. The APIs are different. Do NOT use v1 patterns:

- Import is `charm.land/bubbletea/v2`, not `github.com/charmbracelet/bubbletea`
- Key events are `tea.KeyPressMsg`, not `tea.KeyMsg`
- `View()` returns `tea.View` (via `tea.NewView()`), not a `string`
- `KeyPressMsg.String()` returns `"esc"` for Escape, not `"escape"`
- Always check the actual v2 API when in doubt — do not assume v1 behavior

## Testing Patterns

Tests use dependency injection extensively. The standard test setup helper is `newTestScheduler()` in scheduler_test.go which wires up `FakeClock`, `FakeNetworkChecker`, `FakeExecutor`, and a temp-dir `StateStore`.

`FakeExecutor` accepts a callback `func(TaskConfig) ExecutorResult` to control per-task outcomes. `FakeClock` time is advanced manually with `clock.Set(t)`.

The scheduler tests drive the system by calling `sched.tick()` directly rather than running the full `Start()` loop, then reading results from `pool.Results()`.

## Design Doc

The full design specification is at `design/protomolecule-design-002.md` (981 lines). All architectural decisions trace back to this document.
