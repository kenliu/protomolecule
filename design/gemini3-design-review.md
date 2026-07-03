# Protomolecule Design Critique & Architectural Review

## Overview
The design for moving from a log-grepping polling loop to a dedicated Go-based scheduler with a worker pool is a significant architectural upgrade. The separation of concerns between the scheduler, the state store, and the executor is logically sound. However, there are several areas where the design introduces potential race conditions, state corruption risks, and operational fragility.

## 1. State Store Integrity (Atomic Writes vs. flock)
**The Design:** The system protects `state/activity-state.json` with an in-process `sync.RWMutex` and an `flock` file lock. The state store is owned exclusively by the scheduler.

**The Issue:** `flock` is a cooperative lock that does nothing to protect the file if the writer dies mid-write. If `launchd` sends a `SIGTERM` or `SIGKILL` while the JSON encoder is flushing, the file will be truncated. The next boot will result in a JSON parsing panic, destroying all schedule history.

**The Fix:** Implement the "atomic rename" pattern. Write the new state to a temporary file (e.g., `state/activity-state.tmp`), call `fsync()`, and then use `os.Rename()` to overwrite the original. POSIX guarantees this rename is atomic. Because the scheduler is the exclusive writer, you can safely drop the `flock` requirement entirely and rely purely on the `sync.RWMutex` for internal thread safety and atomic renames for crash safety.

## 2. The Danger of "Immediate" Retries
**The Design:** Failed activities are retried up to `max_retries` times, and retries are attempted immediately without delay by re-queuing into the worker pool. 

**The Issue:** The executor shells out to `claude -p`, relying heavily on external APIs. The majority of transient failures here will be `HTTP 429 Too Many Requests`. Immediately re-queuing an activity that just hit a rate limit guarantees the worker pool will pick it up milliseconds later, hit the same limit, and burn through all default attempts in under a second.

**The Fix:** Introduce a mandatory delay (e.g., linear or exponential backoff) for retries. When an executor fails, calculate a future retry timestamp. The activity is queued, but the worker pool must ignore it until the clock passes that timestamp.

## 3. Relying on an LLM for POSIX Exit Codes
**The Design:** For backoff scheduling, the executor captures exit codes (`0`, `1`, `2`) to determine if work was found, assuming the LLM will follow instructions passed via the `sync-activities` skill. 

**The Issue:** Asking a non-deterministic LLM to act as a deterministic POSIX utility is highly fragile. If the agent encounters an unexpected error or outputs conversational text instead of triggering an OS exit command, the binary will likely exit with `0` or `1`, silently breaking the backoff logic.

**The Fix:** Do not read the exit code for business logic. Since `claude -p` is configured with `--output-format stream-json`, require the agent to output a specific JSON payload upon completion (e.g., `{"work_found": true}`). The Go executor can parse this stdout stream to manage backoff intervals safely.

## 4. Ambiguity of the "Run Cycle" in the DAG
**The Design:** A workflow step runs when all its `depends_on` activities have succeeded in the "current run cycle". 

**The Issue:** The scheduler runs on a 1-minute tick loop. If a dependency succeeds at 09:02, how does the downstream activity know if that success belongs to the scheduled 9:00 AM cycle or a manual force-run at 08:55? The JSON state simply tracks `last_success`.

**The Fix:** Formalize a "Workflow Instance ID" (e.g., using the scheduled cron timestamp like `2026-03-08T09:00:00Z`). Pass this ID to the state store upon success. Downstream activities should only evaluate as "due" if their parent succeeded with that specific Run ID.

## 5. fsnotify Race Conditions on Config Reloads
**The Design:** Protomolecule uses `fsnotify` to watch `protomolecule.yaml` for live reloads. 

**The Issue:** File writes are not instantaneous. `fsnotify` will fire a `WRITE` event the moment the `sync-activities` skill opens the file. If the Go scheduler instantly attempts to parse it, it will read a half-written file, resulting in a YAML parsing error that could drop all workflows.

**The Fix:** Implement a debounce timer (e.g., 500ms) in the `config.go` event loop. Alternatively, mandate that `sync-activities` uses atomic renames, so `fsnotify` only sees a clean `RENAME` or `CREATE` event.

## 6. Unix Socket Leftovers
**The Design:** The daemon listens on `state/protomolecule.sock` and cleans it up via graceful shutdown. 

**The Issue:** If `launchd` sends a `SIGKILL` or the daemon panics, the graceful shutdown will not execute. Upon restart, `net.Listen("unix", ...)` will fail with an "address already in use" error, causing a permanent crash loop.

**The Fix:** Implement aggressive socket cleanup on startup. Check if `protomolecule.sock` exists; if it does, attempt to `net.Dial` it. If the connection is refused, the previous instance crashed—safely delete the stale socket file and proceed.

## 7. Vague Network Checking
**The Design:** The system uses a "lightweight check" to determine network availability before dispatching activities. 

**The Issue:** A simple ICMP `ping` might succeed while the actual required APIs (like Anthropic or Slack) are blocked by a firewall or captive portal. This leads to false positives and a flood of failing activities.

**The Fix:** Define the check explicitly. Perform an HTTP `HEAD` or `GET` request to the primary API endpoint (e.g., Anthropic's status page) with a strict timeout. This verifies DNS resolution, TCP routing, and TLS handshakes, closely mimicking the actual requirements of the `claude -p` invocations.
