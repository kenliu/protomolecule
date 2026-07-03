package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "protomolecule.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoadConfig_ValidFull(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
timezone: "America/New_York"
worker_slots: 3
max_retries: 5
retry_delay_seconds: 60
timeout_minutes: 20
workflows:
  - id: daily-pipeline
    schedule: "0 9 * * *"
    catchup: true
    tasks:
      - id: fetch-data
        prompt: "Fetch data from API"
      - id: process-data
        depends_on: fetch-data
        prompt: "Process the fetched data"
standalone:
  - id: check-slack
    schedule: "*/15 * * * *"
    catchup: false
    requires_network: true
    prompt: "Check Slack channels"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q, want %q", cfg.Timezone, "America/New_York")
	}
	if cfg.WorkerSlots != 3 {
		t.Errorf("WorkerSlots = %d, want 3", cfg.WorkerSlots)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", cfg.MaxRetries)
	}
	if cfg.RetryDelaySeconds != 60 {
		t.Errorf("RetryDelaySeconds = %d, want 60", cfg.RetryDelaySeconds)
	}
	if cfg.TimeoutMinutes != 20 {
		t.Errorf("TimeoutMinutes = %d, want 20", cfg.TimeoutMinutes)
	}
	if len(cfg.Workflows) != 1 {
		t.Fatalf("len(Workflows) = %d, want 1", len(cfg.Workflows))
	}
	if len(cfg.Workflows[0].Tasks) != 2 {
		t.Fatalf("len(Workflows[0].Tasks) = %d, want 2", len(cfg.Workflows[0].Tasks))
	}
	if cfg.Workflows[0].Tasks[1].DependsOn != "fetch-data" {
		t.Errorf("DependsOn = %q, want %q", cfg.Workflows[0].Tasks[1].DependsOn, "fetch-data")
	}
	if len(cfg.Standalone) != 1 {
		t.Fatalf("len(Standalone) = %d, want 1", len(cfg.Standalone))
	}
	if cfg.Location().String() != "America/New_York" {
		t.Errorf("Location = %q, want %q", cfg.Location().String(), "America/New_York")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: simple-task
    schedule: "0 9 * * *"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.WorkerSlots != 5 {
		t.Errorf("WorkerSlots = %d, want default 5", cfg.WorkerSlots)
	}
	if cfg.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want default 2", cfg.MaxRetries)
	}
	if cfg.RetryDelaySeconds != 30 {
		t.Errorf("RetryDelaySeconds = %d, want default 30", cfg.RetryDelaySeconds)
	}
	if cfg.TimeoutMinutes != 10 {
		t.Errorf("TimeoutMinutes = %d, want default 10", cfg.TimeoutMinutes)
	}
}

func TestLoadConfig_ValidCronExpression(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: every-5-min
    schedule: "*/5 * * * *"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Standalone[0].Schedule != "*/5 * * * *" {
		t.Errorf("Schedule = %q, want %q", cfg.Standalone[0].Schedule, "*/5 * * * *")
	}
}

func TestLoadConfig_InvalidCronExpression(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: bad-schedule
    schedule: "not a cron"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid cron expression, got nil")
	}
	if !strings.Contains(err.Error(), "cron parse error") {
		t.Errorf("error = %q, want to contain 'cron parse error'", err.Error())
	}
}

func TestLoadConfig_OnDemandSchedule(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: manual-task
    schedule: "on-demand"
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: on-demand should be accepted, got: %v", err)
	}
}

func TestLoadConfig_EmptyWorkflows(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows: []
standalone:
  - id: solo
    schedule: "0 9 * * *"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Workflows) != 0 {
		t.Errorf("len(Workflows) = %d, want 0", len(cfg.Workflows))
	}
}

func TestLoadConfig_DAGCycle_TwoNodes(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: cyclic
    schedule: "0 9 * * *"
    tasks:
      - id: a
        depends_on: b
      - id: b
        depends_on: a
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for DAG cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want to contain 'cycle'", err.Error())
	}
}

func TestLoadConfig_SelfReference(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: self-ref
    schedule: "0 9 * * *"
    tasks:
      - id: a
        depends_on: a
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for self-referencing task, got nil")
	}
	if !strings.Contains(err.Error(), "references itself") {
		t.Errorf("error = %q, want to contain 'references itself'", err.Error())
	}
}

func TestLoadConfig_MultiNodeCycle(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: triple-cycle
    schedule: "0 9 * * *"
    tasks:
      - id: a
        depends_on: c
      - id: b
        depends_on: a
      - id: c
        depends_on: b
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for multi-node cycle, got nil")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %q, want to contain 'cycle'", err.Error())
	}
}

func TestLoadConfig_DependsOnNonexistent(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: bad-dep
    schedule: "0 9 * * *"
    tasks:
      - id: a
        depends_on: nonexistent
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for nonexistent depends_on, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %q, want to contain 'nonexistent'", err.Error())
	}
}

func TestLoadConfig_TaskInBothWorkflowAndStandalone(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: wf1
    schedule: "0 9 * * *"
    tasks:
      - id: shared-task
standalone:
  - id: shared-task
    schedule: "0 10 * * *"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for task in both workflow and standalone, got nil")
	}
	if !strings.Contains(err.Error(), "both") {
		t.Errorf("error = %q, want to contain 'both'", err.Error())
	}
}

func TestLoadConfig_DuplicateIDAcrossWorkflows(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: wf1
    schedule: "0 9 * * *"
    tasks:
      - id: dup-task
  - id: wf2
    schedule: "0 10 * * *"
    tasks:
      - id: dup-task
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for duplicate task ID across workflows, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want to contain 'duplicate'", err.Error())
	}
}

func TestLoadConfig_ScheduleOnWorkflowTask(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: wf1
    schedule: "0 9 * * *"
    tasks:
      - id: task-with-schedule
        schedule: "0 10 * * *"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for schedule on workflow-nested task, got nil")
	}
	if !strings.Contains(err.Error(), "must not have a schedule") {
		t.Errorf("error = %q, want to contain 'must not have a schedule'", err.Error())
	}
}

func TestLoadConfig_MissingTimezone(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: task1
    schedule: "0 9 * * *"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Should default to local time
	if cfg.Location() != time.Local {
		t.Errorf("Location = %v, want time.Local", cfg.Location())
	}
}

func TestLoadConfig_InvalidTimezone(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
timezone: "Not/A/Real/Timezone"
standalone:
  - id: task1
    schedule: "0 9 * * *"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid timezone, got nil")
	}
	if !strings.Contains(err.Error(), "invalid timezone") {
		t.Errorf("error = %q, want to contain 'invalid timezone'", err.Error())
	}
}

func TestTaskConfig_RequiresNetworkDefault(t *testing.T) {
	a := TaskConfig{}
	if !a.IsNetworkRequired() {
		t.Error("IsNetworkRequired() = false for nil RequiresNetwork, want true")
	}
}

func TestTaskConfig_RequiresNetworkExplicit(t *testing.T) {
	f := false
	a := TaskConfig{RequiresNetwork: &f}
	if a.IsNetworkRequired() {
		t.Error("IsNetworkRequired() = true for explicit false, want false")
	}

	tr := true
	a2 := TaskConfig{RequiresNetwork: &tr}
	if !a2.IsNetworkRequired() {
		t.Error("IsNetworkRequired() = false for explicit true, want true")
	}
}

func TestTaskConfig_PerTaskOverrides(t *testing.T) {
	timeout := 30
	retries := 5
	delay := 120

	a := TaskConfig{
		TimeoutMinutes:    &timeout,
		MaxRetries:        &retries,
		RetryDelaySeconds: &delay,
	}

	if got := a.GetTimeoutMinutes(10); got != 30 {
		t.Errorf("GetTimeoutMinutes = %d, want 30", got)
	}
	if got := a.GetMaxRetries(2); got != 5 {
		t.Errorf("GetMaxRetries = %d, want 5", got)
	}
	if got := a.GetRetryDelaySeconds(30); got != 120 {
		t.Errorf("GetRetryDelaySeconds = %d, want 120", got)
	}

	// Without overrides, should return global default
	a2 := TaskConfig{}
	if got := a2.GetTimeoutMinutes(10); got != 10 {
		t.Errorf("GetTimeoutMinutes (no override) = %d, want 10", got)
	}
	if got := a2.GetMaxRetries(2); got != 2 {
		t.Errorf("GetMaxRetries (no override) = %d, want 2", got)
	}
	if got := a2.GetRetryDelaySeconds(30); got != 30 {
		t.Errorf("GetRetryDelaySeconds (no override) = %d, want 30", got)
	}
}

func TestConfigWatcher_ReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: task1
    schedule: "0 9 * * *"
`)

	var reloadCount atomic.Int32
	var lastErr atomic.Value
	var lastCfg atomic.Value

	watcher, err := NewConfigWatcher(path, func(cfg *Config, err error) {
		reloadCount.Add(1)
		if err != nil {
			lastErr.Store(err)
		} else {
			lastCfg.Store(cfg)
		}
	})
	if err != nil {
		t.Fatalf("NewConfigWatcher: %v", err)
	}
	defer watcher.Close()

	// Modify the file
	if err := os.WriteFile(path, []byte(`
standalone:
  - id: task2
    schedule: "0 10 * * *"
`), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	// Wait for debounce + processing
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reloadCount.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if reloadCount.Load() == 0 {
		t.Fatal("expected at least one reload callback, got none")
	}

	if v := lastErr.Load(); v != nil {
		t.Errorf("expected no error on reload, got: %v", v)
	}

	if v := lastCfg.Load(); v != nil {
		cfg := v.(*Config)
		if len(cfg.Standalone) != 1 || cfg.Standalone[0].ID != "task2" {
			t.Errorf("reloaded config has wrong standalone: %+v", cfg.Standalone)
		}
	}
}

func TestConfigWatcher_InvalidConfigCallsOnReloadWithError(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: task1
    schedule: "0 9 * * *"
`)

	var gotError atomic.Value

	watcher, err := NewConfigWatcher(path, func(cfg *Config, err error) {
		if err != nil {
			gotError.Store(err)
		}
	})
	if err != nil {
		t.Fatalf("NewConfigWatcher: %v", err)
	}
	defer watcher.Close()

	// Write invalid config
	if err := os.WriteFile(path, []byte(`
standalone:
  - id: bad
    schedule: "not valid cron"
`), 0644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if gotError.Load() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if gotError.Load() == nil {
		t.Fatal("expected error callback for invalid config, got none")
	}
}

func TestLoadConfig_ZeroTimeoutWorkflowTask(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: wf1
    schedule: "0 9 * * *"
    tasks:
      - id: task-zero-timeout
        timeout_minutes: 0
        prompt: "Do something"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for timeout_minutes: 0 in workflow task, got nil")
	}
	if !strings.Contains(err.Error(), "timeout_minutes must be > 0") {
		t.Errorf("error = %q, want to contain 'timeout_minutes must be > 0'", err.Error())
	}
}

func TestLoadConfig_ZeroTimeoutStandaloneTask(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: task-zero-timeout
    schedule: "0 9 * * *"
    timeout_minutes: 0
    prompt: "Do something"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for timeout_minutes: 0 in standalone task, got nil")
	}
	if !strings.Contains(err.Error(), "timeout_minutes must be > 0") {
		t.Errorf("error = %q, want to contain 'timeout_minutes must be > 0'", err.Error())
	}
}

func TestLoadConfig_ValidPerTaskTimeout(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
standalone:
  - id: long-task
    schedule: "0 9 * * *"
    timeout_minutes: 45
    prompt: "Do something slow"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Standalone[0].GetTimeoutMinutes(10); got != 45 {
		t.Errorf("GetTimeoutMinutes = %d, want 45", got)
	}
}

func TestLoadConfig_GlobalMaxRetriesExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
timezone: "America/New_York"
max_retries: 11
standalone:
  - id: task1
    schedule: "0 9 * * *"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for global max_retries > 10, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds hard limit") {
		t.Errorf("error = %q, want to contain 'exceeds hard limit'", err.Error())
	}
}

func TestLoadConfig_PerTaskMaxRetriesExceedsLimit_Workflow(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
timezone: "America/New_York"
workflows:
  - id: wf1
    schedule: "0 9 * * *"
    tasks:
      - id: task1
        max_retries: 11
        prompt: "Do something"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for per-task max_retries > 10 in workflow, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds hard limit") {
		t.Errorf("error = %q, want to contain 'exceeds hard limit'", err.Error())
	}
}

func TestLoadConfig_PerTaskMaxRetriesExceedsLimit_Standalone(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
timezone: "America/New_York"
standalone:
  - id: task1
    schedule: "0 9 * * *"
    max_retries: 11
    prompt: "Do something"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for per-task max_retries > 10 in standalone, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds hard limit") {
		t.Errorf("error = %q, want to contain 'exceeds hard limit'", err.Error())
	}
}

func TestLoadConfig_MaxRetriesAtLimit(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
timezone: "America/New_York"
max_retries: 10
standalone:
  - id: task1
    schedule: "0 9 * * *"
    max_retries: 10
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: max_retries: 10 should be accepted, got: %v", err)
	}
	if cfg.MaxRetries != 10 {
		t.Errorf("MaxRetries = %d, want 10", cfg.MaxRetries)
	}
}

func TestLoadConfig_InvalidWorkflowCron(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, `
workflows:
  - id: bad-wf
    schedule: "invalid cron here"
    tasks:
      - id: task1
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid workflow cron, got nil")
	}
	if !strings.Contains(err.Error(), "cron parse error") {
		t.Errorf("error = %q, want to contain 'cron parse error'", err.Error())
	}
}
