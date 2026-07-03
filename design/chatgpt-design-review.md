# Protomolecule Architecture Critique (Detailed)

## 1) Execution Instance Identity and "Run Cycle" Semantics

### Problem

The design refers to dependencies being satisfied in the "current run
cycle," but the scheduler operates on a minute-based tick loop. Without
a clearly defined execution instance identity, this can lead to: -
Long-running parent tasks spanning multiple ticks and breaking
dependency gating. - Duplicate enqueueing of tasks across ticks. -
Incorrect dependency satisfaction based on stale success state.

### Recommendation

Introduce a first-class **Execution Instance** concept:

    instance_key = (workflow_id OR activity_id, scheduled_fire_time_utc)

All dependency checks, retries, queueing, and completion state should be
keyed by this tuple.

Define explicitly: - Dependencies apply to the same
`scheduled_fire_time`. - Failures skip dependents only for that same
instance. - Tasks cannot be enqueued more than once per instance.

### Tradeoffs

-   **Pros:** Eliminates duplicate runs and ambiguous dependency
    behavior.
-   **Cons:** Requires more state tracking and retention logic.

------------------------------------------------------------------------

## 2) Time Handling: Local Cron vs UTC State and DST

### Problem

Cron is evaluated in local time, but timestamps in logs and state appear
to be UTC. This mismatch can cause: - Incorrect catchup detection. -
DST-related duplicate or missed runs. - Inconsistent behavior across
environments.

### Recommendation

Adopt a strict model:

-   Cron evaluated in explicit configured IANA timezone.
-   All scheduled fire times converted to UTC instants.
-   All stored timestamps persisted in UTC (RFC3339).
-   Explicit DST rules documented (e.g., run first occurrence only
    during fall-back).

### Tradeoffs

-   **Pros:** Deterministic, portable, explainable behavior.
-   **Cons:** Requires timezone-aware cron library and clear
    documentation.

------------------------------------------------------------------------

## 3) Retry Policy and Timeouts

### Problem

Immediate retries without delay risk hammering APIs or consuming all
worker slots. No defined timeout may cause hung subprocesses.

### Recommendation

Add: - `timeout_seconds` (global + per activity) - Exponential backoff
with jitter - Explicit differentiation between infrastructure failure
and task failure

Kill strategy: 1. SIGTERM 2. Wait N seconds 3. SIGKILL

### Tradeoffs

-   **Pros:** Safer failure handling, predictable behavior.
-   **Cons:** More configuration complexity.

------------------------------------------------------------------------

## 4) State Store Reliability

### Problem

Single JSON state file without defined atomic write strategy risks
corruption. No schema versioning defined.

### Recommendation

Use atomic write pattern: 1. Write to temp file 2. fsync temp 3. Rename
to primary file 4. fsync directory

Add: - `schema_version` - `.bak` recovery copy - Clear corruption
recovery procedure

### Tradeoffs

-   **Pros:** High reliability and upgrade safety.
-   **Cons:** Slightly more I/O and code complexity.

------------------------------------------------------------------------

## 5) Network Probe and Business Hours Definition

### Problem

"Lightweight network check" and "business hours" are underspecified,
leading to inconsistent behavior.

### Recommendation

Define network probe type explicitly (DNS, TCP, HTTPS) with configurable
timeout.

Define business hours config:

    business_hours:
      tz: America/New_York
      start: "09:00"
      end: "17:00"
      weekdays: [Mon, Tue, Wed, Thu, Fri]

Clarify behavior outside business hours (pause vs max interval).

### Tradeoffs

-   **Pros:** Predictable and debuggable scheduling.
-   **Cons:** Additional configuration surface area.

------------------------------------------------------------------------

## 6) Config Hot Reload Semantics

### Problem

fsnotify-based reload may process partial writes or create inconsistent
in-flight behavior.

### Recommendation

-   Debounce file change events.
-   Load into new immutable config object.
-   Atomically swap config pointer.
-   Define in-flight reconciliation policy.

### Tradeoffs

-   **Pros:** Stable runtime behavior.
-   **Cons:** Slight reload delay.

------------------------------------------------------------------------

## 7) Backoff Mode Exit Code Semantics

### Problem

Using exit codes to encode business meaning is fragile.

### Recommendation

Prefer structured output signaling:

    {"pm_outcome":"found|empty|fail"}

Treat exit codes as fallback only.

### Tradeoffs

-   **Pros:** Robust and debuggable.
-   **Cons:** Slight protocol complexity increase.

------------------------------------------------------------------------

## 8) Worker Pool Fairness and Starvation

### Problem

Fixed worker pool risks head-of-line blocking and starvation.

### Recommendation

-   Per-activity concurrency limits.
-   Fair scheduling (round-robin by activity/workflow).
-   Optional priorities.

### Tradeoffs

-   **Pros:** Smoother behavior under load.
-   **Cons:** More scheduler logic.

------------------------------------------------------------------------

## 9) IPC Socket Protocol Hardening

### Problem

Simple newline-delimited JSON lacks framing guarantees and auth
constraints.

### Recommendation

Define request/response envelopes:

Request:

    {"type":"status","request_id":"123","params":{}}

Response:

    {"request_id":"123","ok":true,"result":{...}}

Restrict socket permissions (0600) and enforce request size limits.

### Tradeoffs

-   **Pros:** Safer tooling interface.
-   **Cons:** Slightly more protocol complexity.

------------------------------------------------------------------------

## 10) Logging and Retention

### Problem

Unbounded log growth risks disk exhaustion. Legacy log compatibility may
reintroduce grep-based state reliance.

### Recommendation

-   Rotate logs (size-based or OS-managed).
-   Make state file authoritative.
-   Cap legacy retention.

### Tradeoffs

-   **Pros:** Predictable disk usage.
-   **Cons:** Requires rotation strategy decisions.
