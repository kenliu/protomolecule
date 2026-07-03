package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OutputBuffer accumulates lines written by a running job.
// It is safe for concurrent use.
type OutputBuffer struct {
	mu    sync.RWMutex
	lines []string
}

// Append adds a line to the buffer.
func (b *OutputBuffer) Append(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
}

// Lines returns a copy of all accumulated lines.
func (b *OutputBuffer) Lines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cp := make([]string, len(b.lines))
	copy(cp, b.lines)
	return cp
}

// LiveOutputRegistry maps running task IDs to their OutputBuffer.
// It is safe for concurrent use.
type LiveOutputRegistry struct {
	mu      sync.RWMutex
	buffers map[string]*OutputBuffer
}

// NewLiveOutputRegistry creates an empty registry.
func NewLiveOutputRegistry() *LiveOutputRegistry {
	return &LiveOutputRegistry{buffers: make(map[string]*OutputBuffer)}
}

// Register creates and stores a buffer for the given task ID and returns it.
func (r *LiveOutputRegistry) Register(taskID string) *OutputBuffer {
	b := &OutputBuffer{}
	r.mu.Lock()
	r.buffers[taskID] = b
	r.mu.Unlock()
	return b
}

// Deregister removes the buffer for the given task ID.
func (r *LiveOutputRegistry) Deregister(taskID string) {
	r.mu.Lock()
	delete(r.buffers, taskID)
	r.mu.Unlock()
}

// Get returns the buffer for the given task ID, or false if not found.
func (r *LiveOutputRegistry) Get(taskID string) (*OutputBuffer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.buffers[taskID]
	return b, ok
}

// taskLogWriter wraps an slog.Logger and emits each line of output as a log
// entry tagged with the task ID. It also accumulates the full output into a
// buffer so the result can still include it.
type taskLogWriter struct {
	taskID  string
	logger  *slog.Logger
	buf     bytes.Buffer  // accumulated full output
	liveOut *OutputBuffer // optional; if non-nil, each line is also appended here
}

// newTaskLogWriter creates a writer that logs each line and keeps a copy.
func newTaskLogWriter(taskID string, logger *slog.Logger) *taskLogWriter {
	return &taskLogWriter{taskID: taskID, logger: logger}
}

// Stream reads lines from r and logs each one. Call this in a goroutine.
// It returns when r is closed / reaches EOF.
func (w *taskLogWriter) Stream(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Claude stream-json lines can be large; raise the buffer limit.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		w.buf.WriteString(line)
		w.buf.WriteByte('\n')
		if w.liveOut != nil {
			w.liveOut.Append(line)
		}
		if w.logger != nil {
			w.logger.Info("claude output",
				"type", "claude_output",
				"task_id", w.taskID,
				"line", strings.TrimRight(line, "\r\n"),
			)
		}
	}
}

// String returns the accumulated output.
func (w *taskLogWriter) String() string {
	return w.buf.String()
}

// ClaudeExecutor shells out to "claude -p" to run tasks.
type ClaudeExecutor struct {
	ProjectRoot string              // working directory for claude -p
	Logger      *slog.Logger        // optional; if nil, verbose logging is silenced
	LiveOutputs *LiveOutputRegistry // optional; if non-nil, streams output to registry
}

// Run executes the task by invoking claude -p with the given prompt.
// It respects the context for timeout/cancellation, sending SIGTERM then SIGKILL
// if the process does not exit in time.
func (e *ClaudeExecutor) Run(ctx context.Context, task TaskConfig, prompt string) (ExecutorResult, error) {
	startedAt := time.Now()

	args := []string{"-p", "--verbose", "--no-session-persistence", "--output-format", "stream-json"}
	if task.Agent != "" {
		args = append(args, "--agent", task.Agent)
	}
	if task.Model != "" {
		args = append(args, "--model", task.Model)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = e.ProjectRoot
	// Use process group so we can signal the entire group
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if e.Logger != nil {
		e.Logger.Debug("executing claude",
			"task", task.ID,
			"args", args,
			"prompt", prompt,
			"cwd", e.ProjectRoot,
		)
	}

	// Set up stdout pipe so we can stream lines through the logger in real time.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return ExecutorResult{
			TaskID:     task.ID,
			Status:     "failed",
			StartedAt:  startedAt,
			FinishedAt: time.Now(),
			Error:      fmt.Sprintf("failed to create stdout pipe: %v", err),
		}, nil
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Use a taskLogWriter to stream stdout lines to the logger in real time
	// while also accumulating the full output for the result.
	logWriter := newTaskLogWriter(task.ID, e.Logger)
	if e.LiveOutputs != nil {
		logWriter.liveOut = e.LiveOutputs.Register(task.ID)
		defer e.LiveOutputs.Deregister(task.ID)
	}
	streamDone := make(chan struct{})

	err = cmd.Start()
	if err != nil {
		// Distinguish permanent infrastructure failures from transient errors.
		// A missing binary will never succeed on retry.
		status := "failed"
		if errors.Is(err, exec.ErrNotFound) {
			status = "fatal"
		}
		return ExecutorResult{
			TaskID:     task.ID,
			Status:     status,
			StartedAt:  startedAt,
			FinishedAt: time.Now(),
			Error:      fmt.Sprintf("failed to start: %v", err),
		}, nil
	}

	// Stream stdout in a goroutine; closes when the pipe hits EOF (process exits).
	go func() {
		logWriter.Stream(stdoutPipe)
		close(streamDone)
	}()

	// Wait for the process in a goroutine so we can handle context cancellation
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- cmd.Wait()
	}()

	var result ExecutorResult
	result.TaskID = task.ID
	result.StartedAt = startedAt

	select {
	case waitErr := <-doneCh:
		<-streamDone // ensure all stdout is consumed before reading the buffer
		result.FinishedAt = time.Now()
		result.Output = logWriter.String()
		result.Error = stderr.String()
		if waitErr == nil {
			// Check if the agent explicitly signalled failure via TASK_FAILED: prefix.
			// This allows skills/activities to surface auth errors, missing deps, or
			// other fatal conditions that should trigger a retry even though claude -p
			// itself exits 0.
			lines := strings.Split(result.Output, "\n")
			summary := parseRunSummary(lines)
			if strings.HasPrefix(strings.TrimSpace(summary.Result), "TASK_FAILED:") {
				result.Status = "failed"
				if result.Error == "" {
					result.Error = summary.Result
				}
			} else {
				result.Status = "success"
			}
		} else {
			result.Status = "failed"
			if result.Error == "" {
				result.Error = waitErr.Error()
			}
		}
		return result, nil

	case <-ctx.Done():
		// Context expired (timeout or cancellation). Send SIGTERM to process group.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}

		// Wait up to 10 seconds for graceful shutdown
		select {
		case <-doneCh:
			// Process exited after SIGTERM
		case <-time.After(10 * time.Second):
			// Force kill
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			<-doneCh // Wait for process to be reaped
		}

		<-streamDone // ensure all stdout is consumed
		result.FinishedAt = time.Now()
		result.Output = logWriter.String()
		result.Error = stderr.String()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Status = "timeout"
		} else {
			result.Status = "failed"
		}
		return result, nil
	}
}

// FakeExecutorCall records the arguments of a single call to FakeExecutor.Run.
type FakeExecutorCall struct {
	TaskID   string
	Prompt string
	CalledAt     time.Time
}

// FakeExecutor is a controllable executor for tests.
type FakeExecutor struct {
	mu      sync.Mutex
	results map[string]ExecutorResult // keyed by task ID
	errors  map[string]error
	calls   []FakeExecutorCall
	delay   time.Duration
}

// NewFakeExecutor creates a new FakeExecutor with empty configuration.
func NewFakeExecutor() *FakeExecutor {
	return &FakeExecutor{
		results: make(map[string]ExecutorResult),
		errors:  make(map[string]error),
	}
}

// SetResult configures what Run returns for a given task ID.
func (f *FakeExecutor) SetResult(taskID string, result ExecutorResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[taskID] = result
}

// SetError configures Run to return an error for a given task ID.
func (f *FakeExecutor) SetError(taskID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors[taskID] = err
}

// SetDelay adds an artificial delay to all Run calls.
func (f *FakeExecutor) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// Calls returns a copy of all recorded calls for assertions.
func (f *FakeExecutor) Calls() []FakeExecutorCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]FakeExecutorCall, len(f.calls))
	copy(cp, f.calls)
	return cp
}

// Run records the call, sleeps for the configured delay, and returns
// the configured result/error. If no result is configured for the
// task ID, it returns a default success result.
func (f *FakeExecutor) Run(ctx context.Context, task TaskConfig, prompt string) (ExecutorResult, error) {
	f.mu.Lock()
	call := FakeExecutorCall{
		TaskID: task.ID,
		Prompt:     prompt,
		CalledAt:   time.Now(),
	}
	f.calls = append(f.calls, call)
	delay := f.delay
	configuredResult, hasResult := f.results[task.ID]
	configuredErr, hasErr := f.errors[task.ID]
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ExecutorResult{
				TaskID: task.ID,
				Status:     "timeout",
				StartedAt:  call.CalledAt,
				FinishedAt: time.Now(),
			}, nil
		}
	}

	if hasErr {
		return ExecutorResult{}, configuredErr
	}

	if hasResult {
		return configuredResult, nil
	}

	// Default success result
	return ExecutorResult{
		TaskID: task.ID,
		Status:     "success",
		StartedAt:  call.CalledAt,
		FinishedAt: time.Now(),
		Output:     "fake output",
	}, nil
}
