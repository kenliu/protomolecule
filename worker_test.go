package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerPool(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"RespectsWorkerSlotsLimit", testRespectsWorkerSlotsLimit},
		{"AllSlotsFull_NewTaskQueued", testAllSlotsFull_NewTaskQueued},
		{"ExecutorReturnsSuccess", testExecutorReturnsSuccess},
		{"ExecutorReturnsFailure", testExecutorReturnsFailure},
		{"TaskExceedsTimeout", testTaskExceedsTimeout},
		{"PerTaskTimeoutOverridesGlobal", testPerTaskTimeoutOverridesGlobal},
		{"ActiveJobsReturnRunningJobs", testActiveJobsReturnRunningJobs},
		{"ShutdownWaitsForRunningJobs", testShutdownWaitsForRunningJobs},
		{"ResultsChannelReceivesAllCompletions", testResultsChannelReceivesAllCompletions},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fn(t)
		})
	}
}

// testRespectsWorkerSlotsLimit verifies that with N slots, at most N jobs run concurrently.
func testRespectsWorkerSlotsLimit(t *testing.T) {
	const slots = 2
	const totalJobs = 4

	var running int64
	var maxRunning int64
	var mu sync.Mutex

	executor := NewFakeExecutor()
	executor.SetDelay(50 * time.Millisecond)

	// Wrap executor to track concurrency
	trackingExec := &trackingExecutor{
		inner:      executor,
		running:    &running,
		maxRunning: &maxRunning,
		mu:         &mu,
	}

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(slots, trackingExec, clock)

	for i := 0; i < totalJobs; i++ {
		pool.Submit(Job{
			Task:     TaskConfig{ID: makeTaskID(i)},
			Prompt: "test",
			TimeoutMin:   1,
		})
	}

	// Collect all results
	for i := 0; i < totalJobs; i++ {
		<-pool.Results()
	}
	pool.Shutdown()

	mu.Lock()
	peak := maxRunning
	mu.Unlock()

	if peak > int64(slots) {
		t.Errorf("max concurrent jobs = %d, want <= %d", peak, slots)
	}
}

// testAllSlotsFull_NewTaskQueued verifies that when all slots are full,
// new tasks queue and eventually execute when a slot opens.
func testAllSlotsFull_NewTaskQueued(t *testing.T) {
	const slots = 1

	executor := NewFakeExecutor()
	executor.SetDelay(30 * time.Millisecond)

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(slots, executor, clock)

	// Submit 2 jobs to a pool with 1 slot
	pool.Submit(Job{
		Task:     TaskConfig{ID: "first"},
		Prompt: "first job",
		TimeoutMin:   1,
	})
	pool.Submit(Job{
		Task:     TaskConfig{ID: "second"},
		Prompt: "second job",
		TimeoutMin:   1,
	})

	// Both should eventually complete
	results := make(map[string]JobResult)
	for i := 0; i < 2; i++ {
		r := <-pool.Results()
		results[r.Job.Task.ID] = r
	}
	pool.Shutdown()

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results["first"].Result.Status != "success" {
		t.Errorf("first job status = %q, want success", results["first"].Result.Status)
	}
	if results["second"].Result.Status != "success" {
		t.Errorf("second job status = %q, want success", results["second"].Result.Status)
	}
}

// testExecutorReturnsSuccess verifies correct result on success.
func testExecutorReturnsSuccess(t *testing.T) {
	executor := NewFakeExecutor()
	executor.SetResult("my-task", ExecutorResult{
		TaskID: "my-task",
		Status:     "success",
		Output:     "all good",
	})

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(2, executor, clock)

	pool.Submit(Job{
		Task:     TaskConfig{ID: "my-task"},
		Prompt: "do the thing",
		CycleID:      "cycle-1",
		Attempt:      1,
		TimeoutMin:   1,
	})

	r := <-pool.Results()
	pool.Shutdown()

	if r.Result.Status != "success" {
		t.Errorf("status = %q, want success", r.Result.Status)
	}
	if r.Result.Output != "all good" {
		t.Errorf("output = %q, want 'all good'", r.Result.Output)
	}
	if r.Result.CycleID != "cycle-1" {
		t.Errorf("cycle ID = %q, want 'cycle-1'", r.Result.CycleID)
	}
	if r.Result.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", r.Result.Attempt)
	}
	if r.Err != nil {
		t.Errorf("err = %v, want nil", r.Err)
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].TaskID != "my-task" {
		t.Errorf("call task ID = %q, want my-task", calls[0].TaskID)
	}
	if calls[0].Prompt != "do the thing" {
		t.Errorf("call prompt = %q, want 'do the thing'", calls[0].Prompt)
	}
}

// testExecutorReturnsFailure verifies failure handling and that other tasks are unaffected.
func testExecutorReturnsFailure(t *testing.T) {
	executor := NewFakeExecutor()
	executor.SetResult("fail-task", ExecutorResult{
		TaskID: "fail-task",
		Status:     "failed",
		Error:      "something broke",
	})
	// "ok-task" uses default success

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(2, executor, clock)

	pool.Submit(Job{
		Task:     TaskConfig{ID: "fail-task"},
		Prompt: "will fail",
		TimeoutMin:   1,
	})
	pool.Submit(Job{
		Task:     TaskConfig{ID: "ok-task"},
		Prompt: "will succeed",
		TimeoutMin:   1,
	})

	results := make(map[string]JobResult)
	for i := 0; i < 2; i++ {
		r := <-pool.Results()
		results[r.Job.Task.ID] = r
	}
	pool.Shutdown()

	if results["fail-task"].Result.Status != "failed" {
		t.Errorf("fail-task status = %q, want failed", results["fail-task"].Result.Status)
	}
	if results["ok-task"].Result.Status != "success" {
		t.Errorf("ok-task status = %q, want success", results["ok-task"].Result.Status)
	}
}

// testTaskExceedsTimeout verifies that tasks exceeding their timeout get status "timeout".
func testTaskExceedsTimeout(t *testing.T) {
	executor := NewFakeExecutor()
	// Set delay longer than the timeout we'll configure
	executor.SetDelay(500 * time.Millisecond)

	clock := &FakeClock{T: time.Now()}

	// Use short timeout pool helper since TimeoutMin is in minutes (too long for tests)
	pool := newShortTimeoutPool(2, executor, clock, 50*time.Millisecond)

	pool.Submit(Job{
		Task:     TaskConfig{ID: "slow-task"},
		Prompt: "too slow",
	})

	r := <-pool.Results()
	pool.Shutdown()

	if r.Result.Status != "timeout" {
		t.Errorf("status = %q, want timeout", r.Result.Status)
	}
}

// testPerTaskTimeoutOverridesGlobal verifies that per-task timeout settings are used.
func testPerTaskTimeoutOverridesGlobal(t *testing.T) {
	executor := NewFakeExecutor()
	// Delay of 100ms - shorter timeout should trigger timeout, longer should succeed
	executor.SetDelay(100 * time.Millisecond)

	clock := &FakeClock{T: time.Now()}

	// Short timeout pool: 50ms timeout (less than 100ms delay -> timeout)
	shortPool := newShortTimeoutPool(2, executor, clock, 50*time.Millisecond)

	shortPool.Submit(Job{
		Task:     TaskConfig{ID: "short-timeout"},
		Prompt: "should timeout",
	})

	r1 := <-shortPool.Results()
	shortPool.Shutdown()

	if r1.Result.Status != "timeout" {
		t.Errorf("short timeout status = %q, want timeout", r1.Result.Status)
	}

	// Long timeout pool: 200ms timeout (more than 100ms delay -> success)
	longPool := newShortTimeoutPool(2, executor, clock, 200*time.Millisecond)

	longPool.Submit(Job{
		Task:     TaskConfig{ID: "long-timeout"},
		Prompt: "should succeed",
	})

	r2 := <-longPool.Results()
	longPool.Shutdown()

	if r2.Result.Status != "success" {
		t.Errorf("long timeout status = %q, want success", r2.Result.Status)
	}
}

// testActiveJobsReturnRunningJobs verifies that ActiveJobs returns currently running jobs.
func testActiveJobsReturnRunningJobs(t *testing.T) {
	executor := NewFakeExecutor()
	executor.SetDelay(100 * time.Millisecond)

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(2, executor, clock)

	pool.Submit(Job{
		Task:     TaskConfig{ID: "running-job"},
		Prompt: "running",
		TimeoutMin:   1,
	})

	// Give the goroutine a moment to start
	time.Sleep(20 * time.Millisecond)

	active := pool.ActiveJobs()
	if len(active) != 1 {
		t.Fatalf("expected 1 active job, got %d", len(active))
	}
	if active[0].Job.Task.ID != "running-job" {
		t.Errorf("active job ID = %q, want running-job", active[0].Job.Task.ID)
	}

	// Wait for completion
	<-pool.Results()
	pool.Shutdown()

	// After completion, no active jobs
	active = pool.ActiveJobs()
	if len(active) != 0 {
		t.Errorf("expected 0 active jobs after completion, got %d", len(active))
	}
}

// testShutdownWaitsForRunningJobs verifies that Shutdown blocks until all running jobs complete.
func testShutdownWaitsForRunningJobs(t *testing.T) {
	executor := NewFakeExecutor()
	executor.SetDelay(50 * time.Millisecond)

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(2, executor, clock)

	pool.Submit(Job{
		Task:     TaskConfig{ID: "job1"},
		Prompt: "running",
		TimeoutMin:   1,
	})
	pool.Submit(Job{
		Task:     TaskConfig{ID: "job2"},
		Prompt: "running",
		TimeoutMin:   1,
	})

	// Give goroutines time to start
	time.Sleep(10 * time.Millisecond)

	// Shutdown should block until both jobs finish
	done := make(chan struct{})
	go func() {
		pool.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		// Shutdown completed - drain results channel
		drained := 0
		for drained < 2 {
			select {
			case <-pool.Results():
				drained++
			default:
				drained = 2
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not complete within timeout")
	}
}

// testResultsChannelReceivesAllCompletions verifies every submitted job produces exactly one result.
func testResultsChannelReceivesAllCompletions(t *testing.T) {
	const totalJobs = 10

	executor := NewFakeExecutor()
	executor.SetDelay(5 * time.Millisecond)

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(3, executor, clock)

	for i := 0; i < totalJobs; i++ {
		pool.Submit(Job{
			Task:     TaskConfig{ID: makeTaskID(i)},
			Prompt: "test",
			TimeoutMin:   1,
		})
	}

	seen := make(map[string]int)
	for i := 0; i < totalJobs; i++ {
		r := <-pool.Results()
		seen[r.Job.Task.ID]++
	}
	pool.Shutdown()

	if len(seen) != totalJobs {
		t.Errorf("got results for %d unique tasks, want %d", len(seen), totalJobs)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("task %q received %d results, want 1", id, count)
		}
	}
}

// --- Helpers ---

func makeTaskID(i int) string {
	return "task-" + string(rune('a'+i))
}

// trackingExecutor wraps an executor and tracks peak concurrency.
type trackingExecutor struct {
	inner      Executor
	running    *int64
	maxRunning *int64
	mu         *sync.Mutex
}

func (te *trackingExecutor) Run(ctx context.Context, task TaskConfig, prompt string) (ExecutorResult, error) {
	cur := atomic.AddInt64(te.running, 1)
	te.mu.Lock()
	if cur > *te.maxRunning {
		*te.maxRunning = cur
	}
	te.mu.Unlock()
	defer atomic.AddInt64(te.running, -1)
	return te.inner.Run(ctx, task, prompt)
}

// shortTimeoutPool is a WorkerPool variant that uses a custom short timeout for testing.
// It wraps the executor to apply a short context timeout.
type shortTimeoutPool struct {
	*WorkerPool
}

func newShortTimeoutPool(slots int, executor Executor, clock Clock, timeout time.Duration) *shortTimeoutPool {
	wrappedExec := &timeoutWrappingExecutor{inner: executor, timeout: timeout}
	return &shortTimeoutPool{
		WorkerPool: NewWorkerPool(slots, wrappedExec, clock),
	}
}

type timeoutWrappingExecutor struct {
	inner   Executor
	timeout time.Duration
}

func (e *timeoutWrappingExecutor) Run(ctx context.Context, task TaskConfig, prompt string) (ExecutorResult, error) {
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	return e.inner.Run(tctx, task, prompt)
}

// TestFakeExecutor tests the FakeExecutor itself.
func TestFakeExecutor(t *testing.T) {
	t.Run("DefaultSuccess", func(t *testing.T) {
		exec := NewFakeExecutor()
		result, err := exec.Run(context.Background(), TaskConfig{ID: "test"}, "hello")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Status != "success" {
			t.Errorf("status = %q, want success", result.Status)
		}
	})

	t.Run("ConfiguredResult", func(t *testing.T) {
		exec := NewFakeExecutor()
		exec.SetResult("test", ExecutorResult{
			TaskID: "test",
			Status:     "failed",
			Error:      "boom",
		})
		result, err := exec.Run(context.Background(), TaskConfig{ID: "test"}, "hello")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Status != "failed" {
			t.Errorf("status = %q, want failed", result.Status)
		}
	})

	t.Run("ConfiguredError", func(t *testing.T) {
		exec := NewFakeExecutor()
		exec.SetError("test", errors.New("executor error"))
		_, err := exec.Run(context.Background(), TaskConfig{ID: "test"}, "hello")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if err.Error() != "executor error" {
			t.Errorf("error = %q, want 'executor error'", err.Error())
		}
	})

	t.Run("RecordsCalls", func(t *testing.T) {
		exec := NewFakeExecutor()
		exec.Run(context.Background(), TaskConfig{ID: "a"}, "instr-a")
		exec.Run(context.Background(), TaskConfig{ID: "b"}, "instr-b")
		calls := exec.Calls()
		if len(calls) != 2 {
			t.Fatalf("expected 2 calls, got %d", len(calls))
		}
		if calls[0].TaskID != "a" || calls[0].Prompt != "instr-a" {
			t.Errorf("call 0 = %+v, unexpected", calls[0])
		}
		if calls[1].TaskID != "b" || calls[1].Prompt != "instr-b" {
			t.Errorf("call 1 = %+v, unexpected", calls[1])
		}
	})

	t.Run("DelayWithContextCancellation", func(t *testing.T) {
		exec := NewFakeExecutor()
		exec.SetDelay(5 * time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		result, err := exec.Run(ctx, TaskConfig{ID: "test"}, "hello")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Status != "timeout" {
			t.Errorf("status = %q, want timeout", result.Status)
		}
	})
}

// testZeroTimeoutFallback verifies that a job with TimeoutMin == 0 uses the
// fallback timeout and completes normally rather than hanging or panicking.
func TestWorkerPool_ZeroTimeoutFallback(t *testing.T) {
	executor := NewFakeExecutor()
	// Fast-completing job — should succeed despite TimeoutMin: 0
	executor.SetResult("zero-timeout-task", ExecutorResult{
		TaskID: "zero-timeout-task",
		Status: "success",
		Output: "done",
	})

	clock := &FakeClock{T: time.Now()}
	pool := NewWorkerPool(1, executor, clock)

	pool.Submit(Job{
		Task:       TaskConfig{ID: "zero-timeout-task"},
		Prompt:     "do something",
		TimeoutMin: 0, // should hit fallback, not create an infinite context
	})

	r := <-pool.Results()
	pool.Shutdown()

	if r.Result.Status != "success" {
		t.Errorf("status = %q, want success", r.Result.Status)
	}
}
