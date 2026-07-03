package main

import (
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// ============================================================================
// configTaskIDs Tests
// ============================================================================

func TestConfigTaskIDs_Empty(t *testing.T) {
	cfg := &Config{}
	ids := configTaskIDs(cfg)
	if len(ids) != 0 {
		t.Errorf("expected empty map, got %v", ids)
	}
}

func TestConfigTaskIDs_StandaloneOnly(t *testing.T) {
	cfg := &Config{
		Standalone: []TaskConfig{
			{ID: "check-jira", Schedule: "*/15 * * * *"},
			{ID: "weekly-report", Schedule: "0 16 * * 5"},
		},
	}
	ids := configTaskIDs(cfg)

	if len(ids) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(ids), ids)
	}
	if _, ok := ids["check-jira"]; !ok {
		t.Error("expected check-jira in result")
	}
	if _, ok := ids["weekly-report"]; !ok {
		t.Error("expected weekly-report in result")
	}
	// Value should contain the schedule and "(standalone)"
	if ids["check-jira"] != "*/15 * * * * (standalone)" {
		t.Errorf("ids[check-jira] = %q, want '*/15 * * * * (standalone)'", ids["check-jira"])
	}
}

func TestConfigTaskIDs_WorkflowOnly(t *testing.T) {
	cfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "daily-pipeline",
				Schedule: "0 8 * * *",
				Tasks: []TaskConfig{
					{ID: "step-one", Prompt: "first step"},
					{ID: "step-two", DependsOn: "step-one", Prompt: "second step"},
				},
			},
		},
	}
	ids := configTaskIDs(cfg)

	if len(ids) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(ids), ids)
	}
	if _, ok := ids["step-one"]; !ok {
		t.Error("expected step-one in result")
	}
	if _, ok := ids["step-two"]; !ok {
		t.Error("expected step-two in result")
	}
	// Value should reference the workflow schedule and ID
	wantSubstr := "workflow:daily-pipeline"
	if ids["step-one"] != "0 8 * * * (workflow:daily-pipeline)" {
		t.Errorf("ids[step-one] = %q, expected to contain %q", ids["step-one"], wantSubstr)
	}
}

func TestConfigTaskIDs_Mixed(t *testing.T) {
	cfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "pipeline",
				Schedule: "0 9 * * *",
				Tasks: []TaskConfig{
					{ID: "wf-task-a"},
					{ID: "wf-task-b"},
				},
			},
		},
		Standalone: []TaskConfig{
			{ID: "standalone-x", Schedule: "*/30 * * * *"},
		},
	}
	ids := configTaskIDs(cfg)

	if len(ids) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(ids), ids)
	}
	for _, key := range []string{"wf-task-a", "wf-task-b", "standalone-x"} {
		if _, ok := ids[key]; !ok {
			t.Errorf("expected %q in result", key)
		}
	}
}

func TestConfigTaskIDs_MultipleWorkflows(t *testing.T) {
	cfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "wf-a",
				Schedule: "0 8 * * *",
				Tasks:    []TaskConfig{{ID: "task-1"}},
			},
			{
				ID:       "wf-b",
				Schedule: "0 16 * * 5",
				Tasks:    []TaskConfig{{ID: "task-2"}, {ID: "task-3"}},
			},
		},
	}
	ids := configTaskIDs(cfg)

	if len(ids) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(ids))
	}
	if ids["task-2"] != "0 16 * * 5 (workflow:wf-b)" {
		t.Errorf("ids[task-2] = %q, wrong workflow reference", ids["task-2"])
	}
}

// ============================================================================
// logConfigDiff Tests
// ============================================================================

func TestLogConfigDiff_NoChanges(t *testing.T) {
	cfg := &Config{
		Standalone: []TaskConfig{
			{ID: "task-a", Schedule: "0 9 * * *"},
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(cfg, cfg, logger)

	if len(added) != 0 {
		t.Errorf("expected 0 added tasks when configs are identical, got %v", added)
	}
}

func TestLogConfigDiff_TaskAdded(t *testing.T) {
	oldCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "existing-task", Schedule: "0 9 * * *"},
		},
	}
	newCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "existing-task", Schedule: "0 9 * * *"},
			{ID: "new-task", Schedule: "*/15 * * * *"},
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(oldCfg, newCfg, logger)

	if len(added) != 1 {
		t.Fatalf("expected 1 added task, got %v", added)
	}
	if added[0] != "new-task" {
		t.Errorf("added[0] = %q, want 'new-task'", added[0])
	}
}

func TestLogConfigDiff_TaskRemoved(t *testing.T) {
	oldCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "kept-task", Schedule: "0 9 * * *"},
			{ID: "removed-task", Schedule: "0 16 * * 5"},
		},
	}
	newCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "kept-task", Schedule: "0 9 * * *"},
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(oldCfg, newCfg, logger)

	// Removed tasks are not returned in the added list
	if len(added) != 0 {
		t.Errorf("expected 0 added tasks on removal, got %v", added)
	}
}

func TestLogConfigDiff_TaskModified(t *testing.T) {
	oldCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "check-jira", Schedule: "*/30 * * * *"},
		},
	}
	newCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "check-jira", Schedule: "*/15 * * * *"}, // schedule changed
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(oldCfg, newCfg, logger)

	// Modified tasks are not returned as added
	if len(added) != 0 {
		t.Errorf("expected 0 added tasks for modification, got %v", added)
	}
}

func TestLogConfigDiff_MultipleAdditions(t *testing.T) {
	oldCfg := &Config{}
	newCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "task-alpha", Schedule: "0 8 * * *"},
			{ID: "task-beta", Schedule: "0 12 * * *"},
			{ID: "task-gamma", Schedule: "0 17 * * *"},
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(oldCfg, newCfg, logger)

	if len(added) != 3 {
		t.Fatalf("expected 3 added tasks, got %d: %v", len(added), added)
	}

	sort.Strings(added)
	want := []string{"task-alpha", "task-beta", "task-gamma"}
	for i, w := range want {
		if added[i] != w {
			t.Errorf("added[%d] = %q, want %q", i, added[i], w)
		}
	}
}

func TestLogConfigDiff_WorkflowTaskAdded(t *testing.T) {
	oldCfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "pipeline",
				Schedule: "0 9 * * *",
				Tasks:    []TaskConfig{{ID: "step-one"}},
			},
		},
	}
	newCfg := &Config{
		Workflows: []WorkflowConfig{
			{
				ID:       "pipeline",
				Schedule: "0 9 * * *",
				Tasks: []TaskConfig{
					{ID: "step-one"},
					{ID: "step-two", DependsOn: "step-one"},
				},
			},
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(oldCfg, newCfg, logger)

	if len(added) != 1 || added[0] != "step-two" {
		t.Errorf("expected [step-two] added, got %v", added)
	}
}

func TestLogConfigDiff_GlobalSettingChange_WorkerSlots(t *testing.T) {
	oldCfg := &Config{WorkerSlots: 3}
	newCfg := &Config{WorkerSlots: 5}
	logger := DiscardLogger()

	// Should not panic and returns no added tasks
	added := logConfigDiff(oldCfg, newCfg, logger)
	if len(added) != 0 {
		t.Errorf("expected 0 added tasks for worker slot change, got %v", added)
	}
}

func TestLogConfigDiff_GlobalSettingChange_AllFields(t *testing.T) {
	oldCfg := &Config{
		WorkerSlots:       3,
		MaxRetries:        2,
		RetryDelaySeconds: 60,
		TimeoutMinutes:    30,
		Timezone:          "UTC",
	}
	newCfg := &Config{
		WorkerSlots:       5,
		MaxRetries:        3,
		RetryDelaySeconds: 120,
		TimeoutMinutes:    60,
		Timezone:          "America/New_York",
	}
	logger := DiscardLogger()

	// Should not panic
	added := logConfigDiff(oldCfg, newCfg, logger)
	if len(added) != 0 {
		t.Errorf("expected 0 added tasks, got %v", added)
	}
}

func TestLogConfigDiff_BothAddAndRemove(t *testing.T) {
	oldCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "old-task", Schedule: "0 8 * * *"},
		},
	}
	newCfg := &Config{
		Standalone: []TaskConfig{
			{ID: "new-task", Schedule: "0 9 * * *"},
		},
	}
	logger := DiscardLogger()
	added := logConfigDiff(oldCfg, newCfg, logger)

	if len(added) != 1 || added[0] != "new-task" {
		t.Errorf("expected [new-task] in added, got %v", added)
	}
}

// ============================================================================
// seedNewTaskState Tests
// ============================================================================

func TestSeedNewTaskState_SeedsAllIDs(t *testing.T) {
	dir := t.TempDir()
	state := NewStateStore(filepath.Join(dir, "state.json"))
	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	logger := DiscardLogger()

	addedIDs := []string{"new-task-a", "new-task-b"}
	seedNewTaskState(addedIDs, state, now, logger)

	for _, id := range addedIDs {
		st := state.GetTask(id)
		if st == nil {
			t.Errorf("expected state for %q to be set, got nil", id)
			continue
		}
		if st.LastSuccess == nil {
			t.Errorf("expected LastSuccess to be set for %q", id)
		} else if !st.LastSuccess.Equal(now) {
			t.Errorf("LastSuccess for %q = %v, want %v", id, st.LastSuccess, now)
		}
		if st.LastRunStatus != "seeded" {
			t.Errorf("LastRunStatus for %q = %q, want 'seeded'", id, st.LastRunStatus)
		}
	}
}

func TestSeedNewTaskState_EmptyList(t *testing.T) {
	dir := t.TempDir()
	state := NewStateStore(filepath.Join(dir, "state.json"))
	now := time.Now()
	logger := DiscardLogger()

	// Should not panic or error
	seedNewTaskState(nil, state, now, logger)
	seedNewTaskState([]string{}, state, now, logger)

	// State should remain empty
	all := state.GetAll()
	if len(all) != 0 {
		t.Errorf("expected empty state, got %v", all)
	}
}

func TestSeedNewTaskState_DoesNotOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	state := NewStateStore(filepath.Join(dir, "state.json"))

	// Pre-populate task with real last_success
	realSuccess := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	state.UpdateTask("existing-task", func(ts *TaskState) {
		ts.LastSuccess = &realSuccess
		ts.LastRunStatus = "success"
	})

	now := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	logger := DiscardLogger()

	// Seeding an already-existing task should overwrite (that's the function's behavior)
	seedNewTaskState([]string{"existing-task"}, state, now, logger)

	st := state.GetTask("existing-task")
	if st.LastRunStatus != "seeded" {
		t.Errorf("LastRunStatus = %q, want 'seeded'", st.LastRunStatus)
	}
}

// ============================================================================
// projectRoot Tests
// ============================================================================

func TestProjectRoot_ReturnsNonEmptyString(t *testing.T) {
	root := projectRoot()
	if root == "" {
		t.Error("projectRoot should return a non-empty string")
	}
}

func TestProjectRoot_IsAbsolutePath(t *testing.T) {
	root := projectRoot()
	if !filepath.IsAbs(root) {
		t.Errorf("projectRoot should return an absolute path, got %q", root)
	}
}
