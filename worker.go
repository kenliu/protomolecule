package main

import (
	"context"
	"sync"
	"time"
)

// Job represents a unit of work to be executed by the worker pool.
type Job struct {
	Task     TaskConfig
	Prompt string
	CycleID      string
	Attempt      int
	MaxAttempts  int
	TimeoutMin   int
	RetryDelay   time.Duration
}

// JobResult captures the outcome of a job execution.
type JobResult struct {
	Job       Job
	Result    ExecutorResult
	Err       error
	Completed time.Time
}

// activeJob tracks a currently running job.
type activeJob struct {
	Job       Job
	StartedAt time.Time
	Cancel    context.CancelFunc
}

// WorkerPool dispatches jobs to executors with concurrency limits and timeout handling.
type WorkerPool struct {
	slots    int
	executor Executor
	clock    Clock
	sem      chan struct{}    // semaphore for limiting concurrency
	results  chan JobResult   // completion notifications
	wg       sync.WaitGroup
	active   map[string]*activeJob // currently running jobs, keyed by task ID
	activeMu sync.RWMutex
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewWorkerPool creates a new WorkerPool with the given concurrency limit, executor, and clock.
func NewWorkerPool(slots int, executor Executor, clock Clock) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &WorkerPool{
		slots:    slots,
		executor: executor,
		clock:    clock,
		sem:      make(chan struct{}, slots),
		results:  make(chan JobResult, 100),
		active:   make(map[string]*activeJob),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Submit enqueues a job for execution. It launches a goroutine that waits for
// a semaphore slot, then executes the job with a timeout-scoped context.
// This method is non-blocking.
func (wp *WorkerPool) Submit(job Job) {
	wp.wg.Add(1)
	go func() {
		defer wp.wg.Done()

		// Acquire semaphore slot (blocks if all slots are full)
		select {
		case wp.sem <- struct{}{}:
			// Got a slot
		case <-wp.ctx.Done():
			// Pool is shutting down
			wp.results <- JobResult{
				Job: job,
				Result: ExecutorResult{
					TaskID: job.Task.ID,
					Status:     "failed",
					Error:      "worker pool shutdown",
				},
				Completed: wp.clock.Now(),
			}
			return
		}
		defer func() { <-wp.sem }() // Release slot when done

		// Create timeout context for this job.
		// A zero or negative timeout is never allowed — fall back to a hard 60-minute
		// ceiling so a misconfigured job cannot run indefinitely.
		const fallbackTimeoutMinutes = 60
		timeoutMin := job.TimeoutMin
		if timeoutMin <= 0 {
			timeoutMin = fallbackTimeoutMinutes
		}
		jobCtx, jobCancel := context.WithTimeout(wp.ctx, time.Duration(timeoutMin)*time.Minute)
		defer jobCancel()

		// Track as active
		aj := &activeJob{
			Job:       job,
			StartedAt: wp.clock.Now(),
			Cancel:    jobCancel,
		}
		wp.activeMu.Lock()
		wp.active[job.Task.ID] = aj
		wp.activeMu.Unlock()

		// Execute
		result, err := wp.executor.Run(jobCtx, job.Task, job.Prompt)
		result.CycleID = job.CycleID
		result.Attempt = job.Attempt

		// Remove from active
		wp.activeMu.Lock()
		delete(wp.active, job.Task.ID)
		wp.activeMu.Unlock()

		// Send result
		wp.results <- JobResult{
			Job:       job,
			Result:    result,
			Err:       err,
			Completed: wp.clock.Now(),
		}
	}()
}

// Results returns the channel that receives job completion notifications.
func (wp *WorkerPool) Results() <-chan JobResult {
	return wp.results
}

// IsActive returns true if a job with the given task ID is currently running.
func (wp *WorkerPool) IsActive(taskID string) bool {
	wp.activeMu.RLock()
	defer wp.activeMu.RUnlock()
	_, ok := wp.active[taskID]
	return ok
}

// ActiveJobs returns a snapshot of currently running jobs.
func (wp *WorkerPool) ActiveJobs() []activeJob {
	wp.activeMu.RLock()
	defer wp.activeMu.RUnlock()

	jobs := make([]activeJob, 0, len(wp.active))
	for _, aj := range wp.active {
		jobs = append(jobs, *aj)
	}
	return jobs
}

// Shutdown cancels the pool context and waits for all goroutines to finish.
func (wp *WorkerPool) Shutdown() {
	wp.cancel()
	wp.wg.Wait()
}
