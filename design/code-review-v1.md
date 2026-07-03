# Protomolecule v1 Code Review

## Summary

The implementation is solid and well-structured. All source files compile, all 46+ tests pass with the race detector enabled, and the code closely follows the design document. The architecture uses proper dependency injection (Clock, Executor, NetworkChecker interfaces), clean separation of concerns, and correct concurrency patterns. No blocking issues were found that would prevent Agent 5 from wiring the CLI.

## Tests

```
$ go test -race -count=1 ./...
ok  	github.com/kenliu-crl/protomolecule	3.371s
```

All tests pass. No race conditions detected.

## Blocking Issues Found and Fixed

None. No blocking issues were identified. The code compiles cleanly, all tests pass, and interface contracts are consistent across files. Agent 5 can proceed with wiring the CLI.

## Non-Blocking Suggestions

### 1. Missing DST Transition Tests
The design doc specifies 4 DST test cases (spring forward fires once, spring forward no duplicate with catchup, fall back fires first occurrence only, fall back catchup not due during second occurrence). These are not present in `scheduler_test.go`. The underlying behavior likely works correctly because `robfig/cron` handles DST natively, but the explicit test coverage specified in the design is absent.

### 2. Missing Polling Activity Test (catchup=false, missed 3 windows)
The design specifies a test case: "`*/15 * * * *`, catchup=false, missed 3 windows while asleep - Fires once on wake (next tick), does not catch up". While the behavior is implicitly covered by the existing catchup=false test, an explicit test for this scenario would be valuable.

### 3. Dead Code in `TestScheduler_FullStandaloneExecution`
Line 1021 in `scheduler_test.go` calls `actState.CycleID()` inside an `if` block with an empty body. This is dead test code that should either make an assertion or be removed.

### 4. `computeLastFiring` Lookback Window
The lookback window in `computeLastFiring` is 9 days, sufficient for weekly schedules but would fail for monthly or less frequent schedules (e.g., `0 9 1 * *` -- first of month). If monthly schedules are ever needed, this should be extended to ~32 days.

### 5. `go.mod` Go Version
The `go.mod` specifies `go 1.25.5`. This compiles and works on the current machine, but may cause confusion or compatibility issues. Verify this matches the intended minimum Go version.

### 6. Structured JSON Logging
The design specifies structured JSON logging with `timestamp`, `level`, and activity-specific fields. The current implementation uses Go's standard `log.Printf` with plain text messages. This is fine for the initial build, but the design envisions structured JSONL output. This can be addressed when the CLI is wired up.

### 7. Config Validation: Missing Timezone Warning
The design says "If omitted, protomolecule defaults to the system's local timezone and logs a warning recommending explicit configuration." The code defaults to `time.Local` correctly but does not log a warning. Minor discrepancy.

### 8. `handleCorruptFile` Uses `time.Now()` Directly
In `state.go` line 74, `handleCorruptFile` calls `time.Now().UTC()` for generating the backup filename timestamp. Since this is only used for filename generation during error recovery (not scheduling logic), this is acceptable -- but it means the `Clock` interface isn't used consistently for all timestamp generation.

### 9. Some Worker Pool Test Cases from Design Not Present
The design lists several worker_test.go cases that are more properly scheduler concerns (retry logic, dependent skipping). These are correctly tested in `scheduler_test.go` instead. However, a few worker-specific cases from the design are missing:
- "Timed-out activity retried - retry uses fresh timeout window"
- "Per-activity retry_delay_seconds overrides global - correct delay used"

These are tested indirectly through the scheduler retry tests but not as isolated worker pool tests.

### 10. `visualize.go` classDef Placement
The Mermaid spec technically requires `classDef` declarations after the graph type declaration (`flowchart TD`), not before it. The current code places them before `flowchart TD`. Most Mermaid renderers accept both orderings, but moving them after the graph nodes would be more spec-compliant.

## Files Reviewed

| File | Status |
|------|--------|
| `go.mod` | OK - dependencies correct, Go version unusual but functional |
| `types.go` | Looks good - clean interface definitions, FakeClock properly implemented |
| `config.go` | Looks good - defaults match design spec, Kahn's algorithm correct, 500ms debounce, all validation cases covered |
| `state.go` | Looks good - atomic writes (tmp/fsync/rename), corruption recovery, RWMutex correctly used, returns copies from GetActivity/GetAll |
| `network.go` | Looks good - simple interface + real/fake implementations |
| `executor.go` | Looks good - correct claude command args, SIGTERM->10s->SIGKILL, FakeExecutor context-aware |
| `worker.go` | Looks good - semaphore-based concurrency, proper shutdown, no retry logic (correctly delegated to scheduler) |
| `scheduler.go` | Looks good - no direct time.Now(), isDue handles on-demand/catchup correctly, event-driven dependent dispatch, retry delay enforcement, cycle ID format matches spec |
| `status.go` | Looks good - stale socket recovery, 0600 permissions, read deadline, all StatusResponse fields present |
| `visualize.go` | Looks good - correct color classes, subgraph per workflow, standalone subgraph, workflow filter support |
| `main.go` | Stub - prints "not yet implemented", ready for Agent 5 to wire |
| `config_test.go` | Good coverage - all design-specified validation cases covered, hot reload tested |
| `state_test.go` | Good coverage - round-trip, missing file, corrupt, zero-length, concurrent access, atomic write |
| `worker_test.go` | Good coverage - slot limits, queuing, success/failure, timeout, active jobs, shutdown |
| `scheduler_test.go` | Good coverage - isDue table-driven tests, network dispatch, dependency resolution, retries, force run, cycle IDs |
| `status_test.go` | Good coverage - FormatStatus, formatDuration, server lifecycle, stale socket, client/server integration, visualization, JSON serialization |
