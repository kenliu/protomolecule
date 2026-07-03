package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSocketPath returns a short socket path that fits within the Unix socket
// path length limit (104 bytes on macOS). t.TempDir() paths are often too long.
func testSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "proto-")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// ============================================================================
// FormatStatus Tests
// ============================================================================

func TestFormatStatus_Empty(t *testing.T) {
	r := &StatusResponse{
		Uptime:      "4h 23m",
		TickCount:   42,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
	}

	out := FormatStatus(r)

	if !strings.Contains(out, "Protomolecule") {
		t.Error("expected header with 'Protomolecule'")
	}
	if !strings.Contains(out, "4h 23m") {
		t.Error("expected uptime in output")
	}
	// Empty sections should be omitted
	if strings.Contains(out, "ACTIVE") {
		t.Error("ACTIVE section should be omitted when empty")
	}
	if strings.Contains(out, "QUEUED") {
		t.Error("QUEUED section should be omitted when empty")
	}
	if strings.Contains(out, "RECENT") {
		t.Error("RECENT section should be omitted when empty")
	}
	if strings.Contains(out, "UPCOMING") {
		t.Error("UPCOMING section should be omitted when empty")
	}
}

func TestFormatStatus_Paused(t *testing.T) {
	r := &StatusResponse{
		Uptime: "2h 10m",
		Paused: true,
	}
	out := FormatStatus(r)

	if !strings.Contains(out, "[PAUSED]") {
		t.Errorf("expected [PAUSED] in output when paused, got:\n%s", out)
	}
}

func TestFormatStatus_NotPaused(t *testing.T) {
	r := &StatusResponse{
		Uptime: "2h 10m",
		Paused: false,
	}
	out := FormatStatus(r)

	if strings.Contains(out, "[PAUSED]") {
		t.Errorf("expected no [PAUSED] in output when not paused, got:\n%s", out)
	}
}

func TestFormatStatus_WithActive(t *testing.T) {
	r := &StatusResponse{
		Uptime:      "1h 5m",
		TickCount:   10,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
		Active: []ActiveJobInfo{
			{TaskID: "check-jira", StartedAt: "09:14:22", Elapsed: "0:42", Attempt: 1, MaxAttempt: 3},
		},
	}

	out := FormatStatus(r)

	if !strings.Contains(out, "ACTIVE") {
		t.Error("expected ACTIVE section")
	}
	if !strings.Contains(out, "check-jira") {
		t.Error("expected task ID in ACTIVE section")
	}
	if !strings.Contains(out, "running") {
		t.Error("expected 'running' status")
	}
	if !strings.Contains(out, "attempt 1/3") {
		t.Error("expected attempt info")
	}
}

func TestFormatStatus_WithQueued(t *testing.T) {
	r := &StatusResponse{
		Uptime:      "2h",
		TickCount:   20,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
		Queued: []QueuedJobInfo{
			{TaskID: "slack-review", Reason: "waiting on: project-manager"},
		},
	}

	out := FormatStatus(r)

	if !strings.Contains(out, "QUEUED") {
		t.Error("expected QUEUED section")
	}
	if !strings.Contains(out, "slack-review") {
		t.Error("expected task ID in QUEUED section")
	}
	if !strings.Contains(out, "waiting on: project-manager") {
		t.Error("expected reason in QUEUED section")
	}
}

func TestFormatStatus_WithRecent(t *testing.T) {
	r := &StatusResponse{
		Uptime:      "3h",
		TickCount:   30,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
		Recent: []RecentRunInfo{
			{TaskID: "slack-check", Status: "success", RunAt: "09:15:01", Duration: "0:04"},
			{TaskID: "weekly-report", Status: "failed", RunAt: "09:00:00", Duration: "4:17", Error: "exhausted 3/3 attempts"},
		},
	}

	out := FormatStatus(r)

	if !strings.Contains(out, "RECENT") {
		t.Error("expected RECENT section")
	}
	if !strings.Contains(out, "slack-check") {
		t.Error("expected slack-check in RECENT")
	}
	if !strings.Contains(out, "success") {
		t.Error("expected success status")
	}
	if !strings.Contains(out, "weekly-report") {
		t.Error("expected weekly-report in RECENT")
	}
	if !strings.Contains(out, "exhausted 3/3 attempts") {
		t.Error("expected error info in RECENT")
	}
}

func TestFormatStatus_WithUpcoming(t *testing.T) {
	r := &StatusResponse{
		Uptime:      "5m",
		TickCount:   5,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
		Upcoming: []UpcomingInfo{
			{TaskID: "slack-check", NextRun: "2026-03-09T10:15:00Z", InDuration: "15m"},
			{TaskID: "weekly-report", NextRun: "2026-03-13T16:00:00Z", InDuration: "4d 6h"},
		},
	}

	out := FormatStatus(r)

	if !strings.Contains(out, "UPCOMING") {
		t.Error("expected UPCOMING section")
	}
	if !strings.Contains(out, "slack-check") {
		t.Error("expected slack-check in UPCOMING")
	}
	if !strings.Contains(out, "in 15m") {
		t.Error("expected 'in 15m'")
	}
	if !strings.Contains(out, "in 4d 6h") {
		t.Error("expected 'in 4d 6h'")
	}
}

func TestFormatStatus_AllSections(t *testing.T) {
	r := &StatusResponse{
		Uptime:      "4h 23m",
		TickCount:   263,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
		Active: []ActiveJobInfo{
			{TaskID: "check-jira-tickets", StartedAt: "09:14:22", Elapsed: "0:42", Attempt: 1, MaxAttempt: 3},
		},
		Queued: []QueuedJobInfo{
			{TaskID: "slack-pm-review", Reason: "waiting on: project-manager-daily"},
		},
		Upcoming: []UpcomingInfo{
			{TaskID: "slack-check", NextRun: "2026-03-09T10:03:00Z", InDuration: "3m"},
		},
		Recent: []RecentRunInfo{
			{TaskID: "slack-check", Status: "success", RunAt: "09:15:01", Duration: "0:04"},
		},
	}

	out := FormatStatus(r)

	// Verify section ordering: ACTIVE before QUEUED before UPCOMING before RECENT
	activeIdx := strings.Index(out, "ACTIVE")
	queuedIdx := strings.Index(out, "QUEUED")
	upcomingIdx := strings.Index(out, "UPCOMING")
	recentIdx := strings.Index(out, "RECENT")

	if activeIdx >= queuedIdx {
		t.Error("ACTIVE should come before QUEUED")
	}
	if queuedIdx >= upcomingIdx {
		t.Error("QUEUED should come before UPCOMING")
	}
	if upcomingIdx >= recentIdx {
		t.Error("UPCOMING should come before RECENT")
	}
}

// ============================================================================
// formatDuration Tests
// ============================================================================

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{1 * time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{1*time.Hour + 30*time.Minute, "1h 30m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d 1h"},
		{48 * time.Hour, "2d"},
		{50*time.Hour + 30*time.Minute, "2d 2h"},
		{-1 * time.Minute, "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0:00"},
		{42 * time.Second, "0:42"},
		{1*time.Minute + 5*time.Second, "1:05"},
		{10*time.Minute + 30*time.Second, "10:30"},
	}

	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			got := formatElapsed(tt.d)
			if got != tt.want {
				t.Errorf("formatElapsed(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

// ============================================================================
// StatusServer Start/Stop Lifecycle Tests
// ============================================================================

func TestStatusServer_StartStop(t *testing.T) {
	socketPath := testSocketPath(t, "t.sock")

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = clock.Now()
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	ss := NewStatusServer(socketPath, sched, state, pool, clock)

	if err := ss.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify socket file exists
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("socket file should exist after Start")
	}

	ss.Stop()

	// Verify socket file is cleaned up
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatal("socket file should be removed after Stop")
	}
}

func TestStatusServer_StaleSocketRecovery(t *testing.T) {
	socketPath := testSocketPath(t, "s.sock")

	// Create a stale socket file (just a regular file, not a real socket)
	if err := os.WriteFile(socketPath, []byte("stale"), 0600); err != nil {
		t.Fatalf("creating stale file: %v", err)
	}

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = clock.Now()
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	ss := NewStatusServer(socketPath, sched, state, pool, clock)

	// Should recover from stale socket
	if err := ss.Start(); err != nil {
		t.Fatalf("Start should recover from stale socket: %v", err)
	}
	defer ss.Stop()
}

func TestStatusServer_AnotherInstanceRunning(t *testing.T) {
	socketPath := testSocketPath(t, "a.sock")

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = clock.Now()
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	// Start first instance
	ss1 := NewStatusServer(socketPath, sched, state, pool, clock)
	if err := ss1.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer ss1.Stop()

	// Second instance should fail
	ss2 := NewStatusServer(socketPath, sched, state, pool, clock)
	err := ss2.Start()
	if err == nil {
		ss2.Stop()
		t.Fatal("second Start should fail when another instance is running")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want to contain 'already running'", err.Error())
	}
}

// ============================================================================
// StatusServer Client/Server Integration Tests
// ============================================================================

func TestStatusServer_FetchStatus(t *testing.T) {
	socketPath := testSocketPath(t, "f.sock")

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "task-a", Schedule: "*/15 * * * *", Catchup: true},
	}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = now.Add(-1 * time.Hour)
	sched.tickCount = 42
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	// Add some state
	runTime := now.Add(-5 * time.Minute)
	state.UpdateTask("task-a", func(as *TaskState) {
		as.LastRun = &runTime
		as.LastRunStatus = "success"
		as.LastSuccess = &runTime
	})

	ss := NewStatusServer(socketPath, sched, state, pool, clock)
	if err := ss.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ss.Stop()

	client := NewStatusClient(socketPath)
	resp, err := client.FetchStatus()
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}

	if resp.TickCount != 42 {
		t.Errorf("TickCount = %d, want 42", resp.TickCount)
	}
	if !resp.ConfigValid {
		t.Error("ConfigValid should be true")
	}
	if resp.Uptime != "1h" {
		t.Errorf("Uptime = %q, want '1h'", resp.Uptime)
	}

	// Should have recent entry for task-a
	if len(resp.Recent) != 1 {
		t.Fatalf("expected 1 recent entry, got %d", len(resp.Recent))
	}
	if resp.Recent[0].TaskID != "task-a" {
		t.Errorf("recent[0].TaskID = %q, want task-a", resp.Recent[0].TaskID)
	}

	// Should have upcoming entry for task-a
	if len(resp.Upcoming) < 1 {
		t.Fatal("expected at least 1 upcoming entry")
	}
	foundUpcoming := false
	for _, u := range resp.Upcoming {
		if u.TaskID == "task-a" {
			foundUpcoming = true
		}
	}
	if !foundUpcoming {
		t.Error("expected task-a in upcoming")
	}
}

func TestStatusServer_InvalidRequest(t *testing.T) {
	socketPath := testSocketPath(t, "i.sock")

	clock := &FakeClock{T: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = clock.Now()
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	ss := NewStatusServer(socketPath, sched, state, pool, clock)
	if err := ss.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ss.Stop()

	// Send garbage data
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("not json\n"))
	// Server should handle gracefully without crashing
	// Just verify the server is still accepting connections
	conn2, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("second dial failed (server crashed?): %v", err)
	}
	conn2.Close()
}

// ============================================================================
// buildStatusResponse Tests
// ============================================================================

func TestBuildStatusResponse_QueuedFromDependencies(t *testing.T) {
	socketPath := testSocketPath(t, "q.sock")

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	cfg := newTestConfig("UTC")
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "pipeline",
			Schedule: "0 9 * * *",
			Tasks: []TaskConfig{
				{ID: "step1", Prompt: "step 1"},
				{ID: "step2", DependsOn: "step1", Prompt: "step 2"},
			},
		},
	}

	// Use a slow executor so step1 stays active
	executor := NewFakeExecutor()
	executor.SetDelay(10 * time.Second)
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = now.Add(-1 * time.Hour)

	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)

	// Submit step1 so it appears as active
	pool.Submit(Job{
		Task:    cfg.Workflows[0].Tasks[0],
		CycleID:     "pipeline:test",
		Attempt:     1,
		MaxAttempts: 3,
		TimeoutMin:  10,
	})

	// Give the goroutine a moment to start
	time.Sleep(50 * time.Millisecond)

	ss := NewStatusServer(socketPath, sched, state, pool, clock)
	resp := ss.buildStatusResponse()

	pool.Shutdown()

	// step1 should be active
	if len(resp.Active) != 1 {
		t.Fatalf("expected 1 active job, got %d", len(resp.Active))
	}
	if resp.Active[0].TaskID != "step1" {
		t.Errorf("active[0].TaskID = %q, want step1", resp.Active[0].TaskID)
	}

	// step2 should be queued (depends on step1 which is active)
	if len(resp.Queued) != 1 {
		t.Fatalf("expected 1 queued job, got %d", len(resp.Queued))
	}
	if resp.Queued[0].TaskID != "step2" {
		t.Errorf("queued[0].TaskID = %q, want step2", resp.Queued[0].TaskID)
	}
	if !strings.Contains(resp.Queued[0].Reason, "step1") {
		t.Errorf("queued reason = %q, want to contain 'step1'", resp.Queued[0].Reason)
	}
}

// ============================================================================
// GenerateVisualization Tests
// ============================================================================

func TestGenerateVisualization_WorkflowWithDeps(t *testing.T) {
	cfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "daily-pipeline",
				Schedule: "0 8 * * *",
				Tasks: []TaskConfig{
					{ID: "project-manager-daily"},
					{ID: "slack-project-manager-review", DependsOn: "project-manager-daily"},
				},
			},
		},
		Standalone: []TaskConfig{
			{ID: "check-jira-tickets", Schedule: "*/15 * * * *"},
			{ID: "weekly-exec-report", Schedule: "0 16 * * 5"},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 7, 10, 32, 1, 0, time.UTC)}
	state := NewStateStore(filepath.Join(t.TempDir(), "state.json"))

	// Set up some state
	now := clock.Now()
	state.UpdateTask("project-manager-daily", func(as *TaskState) {
		as.LastRun = &now
		as.LastRunStatus = "success"
	})
	state.UpdateTask("check-jira-tickets", func(as *TaskState) {
		as.LastRun = &now
		as.LastRunStatus = "success"
	})
	state.UpdateTask("weekly-exec-report", func(as *TaskState) {
		as.LastRun = &now
		as.LastRunStatus = "failed"
	})

	output := GenerateVisualization(cfg, state, clock, nil, "")

	// Verify structure
	if !strings.Contains(output, "# Protomolecule Workflows") {
		t.Error("expected markdown header")
	}
	if !strings.Contains(output, "```mermaid") {
		t.Error("expected mermaid code block")
	}
	if !strings.Contains(output, "flowchart TD") {
		t.Error("expected flowchart directive")
	}
	if !strings.Contains(output, "daily-pipeline") {
		t.Error("expected workflow subgraph")
	}
	if !strings.Contains(output, "project-manager-daily --> slack-project-manager-review") {
		t.Error("expected dependency arrow")
	}
	if !strings.Contains(output, "Standalone") {
		t.Error("expected standalone subgraph")
	}

	// Verify class assignments
	if !strings.Contains(output, "class project-manager-daily success") {
		t.Error("expected project-manager-daily to have success class")
	}
	if !strings.Contains(output, "class weekly-exec-report failed") {
		t.Error("expected weekly-exec-report to have failed class")
	}
	if !strings.Contains(output, "class slack-project-manager-review never") {
		t.Error("expected slack-project-manager-review to have never class")
	}

	// Verify class definitions
	if !strings.Contains(output, "classDef success fill:#27AE60") {
		t.Error("expected success class definition")
	}
	if !strings.Contains(output, "classDef failed fill:#E74C3C") {
		t.Error("expected failed class definition")
	}
}

func TestGenerateVisualization_WorkflowFilter(t *testing.T) {
	cfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "pipeline-a",
				Schedule: "0 8 * * *",
				Tasks: []TaskConfig{
					{ID: "step-a1"},
				},
			},
			{
				ID:       "pipeline-b",
				Schedule: "0 9 * * *",
				Tasks: []TaskConfig{
					{ID: "step-b1"},
				},
			},
		},
		Standalone: []TaskConfig{
			{ID: "standalone-task", Schedule: "*/15 * * * *"},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)}
	state := NewStateStore(filepath.Join(t.TempDir(), "state.json"))

	output := GenerateVisualization(cfg, state, clock, nil, "pipeline-a")

	if !strings.Contains(output, "pipeline-a") {
		t.Error("expected pipeline-a in filtered output")
	}
	if strings.Contains(output, "pipeline-b") {
		t.Error("pipeline-b should be excluded by filter")
	}
	if strings.Contains(output, "Standalone") {
		t.Error("Standalone section should be excluded when filtering")
	}
}

func TestGenerateVisualization_OnDemandClass(t *testing.T) {
	cfg := &Config{
		Standalone: []TaskConfig{
			{ID: "manual-task", Schedule: "on-demand"},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)}
	state := NewStateStore(filepath.Join(t.TempDir(), "state.json"))

	output := GenerateVisualization(cfg, state, clock, nil, "")

	if !strings.Contains(output, "class manual-task ondemand") {
		t.Error("expected on-demand task to have ondemand class")
	}
}

// ============================================================================
// WriteVisualization Tests
// ============================================================================

func TestWriteVisualization(t *testing.T) {
	cfg := &Config{
		Standalone: []TaskConfig{
			{ID: "test-task", Schedule: "*/15 * * * *"},
		},
	}

	clock := &FakeClock{T: time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)}
	state := NewStateStore(filepath.Join(t.TempDir(), "state.json"))

	outputPath := filepath.Join(t.TempDir(), "output", "workflows", "protomolecule.md")

	err := WriteVisualization(cfg, state, clock, nil, "", outputPath)
	if err != nil {
		t.Fatalf("WriteVisualization: %v", err)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}

	if !strings.Contains(string(data), "# Protomolecule Workflows") {
		t.Error("expected markdown header in written file")
	}
}

// ============================================================================
// StatusResponse JSON Serialization
// ============================================================================

func TestStatusResponse_JSONRoundTrip(t *testing.T) {
	original := StatusResponse{
		Uptime:      "4h 23m",
		TickCount:   263,
		Timestamp:   "2026-03-09T10:00:00Z",
		ConfigValid: true,
		Active: []ActiveJobInfo{
			{TaskID: "task1", StartedAt: "09:14:22", Elapsed: "0:42", Attempt: 1, MaxAttempt: 3},
		},
		Queued: []QueuedJobInfo{
			{TaskID: "task2", Reason: "waiting on: task1"},
		},
		Recent: []RecentRunInfo{
			{TaskID: "task3", Status: "success", RunAt: "09:15:01", Duration: "0:04"},
		},
		Upcoming: []UpcomingInfo{
			{TaskID: "task4", NextRun: "2026-03-09T10:15:00Z", InDuration: "15m"},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded StatusResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.TickCount != original.TickCount {
		t.Errorf("TickCount = %d, want %d", decoded.TickCount, original.TickCount)
	}
	if len(decoded.Active) != 1 {
		t.Fatalf("Active length = %d, want 1", len(decoded.Active))
	}
	if decoded.Active[0].TaskID != "task1" {
		t.Errorf("Active[0].TaskID = %q, want task1", decoded.Active[0].TaskID)
	}
	if len(decoded.Queued) != 1 || decoded.Queued[0].TaskID != "task2" {
		t.Error("Queued did not round-trip correctly")
	}
	if len(decoded.Recent) != 1 || decoded.Recent[0].TaskID != "task3" {
		t.Error("Recent did not round-trip correctly")
	}
	if len(decoded.Upcoming) != 1 || decoded.Upcoming[0].TaskID != "task4" {
		t.Error("Upcoming did not round-trip correctly")
	}
}

// ============================================================================
// RecentRunInfo error field omitempty
// ============================================================================

func TestRecentRunInfo_ErrorOmitEmpty(t *testing.T) {
	info := RecentRunInfo{
		TaskID: "test",
		Status:     "success",
		RunAt:      "09:00:00",
	}

	data, _ := json.Marshal(info)
	if strings.Contains(string(data), "error") {
		t.Error("error field should be omitted when empty")
	}

	info.Error = "something went wrong"
	data, _ = json.Marshal(info)
	if !strings.Contains(string(data), "error") {
		t.Error("error field should be present when non-empty")
	}
}

// ============================================================================
// StatusServer RunTask (client-side) Tests
// ============================================================================

func newTestStatusServer(t *testing.T, socketName string, cfg *Config) (*StatusServer, *Scheduler, *StateStore, *WorkerPool) {
	t.Helper()
	socketPath := testSocketPath(t, socketName)
	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = now
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	t.Cleanup(func() { pool.Shutdown() })
	ss := NewStatusServer(socketPath, sched, state, pool, clock)
	if err := ss.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { ss.Stop() })
	return ss, sched, state, pool
}

func TestStatusServer_RunTask_Success(t *testing.T) {
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "my-task", Schedule: "0 9 * * *", Prompt: "do the thing"},
	}
	ss, _, _, _ := newTestStatusServer(t, "rt.sock", cfg)

	client := NewStatusClient(ss.socketPath)
	if err := client.RunTask("my-task"); err != nil {
		t.Errorf("RunTask should succeed for known task, got: %v", err)
	}
}

func TestStatusServer_RunTask_EmptyTaskID(t *testing.T) {
	cfg := newTestConfig("UTC")
	ss, _, _, _ := newTestStatusServer(t, "rte.sock", cfg)

	client := NewStatusClient(ss.socketPath)
	err := client.RunTask("")
	if err == nil {
		t.Fatal("expected error for empty task_id, got nil")
	}
	if !strings.Contains(err.Error(), "task_id is required") {
		t.Errorf("error = %q, want to contain 'task_id is required'", err.Error())
	}
}

func TestStatusServer_RunTask_UnknownTaskID(t *testing.T) {
	cfg := newTestConfig("UTC")
	ss, _, _, _ := newTestStatusServer(t, "rtu.sock", cfg)

	client := NewStatusClient(ss.socketPath)
	err := client.RunTask("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown task, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error = %q, expected to mention task ID", err.Error())
	}
}

func TestStatusServer_RunTask_ServerNotRunning(t *testing.T) {
	client := NewStatusClient("/tmp/protomolecule-nonexistent-socket.sock")
	err := client.RunTask("any-task")
	if err == nil {
		t.Fatal("expected error when server is not running")
	}
}

// ============================================================================
// TrackConfigReload + buildStatusResponse Tests
// ============================================================================

func TestStatusServer_TrackConfigReload_ShowsInResponse(t *testing.T) {
	socketPath := testSocketPath(t, "tr.sock")
	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	cfg := newTestConfig("UTC")
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = now
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	ss := NewStatusServer(socketPath, sched, state, pool, clock)

	// Before any reload tracking, fields should be empty
	resp := ss.buildStatusResponse()
	if resp.ConfigLastReload != "" {
		t.Errorf("ConfigLastReload should be empty before tracking, got %q", resp.ConfigLastReload)
	}
	if resp.ConfigReloadOK != nil {
		t.Errorf("ConfigReloadOK should be nil before tracking")
	}

	// After a successful reload
	ss.TrackConfigReload(true)
	resp = ss.buildStatusResponse()
	if resp.ConfigLastReload == "" {
		t.Error("ConfigLastReload should be non-empty after TrackConfigReload")
	}
	if resp.ConfigReloadOK == nil {
		t.Fatal("ConfigReloadOK should not be nil after TrackConfigReload")
	}
	if !*resp.ConfigReloadOK {
		t.Error("ConfigReloadOK should be true after successful reload")
	}

	// After a failed reload
	ss.TrackConfigReload(false)
	resp = ss.buildStatusResponse()
	if resp.ConfigReloadOK == nil {
		t.Fatal("ConfigReloadOK should not be nil")
	}
	if *resp.ConfigReloadOK {
		t.Error("ConfigReloadOK should be false after failed reload")
	}
}

// ============================================================================
// FormatStatus — missing branches
// ============================================================================

func TestFormatStatus_ConfigReloadFailed(t *testing.T) {
	failed := false
	r := &StatusResponse{
		Uptime:           "2h",
		TickCount:        10,
		Timestamp:        "2026-03-09T10:00:00Z",
		ConfigValid:      true,
		ConfigLastReload: "2026-03-09T09:55:00Z",
		ConfigReloadOK:   &failed,
	}
	out := FormatStatus(r)

	if !strings.Contains(out, "FAILED") {
		t.Errorf("expected 'FAILED' in output when ConfigReloadOK=false, got: %s", out)
	}
	if !strings.Contains(out, "Config last reloaded") {
		t.Errorf("expected reload header in output, got: %s", out)
	}
}

func TestFormatStatus_ConfigReloadOK(t *testing.T) {
	ok := true
	r := &StatusResponse{
		Uptime:           "1h",
		TickCount:        5,
		Timestamp:        "2026-03-09T10:00:00Z",
		ConfigValid:      true,
		ConfigLastReload: "2026-03-09T09:55:00Z",
		ConfigReloadOK:   &ok,
	}
	out := FormatStatus(r)

	if !strings.Contains(out, "ok") {
		t.Errorf("expected 'ok' in output when ConfigReloadOK=true, got: %s", out)
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("should not contain 'FAILED' when ConfigReloadOK=true, got: %s", out)
	}
}

// ============================================================================
// buildStatusResponse — on-demand tasks
// ============================================================================

func TestBuildStatusResponse_OnDemandTasks(t *testing.T) {
	socketPath := testSocketPath(t, "od.sock")
	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	clock := &FakeClock{T: now}
	cfg := newTestConfig("UTC")
	cfg.Standalone = []TaskConfig{
		{ID: "manual-run", Schedule: "on-demand", Prompt: "manual task"},
		{ID: "scheduled-task", Schedule: "*/15 * * * *", Prompt: "scheduled"},
	}
	cfg.Workflows = []WorkflowConfig{
		{
			ID:       "on-demand-pipeline",
			Schedule: "on-demand",
			Tasks:    []TaskConfig{{ID: "pipeline-step", Prompt: "step"}},
		},
	}
	executor := NewFakeExecutor()
	network := &FakeNetworkChecker{Available: true}
	sched, state := newTestScheduler(cfg, clock, network, executor)
	sched.config = cfg
	sched.startedAt = now
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	defer pool.Shutdown()

	ss := NewStatusServer(socketPath, sched, state, pool, clock)
	resp := ss.buildStatusResponse()

	// Only on-demand tasks should appear in OnDemand
	onDemandSet := make(map[string]bool)
	for _, id := range resp.OnDemand {
		onDemandSet[id] = true
	}

	if !onDemandSet["manual-run"] {
		t.Error("expected manual-run in OnDemand")
	}
	if !onDemandSet["pipeline-step"] {
		t.Error("expected pipeline-step in OnDemand")
	}
	if onDemandSet["scheduled-task"] {
		t.Error("scheduled-task should NOT be in OnDemand (has a cron schedule)")
	}
}
