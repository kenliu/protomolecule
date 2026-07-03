package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Helper to create a minimal config with timezone
func newTestConfig(tz string) *Config {
	loc, _ := time.LoadLocation(tz)
	return &Config{
		Timezone:          tz,
		WorkerSlots:       5,
		MaxRetries:        2,
		RetryDelaySeconds: 1, // short for tests
		TimeoutMinutes:    10,
		location:          loc,
	}
}

// Helper to create a scheduler with fakes
func newTestScheduler(cfg *Config, clock *FakeClock, network *FakeNetworkChecker, executor *FakeExecutor) (*Scheduler, *StateStore) {
	dir := "/tmp/protomolecule-test-" + time.Now().Format("20060102T150405.000000000")
	statePath := filepath.Join(dir, "state.json")
	state := NewStateStore(statePath)
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	logger := DiscardLogger()
	sched := NewScheduler(cfg, state, pool, clock, network, logger)
	return sched, state
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool { return &b }

// intPtr returns a pointer to an int value.
func intPtr(i int) *int { return &i }

// timePtr returns a pointer to a time.Time value.
func timePtr(t time.Time) *time.Time { return &t }

// ============================================================================
// Schedule Evaluation Tests (table-driven)
// ============================================================================

func TestScheduler_IsDue(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")

	// Monday March 9, 2026 at various times (Monday)
	monday9am := time.Date(2026, 3, 9, 9, 0, 0, 0, loc)
	monday901am := time.Date(2026, 3, 9, 9, 1, 0, 0, loc)
	monday10am := time.Date(2026, 3, 9, 10, 0, 0, 0, loc)
	monday1030am := time.Date(2026, 3, 9, 10, 30, 0, 0, loc)
	sunday9am := time.Date(2026, 3, 8, 9, 0, 0, 0, loc)
	tuesday9am := time.Date(2026, 3, 10, 9, 0, 0, 0, loc)

	tests := []struct {
		name        string
		schedule    string
		taskID  string
		catchup     bool
		now         time.Time
		state       *TaskState // nil = never run
		wantDue     bool
	}{
		{
			name:       "Daily 9am, never run, waits for next window",
			schedule:   "0 9 * * *",
			taskID: "daily-task",
			catchup:    true,
			now:        monday901am,
			state:      nil,
			wantDue:    false,
		},
		{
			name:       "Daily 9am, ran today before 9am",
			schedule:   "0 9 * * *",
			taskID: "daily-task",
			catchup:    true,
			now:        monday901am,
			state: &TaskState{
				LastSuccess: timePtr(time.Date(2026, 3, 9, 8, 0, 0, 0, loc).UTC()),
			},
			wantDue: true,
		},
		{
			name:       "Daily 9am, ran today at 9am",
			schedule:   "0 9 * * *",
			taskID: "daily-task",
			catchup:    true,
			now:        monday10am,
			state: &TaskState{
				LastSuccess: timePtr(monday9am.UTC()),
			},
			wantDue: false,
		},
		{
			name:       "Daily 9am, catchup=true, laptop slept through 9am",
			schedule:   "0 9 * * *",
			taskID: "daily-task",
			catchup:    true,
			now:        monday1030am,
			state: &TaskState{
				LastSuccess: timePtr(sunday9am.UTC()),
			},
			wantDue: true,
		},
		{
			name:       "Daily 9am, catchup=true, already ran today",
			schedule:   "0 9 * * *",
			taskID: "daily-task",
			catchup:    true,
			now:        monday1030am,
			state: &TaskState{
				LastSuccess: timePtr(monday9am.UTC()),
			},
			wantDue: false,
		},
		{
			name:       "Daily 9am, catchup=false, laptop slept through 9am",
			schedule:   "0 9 * * *",
			taskID: "daily-task",
			catchup:    false,
			now:        monday1030am,
			state: &TaskState{
				LastSuccess: timePtr(sunday9am.UTC()),
			},
			wantDue: false,
		},
		{
			name:       "Every 15min, catchup=true, slept 9:00-10:05",
			schedule:   "*/15 * * * *",
			taskID: "frequent-task",
			catchup:    true,
			now:        time.Date(2026, 3, 9, 10, 5, 0, 0, loc),
			state: &TaskState{
				LastSuccess: timePtr(time.Date(2026, 3, 9, 8, 45, 0, 0, loc).UTC()),
			},
			wantDue: true, // catches up once for 10:00 window
		},
		{
			name:       "Weekly Monday 9am, it's Tuesday",
			schedule:   "0 9 * * 1",
			taskID: "weekly-task",
			catchup:    false,
			now:        tuesday9am,
			state: &TaskState{
				LastSuccess: timePtr(monday9am.UTC()),
			},
			wantDue: false,
		},
		{
			name:       "Weekly Monday 9am, never run, waits for next window",
			schedule:   "0 9 * * 1",
			taskID: "weekly-task",
			catchup:    true,
			now:        monday901am,
			state:      nil,
			wantDue:    false,
		},
		{
			name:       "Every 15min, ran 10 min ago",
			schedule:   "*/15 * * * *",
			taskID: "frequent-task",
			catchup:    true,
			now:        time.Date(2026, 3, 9, 10, 10, 0, 0, loc),
			state: &TaskState{
				LastSuccess: timePtr(time.Date(2026, 3, 9, 10, 0, 0, 0, loc).UTC()),
			},
			wantDue: false,
		},
		{
			name:       "Every 15min, ran 16 min ago",
			schedule:   "*/15 * * * *",
			taskID: "frequent-task",
			catchup:    true,
			now:        time.Date(2026, 3, 9, 10, 16, 0, 0, loc),
			state: &TaskState{
				LastSuccess: timePtr(time.Date(2026, 3, 9, 9, 59, 0, 0, loc).UTC()),
			},
			wantDue: true,
		},
		{
			name:       "On-demand schedule",
			schedule:   "on-demand",
			taskID: "manual-task",
			catchup:    true,
			now:        monday9am,
			state:      nil,
			wantDue:    false,
		},
		{
			name:       "Empty schedule",
			schedule:   "",
			taskID: "no-schedule",
			catchup:    true,
			now:        monday9am,
			state:      nil,
			wantDue:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &FakeClock{T: tt.now}
			cfg := newTestConfig("America/New_York")
			executor := NewFakeExecutor()
			network := &FakeNetworkChecker{Available: true}
			sched, state := newTestScheduler(cfg, clock, network, executor)
			_ = sched

			// Set up state if provided
			if tt.state != nil {
				state.UpdateTask(tt.taskID, func(as *TaskState) {
					*as = *tt.state
				})
			}

			got := sched.isDue(tt.schedule, tt.taskID, tt.catchup, tt.now, loc)
			if got != tt.wantDue {
				t.Errorf("isDue() = %v, want %v", got, tt.wantDue)
			}
		})
	}
}

// ============================================================================
// Network-Dependent Dispatch Tests
// ============================================================================

func TestScheduler_NetworkRequired_NoNetwork(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{
			ID:              "net-task",
			Schedule:        "*/15 * * * *",
			Catchup:         true,
			RequiresNetwork: boolPtr(true),
			Prompt:    "do network stuff",
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: false}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("net-task", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	sched.tick()

	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches with no network, got %d", len(calls))
	}
}

func TestScheduler_NetworkRequired_WithNetwork(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{
			ID:              "net-task",
			Schedule:        "*/15 * * * *",
			Catchup:         true,
			RequiresNetwork: boolPtr(true),
			Prompt:    "do network stuff",
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("net-task", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	sched.tick()

	// Drain results
	drainResults(sched.pool, 1, 100*time.Millisecond)

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Errorf("expected 1 dispatch with network, got %d", len(calls))
	}
}

func TestScheduler_NetworkNotRequired_NoNetwork(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{
			ID:              "local-task",
			Schedule:        "*/15 * * * *",
			Catchup:         true,
			RequiresNetwork: boolPtr(false),
			Prompt:    "local work",
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: false}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("local-task", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	sched.tick()

	// Drain results
	drainResults(sched.pool, 1, 100*time.Millisecond)

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Errorf("expected 1 dispatch even without network (not required), got %d", len(calls))
	}
}

// ============================================================================
// Dependency Resolution Tests
// ============================================================================

func TestScheduler_RootTaskDispatched(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "root-task", Prompt: "do root work"},
				{ID: "dependent-task", DependsOn: "root-task", Prompt: "do dependent work"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers the workflow due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	sched.tick()

	drainResults(sched.pool, 1, 100*time.Millisecond)

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 dispatch (root only), got %d", len(calls))
	}
	if calls[0].TaskID != "root-task" {
		t.Errorf("dispatched task = %q, want root-task", calls[0].TaskID)
	}
}

func TestScheduler_DependentDispatchedAfterParentSuccess(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "step1", Prompt: "step 1"},
				{ID: "step2", DependsOn: "step1", Prompt: "step 2"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers the workflow due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Tick to dispatch step1
	sched.tick()

	// Get step1 result
	result := <-sched.pool.Results()
	if result.Job.Task.ID != "step1" {
		t.Fatalf("expected step1 result, got %q", result.Job.Task.ID)
	}
	if result.Result.Status != "success" {
		t.Fatalf("step1 status = %q, want success", result.Result.Status)
	}

	// Handle completion - should trigger step2 dispatch
	sched.handleCompletion(result)

	// step2 should now be dispatched
	result2 := <-sched.pool.Results()
	if result2.Job.Task.ID != "step2" {
		t.Fatalf("expected step2 result, got %q", result2.Job.Task.ID)
	}
}

func TestScheduler_DependentNotDispatchedWithStaleyCycleID(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "step1", Prompt: "step 1"},
				{ID: "step2", DependsOn: "step1", Prompt: "step 2"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Set up state: step1 succeeded but with a different cycle ID
	now := clock.Now()
	state.UpdateTask("step1", func(as *TaskState) {
		as.LastRun = &now
		as.LastRunStatus = "success"
		as.LastSuccess = &now
		as.LastCycleID = "pipeline:2026-03-08T09:00:00Z" // yesterday's cycle
	})

	// Try to dispatch dependents for today's cycle
	todayCycleID := "pipeline:2026-03-09T09:00:00Z"
	sched.dispatchReadyDependents("step1", todayCycleID, cfg)

	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches with stale cycle ID, got %d", len(calls))
	}
}

func TestScheduler_FanOut_BothChildrenDispatched(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "parent", Prompt: "parent work"},
				{ID: "child-a", DependsOn: "parent", Prompt: "child a"},
				{ID: "child-b", DependsOn: "parent", Prompt: "child b"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers the workflow due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Dispatch parent
	sched.tick()

	// Complete parent
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Both children should be dispatched
	results := make(map[string]bool)
	for i := 0; i < 2; i++ {
		r := <-sched.pool.Results()
		results[r.Job.Task.ID] = true
	}

	if !results["child-a"] {
		t.Error("child-a was not dispatched")
	}
	if !results["child-b"] {
		t.Error("child-b was not dispatched")
	}
}

func TestScheduler_Chain_BFails_CSkipped(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 0 // no retries, fail immediately
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "step-a", Prompt: "step a"},
				{ID: "step-b", DependsOn: "step-a", Prompt: "step b"},
				{ID: "step-c", DependsOn: "step-b", Prompt: "step c"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	executor.SetResult("step-b", ExecutorResult{
		TaskID: "step-b",
		Status:     "failed",
		Error:      "boom",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers the workflow due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Dispatch step-a
	sched.tick()
	resultA := <-sched.pool.Results()
	sched.handleCompletion(resultA)

	// step-b should be dispatched (dependent of step-a)
	resultB := <-sched.pool.Results()
	if resultB.Job.Task.ID != "step-b" {
		t.Fatalf("expected step-b, got %q", resultB.Job.Task.ID)
	}

	// Handle step-b failure
	sched.handleCompletion(resultB)

	// step-c should NOT be dispatched because step-b failed
	// Give a brief moment to check
	calls := executor.Calls()
	dispatched := make(map[string]bool)
	for _, c := range calls {
		dispatched[c.TaskID] = true
	}
	if dispatched["step-c"] {
		t.Error("step-c should not have been dispatched after step-b failure")
	}
}

func TestScheduler_DependentNotDispatchedWhenParentNotRun(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "parent", Prompt: "parent"},
				{ID: "child", DependsOn: "parent", Prompt: "child"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Try to dispatch dependents without parent having run
	sched.dispatchReadyDependents("parent", "pipeline:2026-03-09T09:00:00Z", cfg)

	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches when parent not run, got %d", len(calls))
	}
}

// ============================================================================
// Retry Tests
// ============================================================================

func TestScheduler_RetryScheduled(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 5
	cfg.Standalone = []TaskConfig{
		{ID: "flaky", Schedule: "0 9 * * *", Catchup: true, Prompt: "flaky work"},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	executor.SetResult("flaky", ExecutorResult{
		TaskID: "flaky",
		Status:     "failed",
		Error:      "transient error",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("flaky", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Dispatch
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Check state: retry should be scheduled
	actState := state.GetTask("flaky")
	if actState == nil {
		t.Fatal("expected non-nil state for flaky")
	}
	if actState.RetryAt == nil {
		t.Fatal("expected RetryAt to be set")
	}
	if actState.RetryAttempt != 1 {
		t.Errorf("RetryAttempt = %d, want 1", actState.RetryAttempt)
	}
	if actState.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", actState.ConsecutiveFailures)
	}
}

func TestScheduler_RetryFiresAfterDelay(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 5
	cfg.Standalone = []TaskConfig{
		{ID: "flaky", Schedule: "0 9 * * *", Catchup: true, Prompt: "flaky work"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("flaky", ExecutorResult{
		TaskID: "flaky",
		Status:     "failed",
		Error:      "transient error",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("flaky", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Dispatch and fail
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Advance clock past retry delay
	clock.Set(now.Add(6 * time.Second))

	// Clear old calls
	executor = NewFakeExecutor()
	executor.SetResult("flaky", ExecutorResult{
		TaskID: "flaky",
		Status:     "failed",
		Error:      "still failing",
	})
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor, clock)

	// Tick should pick up the retry
	sched.tick()

	// Verify retry was dispatched
	drainResults(sched.pool, 1, 100*time.Millisecond)
	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 retry dispatch, got %d", len(calls))
	}
	if calls[0].TaskID != "flaky" {
		t.Errorf("retried task = %q, want flaky", calls[0].TaskID)
	}

	// Verify retry_at was cleared
	actState := state.GetTask("flaky")
	if actState.RetryAt != nil {
		t.Error("RetryAt should be cleared after retry dispatch")
	}
}

func TestScheduler_RetryNotFiredBeforeDelay(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 60
	cfg.Standalone = []TaskConfig{
		{ID: "flaky", Schedule: "0 9 * * *", Catchup: true, Prompt: "flaky work"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("flaky", ExecutorResult{
		TaskID: "flaky",
		Status:     "failed",
		Error:      "transient error",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("flaky", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Dispatch and fail
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Advance clock only partially (30s of 60s delay)
	clock.Set(now.Add(30 * time.Second))

	// Replace executor to track new calls
	executor2 := NewFakeExecutor()
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)

	// Tick should NOT dispatch retry
	sched.tick()

	calls := executor2.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches before retry delay, got %d", len(calls))
	}
}

func TestScheduler_AllRetriesExhausted(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 1 // 2 total attempts
	cfg.RetryDelaySeconds = 1
	cfg.Standalone = []TaskConfig{
		{ID: "hopeless", Schedule: "0 9 * * *", Catchup: true, Prompt: "hopeless work"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("hopeless", ExecutorResult{
		TaskID: "hopeless",
		Status:     "failed",
		Error:      "permanent error",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("hopeless", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// First attempt
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	actState := state.GetTask("hopeless")
	if actState.RetryAttempt != 1 {
		t.Fatalf("after first failure, RetryAttempt = %d, want 1", actState.RetryAttempt)
	}

	// Advance clock and retry
	clock.Set(now.Add(2 * time.Second))
	sched.tick()

	// Get retry result
	result2 := <-sched.pool.Results()
	sched.handleCompletion(result2)

	// Should be definitively failed now
	actState = state.GetTask("hopeless")
	if actState.RetryAt != nil {
		t.Error("RetryAt should be nil after all retries exhausted")
	}
	if actState.RetryAttempt != 0 {
		t.Errorf("RetryAttempt = %d, want 0 (cleared after exhaustion)", actState.RetryAttempt)
	}
	if actState.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", actState.ConsecutiveFailures)
	}
}

func TestScheduler_SuccessfulRetryClearsFailureState(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 1
	cfg.Standalone = []TaskConfig{
		{ID: "recovers", Schedule: "0 9 * * *", Catchup: true, Prompt: "recovering work"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}

	// First call fails, second succeeds
	executor := NewFakeExecutor()
	callCount := 0
	executor.SetResult("recovers", ExecutorResult{
		TaskID: "recovers",
		Status:     "failed",
		Error:      "transient",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("recovers", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// First attempt - fails
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)
	callCount++

	// Now make it succeed on retry
	executor2 := NewFakeExecutor()
	executor2.SetResult("recovers", ExecutorResult{
		TaskID: "recovers",
		Status:     "success",
		Output:     "recovered!",
	})
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)

	// Advance clock past retry delay
	clock.Set(now.Add(2 * time.Second))

	// Tick to trigger retry
	sched.tick()
	result2 := <-sched.pool.Results()
	sched.handleCompletion(result2)

	// Verify failure state is cleared
	actState := state.GetTask("recovers")
	if actState.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0 after successful retry", actState.ConsecutiveFailures)
	}
	if actState.RetryAt != nil {
		t.Error("RetryAt should be nil after successful retry")
	}
	if actState.RetryAttempt != 0 {
		t.Errorf("RetryAttempt = %d, want 0 after successful retry", actState.RetryAttempt)
	}
	if actState.LastRunStatus != "success" {
		t.Errorf("LastRunStatus = %q, want success", actState.LastRunStatus)
	}
}

// ============================================================================
// Force Run Tests
// ============================================================================

func TestScheduler_ForceRunTask(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "task1", Schedule: "on-demand", Prompt: "do task 1"},
	}

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	err := sched.ForceRunTask("task1")
	if err != nil {
		t.Fatalf("ForceRunTask: %v", err)
	}

	drainResults(sched.pool, 1, 100*time.Millisecond)

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(calls))
	}
	if calls[0].TaskID != "task1" {
		t.Errorf("dispatched = %q, want task1", calls[0].TaskID)
	}
}

func TestScheduler_ForceRunTask_NotFound(t *testing.T) {
	cfg := newTestConfig("UTC")
	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	err := sched.ForceRunTask("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want to contain 'not found'", err.Error())
	}
}

func TestScheduler_ForceRunGeneratesDistinctCycleID(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "root", Prompt: "root work"},
			},
		},
	}

	now := time.Date(2026, 3, 9, 10, 32, 15, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	sched.ForceRunTask("root")

	result := <-sched.pool.Results()
	if !strings.Contains(result.Result.CycleID, "force:") {
		t.Errorf("force cycle ID = %q, want to contain 'force:'", result.Result.CycleID)
	}
	if !strings.Contains(result.Result.CycleID, "pipeline:") {
		t.Errorf("force cycle ID = %q, want to contain 'pipeline:'", result.Result.CycleID)
	}
}

// ============================================================================
// Cycle ID Generation Tests
// ============================================================================

func TestScheduler_GenerateCycleID(t *testing.T) {
	clock := &FakeClock{T: time.Now()}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)

	fireTime := time.Date(2026, 3, 7, 8, 0, 0, 0, time.UTC)
	cycleID := sched.generateCycleID("daily-pipeline", fireTime)
	expected := "daily-pipeline:2026-03-07T08:00:00Z"
	if cycleID != expected {
		t.Errorf("generateCycleID = %q, want %q", cycleID, expected)
	}
}

func TestScheduler_GenerateForceCycleID(t *testing.T) {
	clock := &FakeClock{T: time.Now()}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)

	forceTime := time.Date(2026, 3, 7, 10, 32, 15, 0, time.UTC)
	cycleID := sched.generateForceCycleID("daily-pipeline", forceTime)
	expected := "daily-pipeline:force:2026-03-07T10:32:15Z"
	if cycleID != expected {
		t.Errorf("generateForceCycleID = %q, want %q", cycleID, expected)
	}
}

// ============================================================================
// Workflow State Propagation Tests
// ============================================================================

// TestScheduler_LeafTaskUpdatesWorkflowState verifies that when a leaf task
// (one with no dependents) succeeds, the workflow ID's LastSuccess is updated.
// This is the fix for the bug where new workflows never fired on schedule
// because their ID had no state.
func TestScheduler_LeafTaskUpdatesWorkflowState(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "step1", Prompt: "step 1"},
				{ID: "step2", DependsOn: "step1", Prompt: "step 2"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 5, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed workflow state so it fires
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Run step1
	sched.tick()
	result1 := <-sched.pool.Results()
	sched.handleCompletion(result1)

	// step1 is NOT a leaf (step2 depends on it) — workflow state should NOT be updated yet
	wfState := state.GetTask("pipeline")
	if wfState.LastSuccess != nil && !wfState.LastSuccess.Equal(oldSuccess) {
		t.Errorf("workflow LastSuccess updated after non-leaf step1, expected it to remain at oldSuccess")
	}

	// Run step2 (leaf)
	result2 := <-sched.pool.Results()
	sched.handleCompletion(result2)

	// step2 IS a leaf — workflow state should now be updated
	wfState = state.GetTask("pipeline")
	if wfState == nil || wfState.LastSuccess == nil {
		t.Fatal("expected workflow LastSuccess to be set after leaf task success")
	}
	if !wfState.LastSuccess.After(oldSuccess) {
		t.Errorf("workflow LastSuccess = %v, want after %v", wfState.LastSuccess, oldSuccess)
	}
	if wfState.LastRunStatus != "success" {
		t.Errorf("workflow LastRunStatus = %q, want success", wfState.LastRunStatus)
	}
}

// TestScheduler_NonLeafTaskDoesNotUpdateWorkflowState verifies that completing
// a non-leaf task does not prematurely update the workflow's LastSuccess.
func TestScheduler_NonLeafTaskDoesNotUpdateWorkflowState(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "root", Prompt: "root"},
				{ID: "leaf", DependsOn: "root", Prompt: "leaf"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 5, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Complete root (non-leaf)
	cycleID := "pipeline:2026-03-09T09:00:00Z"
	rootState := time.Date(2026, 3, 9, 9, 2, 0, 0, time.UTC)
	state.UpdateTask("root", func(as *TaskState) {
		as.LastSuccess = &rootState
		as.LastRunStatus = "success"
		as.LastCycleID = cycleID
	})
	sched.updateWorkflowStateOnLeafSuccess("root", clock.Now(), cfg)

	// Workflow state should still be old — root is not a leaf
	wfState := state.GetTask("pipeline")
	if wfState.LastSuccess == nil || !wfState.LastSuccess.Equal(oldSuccess) {
		t.Errorf("workflow LastSuccess changed after non-leaf completion: got %v, want %v", wfState.LastSuccess, oldSuccess)
	}
}

// TestScheduler_WorkflowFiresOnNextWindowAfterLeafSuccess verifies the end-to-end
// fix: after a workflow completes, it fires again on the next cron window without
// manual seeding of the workflow ID's state.
func TestScheduler_WorkflowFiresOnNextWindowAfterLeafSuccess(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "daily-pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "solo-step", Prompt: "do work"},
			},
		},
	}

	// Day 1: 9:01 AM. Seed workflow state (simulating a previous run).
	day1 := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: day1}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed: workflow last succeeded yesterday
	yesterday := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("daily-pipeline", func(as *TaskState) {
		as.LastSuccess = &yesterday
	})

	// Day 1: tick dispatches solo-step
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// solo-step is a leaf — workflow LastSuccess should now be updated to ~day1
	wfState := state.GetTask("daily-pipeline")
	if wfState == nil || wfState.LastSuccess == nil || !wfState.LastSuccess.After(yesterday) {
		t.Fatal("workflow LastSuccess not updated after leaf task success")
	}

	// Day 2: advance clock to next day's cron window
	day2 := time.Date(2026, 3, 10, 9, 1, 0, 0, time.UTC)
	clock.Set(day2)
	executor2 := NewFakeExecutor()
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)

	// Day 2: tick should dispatch again without any manual state seeding
	sched.tick()
	drainResults(sched.pool, 1, 200*time.Millisecond)

	calls := executor2.Calls()
	if len(calls) != 1 {
		t.Errorf("expected 1 dispatch on day 2 (workflow scheduled from leaf state), got %d", len(calls))
	}
	if len(calls) > 0 && calls[0].TaskID != "solo-step" {
		t.Errorf("dispatched task = %q, want solo-step", calls[0].TaskID)
	}
}

// ============================================================================
// Integration-style Tests (still using fakes)
// ============================================================================

func TestScheduler_FullWorkflowExecution(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "two-step",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "gather", Prompt: "gather data"},
				{ID: "report", DependsOn: "gather", Prompt: "generate report"},
			},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers the workflow due
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("two-step", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Tick: should dispatch "gather" (root task)
	sched.tick()

	// Get gather result
	result1 := <-sched.pool.Results()
	if result1.Job.Task.ID != "gather" {
		t.Fatalf("expected gather, got %q", result1.Job.Task.ID)
	}
	if result1.Result.Status != "success" {
		t.Fatalf("gather status = %q, want success", result1.Result.Status)
	}

	// Handle completion - should trigger "report" dispatch
	sched.handleCompletion(result1)

	// Get report result
	result2 := <-sched.pool.Results()
	if result2.Job.Task.ID != "report" {
		t.Fatalf("expected report, got %q", result2.Job.Task.ID)
	}
	if result2.Result.Status != "success" {
		t.Fatalf("report status = %q, want success", result2.Result.Status)
	}

	sched.handleCompletion(result2)

	// Verify both tasks have correct state
	gatherState := state.GetTask("gather")
	if gatherState.LastRunStatus != "success" {
		t.Errorf("gather state = %q, want success", gatherState.LastRunStatus)
	}
	reportState := state.GetTask("report")
	if reportState.LastRunStatus != "success" {
		t.Errorf("report state = %q, want success", reportState.LastRunStatus)
	}

	// Cycle IDs should match
	if gatherState.LastCycleID != reportState.LastCycleID {
		t.Errorf("cycle IDs don't match: gather=%q, report=%q", gatherState.LastCycleID, reportState.LastCycleID)
	}
}

func TestScheduler_FullStandaloneExecution(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "solo-task", Schedule: "*/15 * * * *", Catchup: true, Prompt: "solo work"},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old state so isDue considers this task due
	oldSuccess := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("solo-task", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	sched.tick()

	result := <-sched.pool.Results()
	if result.Job.Task.ID != "solo-task" {
		t.Fatalf("expected solo-task, got %q", result.Job.Task.ID)
	}
	sched.handleCompletion(result)

	actState := state.GetTask("solo-task")
	if actState == nil {
		t.Fatal("expected non-nil state for solo-task")
	}
	if actState.LastRunStatus != "success" {
		t.Errorf("LastRunStatus = %q, want success", actState.LastRunStatus)
	}
}

// ============================================================================
// DST Transition Tests
// ============================================================================

func TestScheduler_DST_SpringForward_FiresOnce(t *testing.T) {
	// March 8, 2026: clocks jump from 2:00 AM PST to 3:00 AM PDT in America/Los_Angeles.
	// Schedule: "0 2 * * *" (daily at 2am local). 2:00 AM doesn't exist on this day.
	// The task should fire once, not be skipped.
	//
	// The cron library evaluates in the timezone of the input time. Since 2:00 AM
	// doesn't exist on March 8 in LA, the cron's last firing before 3:00 AM PDT
	// is 2:00 AM PST on March 7. By the time we reach March 9, 2:00 AM PDT exists
	// and the schedule fires normally. With catchup=true, the task fires on the
	// first tick after March 9 2:00 AM PDT if it hasn't already.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("failed to load location: %v", err)
	}

	cfg := newTestConfig("America/Los_Angeles")

	// Set clock to 3:00 AM PDT on March 9 — the first day after spring forward where
	// 2:00 AM exists again. The last firing is 2:00 AM PDT on March 9.
	now := time.Date(2026, 3, 9, 3, 0, 0, 0, loc)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)

	// Last success was March 7 2:00 AM PST — the last day before the spring-forward gap.
	// March 8 had no 2:00 AM, so the schedule was skipped that day.
	// With catchup=true, the task should fire for the March 9 window.
	lastSuccess := time.Date(2026, 3, 7, 2, 0, 0, 0, loc)
	state.UpdateTask("dst-spring", func(as *TaskState) {
		as.LastSuccess = timePtr(lastSuccess.UTC())
	})

	// isDue should return true — catchup detects the missed window and fires
	due := sched.isDue("0 2 * * *", "dst-spring", true, now, loc)
	if !due {
		t.Errorf("expected isDue=true after spring forward gap, got false")
	}
}

func TestScheduler_DST_SpringForward_NoDuplicate(t *testing.T) {
	// Same scenario as above, but with catchup=true. After the task fires at 3:00 AM,
	// the catchup logic should NOT trigger a duplicate run. The single 3:00 run satisfies the 2:00 window.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("failed to load location: %v", err)
	}

	cfg := newTestConfig("America/Los_Angeles")
	cfg.Standalone = []TaskConfig{
		{ID: "dst-spring-nodup", Schedule: "0 2 * * *", Catchup: true, Prompt: "spring forward no dup"},
	}

	// At 3:00 AM PDT, the task already ran (simulating it fired at 3:00 AM)
	now := time.Date(2026, 3, 8, 3, 1, 0, 0, loc)
	ranAt := time.Date(2026, 3, 8, 3, 0, 0, 0, loc)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Set last success to 3:00 AM PDT today (the run that satisfied the 2:00 window)
	state.UpdateTask("dst-spring-nodup", func(as *TaskState) {
		as.LastSuccess = timePtr(ranAt.UTC())
	})

	// isDue should return false — the 3:00 AM run satisfies the 2:00 AM window
	due := sched.isDue("0 2 * * *", "dst-spring-nodup", true, now, loc)
	if due {
		t.Error("expected not due after spring forward run at 3:00 AM, but isDue returned true (duplicate)")
	}
}

func TestScheduler_DST_FallBack_FiresFirstOnly(t *testing.T) {
	// November 1, 2026: clocks fall back from 2:00 AM PDT to 1:00 AM PST in America/Los_Angeles.
	// Schedule: "0 1 * * *" (daily at 1am local). 1:00 AM occurs twice.
	// The task should fire on the first occurrence only.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("failed to load location: %v", err)
	}

	cfg := newTestConfig("America/Los_Angeles")
	cfg.Standalone = []TaskConfig{
		{ID: "dst-fall", Schedule: "0 1 * * *", Catchup: true, Prompt: "fall back task"},
	}

	// First 1:00 AM PDT (UTC-7) = 8:00 AM UTC
	firstOneAM := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: firstOneAM}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Last success was yesterday
	yesterday := time.Date(2026, 10, 31, 1, 0, 0, 0, loc)
	state.UpdateTask("dst-fall", func(as *TaskState) {
		as.LastSuccess = timePtr(yesterday.UTC())
	})

	// First 1:00 AM should be due
	due := sched.isDue("0 1 * * *", "dst-fall", true, firstOneAM, loc)
	if !due {
		t.Error("expected due during first 1:00 AM occurrence, but isDue returned false")
	}

	// Simulate successful run at first 1:00 AM
	state.UpdateTask("dst-fall", func(as *TaskState) {
		as.LastSuccess = timePtr(firstOneAM)
		as.LastRunStatus = "success"
	})

	// Second 1:00 AM PST (UTC-8) = 9:00 AM UTC
	secondOneAM := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	// Should NOT be due during second 1:00 AM
	due2 := sched.isDue("0 1 * * *", "dst-fall", true, secondOneAM, loc)
	if due2 {
		t.Error("expected not due during second 1:00 AM occurrence, but isDue returned true")
	}
}

func TestScheduler_DST_FallBack_CatchupNotDueDuringSecond(t *testing.T) {
	// Same fall-back scenario with catchup=true. After the task runs during the first 1:00 AM,
	// it should NOT be due during the second 1:00 AM occurrence.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("failed to load location: %v", err)
	}

	cfg := newTestConfig("America/Los_Angeles")

	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// First 1:00 AM PDT (UTC-7) = 8:00 AM UTC on Nov 1
	firstOneAM := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

	// Set last success to the first 1:00 AM
	state.UpdateTask("dst-fall-catchup", func(as *TaskState) {
		as.LastSuccess = timePtr(firstOneAM)
		as.LastRunStatus = "success"
	})

	// Second 1:00 AM PST (UTC-8) = 9:00 AM UTC
	secondOneAM := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	// With catchup=true, should NOT be due during the second 1:00 AM
	due := sched.isDue("0 1 * * *", "dst-fall-catchup", true, secondOneAM, loc)
	if due {
		t.Error("expected not due during second 1:00 AM with catchup=true, but isDue returned true")
	}
}

// ============================================================================
// Polling Task Tests
// ============================================================================

func TestScheduler_Polling_CatchupFalse_MissedMultipleWindows(t *testing.T) {
	// Schedule: */15 * * * *, catchup=false
	// Last success: 8:45 AM
	// Current time: 10:05 AM (missed 9:00, 9:15, 9:30, 9:45, 10:00 — 5 windows)
	// Expected: fires once on the next tick that matches the cron (10:15), does NOT catch up
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("failed to load location: %v", err)
	}

	cfg := newTestConfig("America/New_York")
	cfg.Standalone = []TaskConfig{
		{ID: "poll-task", Schedule: "*/15 * * * *", Catchup: false, Prompt: "polling work"},
	}

	// At 10:05 AM with last success at 8:45 AM, catchup=false: should NOT be due
	// (10:05 is not on a cron boundary, and catchup is off)
	now := time.Date(2026, 3, 9, 10, 5, 0, 0, loc)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	lastSuccess := time.Date(2026, 3, 9, 8, 45, 0, 0, loc)
	state.UpdateTask("poll-task", func(as *TaskState) {
		as.LastSuccess = timePtr(lastSuccess.UTC())
	})

	// At 10:05, not on a cron boundary, catchup=false: not due
	due := sched.isDue("*/15 * * * *", "poll-task", false, now, loc)
	if due {
		t.Error("expected not due at 10:05 with catchup=false, but isDue returned true")
	}

	// At 10:15 (next cron boundary), should be due
	next := time.Date(2026, 3, 9, 10, 15, 0, 0, loc)
	due2 := sched.isDue("*/15 * * * *", "poll-task", false, next, loc)
	if !due2 {
		t.Error("expected due at 10:15 (next cron match) with catchup=false, but isDue returned false")
	}

	// Verify it only fires once via tick (not 5 catchup runs)
	clock.Set(next)
	sched.tick()
	drainResults(sched.pool, 5, 100*time.Millisecond)

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Errorf("expected exactly 1 dispatch at 10:15 (no catchup), got %d", len(calls))
	}
}

// ============================================================================
// Config Reload Tests
// ============================================================================

func TestScheduler_ReloadConfig(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "old-task", Schedule: "0 9 * * *"},
	}

	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	newCfg := newTestConfig("UTC")
	newCfg.Standalone = []TaskConfig{
		{ID: "new-task", Schedule: "0 10 * * *"},
	}

	sched.ReloadConfig(newCfg)

	got := sched.GetConfig()
	if len(got.Standalone) != 1 {
		t.Fatalf("expected 1 standalone after reload, got %d", len(got.Standalone))
	}
	if got.Standalone[0].ID != "new-task" {
		t.Errorf("standalone ID = %q, want new-task", got.Standalone[0].ID)
	}
}

// ============================================================================
// Status Helper Tests
// ============================================================================

func TestScheduler_TickCount(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "task1", Schedule: "on-demand"},
	}

	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	if sched.GetTickCount() != 0 {
		t.Errorf("initial tick count = %d, want 0", sched.GetTickCount())
	}

	sched.tick()
	if sched.GetTickCount() != 1 {
		t.Errorf("after 1 tick = %d, want 1", sched.GetTickCount())
	}

	sched.tick()
	sched.tick()
	if sched.GetTickCount() != 3 {
		t.Errorf("after 3 ticks = %d, want 3", sched.GetTickCount())
	}
}

// ============================================================================
// LastScheduledFiring Tests
// ============================================================================

func TestScheduler_LastScheduledFiring(t *testing.T) {
	clock := &FakeClock{T: time.Now()}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)

	// Daily 9am: at 10:30am, last firing was 9am today
	now := time.Date(2026, 3, 9, 10, 30, 0, 0, time.UTC)
	lastFiring := sched.lastScheduledFiring("0 9 * * *", now, time.UTC)
	expected := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	if !lastFiring.Equal(expected) {
		t.Errorf("last firing = %v, want %v", lastFiring, expected)
	}

	// Every 15 min: at 10:05, last firing was 10:00
	now2 := time.Date(2026, 3, 9, 10, 5, 0, 0, time.UTC)
	lastFiring2 := sched.lastScheduledFiring("*/15 * * * *", now2, time.UTC)
	expected2 := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	if !lastFiring2.Equal(expected2) {
		t.Errorf("last firing = %v, want %v", lastFiring2, expected2)
	}

	// Weekly Monday 9am: on Tuesday, last firing was Monday 9am
	tuesday := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	lastFiring3 := sched.lastScheduledFiring("0 9 * * 1", tuesday, time.UTC)
	expectedMonday := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	if !lastFiring3.Equal(expectedMonday) {
		t.Errorf("last firing = %v, want %v", lastFiring3, expectedMonday)
	}
}

// ============================================================================
// Helpers
// ============================================================================

// drainResults reads up to n results from the pool, with a timeout.
func drainResults(pool *WorkerPool, n int, timeout time.Duration) []JobResult {
	var results []JobResult
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for i := 0; i < n; i++ {
		select {
		case r := <-pool.Results():
			results = append(results, r)
		case <-timer.C:
			return results
		}
	}
	return results
}

// ============================================================================
// lastScheduledFiring — edge cases
// ============================================================================

func TestScheduler_LastScheduledFiring_InvalidCron(t *testing.T) {
	clock := &FakeClock{T: time.Now()}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)

	result := sched.lastScheduledFiring("not a cron expression", time.Now(), time.UTC)
	if !result.IsZero() {
		t.Errorf("expected zero time for invalid cron, got %v", result)
	}
}

// ============================================================================
// checkRetries — network unavailable skips network-required retry
// ============================================================================

func TestScheduler_CheckRetries_NetworkSkip(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 5
	cfg.Standalone = []TaskConfig{
		// RequiresNetwork is nil, so IsNetworkRequired() returns true (default)
		{ID: "net-task", Schedule: "0 9 * * *", Prompt: "needs internet"},
	}

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: false} // network is DOWN
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Set up retry state: due now, attempt 1 pending
	retryAt := now.Add(-1 * time.Second) // retry time is in the past
	state.UpdateTask("net-task", func(as *TaskState) {
		as.RetryAt = &retryAt
		as.RetryAttempt = 1
		as.LastCycleID = "net-task:2026-03-09T09:00:00Z"
	})

	sched.checkRetries(cfg, now, false) // network unavailable

	// No task should have been submitted
	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 calls when network unavailable, got %d", len(calls))
	}

	// RetryAt should NOT have been cleared (task was skipped, not dispatched)
	st := state.GetTask("net-task")
	if st.RetryAt == nil {
		t.Error("RetryAt should still be set (task was skipped, not dispatched)")
	}
}

func TestScheduler_CheckRetries_NoNetworkRequired_FiresWhenDown(t *testing.T) {
	falseVal := false
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 5
	cfg.Standalone = []TaskConfig{
		{ID: "offline-task", Schedule: "0 9 * * *", Prompt: "works offline", RequiresNetwork: &falseVal},
	}

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: false} // network is DOWN
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	retryAt := now.Add(-1 * time.Second)
	state.UpdateTask("offline-task", func(as *TaskState) {
		as.RetryAt = &retryAt
		as.RetryAttempt = 1
		as.LastCycleID = "offline-task:2026-03-09T09:00:00Z"
	})

	sched.checkRetries(cfg, now, false)

	// Task with requires_network=false should still fire even when offline
	result := <-sched.pool.Results()
	if result.Job.Task.ID != "offline-task" {
		t.Errorf("expected offline-task to fire, got %q", result.Job.Task.ID)
	}
}

// ============================================================================
// dispatchReadyDependents — edge cases
// ============================================================================

func TestScheduler_DispatchReadyDependents_ParentStatusNotSuccess(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Tasks: []TaskConfig{
				{ID: "parent", Prompt: "parent"},
				{ID: "child", DependsOn: "parent", Prompt: "child"},
			},
		},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	cycleID := "pipeline:2026-03-09T09:00:00Z"

	// Parent ran but failed — CycleID matches, but status is "failed"
	state.UpdateTask("parent", func(as *TaskState) {
		as.LastCycleID = cycleID
		as.LastRunStatus = "failed"
		as.ConsecutiveFailures = 1
	})

	sched.dispatchReadyDependents("parent", cycleID, cfg)

	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches when parent status is 'failed', got %d", len(calls))
	}
}

func TestScheduler_DispatchReadyDependents_NetworkSkip(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Tasks: []TaskConfig{
				{ID: "parent", Prompt: "parent"},
				// Child has RequiresNetwork=nil (defaults to true)
				{ID: "net-child", DependsOn: "parent", Prompt: "needs network"},
			},
		},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: false} // network is DOWN
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	cycleID := "pipeline:2026-03-09T09:00:00Z"

	// Parent succeeded with matching cycle
	state.UpdateTask("parent", func(as *TaskState) {
		as.LastCycleID = cycleID
		as.LastRunStatus = "success"
	})

	sched.dispatchReadyDependents("parent", cycleID, cfg)

	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches when child requires network and network is down, got %d", len(calls))
	}
}

// CycleID is a test-only helper method on TaskState.
func (as *TaskState) CycleID() string {
	return as.LastCycleID
}

// ============================================================================
// Pause Tests
// ============================================================================

func TestScheduler_Pause_DefaultFalse(t *testing.T) {
	cfg := newTestConfig("UTC")
	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)

	if sched.IsPaused() {
		t.Error("scheduler should not be paused by default")
	}
}

func TestScheduler_Pause_SetAndClear(t *testing.T) {
	cfg := newTestConfig("UTC")
	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)

	sched.SetPaused(true)
	if !sched.IsPaused() {
		t.Error("expected paused=true after SetPaused(true)")
	}

	sched.SetPaused(false)
	if sched.IsPaused() {
		t.Error("expected paused=false after SetPaused(false)")
	}
}

func TestScheduler_Pause_SkipsDispatch(t *testing.T) {
	// A due task should not be dispatched when the scheduler is paused.
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "due-task", Schedule: "*/15 * * * *", Catchup: true, Prompt: "do work"},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed old success so isDue returns true
	oldSuccess := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("due-task", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	sched.SetPaused(true)
	sched.tick()

	calls := executor.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches when paused, got %d", len(calls))
	}
}

func TestScheduler_Pause_TickCountStillIncrements(t *testing.T) {
	// tick count should increment even when paused — the server is still running.
	cfg := newTestConfig("UTC")
	clock := &FakeClock{T: time.Now()}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	sched.SetPaused(true)
	sched.tick()
	sched.tick()
	sched.tick()

	if sched.GetTickCount() != 3 {
		t.Errorf("tick count = %d, want 3 (should increment even when paused)", sched.GetTickCount())
	}
}

func TestScheduler_Pause_UnpauseAllowsDispatch(t *testing.T) {
	// After unpausing, a due task should be dispatched on the next tick.
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "waiting-task", Schedule: "*/15 * * * *", Catchup: true, Prompt: "do work"},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	oldSuccess := time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("waiting-task", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Tick while paused — no dispatch
	sched.SetPaused(true)
	sched.tick()
	if len(executor.Calls()) != 0 {
		t.Fatal("expected no dispatch while paused")
	}

	// Unpause and tick — task should now dispatch
	sched.SetPaused(false)
	sched.tick()

	result := <-sched.pool.Results()
	if result.Job.Task.ID != "waiting-task" {
		t.Errorf("expected waiting-task to dispatch after unpause, got %q", result.Job.Task.ID)
	}
}

// ============================================================================
// Fatal Error & Re-fire Guard Tests
// ============================================================================

// TestScheduler_FatalError_NoRetry verifies that a "fatal" result (e.g., binary
// not found) is never retried, even when max_retries > 0.
func TestScheduler_FatalError_NoRetry(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 1
	cfg.Standalone = []TaskConfig{
		{ID: "broken", Schedule: "0 9 * * *", Catchup: true, Prompt: "do stuff"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("broken", ExecutorResult{
		TaskID: "broken",
		Status: "fatal",
		Error:  "failed to start: exec: \"claude\": executable file not found in $PATH",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed state so isDue fires
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("broken", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Dispatch
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Should NOT have scheduled a retry
	actState := state.GetTask("broken")
	if actState.RetryAt != nil {
		t.Error("fatal error should not schedule a retry, but RetryAt is set")
	}
	if actState.RetryAttempt != 0 {
		t.Errorf("RetryAttempt = %d, want 0 for fatal error", actState.RetryAttempt)
	}
	if actState.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", actState.ConsecutiveFailures)
	}
}

// TestScheduler_FailedTask_NoRefire verifies that after retries exhaust for a
// standalone task, isDue does not re-dispatch it for the same cron window.
func TestScheduler_FailedTask_NoRefire(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 0 // no retries — fail once and done
	cfg.Standalone = []TaskConfig{
		{ID: "once", Schedule: "0 9 * * *", Catchup: true, Prompt: "try once"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("once", ExecutorResult{
		TaskID: "once",
		Status: "failed",
		Error:  "some error",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed state so isDue fires
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("once", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// First tick: dispatches, fails
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Verify LastAttemptedFiring was set
	actState := state.GetTask("once")
	if actState.LastAttemptedFiring == nil {
		t.Fatal("expected LastAttemptedFiring to be set after dispatch")
	}

	// Second tick (same cron window): should NOT re-dispatch
	clock.Set(now.Add(1 * time.Minute))
	executor2 := NewFakeExecutor()
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)
	sched.tick()

	calls := executor2.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches after retry exhaustion in same window, got %d", len(calls))
	}
}

// TestScheduler_FailedTask_FiresNextWindow verifies that a task which failed
// in one cron window will fire again in the next window.
func TestScheduler_FailedTask_FiresNextWindow(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 0
	cfg.Standalone = []TaskConfig{
		{ID: "daily", Schedule: "0 9 * * *", Catchup: true, Prompt: "daily work"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("daily", ExecutorResult{
		TaskID: "daily",
		Status: "failed",
		Error:  "transient error",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed state so isDue fires
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("daily", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Day 1: dispatches, fails
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Advance to the next day's cron window
	nextDay := time.Date(2026, 3, 10, 9, 1, 0, 0, time.UTC)
	clock.Set(nextDay)

	executor2 := NewFakeExecutor()
	executor2.SetResult("daily", ExecutorResult{
		TaskID: "daily",
		Status: "success",
	})
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)

	// Day 2: should fire again for the new window
	sched.tick()

	// Wait for the pool to process the job
	results := drainResults(sched.pool, 1, 2*time.Second)
	if len(results) != 1 {
		t.Fatalf("expected 1 result in next cron window, got %d", len(results))
	}
	if results[0].Job.Task.ID != "daily" {
		t.Errorf("expected daily task, got %q", results[0].Job.Task.ID)
	}
}

// TestScheduler_WorkflowFailedTask_NoRefire verifies that a workflow whose root
// task fails does not re-fire for the same cron window.
func TestScheduler_WorkflowFailedTask_NoRefire(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 0
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Catchup:  true,
			Tasks: []TaskConfig{
				{ID: "step-one", Prompt: "first step"},
			},
		},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("step-one", ExecutorResult{
		TaskID: "step-one",
		Status: "failed",
		Error:  "boom",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	// Seed state for the WORKFLOW ID (that's what isDue checks)
	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("pipeline", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// First tick: dispatches step-one, fails
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// Verify LastAttemptedFiring was set on workflow ID
	wfState := state.GetTask("pipeline")
	if wfState.LastAttemptedFiring == nil {
		t.Fatal("expected LastAttemptedFiring on workflow state")
	}

	// Next tick: should NOT re-dispatch
	clock.Set(now.Add(1 * time.Minute))
	executor2 := NewFakeExecutor()
	sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)
	sched.tick()

	calls := executor2.Calls()
	if len(calls) != 0 {
		t.Errorf("expected 0 dispatches after workflow failure in same window, got %d", len(calls))
	}
}

// TestScheduler_FatalError_CombinedNoRefire verifies the full scenario:
// a fatal error is not retried AND the task is not re-fired by cron.
func TestScheduler_FatalError_CombinedNoRefire(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2
	cfg.RetryDelaySeconds = 1
	cfg.Standalone = []TaskConfig{
		{ID: "missing-binary", Schedule: "0 9 * * *", Catchup: true, Prompt: "run claude"},
	}

	now := time.Date(2026, 3, 9, 9, 1, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	executor.SetResult("missing-binary", ExecutorResult{
		TaskID: "missing-binary",
		Status: "fatal",
		Error:  "failed to start: exec: \"claude\": executable file not found in $PATH",
	})
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	oldSuccess := time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("missing-binary", func(as *TaskState) {
		as.LastSuccess = &oldSuccess
	})

	// Tick 1: dispatches, gets fatal error
	sched.tick()
	result := <-sched.pool.Results()
	sched.handleCompletion(result)

	// No retry scheduled
	actState := state.GetTask("missing-binary")
	if actState.RetryAt != nil {
		t.Error("fatal error should not schedule retry")
	}

	// Tick 2-5: hammer it — should never re-dispatch
	for i := 0; i < 4; i++ {
		clock.Set(now.Add(time.Duration(i+2) * time.Minute))
		executor2 := NewFakeExecutor()
		sched.pool = NewWorkerPool(cfg.WorkerSlots, executor2, clock)
		sched.tick()

		calls := executor2.Calls()
		if len(calls) != 0 {
			t.Fatalf("tick %d: expected 0 dispatches, got %d", i+2, len(calls))
		}
	}
}

// TestScheduler_DispatchTask_MaxRetriesCap verifies that dispatchTask clamps
// MaxAttempts to maxRetriesLimit+1 even when the task config specifies more.
// This is defense-in-depth: config validation rejects values > maxRetriesLimit,
// but dispatchTask enforces the ceiling at runtime as well.
func TestScheduler_DispatchTask_MaxRetriesCap(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.MaxRetries = 2 // global default

	// Construct a task with a per-task MaxRetries above the hard limit directly,
	// bypassing config validation (which would normally reject this).
	aboveLimit := maxRetriesLimit + 5
	task := TaskConfig{
		ID:         "greedy-task",
		Schedule:   "on-demand",
		Prompt:     "do work",
		MaxRetries: &aboveLimit,
	}
	cfg.Standalone = []TaskConfig{task}

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, _ := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg

	sched.dispatchTask(task, task.Prompt, "", cfg)
	drainResults(sched.pool, 1, 100*time.Millisecond)

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(calls))
	}

	// Verify the job that was submitted had MaxAttempts capped.
	// The job's MaxAttempts = min(aboveLimit, maxRetriesLimit) + 1.
	// We can't inspect the Job struct directly from the executor, but we can
	// verify behavior by running the full retry cycle via handleCompletion.
	//
	// Reset and re-dispatch, then drive failures through handleCompletion to
	// confirm the scheduler gives up after maxRetriesLimit+1 total attempts.
	executor2 := NewFakeExecutor()
	executor2.SetResult("greedy-task", ExecutorResult{
		TaskID: "greedy-task",
		Status: "failed",
		Error:  "always fails",
	})
	pool2 := NewWorkerPool(cfg.WorkerSlots, executor2, clock)
	sched.pool = pool2

	sched.dispatchTask(task, task.Prompt, "", cfg)

	totalAttempts := 0
	for {
		select {
		case result := <-pool2.Results():
			totalAttempts++
			sched.handleCompletion(result)

			// Check if scheduler scheduled a retry
			taskState := sched.state.GetTask("greedy-task")
			if taskState == nil || taskState.RetryAt == nil {
				// No more retries scheduled — done
				goto done
			}
			// Advance clock past retry delay and re-dispatch
			clock.Set(clock.Now().Add(2 * time.Second))
			pool2 = NewWorkerPool(cfg.WorkerSlots, executor2, clock)
			sched.pool = pool2
			sched.checkRetries(cfg, clock.Now(), true)

		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for retry cycle to complete")
		}
	}
done:
	wantMaxAttempts := maxRetriesLimit + 1
	if totalAttempts > wantMaxAttempts {
		t.Errorf("total attempts = %d, want <= %d (hard cap)", totalAttempts, wantMaxAttempts)
	}
}
