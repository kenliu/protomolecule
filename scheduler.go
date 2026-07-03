package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler ties together Config, StateStore, WorkerPool, Clock, and NetworkChecker
// to evaluate schedules and dispatch tasks.
type Scheduler struct {
	config   *Config
	configMu sync.RWMutex
	state    *StateStore
	pool     *WorkerPool
	clock    Clock
	network  NetworkChecker
	logger   *slog.Logger

	// internal tracking
	tickCount        int64
	startedAt        time.Time
	ctx              context.Context
	cancel           context.CancelFunc
	configLastReload time.Time
	configReloadOK   bool
	paused           atomic.Bool
}

// NewScheduler creates a new Scheduler with all dependencies injected.
func NewScheduler(cfg *Config, state *StateStore, pool *WorkerPool, clock Clock, network NetworkChecker, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		config:  cfg,
		state:   state,
		pool:    pool,
		clock:   clock,
		network: network,
		logger:  logger,
	}
}

// Start begins the main scheduler loop. It ticks every minute, processes
// completions from the worker pool, and handles graceful shutdown on context cancellation.
func (s *Scheduler) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.startedAt = s.clock.Now()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Run an initial tick immediately
	s.tick()

	for {
		select {
		case <-ticker.C:
			s.tick()
		case result, ok := <-s.pool.Results():
			if !ok {
				return
			}
			s.handleCompletion(result)
		case <-s.ctx.Done():
			return
		}
	}
}

// IsPaused returns whether the scheduler is currently paused.
func (s *Scheduler) IsPaused() bool {
	return s.paused.Load()
}

// SetPaused sets the paused state. When paused, tick() skips all dispatch —
// no scheduled tasks or retries are started. Already-running jobs continue.
func (s *Scheduler) SetPaused(v bool) {
	s.paused.Store(v)
	if v {
		s.logger.Info("scheduler paused", "type", "scheduler_paused")
	} else {
		s.logger.Info("scheduler resumed", "type", "scheduler_resumed")
	}
}

// tick is the main scheduling evaluation, called every minute.
func (s *Scheduler) tick() {
	s.tickCount++

	// When paused, skip all dispatch. Already-running jobs continue to completion.
	if s.paused.Load() {
		return
	}

	now := s.clock.Now()
	networkAvailable := s.network.IsAvailable()

	s.configMu.RLock()
	cfg := s.config
	s.configMu.RUnlock()

	// Check for pending retries across all known tasks
	s.checkRetries(cfg, now, networkAvailable)

	// Evaluate workflows
	for _, wf := range cfg.Workflows {
		if s.isDue(wf.Schedule, wf.ID, wf.Catchup, now, cfg.Location()) {
			fireTime := s.lastScheduledFiring(wf.Schedule, now, cfg.Location())
			cycleID := s.generateCycleID(wf.ID, fireTime)
			// Record that we attempted this firing window so isDue won't
			// re-dispatch after retries exhaust (LastSuccess never advances on failure).
			s.state.UpdateTask(wf.ID, func(as *TaskState) {
				as.LastAttemptedFiring = &fireTime
			})
			// Dispatch all root tasks (those with no depends_on)
			for _, act := range wf.Tasks {
				if act.DependsOn == "" {
					if s.pool.IsActive(act.ID) {
						s.logger.Info("schedule skip",
							"type", "schedule_skip",
							"task_id", act.ID,
							"reason", "already_running",
						)
						continue
					}
					if act.IsNetworkRequired() && !networkAvailable {
						s.logger.Info("schedule skip",
							"type", "schedule_skip",
							"task_id", act.ID,
							"reason", "no_network",
						)
						continue
					}
					s.dispatchTask(act, act.Prompt, cycleID, cfg)
				}
			}
		}
	}

	// Evaluate standalone tasks
	for _, act := range cfg.Standalone {
		if s.isDue(act.Schedule, act.ID, act.Catchup, now, cfg.Location()) {
			if s.pool.IsActive(act.ID) {
				s.logger.Info("schedule skip",
					"type", "schedule_skip",
					"task_id", act.ID,
					"reason", "already_running",
				)
				continue
			}
			if act.IsNetworkRequired() && !networkAvailable {
				s.logger.Info("schedule skip",
					"type", "schedule_skip",
					"task_id", act.ID,
					"reason", "no_network",
				)
				continue
			}
			// Record that we attempted this firing window so isDue won't
			// re-dispatch after retries exhaust.
			fireTime := s.lastScheduledFiring(act.Schedule, now, cfg.Location())
			s.state.UpdateTask(act.ID, func(as *TaskState) {
				as.LastAttemptedFiring = &fireTime
			})
			s.dispatchTask(act, act.Prompt, "", cfg)
		}
	}
}

// checkRetries checks all tasks for pending retries that are now due.
func (s *Scheduler) checkRetries(cfg *Config, now time.Time, networkAvailable bool) {
	allTasks := s.gatherAllTasks(cfg)
	for _, info := range allTasks {
		state := s.state.GetTask(info.task.ID)
		if state == nil {
			continue
		}
		if state.RetryAt != nil && !now.Before(*state.RetryAt) && state.RetryAttempt > 0 {
			if info.task.IsNetworkRequired() && !networkAvailable {
				continue
			}
			job := Job{
				Task:        info.task,
				Prompt:      info.task.Prompt,
				CycleID:     state.LastCycleID,
				Attempt:     state.RetryAttempt + 1,
				MaxAttempts: min(info.task.GetMaxRetries(cfg.MaxRetries), maxRetriesLimit) + 1,
				TimeoutMin:  info.task.GetTimeoutMinutes(cfg.TimeoutMinutes),
				RetryDelay:  time.Duration(info.task.GetRetryDelaySeconds(cfg.RetryDelaySeconds)) * time.Second,
			}
			// Clear retry_at before dispatching to prevent re-dispatch on next tick
			s.state.UpdateTask(info.task.ID, func(as *TaskState) {
				as.RetryAt = nil
			})
			s.pool.Submit(job)
		}
	}
}

// taskInfo pairs an task config with its workflow context.
type taskInfo struct {
	task   TaskConfig
	workflowID string
}

// gatherAllTasks returns all tasks from workflows and standalone.
func (s *Scheduler) gatherAllTasks(cfg *Config) []taskInfo {
	var result []taskInfo
	for _, wf := range cfg.Workflows {
		for _, act := range wf.Tasks {
			result = append(result, taskInfo{task: act, workflowID: wf.ID})
		}
	}
	for _, act := range cfg.Standalone {
		result = append(result, taskInfo{task: act})
	}
	return result
}

// isDue determines whether a task/workflow is due to run.
func (s *Scheduler) isDue(schedule string, taskID string, catchup bool, now time.Time, loc *time.Location) bool {
	if strings.EqualFold(schedule, "on-demand") || schedule == "" {
		return false
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(schedule)
	if err != nil {
		s.logger.Error("invalid cron expression",
			"task_id", taskID,
			"schedule", schedule,
			"error", err.Error(),
		)
		return false
	}

	lastFiring := s.computeLastFiring(sched, now, loc)
	if lastFiring.IsZero() {
		return false
	}

	state := s.state.GetTask(taskID)

	// Never run before: skip and wait for the next natural cron window.
	// Don't treat "no state" as "missed everything" — that would cause all
	// tasks to fire simultaneously on first start or after state file deletion.
	if state == nil || state.LastSuccess == nil {
		return false
	}

	lastSuccess := *state.LastSuccess

	// Already succeeded at or after the last firing: not due
	if !lastSuccess.Before(lastFiring) {
		return false
	}

	// Already attempted this firing window and failed (retries exhausted): not due.
	// Without this check, a task that fails all retries would be re-dispatched
	// on every subsequent tick because LastSuccess never advances.
	if state.LastAttemptedFiring != nil && !state.LastAttemptedFiring.Before(lastFiring) {
		return false
	}

	// Last success is before the last firing
	if catchup {
		// Catchup: due (fires at most once for the most recent missed window)
		return true
	}

	// No catchup: only due if the current minute matches the cron expression
	// i.e., we're within the current firing window
	truncated := now.In(loc).Truncate(time.Minute)
	truncatedUTC := truncated.UTC()
	// The next firing after one minute before truncated should equal truncated
	prev := truncatedUTC.Add(-1 * time.Minute)
	nextAfterPrev := sched.Next(prev)
	return nextAfterPrev.Equal(truncatedUTC)
}

// computeLastFiring finds the most recent time the cron schedule would have fired
// at or before now. Uses an iterative forward approach from a safe start point.
func (s *Scheduler) computeLastFiring(sched cron.Schedule, now time.Time, loc *time.Location) time.Time {
	// Determine lookback window based on schedule frequency.
	// Start from far enough back to guarantee we find the most recent firing.
	// For monthly schedules we need 32 days, for weekly 8 days, for daily 25 hours, etc.
	// Use 32 days as a safe default that covers all reasonable schedules including monthly.
	start := now.Add(-32 * 24 * time.Hour)

	var lastFiring time.Time
	cursor := start
	for {
		next := sched.Next(cursor)
		if next.After(now) {
			break
		}
		lastFiring = next
		cursor = next
	}
	return lastFiring
}

// lastScheduledFiring is a convenience wrapper for computeLastFiring
// that parses the cron expression and returns the last firing time.
func (s *Scheduler) lastScheduledFiring(schedule string, now time.Time, loc *time.Location) time.Time {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(schedule)
	if err != nil {
		return time.Time{}
	}
	return s.computeLastFiring(sched, now, loc)
}

// generateCycleID creates a cycle ID for a workflow run.
func (s *Scheduler) generateCycleID(workflowID string, fireTime time.Time) string {
	return fmt.Sprintf("%s:%s", workflowID, fireTime.UTC().Format(time.RFC3339))
}

// GenerateForceCycleID creates a cycle ID for a force run.
func (s *Scheduler) generateForceCycleID(workflowID string, now time.Time) string {
	return fmt.Sprintf("%s:force:%s", workflowID, now.UTC().Format(time.RFC3339))
}

// dispatchTask builds a Job and submits it to the worker pool.
func (s *Scheduler) dispatchTask(task TaskConfig, prompt string, cycleID string, cfg *Config) {
	maxRetries := min(task.GetMaxRetries(cfg.MaxRetries), maxRetriesLimit)
	job := Job{
		Task:        task,
		Prompt:      prompt,
		CycleID:     cycleID,
		Attempt:     1,
		MaxAttempts: maxRetries + 1,
		TimeoutMin:  task.GetTimeoutMinutes(cfg.TimeoutMinutes),
		RetryDelay:  time.Duration(task.GetRetryDelaySeconds(cfg.RetryDelaySeconds)) * time.Second,
	}
	s.logger.Info("task start",
		"type", "task_start",
		"task_id", task.ID,
		"attempt", job.Attempt,
		"max_attempts", job.MaxAttempts,
	)
	if cycleID != "" {
		s.logger.Info("schedule dispatch",
			"type", "schedule_dispatch",
			"task_id", task.ID,
			"cycle_id", cycleID,
		)
	}
	s.pool.Submit(job)
}

// handleCompletion processes a completed job result, updates state, handles retries,
// and performs event-driven dispatch of dependent tasks.
func (s *Scheduler) handleCompletion(result JobResult) {
	now := s.clock.Now()
	taskID := result.Job.Task.ID
	cycleID := result.Job.CycleID

	s.configMu.RLock()
	cfg := s.config
	s.configMu.RUnlock()

	if result.Result.Status == "success" {
		var duration time.Duration
		if !result.Result.StartedAt.IsZero() {
			duration = now.Sub(result.Result.StartedAt)
		}
		s.state.UpdateTask(taskID, func(as *TaskState) {
			as.LastRun = &now
			as.LastRunStatus = "success"
			as.LastSuccess = &now
			as.LastRunDuration = duration
			as.ConsecutiveFailures = 0
			as.LastCycleID = cycleID
			as.RetryAt = nil
			as.RetryAttempt = 0
			as.RecentRuns = appendRunEntry(as.RecentRuns, RunEntry{Status: "success", RunAt: now, Duration: duration})
		})
		durationMs := duration.Milliseconds()
		s.logger.Info("task end",
			"type", "task_end",
			"task_id", taskID,
			"status", "success",
			"duration_ms", durationMs,
			"attempt", result.Job.Attempt,
			"cycle_id", cycleID,
		)

		// If this task is the leaf of a workflow, update the workflow's LastSuccess
		// so isDue() can schedule future runs of the workflow.
		s.updateWorkflowStateOnLeafSuccess(taskID, now, cfg)

		// Event-driven dependent dispatch for workflows
		if cycleID != "" {
			s.dispatchReadyDependents(taskID, cycleID, cfg)
		}
	} else {
		// Failed or timed out
		var duration time.Duration
		if !result.Result.StartedAt.IsZero() {
			duration = now.Sub(result.Result.StartedAt)
		}
		s.state.UpdateTask(taskID, func(as *TaskState) {
			as.LastRun = &now
			as.LastRunStatus = result.Result.Status
			as.LastRunDuration = duration
			as.LastFailure = &now
			as.ConsecutiveFailures++
			as.LastCycleID = cycleID
			as.RecentRuns = appendRunEntry(as.RecentRuns, RunEntry{Status: result.Result.Status, RunAt: now, Duration: duration})
		})

		// Use MaxAttempts from the Job, which already has the hard cap applied.
		// Do not re-derive from the task config — that bypasses the cap.
		maxRetries := result.Job.MaxAttempts - 1
		currentAttempt := result.Job.Attempt

		durationMs := duration.Milliseconds()
		s.logger.Info("task end",
			"type", "task_end",
			"task_id", taskID,
			"status", result.Result.Status,
			"duration_ms", durationMs,
			"attempt", currentAttempt,
			"cycle_id", cycleID,
			"error", result.Result.Error,
		)

		// Fatal errors (e.g., binary not found) are never retried.
		if result.Result.Status == "fatal" {
			s.state.UpdateTask(taskID, func(as *TaskState) {
				as.RetryAt = nil
				as.RetryAttempt = 0
			})
			s.logger.Error("fatal task error, not retrying",
				"type", "task_fatal",
				"task_id", taskID,
				"error", result.Result.Error,
			)
			return
		}

		if currentAttempt < maxRetries+1 {
			// Schedule a retry
			retryDelay := time.Duration(result.Job.Task.GetRetryDelaySeconds(cfg.RetryDelaySeconds)) * time.Second
			retryAt := now.Add(retryDelay)
			s.state.UpdateTask(taskID, func(as *TaskState) {
				as.RetryAt = &retryAt
				as.RetryAttempt = currentAttempt
			})
			s.logger.Info("retry scheduled",
				"type", "retry_scheduled",
				"task_id", taskID,
				"attempt", currentAttempt + 1,
				"retry_at", retryAt.Format(time.RFC3339),
			)
		} else {
			// All retries exhausted
			s.state.UpdateTask(taskID, func(as *TaskState) {
				as.RetryAt = nil
				as.RetryAttempt = 0
			})
			s.logger.Info("retry exhausted",
				"type", "retry_exhausted",
				"task_id", taskID,
				"total_attempts", maxRetries + 1,
			)

			// Skip downstream dependents for this cycle
			// (they simply won't be dispatched since the parent failed)
		}
	}
}

// updateWorkflowStateOnLeafSuccess checks if the completed task is a leaf in its
// workflow (no other task depends on it). If so, updates LastSuccess for the workflow
// ID so that isDue() can correctly schedule future runs of the workflow.
//
// Without this, a new workflow's ID never gets a LastSuccess in state, so isDue()
// always returns false for it (by design, to avoid firing everything on first start).
// This seeds that state automatically when the pipeline finishes its leaf task.
func (s *Scheduler) updateWorkflowStateOnLeafSuccess(taskID string, now time.Time, cfg *Config) {
	for _, wf := range cfg.Workflows {
		// Check if this task belongs to this workflow
		inWorkflow := false
		for _, act := range wf.Tasks {
			if act.ID == taskID {
				inWorkflow = true
				break
			}
		}
		if !inWorkflow {
			continue
		}

		// Check if this task is a leaf (no other task in the workflow depends on it)
		isLeaf := true
		for _, act := range wf.Tasks {
			if act.DependsOn == taskID {
				isLeaf = false
				break
			}
		}

		if isLeaf {
			s.state.UpdateTask(wf.ID, func(as *TaskState) {
				as.LastSuccess = &now
				as.LastRun = &now
				as.LastRunStatus = "success"
			})
			s.logger.Info("workflow complete",
				"type", "workflow_complete",
				"workflow_id", wf.ID,
				"leaf_task", taskID,
			)
		}
		break // a task can only belong to one workflow
	}
}

// dispatchReadyDependents checks all workflow tasks to find those that depend
// on the completed task with a matching cycle ID, and dispatches them immediately.
func (s *Scheduler) dispatchReadyDependents(completedID string, cycleID string, cfg *Config) {
	networkAvailable := s.network.IsAvailable()

	for _, wf := range cfg.Workflows {
		for _, act := range wf.Tasks {
			if act.DependsOn != completedID {
				continue
			}
			// Check that the parent's last_cycle_id matches
			parentState := s.state.GetTask(completedID)
			if parentState == nil || parentState.LastCycleID != cycleID {
				continue
			}
			if parentState.LastRunStatus != "success" {
				continue
			}
			// Check network
			if act.IsNetworkRequired() && !networkAvailable {
				s.logger.Info("schedule skip",
				"type", "schedule_skip",
				"task_id", act.ID,
				"reason", "no_network",
			)
				continue
			}
			s.logger.Info("dependent dispatch",
				"type", "dependent_dispatch",
				"task_id", act.ID,
				"parent", completedID,
				"cycle_id", cycleID,
			)
			s.dispatchTask(act, act.Prompt, cycleID, cfg)
		}
	}
}

// ForceRunTask dispatches a single task immediately, bypassing schedule checks.
func (s *Scheduler) ForceRunTask(taskID string) error {
	s.configMu.RLock()
	cfg := s.config
	s.configMu.RUnlock()

	// Search in workflows
	for _, wf := range cfg.Workflows {
		for _, act := range wf.Tasks {
			if act.ID == taskID {
				cycleID := s.generateForceCycleID(wf.ID, s.clock.Now())
				s.dispatchTask(act, act.Prompt, cycleID, cfg)
				return nil
			}
		}
	}

	// Search in standalone
	for _, act := range cfg.Standalone {
		if act.ID == taskID {
			cycleID := s.generateForceCycleID(taskID, s.clock.Now())
			s.dispatchTask(act, act.Prompt, cycleID, cfg)
			return nil
		}
	}

	return fmt.Errorf("task %q not found in config", taskID)
}

// ReloadConfig atomically swaps the scheduler's config under a write lock.
func (s *Scheduler) ReloadConfig(cfg *Config) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = cfg
}

// GetTickCount returns the number of ticks that have occurred.
func (s *Scheduler) GetTickCount() int64 {
	return s.tickCount
}

// GetStartedAt returns the time the scheduler started.
func (s *Scheduler) GetStartedAt() time.Time {
	return s.startedAt
}

// GetConfig returns the current config under a read lock.
func (s *Scheduler) GetConfig() *Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

// TrackConfigReload records the result of a config reload attempt.
func (s *Scheduler) TrackConfigReload(ok bool) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.configLastReload = time.Now()
	s.configReloadOK = ok
}

// GetConfigLastReload returns the time of the last config reload attempt.
func (s *Scheduler) GetConfigLastReload() time.Time {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.configLastReload
}

// GetConfigReloadOK returns whether the most recent config reload succeeded.
func (s *Scheduler) GetConfigReloadOK() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.configReloadOK
}
