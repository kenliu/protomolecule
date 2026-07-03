# Protomolecule — Design Document

## Overview

Protomolecule is a scheduling and execution system for a Claude Code-based AI assistant system. It replaces an ad-hoc launchd polling loop and self-scheduling agent with a proper scheduler process, a per-activity execution model, and a clean state store.

The system has three components:

1. **Protomolecule scheduler** — a Go binary that evaluates what's due and dispatches work
2. **Executor** — a per-activity Claude Code invocation via `claude -p`
3. **sync-activities skill** — a Claude Code skill that syncs the activity registry into protomolecule config

---

## Problems with the Current System

- A launchd job fires every 15 minutes and spins up a Claude Code polling agent
- The polling agent grep's its own JSONL logs to determine what's due — fragile and expensive
- All activities run sequentially in one agent session
- No parallelism, no dependency management between activities
- Poor visibility — no execution history or per-activity logging
- Tokens burned every 15 minutes even when nothing is due
- Adding a new scheduled activity requires understanding the polling agent's internal scheduling logic
- The polling agent is doing double duty as both scheduler and executor

---

## Architecture

```
launchd (single plist, on login, keep-alive)
    └── protomolecule (Go binary, persistent process)
            ├── reads protomolecule.yaml       (workflow + schedule config, watched for changes)
            ├── reads/writes state/activity-state.json  (last run state)
            ├── checks network availability
            └── worker pool (5 concurrent slots by default)
                    └── executor (claude -p subprocess, one per activity)
```

---

## Component 1: Protomolecule Scheduler (Go Binary)

### Overview

A Go binary kept alive by a single launchd plist. Runs a tick loop every minute. The old polling agent is retired entirely — all scheduling intelligence moves here.

### Build

```
protomolecule/          # repo root
    main.go
    scheduler.go        # tick loop, schedule evaluation
    executor.go         # claude -p subprocess invocation
    state.go            # read/write JSON state store
    config.go           # protomolecule.yaml and registry parsing
    network.go          # network availability check
    worker.go           # worker pool via goroutines and channels
    bin/
        protomolecule   # compiled Go binary (go build target)
    Makefile            # build, clean, install-plist targets
```

A `Makefile` should be created at the repo root with targets for `build` and `clean`. Plist installation is handled by `protomolecule install-plist`.

### Go Libraries

- `robfig/cron` — cron expression parsing
- `gopkg.in/yaml.v3` — protomolecule.yaml parsing
- `encoding/json` — state store
- `os/exec` — claude -p subprocess invocation
- `fsnotify/fsnotify` — file watching for config and registry changes
- Standard `sync` package — worker pool goroutines and channels

### Tick Loop

On each tick (every minute):

1. Check network availability
2. Read current state from `state/activity-state.json`
3. Evaluate all activities and workflows in `protomolecule.yaml` for due status
4. For network-dependent activities, skip dispatch if no network — catchup logic ensures they run later
5. For due workflows, generate a **cycle ID** (see Run Cycle Identity below) and dispatch the first activity (or activities with no dependencies) tagged with that cycle ID
6. Respect workflow dependencies — only queue an activity if its `depends_on` activities have succeeded with the same cycle ID
7. Dispatch due standalone activities to the worker pool
8. Update state store on completion of each activity (including cycle ID)
9. Log all scheduling decisions at moderate verbosity (enough to answer "why didn't that activity run?")

### Run Cycle Identity

A "run cycle" is a single end-to-end execution of a workflow, from its trigger through all dependent activities. Each run cycle is identified by a **cycle ID** — a string composed of the workflow ID and the scheduled fire time that triggered it:

```
daily-reporting-pipeline:2026-03-07T08:00:00Z
```

For force-run triggers, the cycle ID uses the dispatch time:

```
daily-reporting-pipeline:force:2026-03-07T10:32:15Z
```

Standalone activities do not use cycle IDs — they have no dependency relationships and cannot participate in multi-step workflows.

The cycle ID solves a concrete problem: the scheduler ticks every minute, but workflow activities can take multiple minutes to complete. Without a cycle ID, when `project-manager-daily` completes at 8:02, the scheduler has no way to determine on the 8:03 tick whether that success belongs to the 8:00 workflow trigger or some other context. The cycle ID makes this unambiguous — a dependent activity checks that its parent succeeded with a matching cycle ID before it becomes eligible.

When a workflow activity completes, the scheduler immediately evaluates whether any dependents are now unblocked for the same cycle ID and dispatches them without waiting for the next tick. This is event-driven dispatch for dependencies, overlaid on the poll-driven tick loop for initial scheduling.

### Schedule Rules

All schedules are expressed as cron expressions in `protomolecule.yaml`. Protomolecule uses `robfig/cron` for evaluation. Cron expressions follow standard 5-field syntax (`minute hour day month weekday`).

Cron expressions are never written directly — the sync-activities skill generates them from the activity instructions. Examples of what sync-activities produces:


| Intent           | Cron expression |
| ---------------- | --------------- |
| Daily at 9am     | `0 9 * * *`     |
| Monday at 9am    | `0 9 * * 1`     |
| Thursday at 4pm  | `0 16 * * 4`    |
| Every 15 minutes | `*/15 * * * *`  |


One special value is supported alongside cron expressions:


| Value       | Behavior                                         |
| ----------- | ------------------------------------------------ |
| `on-demand` | Never runs automatically, only via force command |


All cron expressions are evaluated in a configured IANA timezone. The timezone is set globally in `protomolecule.yaml`:

```yaml
timezone: "America/Los_Angeles"  # IANA timezone, required
```

If omitted, protomolecule defaults to the system's local timezone and logs a warning recommending explicit configuration.

All timestamps persisted in the state store and emitted in logs use UTC (RFC 3339). Scheduled fire times from cron evaluation are converted to UTC instants immediately after computation. This ensures that the catchup logic — which compares `last_success` timestamps against scheduled fire times — operates in a single consistent time domain regardless of local clock changes.

### DST Policy

During DST transitions, cron evaluation follows the `robfig/cron` library's behavior, which matches standard cron conventions:

- **Spring forward** (e.g., 2:00 AM jumps to 3:00 AM): schedules that would have fired during the skipped hour are triggered once at the next valid time. For a `0 2 * * `* schedule, this means the activity fires at 3:00 AM on the transition day. If `catchup: true`, the catchup logic also detects this as a missed firing and dispatches — but because the cron library already triggers the run, the state store will show a success and the catchup check will be satisfied. No duplicate run occurs.
- **Fall back** (e.g., 2:00 AM occurs twice): the schedule fires on the first occurrence only. The second occurrence is not a separate firing.

Protomolecule must initialize `robfig/cron` with `cron.WithLocation()` using the configured timezone to ensure cron evaluation and DST handling are consistent.

### Catchup Behavior

Per-activity `catchup` flag in `protomolecule.yaml`. Catchup logic lives entirely in protomolecule — `robfig/cron` does not track missed runs.

On each tick, for each activity with `catchup: true`:

1. Calculate the most recent time the cron expression would have fired before now (the "last scheduled firing")
2. Check the state store — did the activity succeed at or after that time?
3. If no: the firing was missed, dispatch the activity now
4. If yes: already ran for that window, skip

Example: schedule is `0 9 * * `*, laptop wakes at 10:30am. Last scheduled firing was 9:00am today. State store shows last success was yesterday. Protomolecule dispatches the activity immediately.

Catchup fires **at most once** — for the most recent missed window only. If a `*/15 * * * `* activity misses four consecutive windows (e.g., laptop asleep from 9:00 to 10:05), the catchup logic dispatches a single run for the 10:00 window. The 9:00, 9:15, 9:30, and 9:45 firings are not recovered individually. This is intentional: for agent-driven activities, a single catch-up run is sufficient to reach current state. Activities that need to process every missed time window individually must handle historical window enumeration within their own logic.

If `catchup: false`: protomolecule skips the missed firing check entirely. Missed runs are gone and will not be retried until the next scheduled firing.

The sync-activities skill sets `catchup: true` by default for all activities except polling activities like Slack check, which get `catchup: false`.

### Network Handling

Per-activity `requires_network` flag in `protomolecule.yaml`. Defaults to `true`.

- On each tick, check network connectivity with a lightweight check before dispatching network-dependent activities.
- If no network is available, hold network-dependent activities and retry on the next tick.
- Catchup logic ensures nothing is permanently missed due to a no-network condition at wake time.
- The sync-activities skill sets `requires_network: false` only if the activity instructions clearly involve no external calls.

### Worker Pool

- Default 5 concurrent worker slots, configurable via `worker_slots` in `protomolecule.yaml`.
- Activities with no dependency relationships run in parallel within the pool.
- Activities with unsatisfied `depends_on` wait in queue until dependencies complete.
- Implemented with goroutines and channels.

### Execution Timeout

Every activity execution is bounded by a timeout to prevent hung `claude -p` subprocesses from consuming worker slots indefinitely.

- `timeout_minutes` is configurable globally in `protomolecule.yaml` (default: 10) and overridable per activity.
- The executor launches `claude -p` with a `context.WithTimeout` derived from the activity's timeout value.
- When the context deadline expires, the executor sends `SIGTERM` to the subprocess.
- After a 10-second grace period, if the process has not exited, the executor sends `SIGKILL`.
- A timed-out activity is marked as failed with `"failure_reason": "timeout"` in the state store. Standard retry logic applies.

Without a timeout, 5 hung activities would consume all worker slots and make the scheduler inert while launchd keeps it alive — a state worse than a crash because it is invisible and does not self-recover.

### Failure Handling

- On failure, protomolecule retries the activity up to `max_retries` times (default 2, meaning 3 total executions).
- `max_retries` is configurable globally in `protomolecule.yaml` and can be overridden per activity.
- Retries are delayed, not immediate. `retry_delay_seconds` (default: 30) defines the minimum wait between retry attempts. When an activity fails, the scheduler records a `retry_at` timestamp in the state store. On each subsequent tick, the scheduler checks whether `now >= retry_at` before re-dispatching. This prevents hammering rate-limited APIs — immediate retry of a `claude -p` invocation that failed due to an HTTP 429 would burn through all attempts in under a second.
- `retry_delay_seconds` is configurable globally and can be overridden per activity.
- If all retries are exhausted, the activity is marked as failed for the current scheduling window and will not run again until its next scheduled window.
- A failed activity does not block independent activities in the queue.
- Downstream dependents of a definitively failed activity (all retries exhausted) are skipped for the current run cycle.
- All failures and retry attempts are logged to the scheduler log and to the state store, including which attempt number failed and the `retry_at` time for the next attempt.
- No Slack notifications for failures — log only.

### Force Command

Protomolecule must support a force run command to bypass schedule checks. Claude Code should implement a sensible CLI interface, for example:

```bash
protomolecule run --force                        # run all approved activities immediately
protomolecule run --activity check-jira-tickets      # run a specific activity immediately
```

### Config Hot Reload

Protomolecule watches `protomolecule.yaml` for file changes using `fsnotify`. When the file changes, the scheduler reloads config live without restarting the process. No manual restart or signal required.

**Debounce:** File write events are debounced with a 500ms delay before triggering a reload. Text editors and atomic-rename writes can produce multiple `fsnotify` events in rapid succession (e.g., a `WRITE` followed by a `RENAME`). Without debouncing, the scheduler may attempt to parse a half-written file. The debounce timer resets on each new event within the window, so only the final stable state of the file is parsed.

**Parse-validate-swap:** On reload, the scheduler:

1. Parses the new file into a fresh config struct
2. Validates the config: all cron expressions parse via `robfig/cron`, all `depends_on` references resolve to existing activity IDs, no activity appears in both a workflow and standalone, no DAG cycles exist (see Cycle Detection), and each activity ID is globally unique
3. If validation passes, atomically swaps the new config into the scheduler under a write lock
4. If parsing or validation fails, logs the specific error at `error` level and keeps the previous config — the scheduler continues operating on the last known good config

The `protomolecule status` command should include the config's last-loaded timestamp and whether the most recent reload succeeded or failed, so a user who just edited the config can verify it took effect.

Protomolecule does not watch `activities/registry.md` — that file is the sync-activities skill's concern. The skill reads the registry and updates `protomolecule.yaml`, which protomolecule then picks up via the file watcher.

### Logging

All log output is structured JSON (one JSON object per line). Moderately verbose — sufficient to answer operational questions like:

- Why didn't activity X run?
- What is currently running in the worker pool?
- What did activity X last return?

Every log line must include at minimum a `timestamp` (ISO 8601) and `level` field. Log lines for activity execution must also include:

```json
{
  "timestamp": "2026-03-07T09:14:22Z",
  "level": "info",
  "type": "activity_start",
  "activity_id": "check-jira-tickets",
  "attempt": 1,
  "max_attempts": 3
}

{
  "timestamp": "2026-03-07T09:16:01Z",
  "level": "info",
  "type": "activity_end",
  "activity_id": "check-jira-tickets",
  "status": "success",
  "started_at": "2026-03-07T09:14:22Z",
  "duration_ms": 99000,
  "attempt": 1,
  "output": "output/check-jira-tickets.md"
}
```

Scheduling decisions (skipped, held for network, held for dependency) are also logged as structured JSON with a `reason` field.

Log to `logs/protomolecule.log` (captured by launchd). The existing `logs/loop-TIMESTAMP.jsonl` audit log format is retained for history compatibility.

---

## Component 2: State Store

`state/activity-state.json` — owned and written exclusively by the scheduler, never by the executor.

### Concurrency Protection

The state store must be protected against concurrent writes from multiple goroutines in the worker pool.

**In-process mutex** — a `sync.RWMutex` on the state store struct. All reads acquire a read lock, all writes acquire a write lock. This is the primary protection mechanism against concurrent goroutine access within the scheduler process.

The state store must never be written to directly from outside the `state.go` package. All reads and writes go through the state store API which enforces locking internally.

### Atomic Writes and Crash Safety

All writes to `activity-state.json` must use the atomic rename pattern to prevent corruption if the process is killed mid-write:

1. Marshal the full state to JSON
2. Write to a temporary file (`state/activity-state.json.tmp`)
3. Call `fsync()` on the temporary file to ensure data reaches disk
4. `os.Rename()` the temporary file to `state/activity-state.json`

POSIX guarantees `rename()` is atomic on the same filesystem. This eliminates the window where a crash during write produces a truncated or zero-length state file. The `flock` file lock from the original design is dropped — atomic rename plus the in-process mutex is sufficient since protomolecule is the sole writer.

### Corruption Recovery

On startup, if `activity-state.json` fails to parse:

1. Copy the corrupt file to `state/activity-state.json.corrupt.<timestamp>` for debugging
2. Log a warning at `error` level with the parse error
3. Proceed with empty state

Proceeding with empty state means all activities with `catchup: true` will fire immediately, as if they have never run. This is the correct default for a single-user system — a redundant report or duplicate Slack post is preferable to the scheduler refusing to start. Activity implementations should be idempotent where possible to minimize the impact of a catchup-from-zero event.

```json
{
  "check-jira-tickets": {
    "last_success": "2026-03-07T09:00:00Z",
    "last_failure": null,
    "last_run": "2026-03-07T09:00:00Z",
    "last_run_status": "success",
    "consecutive_failures": 0
  },
  "project-manager-daily": {
    "last_success": "2026-03-07T08:02:15Z",
    "last_failure": "2026-03-06T08:00:00Z",
    "last_run": "2026-03-07T08:02:15Z",
    "last_run_status": "success",
    "last_cycle_id": "daily-reporting-pipeline:2026-03-07T08:00:00Z",
    "consecutive_failures": 0
  }
}
```

For activities within workflows, `last_cycle_id` records which run cycle the most recent execution belonged to. Downstream dependents use this field to verify that their parent succeeded within the correct cycle before becoming eligible (see Run Cycle Identity in Component 1).

---

## Component 3: Protomolecule Config

`protomolecule.yaml` — generated by the sync-activities skill. Never hand-edited manually.

```yaml
timezone: "America/Los_Angeles"
worker_slots: 5
max_retries: 2
retry_delay_seconds: 30
timeout_minutes: 10

workflows:
  - id: daily-reporting-pipeline
    schedule: "0 8 * * *"   # daily at 8am
    catchup: true
    activities:
      - id: project-manager-daily
        requires_network: true
      - id: slack-project-manager-review
        depends_on: project-manager-daily
        requires_network: true

  - id: monday-brief-pipeline
    schedule: "0 9 * * 1"  # Monday at 9am
    catchup: true
    activities:
      - id: monday-brief
        requires_network: true
      - id: notify-brief-published
        depends_on: monday-brief
        requires_network: true
      - id: notify-critical-alerts
        depends_on: monday-brief
        requires_network: true

standalone:
  - id: check-jira-tickets
    schedule: "0 8 * * *"   # daily at 8am
    catchup: true
    requires_network: true
  - id: weekly-exec-report
    schedule: "0 16 * * 4"  # Thursday at 4pm
    catchup: true
    requires_network: true
    timeout_minutes: 20     # override global default for long-running report
  - id: check-slack-messages
    schedule: "*/15 * * * *"  # every 15 minutes
    catchup: false
    requires_network: true
```

---

## Component 4: Executor

Each activity is dispatched as a separate `claude -p` subprocess. The executor invocation pattern:

```bash
claude -p \
  --verbose \
  --no-session-persistence \
  --output-format stream-json \
  --agent <agent-path> \
  '<activity instructions>'
```

- `--no-session-persistence` ensures each invocation is fully isolated with no history entry created in the interactive session
- `--output-format stream-json` provides structured output for the executor to parse
- `--agent` specifies the agent definition to use (path relative to project root)
- `--verbose` ensures sufficient output is captured for logging

The executor:

- Sets working directory to the configured working_dir (the project root)
- Captures stdout and stderr
- Interprets the exit code: 0 = success, non-zero = failure
- Returns the result to the scheduler
- Scheduler writes result to state store

Each `claude -p` invocation is a fresh, isolated session. No shared history between activity executions.

The `agent` field in `protomolecule.yaml` is optional and specified per activity:

```yaml
standalone:
  - id: check-jira-tickets
    schedule: "0 8 * * *"
    catchup: true
    requires_network: true
    agent: .claude/agents/protomolecule.md  # optional, defaults to base Claude Code agent
```

The sync-activities skill should infer the appropriate agent from the activity instructions where possible, and leave it unset otherwise.

---

## Component 5: Backoff Scheduling (Deferred)

Deferred from the initial implementation. See **Future Improvements** for design considerations and rationale. For now, polling activities use fixed-interval cron schedules with `catchup: false`.

---

## Component 6: sync-activities Skill

A Claude Code skill at `.claude/skills/sync-activities/SKILL.md`.

The activity registry (`activities/registry.md`) is the single place to edit when adding a new activity. The sync-activities skill reads the registry and updates `protomolecule.yaml` automatically. `protomolecule.yaml` is never edited directly.

When invoked, Claude:

1. Reads `activities/registry.md` and parses all activity entries
2. Reads current `protomolecule.yaml`
3. Diffs — identifies new, changed, or removed activities
4. For each new or changed activity:
  - Infers schedule from the Instructions field (e.g. "check if it's Monday" → `daily` schedule, Monday only, `catchup: true`)
  - Infers `requires_network` from whether the instructions involve external calls — defaults to `true`
  - Sets `catchup: true` by default for `daily` and `weekly` activities
  - Identifies dependencies by looking for explicit references to other activities' output files in the instructions
  - Flags anything ambiguous for human review rather than guessing
5. Updates `protomolecule.yaml` with changes
6. Removes inline day/time check logic from activity Instructions that should now be handled by the scheduler (e.g. "Step 0: Check if it's Monday — if not, skip") — this logic belongs in the config, not the agent
7. Prints a clear summary of all changes made and anything flagged for review

---

## Component 7: DAG Execution Model

Workflows support linear chains and fan-out. No conditional branching — conditional logic lives inside individual activity tools, not in the workflow definition.

Supported shapes:

```
# Linear chain
A → B → C

# Fan-out (parallel after dependency)
A → B
A → C

# Combined
A → B → D
A → C → D
```

### Cycle Detection

At config load time (both startup and hot-reload), protomolecule must validate the dependency graph for cycles. Build the graph of all `depends_on` relationships within each workflow and run a topological sort (Kahn's algorithm). If the sort fails — i.e., a cycle exists — reject the config with an error message that identifies the activities in the cycle. A cyclic dependency would cause the affected activities to wait on each other indefinitely, silently consuming worker slots with no visible error.

Cycle detection must also catch self-references (an activity that depends on itself).

### Execution Rules

- A step runs when all its `depends_on` activities have succeeded with the same cycle ID as the current workflow trigger (see Run Cycle Identity in Component 1)
- If a step fails (all retries exhausted), all downstream dependents are skipped for that cycle ID
- Independent steps (no dependency relationship) run in parallel within the worker pool
- Dependency relationships are defined in `protomolecule.yaml`, not in the activity registry
- When a workflow activity completes, the scheduler immediately evaluates dependents for the same cycle ID and dispatches any that are now unblocked — this is event-driven, not deferred to the next tick
- Activities inside a workflow inherit the workflow's schedule and must not have their own `schedule` field. Config validation rejects a `schedule` field on a workflow-nested activity.
- Each activity ID must appear exactly once across all workflows and standalone sections. Duplicates are rejected at config validation.

---

## Component 8: launchd Plist

### `protomolecule install-plist`

An interactive command that prompts for key information, generates the plist, and installs it via `launchctl`. Prompts:

- **Project root path** — absolute path to the project directory (default: current working directory)
- **Binary path** — path to the compiled binary (default: `<project-root>/bin/protomolecule`)
- **Launchd label** — reverse-DNS service label (default: `com.protomolecule.daemon`)

After collecting input, the command:

1. Generates the plist at `launchd/<label>.plist` (e.g. `com.protomolecule.daemon.plist`) with `RunAtLoad: true`, `KeepAlive: true`, stdout/stderr captured to `logs/`
2. Copies it to `~/Library/LaunchAgents/`
3. Loads it via `launchctl load`
4. Confirms the daemon is running

Re-running `install-plist` unloads the existing plist before reinstalling, making it safe to use after a binary path change or project move.

---

## File and Directory Layout

```
protomolecule/                  # repo root — Go source and binary
    main.go
    scheduler.go
    executor.go
    state.go
    config.go
    network.go
    worker.go
    status.go           # Unix socket listener and status response
    tui.go              # bubbletea TUI for --watch mode
    scheduler_test.go
    state_test.go
    config_test.go
    worker_test.go
    bin/
        protomolecule           # compiled Go binary
    Makefile                    # build, clean, install-plist targets

state/
    activity-state.json         # scheduler state store (owned by scheduler)
    protomolecule.sock          # Unix socket for IPC (created on start, cleaned up on shutdown)

protomolecule.yaml              # workflow and schedule config (generated by sync-activities)

activities/
    registry.md                 # activity definitions (the only file that should be manually edited)

.claude/skills/
    sync-activities/
        SKILL.md                # sync-activities skill

logs/
    protomolecule.log           # scheduler process log (stdout, captured by launchd)
    protomolecule-error.log     # scheduler error log (stderr, captured by launchd)
    loop-*.jsonl                # per-run audit logs (retained for history)

launchd/
    com.protomolecule.daemon.plist
```

---

## Workflow Visualization

Protomolecule can generate Mermaid diagrams of configured workflows for visual inspection. Diagrams are state-aware — nodes are color-coded based on current activity state read from `state/activity-state.json`.

### Command

```bash
protomolecule visualize                                      # generates all workflows
protomolecule visualize --workflow monday-brief-pipeline     # specific workflow
```

### Output

A single Markdown file containing one Mermaid diagram with all workflows and standalone activities:

```
output/workflows/protomolecule.md
```

All workflows and standalone activities appear in the same diagram. Workflow nodes are visually grouped using Mermaid subgraphs so the boundary between workflows is clear.

### Node Color Coding


| State     | Color      | Meaning                                 |
| --------- | ---------- | --------------------------------------- |
| Success   | Green      | Last run succeeded                      |
| Failed    | Red        | Last run failed (all retries exhausted) |
| Running   | Yellow     | Currently executing                     |
| Never run | Grey       | No successful run recorded              |
| On-demand | Light grey | Never runs automatically                |


### Example Output

```markdown
# Protomolecule Workflows

Generated: 2026-03-07 10:32:01

classDef success fill:#27AE60,color:#fff
classDef failed fill:#E74C3C,color:#fff
classDef running fill:#F39C12,color:#fff
classDef never fill:#95A5A6,color:#fff

flowchart TD
    subgraph monday-brief-pipeline ["monday-brief-pipeline — 0 9 * * 1"]
        monday-brief --> notify-brief-published
        monday-brief --> notify-critical-alerts
    end

    subgraph daily-reporting-pipeline ["daily-reporting-pipeline — 0 8 * * *"]
        project-manager-daily --> slack-project-manager-review
    end

    subgraph standalone ["Standalone"]
        check-jira-tickets
        weekly-exec-report
        slack-check
    end

class monday-brief success
class notify-brief-published success
class notify-critical-alerts failed
class project-manager-daily running
class slack-project-manager-review never
class check-jira-tickets success
class weekly-exec-report success
class slack-check success
```

Each node shows the activity ID. Workflows are grouped in labeled subgraphs showing the workflow ID and schedule. The diagram includes a generation timestamp.

---

## Observability

### Overview

Protomolecule provides three observability surfaces:

1. **Scheduler log** (`logs/protomolecule.log`) — moderately verbose, captured by launchd, sufficient to answer "why didn't X run?" and "what went wrong?"
2. `**protomolecule status`** — one-shot snapshot of current state via Unix socket IPC with the running daemon
3. `**protomolecule status --watch**` — live TUI that streams updates from the daemon

### Unix Socket IPC

The daemon listens on `state/protomolecule.sock`. The socket is created on daemon start and cleaned up on graceful shutdown. The status command connects to the socket, sends a JSON request, and receives a JSON response.

**Stale socket recovery:** If the daemon is killed via `SIGKILL` or panics, the graceful shutdown handler will not execute and the socket file will persist. On startup, protomolecule must handle this: if `protomolecule.sock` already exists, attempt to `net.Dial` it. If the connection is refused, the previous instance is dead — delete the stale socket file and proceed with creating a new listener. If the connection succeeds, another instance is already running — log an error and exit. Without this recovery logic, a single `SIGKILL` creates a permanent crash loop where the new instance fails with "address already in use" on every restart.

Protocol: newline-delimited JSON. Request specifies the type (e.g. `{"type":"status"}`). Daemon responds with a single JSON payload.

The status response includes:

- **Active jobs**: activity ID, start time (ISO 8601), elapsed duration, retry attempt number
- **Queued jobs**: activity ID, reason waiting (dependency unsatisfied, no network, worker slots full)
- **Last run per activity**: start time, duration, status, output path, error if any
- **Next scheduled firing per activity**: computed from cron expression and current state
- **Daemon uptime and tick count**

### `protomolecule status` (one-shot)

Connects to the daemon, fetches a snapshot, prints a human-readable table, and exits. Default mode.

Example output:

```
Protomolecule — uptime 4h 23m                    last tick: 2s ago

ACTIVE
  check-jira-tickets            running    started 09:14:22    0:42    attempt 1/3
  project-manager-daily     running    started 09:12:51    2:13    attempt 1/3

QUEUED
  slack-project-manager-review    waiting on: project-manager-daily

UPCOMING
  slack-check               in 3m
  weekly-exec-report        in 2d 14h
  monday-brief              in 4d 9h

RECENT
  slack-check               success    09:15:01    duration 0:04
  check-jira-tickets            success    09:02:10    duration 1:32
  weekly-exec-report        failed     Thu 16:00   duration 4:17    (exhausted 3/3 attempts)
```

### `protomolecule status --watch` (live TUI)

Opens a live-updating terminal UI powered by `charmbracelet/bubbletea` and `charmbracelet/lipgloss`. Polls the daemon via Unix socket on a short interval (e.g. every second) and re-renders on each update. Same layout as the one-shot view but refreshes in place. Exit with `q` or `Ctrl-C`.

### `protomolecule status --json`

One-shot snapshot in JSON format for scripting. Same data as the human-readable output.

### Additional Go Libraries

- `charmbracelet/bubbletea` — TUI framework
- `charmbracelet/lipgloss` — TUI styling

---

## Testing Requirements

### Principles

All tests must be fully automated and runnable by Claude Code without human involvement. The complete test suite must pass with:

```bash
go test -race ./...
```

No real `claude -p` invocations, no real network calls, no real filesystem side effects outside of temp directories, and no real time dependencies. Tests must pass in CI-like conditions with no assumptions about the local environment, credentials, or running services.

### Design Requirements for Testability

These architectural constraints are mandatory — they are not optional refactors. Claude Code must design with these in place from the start:

**1. Executor must be an interface**

The real executor shells out to `claude -p`. Tests must never do this. Define an `Executor` interface and inject it into the scheduler:

```go
type Executor interface {
    Run(ctx context.Context, activity Activity) (ExecutorResult, error)
}
```

The real implementation uses `os/exec`. Tests use a `FakeExecutor` that returns configurable success/failure immediately without spawning any process.

**2. Clock must be injectable**

Never call `time.Now()` directly in scheduler logic. Define a `Clock` interface and inject it:

```go
type Clock interface {
    Now() time.Time
}
```

The real implementation wraps `time.Now()`. Tests use a `FakeClock` that returns a controlled time. This is essential for testing schedule evaluation, catchup behavior, and daily/weekly window logic without waiting for real time to pass.

**3. Network check must be injectable**

Define a `NetworkChecker` interface:

```go
type NetworkChecker interface {
    IsAvailable() bool
}
```

Tests use a `FakeNetworkChecker` that returns configurable true/false.

**4. State store must use temp directories**

All state store tests must write to `t.TempDir()` and clean up automatically. No tests write to `state/` in the real project directory.

**5. No global state**

The scheduler must be fully instantiable as a struct with injected dependencies. No package-level globals, no `init()` side effects that touch the filesystem or network.

---

### Unit Tests

#### scheduler_test.go — Schedule Evaluation

Table-driven tests covering every schedule rule and edge case. Each test case specifies: current time (via FakeClock), last success time, schedule type, catchup flag, and expected due/not-due outcome.

Required cases:


| Case                                                                                  | Description                                                 |
| ------------------------------------------------------------------------------------- | ----------------------------------------------------------- |
| `0 9 * * `*, never run                                                                | Due                                                         |
| `0 9 * * *`, ran today before 9am                                                     | Not due until 9am                                           |
| `0 9 * * *`, ran yesterday, now past 9am today                                        | Due                                                         |
| `0 9 * * *`, catchup=true, last success yesterday, laptop was asleep at 9am, now 10am | Due — last scheduled firing (9am) is after last success     |
| `0 9 * * *`, catchup=true, last success today at 9am, now 10am                        | Not due — already succeeded after last scheduled firing     |
| `0 9 * * *`, catchup=false, laptop was asleep at 9am, now 10am                        | Not due — missed firing ignored                             |
| `*/15 * * * *`, catchup=true, asleep from 9:00 to 10:05, last success 8:45            | Due — dispatches once for 10:00 window only, not four times |
| `0 9 * * 1`, it's Tuesday                                                             | Not due                                                     |
| `0 9 * * 1`, it's Monday at 9:01am, not yet run                                       | Due                                                         |
| `*/15 * * * *`, ran 10 minutes ago                                                    | Not due                                                     |
| `*/15 * * * *`, ran 16 minutes ago                                                    | Due                                                         |
| `on-demand`                                                                           | Never due                                                   |
| Network required, no network                                                          | Not dispatched                                              |
| Network required, network available                                                   | Dispatched                                                  |
| Network not required, no network                                                      | Dispatched                                                  |


#### scheduler_test.go — DST Transitions


| Case                                                       | Description                                                           |
| ---------------------------------------------------------- | --------------------------------------------------------------------- |
| Spring forward: `0 2 * * *`, clock jumps from 1:59 to 3:00 | Fires once at 3:00, not skipped                                       |
| Spring forward: catchup=true, same schedule                | No duplicate — catchup detects the 3:00 run satisfies the 2:00 window |
| Fall back: `0 1 * * *`, 1:00 AM occurs twice               | Fires on first occurrence only                                        |
| Fall back: catchup=true, ran during first 1:00 AM          | Not due during second 1:00 AM                                         |


#### scheduler_test.go — Dependency Resolution

Tests for workflow DAG evaluation. All dependency tests must use explicit cycle IDs:


| Case                                                            | Description                                           |
| --------------------------------------------------------------- | ----------------------------------------------------- |
| Activity with no depends_on                                     | Immediately queueable                                 |
| Activity whose depends_on succeeded with matching cycle ID      | Queueable                                             |
| Activity whose depends_on succeeded with different cycle ID     | Not queueable — stale success from previous cycle     |
| Activity whose depends_on has not run yet                       | Not queueable, waits                                  |
| Activity whose depends_on failed this cycle (retries exhausted) | Skipped, not queued                                   |
| Fan-out: both children queue after parent succeeds              | Both dispatched with same cycle ID                    |
| Chain: A→B→C, B fails                                           | C is skipped for this cycle ID                        |
| Parent completes mid-tick, dependent dispatched immediately     | Event-driven dispatch, does not wait for next tick    |
| Force-run generates distinct cycle ID                           | Force cycle ID does not conflict with scheduled cycle |
| DAG cycle detected at config load                               | Config rejected with error identifying the cycle      |
| Self-referencing activity (depends_on itself)                   | Config rejected                                       |


#### scheduler_test.go — Polling Activities


| Case                                                         | Description                                       |
| ------------------------------------------------------------ | ------------------------------------------------- |
| `*/15 * * * *`, catchup=false, ran 10 minutes ago            | Not due                                           |
| `*/15 * * * *`, catchup=false, ran 16 minutes ago            | Due                                               |
| `*/15 * * * *`, catchup=false, missed 3 windows while asleep | Fires once on wake (next tick), does not catch up |


#### state_test.go — State Store


| Case                                                 | Description                                                         |
| ---------------------------------------------------- | ------------------------------------------------------------------- |
| Write and read round-trip                            | State persists correctly                                            |
| Missing state file                                   | Returns empty state, no error                                       |
| Corrupt state file                                   | Copies to `.corrupt.<timestamp>`, returns empty state, logs warning |
| Zero-length state file                               | Same as corrupt — recovery triggered                                |
| Atomic write: temp file written then renamed         | No intermediate state visible to concurrent reader                  |
| Atomic write: simulate crash before rename           | Original file unchanged                                             |
| Concurrent reads and writes from multiple goroutines | No data races (run with -race)                                      |
| Concurrent writes to different activity IDs          | Both writes succeed, neither lost                                   |
| Update single activity, others unchanged             | Partial update correct                                              |
| Mutex held during write, concurrent read blocks      | Read waits, does not return stale data                              |
| Cycle ID persisted and read back correctly           | `last_cycle_id` round-trips for workflow activities                 |


#### config_test.go — Config Parsing and Validation


| Case                                                | Description                                                             |
| --------------------------------------------------- | ----------------------------------------------------------------------- |
| Valid protomolecule.yaml                            | Parsed correctly                                                        |
| Missing optional fields                             | Defaults applied correctly (timeout_minutes=10, retry_delay_seconds=30) |
| Unknown fields                                      | Handled gracefully                                                      |
| Valid cron expression                               | Parsed and evaluated correctly                                          |
| Invalid cron expression                             | Returns parse error                                                     |
| `on-demand` schedule                                | Never evaluated as due                                                  |
| Empty workflows section                             | No panic                                                                |
| DAG cycle (A depends on B, B depends on A)          | Rejected with error identifying cycle                                   |
| Self-referencing activity                           | Rejected                                                                |
| Multi-node cycle (A→B→C→A)                          | Rejected                                                                |
| `depends_on` references nonexistent activity ID     | Rejected                                                                |
| Activity ID appears in both workflow and standalone | Rejected                                                                |
| Duplicate activity ID across two workflows          | Rejected                                                                |
| `schedule` field on workflow-nested activity        | Rejected                                                                |
| Missing `timezone` field                            | Defaults to local time, logs warning                                    |
| Invalid IANA timezone                               | Rejected with parse error                                               |
| Hot reload: valid new config                        | Swapped in, old config replaced                                         |
| Hot reload: invalid new config                      | Rejected, old config retained, error logged                             |
| Hot reload: debounce — two rapid file events        | Only one parse attempt after debounce window                            |


#### worker_test.go — Worker Pool


| Case                                              | Description                                                     |
| ------------------------------------------------- | --------------------------------------------------------------- |
| Respects worker_slots limit                       | Never exceeds N concurrent executors                            |
| All slots full, new activity queued               | Waits until slot opens                                          |
| Executor returns success                          | State updated correctly                                         |
| Executor returns failure                          | State updated, other activities unaffected                      |
| Executor fails once, succeeds on retry            | Marked success, retry count logged                              |
| Executor fails all retries (default 2)            | Marked failed after 3 total executions                          |
| max_retries=0                                     | No retries, fails immediately on first failure                  |
| Per-activity max_retries overrides global         | Correct retry count used                                        |
| Downstream dependent, parent exhausted retries    | Dependent skipped                                               |
| Retry delay respected                             | Second attempt not dispatched until retry_delay_seconds elapsed |
| Per-activity retry_delay_seconds overrides global | Correct delay used                                              |
| Activity exceeds timeout_minutes                  | Executor returns timeout error, activity marked failed          |
| Per-activity timeout_minutes overrides global     | Correct timeout used                                            |
| Timed-out activity retried                        | Retry uses fresh timeout window                                 |
| Run with -race flag                               | No data races                                                   |


---

### Integration Tests (Build Verification Only)

A small set of integration tests that verify the binary builds and basic wiring is correct, without invoking real external dependencies. Tag these with `//go:build integration` so they are excluded from `go test ./...` by default and only run explicitly with `go test -tags integration ./...`.

These tests:

- Build the binary and verify exit codes for `--help`, `--version`, and `run --force` with a fake config
- Verify the state store file is created in the expected location
- Verify the config hot reload fires on file change (using a real fsnotify watch against a temp file)
- Verify stale socket cleanup on startup — create a dead socket file, start the daemon, confirm it recovers

---

### What Is Not Tested Automatically

- Real `claude -p` execution — verified manually during migration
- Real Slack API calls — verified manually
- launchd plist installation and keep-alive behavior — verified manually
- End-to-end activity execution — verified manually using `protomolecule run --activity <id>`

---

### Running Tests

```bash
# Full test suite with race detector (required to pass before any commit)
go test -race ./...

# Verbose output
go test -race -v ./...

# Specific package
go test -race ./protomolecule/...

# Integration tests only
go test -tags integration ./...

# Coverage report
go test -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

Claude Code must ensure `go test -race ./...` passes with zero failures and zero race conditions before considering the implementation complete.

---

## Future Improvements

The following are not in scope for the initial implementation but are documented here to preserve context for future iterations.

### Backoff Scheduling

Adaptive polling where an activity runs frequently when there is work and backs off when quiet. Deferred because it introduces significant complexity: a three-way result signal (found/empty/fail), business hours configuration, per-activity interval state, and a structured output or result-file protocol to work around exit code unreliability with LLM subprocesses.

For the initial release, polling activities like Slack message checking use a fixed-interval cron schedule (e.g., `*/15 * * * *`) with `catchup: false`. This is less token-efficient than adaptive backoff but eliminates the entire result signaling and interval management subsystem.

When revisited, the design should address: structured output vs. result files for signaling, business hours configuration (timezone-aware start/end times and weekday filtering), and the interaction between backoff intervals and the retry delay system.

### Log Rotation

`protomolecule.log` will grow unboundedly. Structured JSON at moderate verbosity could produce 1–10 MB per day. Over months this becomes a multi-gigabyte file. Launchd does not rotate logs. The recommended approach is `gopkg.in/natefinished/lumberjack.v2` as the log output writer, with configurable max size (e.g., 50 MB), backup count (e.g., 5), and max age (e.g., 30 days). Alternatives include macOS `newsyslog` or unified logging, but lumberjack is the most portable and keeps logs as greppable JSONL files.

### IPC Protocol Hardening

The Unix socket protocol is currently minimal (newline-delimited JSON, single request type). Future hardening: set socket permissions to `0600`, enforce a read deadline on connections (e.g., 5 seconds), limit request size (e.g., 4 KB), and document the full set of request types with a versioned envelope format. For `--watch` mode, evaluate whether a streaming subscription model is worth the complexity over the current poll-per-second approach.

### Network Probe Specificity

The current "lightweight network check" is underspecified. A generic connectivity check (ping, DNS) can succeed while the actual APIs the activities need are unreachable (captive portal, firewall, API outage). A more robust approach: probe a specific API endpoint (e.g., Anthropic's status page) with a strict timeout, verifying DNS, TCP, and TLS. This more closely mimics what `claude -p` invocations actually require. The tradeoff is coupling the probe to a specific service — if activities talk to different APIs, multiple probes may be needed.

### Worker Pool Fairness

With 5 slots and a handful of activities, starvation is unlikely. At scale, a burst of long-running tasks from one workflow could block shorter standalone activities. Future options: per-workflow slot limits, priority levels, or round-robin scheduling by workflow. Not needed until the activity count grows significantly.

### Graceful Shutdown Sequence

Currently unspecified beyond socket cleanup. A proper shutdown sequence on SIGTERM: stop the tick loop and stop dispatching new activities, let running activities continue for a grace period (e.g., 20 seconds), send SIGTERM to remaining subprocesses, wait 5 seconds, SIGKILL survivors, flush state store, close socket, exit. The total shutdown budget should fit within launchd's `ExitTimeOut`. Activities killed during shutdown should be recorded with a `killed_during_shutdown` status in the state store so catchup logic can make informed decisions on restart.

### sync-activities Skill Testing

The sync-activities skill makes LLM-driven inference decisions (schedule, dependencies, network requirements) that directly control scheduler behavior, but there are no test cases or evaluation criteria for these inferences. A future addition: a table of "given this activity description, expect this config output" cases that can be run against the skill to detect regressions. This is separate from the Go test suite and would live alongside the skill definition.

### Temporary and Expiring Jobs

Support for jobs that run a limited number of times or expire after a date. Use cases: a one-shot task scheduled for a future time ("remind me to follow up on the proposal next Tuesday"), a job that runs daily for a week then auto-removes, or a recurring check that should stop after a condition is met. Possible config fields: `expires_at` (ISO 8601 timestamp after which the activity is ignored), `max_runs` (auto-disable after N successful executions), or `ttl_days` (auto-expire N days after creation). Expired activities should be cleaned from the config automatically or flagged for removal by the sync-activities skill rather than accumulating as dead entries.

### Worker Slot Tuning

The default of 5 concurrent slots is a starting point. May need adjustment based on observed API rate limits and the typical number of concurrent activities in practice.

### Build Tooling

Create a `Makefile` at the repo root with targets for `build` (compiles to `bin/protomolecule`) and `clean`.

---

## Migration from Current System

1. Build and test protomolecule binary
2. Run sync-activities skill to generate initial `protomolecule.yaml` from existing `activities/registry.md`
3. Review generated config, verify dependencies and schedules are correct
4. Install launchd plist for protomolecule
5. Unload and remove any existing polling-agent launchd plist
6. The previous self-scheduling agent definition (if any) is retired — do not delete immediately, keep as reference during transition

