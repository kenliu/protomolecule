package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// ============================================================================
// buildTaskIDs Tests
// ============================================================================

func TestBuildTaskIDs_Nil(t *testing.T) {
	ids := buildTaskIDs(nil)
	if ids != nil {
		t.Errorf("expected nil for nil input, got %v", ids)
	}
}

func TestBuildTaskIDs_Empty(t *testing.T) {
	ids := buildTaskIDs(&StatusResponse{})
	if len(ids) != 0 {
		t.Errorf("expected empty slice for empty status, got %v", ids)
	}
}

func TestBuildTaskIDs_ActiveOnly(t *testing.T) {
	s := &StatusResponse{
		Active: []ActiveJobInfo{
			{TaskID: "task-a"},
			{TaskID: "task-b"},
		},
	}
	ids := buildTaskIDs(s)
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d: %v", len(ids), ids)
	}
	if ids[0] != "task-a" || ids[1] != "task-b" {
		t.Errorf("got %v, want [task-a task-b]", ids)
	}
}

func TestBuildTaskIDs_Deduplication(t *testing.T) {
	// A task that appears in both Upcoming and Recent should only appear once.
	s := &StatusResponse{
		Upcoming: []UpcomingInfo{
			{TaskID: "slack-digest"},
		},
		Recent: []RecentRunInfo{
			{TaskID: "slack-digest"},
		},
	}
	ids := buildTaskIDs(s)
	if len(ids) != 1 {
		t.Errorf("expected 1 unique ID, got %d: %v", len(ids), ids)
	}
	if ids[0] != "slack-digest" {
		t.Errorf("expected slack-digest, got %q", ids[0])
	}
}

func TestBuildTaskIDs_Ordering(t *testing.T) {
	// IDs should appear in order: Active → Queued → Upcoming → Recent → OnDemand.
	s := &StatusResponse{
		Active:   []ActiveJobInfo{{TaskID: "active-task"}},
		Queued:   []QueuedJobInfo{{TaskID: "queued-task"}},
		Upcoming: []UpcomingInfo{{TaskID: "upcoming-task"}},
		Recent:   []RecentRunInfo{{TaskID: "recent-task"}},
		OnDemand: []string{"ondemand-task"},
	}
	ids := buildTaskIDs(s)
	if len(ids) != 5 {
		t.Fatalf("expected 5 IDs, got %d: %v", len(ids), ids)
	}
	want := []string{"active-task", "queued-task", "upcoming-task", "recent-task", "ondemand-task"}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], w)
		}
	}
}

func TestBuildTaskIDs_OnDemandOnly(t *testing.T) {
	s := &StatusResponse{
		OnDemand: []string{"manual-a", "manual-b"},
	}
	ids := buildTaskIDs(s)
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d: %v", len(ids), ids)
	}
	if ids[0] != "manual-a" || ids[1] != "manual-b" {
		t.Errorf("got %v, want [manual-a manual-b]", ids)
	}
}

func TestBuildTaskIDs_DeduplicatesAcrossAllSections(t *testing.T) {
	// Same task ID in Active, Upcoming, Recent, and OnDemand — appears only once.
	s := &StatusResponse{
		Active:   []ActiveJobInfo{{TaskID: "shared"}},
		Upcoming: []UpcomingInfo{{TaskID: "shared"}},
		Recent:   []RecentRunInfo{{TaskID: "shared"}},
		OnDemand: []string{"shared"},
	}
	ids := buildTaskIDs(s)
	if len(ids) != 1 {
		t.Errorf("expected 1 unique ID, got %d: %v", len(ids), ids)
	}
}

// ============================================================================
// updateViewMode Tests
// ============================================================================

func newViewModeModel(taskIDs []string) tuiModel {
	return tuiModel{
		mode:    modeView,
		taskIDs: taskIDs,
	}
}

func TestUpdateViewMode_Q(t *testing.T) {
	m := newViewModeModel([]string{"task-a"})
	result, cmd := m.updateViewMode("q")
	got := result.(tuiModel)

	if !got.quitting {
		t.Error("expected quitting=true after 'q'")
	}
	if cmd == nil {
		t.Error("expected non-nil cmd (tea.Quit) after 'q'")
	}
}

func TestUpdateViewMode_CtrlC(t *testing.T) {
	m := newViewModeModel([]string{"task-a"})
	result, cmd := m.updateViewMode("ctrl+c")
	got := result.(tuiModel)

	if !got.quitting {
		t.Error("expected quitting=true after ctrl+c")
	}
	if cmd == nil {
		t.Error("expected non-nil cmd after ctrl+c")
	}
}

func TestUpdateViewMode_R_WithTasks(t *testing.T) {
	m := newViewModeModel([]string{"task-a", "task-b"})
	result, cmd := m.updateViewMode("r")
	got := result.(tuiModel)

	if got.mode != modeRun {
		t.Errorf("expected modeRun, got %v", got.mode)
	}
	if got.cursor != 0 {
		t.Errorf("expected cursor=0, got %d", got.cursor)
	}
	if cmd != nil {
		t.Error("expected nil cmd when entering run mode")
	}
}

func TestUpdateViewMode_R_WithNoTasks(t *testing.T) {
	m := newViewModeModel(nil)
	result, _ := m.updateViewMode("r")
	got := result.(tuiModel)

	if got.mode != modeView {
		t.Errorf("expected modeView when no tasks, got %v", got.mode)
	}
}

func TestUpdateViewMode_UnknownKey(t *testing.T) {
	m := newViewModeModel([]string{"task-a"})
	original := m
	result, cmd := m.updateViewMode("x")
	got := result.(tuiModel)

	if cmd != nil {
		t.Error("expected nil cmd for unknown key")
	}
	if got.mode != original.mode {
		t.Error("mode should not change for unknown key")
	}
	if got.quitting {
		t.Error("quitting should not be set for unknown key")
	}
}

// ============================================================================
// updateRunMode Tests
// ============================================================================

func newRunModeModel(taskIDs []string, cursor int) tuiModel {
	return tuiModel{
		mode:    modeRun,
		taskIDs: taskIDs,
		cursor:  cursor,
	}
}

func TestUpdateRunMode_Esc(t *testing.T) {
	m := newRunModeModel([]string{"task-a"}, 0)
	result, cmd := m.updateRunMode("esc")
	got := result.(tuiModel)

	if got.mode != modeView {
		t.Errorf("expected modeView after esc, got %v", got.mode)
	}
	if cmd != nil {
		t.Error("expected nil cmd after esc")
	}
}

func TestUpdateRunMode_Escape(t *testing.T) {
	m := newRunModeModel([]string{"task-a"}, 0)
	result, _ := m.updateRunMode("escape")
	got := result.(tuiModel)

	if got.mode != modeView {
		t.Errorf("expected modeView after escape, got %v", got.mode)
	}
}

func TestUpdateRunMode_Q(t *testing.T) {
	m := newRunModeModel([]string{"task-a"}, 0)
	result, _ := m.updateRunMode("q")
	got := result.(tuiModel)

	if got.mode != modeView {
		t.Errorf("expected modeView after q in run mode, got %v", got.mode)
	}
}

func TestUpdateRunMode_J_IncrementsCursor(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b", "task-c"}, 0)
	result, _ := m.updateRunMode("j")
	got := result.(tuiModel)

	if got.cursor != 1 {
		t.Errorf("cursor = %d, want 1", got.cursor)
	}
}

func TestUpdateRunMode_Down_IncrementsCursor(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b"}, 0)
	result, _ := m.updateRunMode("down")
	got := result.(tuiModel)

	if got.cursor != 1 {
		t.Errorf("cursor = %d, want 1", got.cursor)
	}
}

func TestUpdateRunMode_J_WrapsAround(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b", "task-c"}, 2) // at last
	result, _ := m.updateRunMode("j")
	got := result.(tuiModel)

	if got.cursor != 0 {
		t.Errorf("cursor should wrap to 0, got %d", got.cursor)
	}
}

func TestUpdateRunMode_K_DecrementsCursor(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b", "task-c"}, 2)
	result, _ := m.updateRunMode("k")
	got := result.(tuiModel)

	if got.cursor != 1 {
		t.Errorf("cursor = %d, want 1", got.cursor)
	}
}

func TestUpdateRunMode_Up_DecrementsCursor(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b"}, 1)
	result, _ := m.updateRunMode("up")
	got := result.(tuiModel)

	if got.cursor != 0 {
		t.Errorf("cursor = %d, want 0", got.cursor)
	}
}

func TestUpdateRunMode_K_WrapsAround(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b", "task-c"}, 0) // at first
	result, _ := m.updateRunMode("k")
	got := result.(tuiModel)

	if got.cursor != 2 {
		t.Errorf("cursor should wrap to 2 (last), got %d", got.cursor)
	}
}

func TestUpdateRunMode_J_EmptyList(t *testing.T) {
	m := newRunModeModel(nil, 0)
	result, _ := m.updateRunMode("j")
	got := result.(tuiModel)

	if got.cursor != 0 {
		t.Errorf("cursor should stay 0 with empty list, got %d", got.cursor)
	}
}

func TestUpdateRunMode_Enter_DispatchesTask(t *testing.T) {
	m := newRunModeModel([]string{"task-a", "task-b"}, 1)
	result, cmd := m.updateRunMode("enter")
	got := result.(tuiModel)

	// Should return to view mode
	if got.mode != modeView {
		t.Errorf("expected modeView after enter, got %v", got.mode)
	}
	// Should set a flash message mentioning the task
	if !strings.Contains(got.flash, "task-b") {
		t.Errorf("flash = %q, expected it to mention 'task-b'", got.flash)
	}
	// Should return a non-nil cmd (runTask)
	if cmd == nil {
		t.Error("expected non-nil cmd after enter")
	}
	// flashAt should be set recently
	if time.Since(got.flashAt) > time.Second {
		t.Error("flashAt should be set to approximately now")
	}
}

func TestUpdateRunMode_Enter_EmptyList(t *testing.T) {
	m := newRunModeModel(nil, 0)
	result, cmd := m.updateRunMode("enter")
	got := result.(tuiModel)

	// Should stay in run mode (cursor >= len(taskIDs) so condition not met)
	if got.mode != modeRun {
		t.Errorf("expected modeRun with empty list, got %v", got.mode)
	}
	if cmd != nil {
		t.Error("expected nil cmd with empty list")
	}
}

func TestUpdateRunMode_UnknownKey(t *testing.T) {
	m := newRunModeModel([]string{"task-a"}, 0)
	result, cmd := m.updateRunMode("x")
	got := result.(tuiModel)

	if got.mode != modeRun {
		t.Errorf("mode should stay modeRun for unknown key, got %v", got.mode)
	}
	if cmd != nil {
		t.Error("expected nil cmd for unknown key")
	}
}

// ============================================================================
// selectedTaskID Tests
// ============================================================================

func TestSelectedTaskID_ViewMode(t *testing.T) {
	m := tuiModel{
		mode:    modeView,
		taskIDs: []string{"task-a", "task-b"},
		cursor:  0,
	}
	if got := m.selectedTaskID(); got != "" {
		t.Errorf("expected empty string in view mode, got %q", got)
	}
}

func TestSelectedTaskID_RunMode(t *testing.T) {
	m := tuiModel{
		mode:    modeRun,
		taskIDs: []string{"task-a", "task-b", "task-c"},
		cursor:  1,
	}
	if got := m.selectedTaskID(); got != "task-b" {
		t.Errorf("selectedTaskID = %q, want task-b", got)
	}
}

func TestSelectedTaskID_RunMode_FirstElement(t *testing.T) {
	m := tuiModel{
		mode:    modeRun,
		taskIDs: []string{"only-task"},
		cursor:  0,
	}
	if got := m.selectedTaskID(); got != "only-task" {
		t.Errorf("selectedTaskID = %q, want only-task", got)
	}
}

func TestSelectedTaskID_RunMode_CursorOutOfBounds(t *testing.T) {
	m := tuiModel{
		mode:    modeRun,
		taskIDs: []string{"task-a"},
		cursor:  5, // out of bounds
	}
	if got := m.selectedTaskID(); got != "" {
		t.Errorf("expected empty string when cursor out of bounds, got %q", got)
	}
}

func TestSelectedTaskID_RunMode_EmptyList(t *testing.T) {
	m := tuiModel{
		mode:    modeRun,
		taskIDs: nil,
		cursor:  0,
	}
	if got := m.selectedTaskID(); got != "" {
		t.Errorf("expected empty string with empty taskIDs, got %q", got)
	}
}

// ============================================================================
// renderLine Tests
// ============================================================================

func TestRenderLine_NotSelected(t *testing.T) {
	m := tuiModel{}
	line := "  ● task-a   0:42    attempt 1/3"
	got := m.renderLine(line, false)
	if got != line {
		t.Errorf("unselected line should be returned unchanged\ngot:  %q\nwant: %q", got, line)
	}
}

func TestRenderLine_Selected_ContainsCursor(t *testing.T) {
	m := tuiModel{}
	line := "  ● task-a   0:42    attempt 1/3"
	got := m.renderLine(line, true)

	// Should contain the cursor indicator
	if !strings.Contains(got, "▸") {
		t.Error("selected line should contain cursor '▸'")
	}
	// Should not equal the original line (it was modified)
	if got == line {
		t.Error("selected line should differ from original")
	}
}

func TestRenderLine_Selected_ContainsContent(t *testing.T) {
	m := tuiModel{}
	// The function does line[1:], so the rest of the line after the first char
	// should appear in the output.
	line := "  task-name-here"
	got := m.renderLine(line, true)

	// line[1:] = " task-name-here" — this content should appear in output
	if !strings.Contains(got, "task-name-here") {
		t.Errorf("selected line should contain original content, got %q", got)
	}
}

// ============================================================================
// Update (non-key message) Tests
// ============================================================================

func TestUpdate_StatusMsg(t *testing.T) {
	m := tuiModel{}
	status := &StatusResponse{
		Active:   []ActiveJobInfo{{TaskID: "active-a"}},
		Upcoming: []UpcomingInfo{{TaskID: "upcoming-b"}},
	}
	result, cmd := m.Update(statusMsg{status: status})
	got := result.(tuiModel)

	if got.status != status {
		t.Error("status should be updated")
	}
	if got.err != nil {
		t.Errorf("err should be cleared, got %v", got.err)
	}
	if got.lastUpdate.IsZero() {
		t.Error("lastUpdate should be set")
	}
	if len(got.taskIDs) == 0 {
		t.Error("taskIDs should be populated from status")
	}
	if cmd != nil {
		t.Errorf("expected nil cmd from statusMsg, got %v", cmd)
	}
}

func TestUpdate_StatusMsg_ClampsOvershotCursor(t *testing.T) {
	// Cursor at index 5, but new status only has 2 tasks — should clamp to 1.
	m := tuiModel{
		cursor:  5,
		taskIDs: []string{"a", "b", "c", "d", "e", "f"},
	}
	status := &StatusResponse{
		Active: []ActiveJobInfo{{TaskID: "task-x"}, {TaskID: "task-y"}},
	}
	result, _ := m.Update(statusMsg{status: status})
	got := result.(tuiModel)

	if got.cursor >= len(got.taskIDs) {
		t.Errorf("cursor %d out of range [0, %d)", got.cursor, len(got.taskIDs))
	}
	if got.cursor != len(got.taskIDs)-1 {
		t.Errorf("cursor = %d, want %d (clamped)", got.cursor, len(got.taskIDs)-1)
	}
}

func TestUpdate_StatusMsg_PreservesValidCursor(t *testing.T) {
	// Cursor at 0 with 3 tasks — should not be clamped.
	m := tuiModel{cursor: 0}
	status := &StatusResponse{
		Active: []ActiveJobInfo{{TaskID: "a"}, {TaskID: "b"}, {TaskID: "c"}},
	}
	result, _ := m.Update(statusMsg{status: status})
	got := result.(tuiModel)

	if got.cursor != 0 {
		t.Errorf("cursor should remain 0, got %d", got.cursor)
	}
}

func TestUpdate_ErrMsg(t *testing.T) {
	m := tuiModel{}
	testErr := errors.New("connection refused")
	result, cmd := m.Update(errMsg{err: testErr})
	got := result.(tuiModel)

	if got.err != testErr {
		t.Errorf("err = %v, want %v", got.err, testErr)
	}
	if cmd != nil {
		t.Errorf("expected nil cmd from errMsg, got %v", cmd)
	}
}

func TestUpdate_RunResultMsg_Success(t *testing.T) {
	m := tuiModel{}
	result, cmd := m.Update(runResultMsg{taskID: "my-task", err: nil})
	got := result.(tuiModel)

	if !strings.Contains(got.flash, "my-task") {
		t.Errorf("flash = %q, expected it to mention 'my-task'", got.flash)
	}
	if strings.HasPrefix(got.flash, "Failed") {
		t.Errorf("flash should not start with 'Failed' on success, got %q", got.flash)
	}
	if got.flashAt.IsZero() {
		t.Error("flashAt should be set")
	}
	// Should return a fetchStatus cmd
	if cmd == nil {
		t.Error("expected non-nil cmd (fetchStatus) after runResultMsg")
	}
}

func TestUpdate_RunResultMsg_Failure(t *testing.T) {
	m := tuiModel{}
	result, cmd := m.Update(runResultMsg{taskID: "failing-task", err: errors.New("timeout")})
	got := result.(tuiModel)

	if !strings.HasPrefix(got.flash, "Failed") {
		t.Errorf("flash should start with 'Failed' on error, got %q", got.flash)
	}
	if !strings.Contains(got.flash, "failing-task") {
		t.Errorf("flash = %q, expected to mention 'failing-task'", got.flash)
	}
	if !strings.Contains(got.flash, "timeout") {
		t.Errorf("flash = %q, expected to mention error 'timeout'", got.flash)
	}
	if cmd == nil {
		t.Error("expected non-nil cmd after failed runResultMsg")
	}
}

func TestUpdate_WindowSizeMsg(t *testing.T) {
	m := tuiModel{}
	result, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	got := result.(tuiModel)

	if got.width != 120 {
		t.Errorf("width = %d, want 120", got.width)
	}
	if got.height != 40 {
		t.Errorf("height = %d, want 40", got.height)
	}
	if cmd != nil {
		t.Errorf("expected nil cmd from WindowSizeMsg, got %v", cmd)
	}
}

func TestUpdate_TickMsg_ClearsOldFlash(t *testing.T) {
	m := tuiModel{
		flash:   "Started some-task",
		flashAt: time.Now().Add(-6 * time.Second), // 6s ago — older than 5s threshold
	}
	result, _ := m.Update(tickMsg(time.Now()))
	got := result.(tuiModel)

	if got.flash != "" {
		t.Errorf("flash should be cleared after 5s, got %q", got.flash)
	}
}

func TestUpdate_TickMsg_KeepsRecentFlash(t *testing.T) {
	m := tuiModel{
		flash:   "Started some-task",
		flashAt: time.Now().Add(-2 * time.Second), // 2s ago — within 5s threshold
	}
	result, _ := m.Update(tickMsg(time.Now()))
	got := result.(tuiModel)

	if got.flash != "Started some-task" {
		t.Errorf("flash should be preserved within 5s, got %q", got.flash)
	}
}

func TestUpdate_TickMsg_NoFlash(t *testing.T) {
	m := tuiModel{flash: ""}
	result, cmd := m.Update(tickMsg(time.Now()))
	got := result.(tuiModel)

	if got.flash != "" {
		t.Errorf("flash should remain empty, got %q", got.flash)
	}
	// Should return batch cmd (fetchStatus + doTick)
	if cmd == nil {
		t.Error("expected non-nil cmd from tickMsg")
	}
}

// ============================================================================
// cursor wrapping edge cases
// ============================================================================

func TestUpdateRunMode_SingleTask_J_Wraps(t *testing.T) {
	m := newRunModeModel([]string{"only"}, 0)
	result, _ := m.updateRunMode("j")
	got := result.(tuiModel)

	// cursor at 0, len=1: (0+1)%1 = 0
	if got.cursor != 0 {
		t.Errorf("cursor should stay 0 with single task, got %d", got.cursor)
	}
}

func TestUpdateRunMode_SingleTask_K_Wraps(t *testing.T) {
	m := newRunModeModel([]string{"only"}, 0)
	result, _ := m.updateRunMode("k")
	got := result.(tuiModel)

	// cursor at 0, len=1: (0-1+1)%1 = 0
	if got.cursor != 0 {
		t.Errorf("cursor should stay 0 with single task, got %d", got.cursor)
	}
}

// ============================================================================
// Multi-step interaction Tests
// ============================================================================

func TestRunModeRoundTrip(t *testing.T) {
	// Start in view mode, enter run mode, navigate, exit back to view mode.
	m := tuiModel{
		mode:    modeView,
		taskIDs: []string{"alpha", "beta", "gamma"},
		cursor:  0,
	}

	// Enter run mode
	result, _ := m.updateViewMode("r")
	m = result.(tuiModel)
	if m.mode != modeRun {
		t.Fatal("expected modeRun after 'r'")
	}
	if m.cursor != 0 {
		t.Fatalf("expected cursor=0, got %d", m.cursor)
	}

	// Navigate down twice
	result, _ = m.updateRunMode("j")
	m = result.(tuiModel)
	result, _ = m.updateRunMode("j")
	m = result.(tuiModel)
	if m.cursor != 2 {
		t.Fatalf("expected cursor=2 after two j presses, got %d", m.cursor)
	}

	// Verify selected task
	if m.selectedTaskID() != "gamma" {
		t.Errorf("selectedTaskID = %q, want gamma", m.selectedTaskID())
	}

	// Cancel back to view mode
	result, _ = m.updateRunMode("esc")
	m = result.(tuiModel)
	if m.mode != modeView {
		t.Fatalf("expected modeView after esc, got %v", m.mode)
	}
}

func TestEnterAndDispatchTask(t *testing.T) {
	// Navigate to a task and dispatch it.
	m := tuiModel{
		mode:    modeRun,
		taskIDs: []string{"check-jira", "slack-digest"},
		cursor:  0,
	}

	// Move to second task
	result, _ := m.updateRunMode("j")
	m = result.(tuiModel)
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.cursor)
	}

	// Dispatch
	result, cmd := m.updateRunMode("enter")
	m = result.(tuiModel)

	if m.mode != modeView {
		t.Errorf("expected modeView after enter, got %v", m.mode)
	}
	if !strings.Contains(m.flash, "slack-digest") {
		t.Errorf("flash = %q, expected mention of slack-digest", m.flash)
	}
	if cmd == nil {
		t.Error("expected non-nil cmd from enter")
	}
}

// ============================================================================
// Pause key / pauseResultMsg Tests
// ============================================================================

func TestUpdateViewMode_P_ReturnsCmd(t *testing.T) {
	// Pressing 'p' in view mode should return a non-nil cmd (pauseToggle).
	m := newViewModeModel([]string{"task-a"})
	result, cmd := m.updateViewMode("p")
	got := result.(tuiModel)

	// Mode should stay modeView — 'p' doesn't switch modes
	if got.mode != modeView {
		t.Errorf("mode should stay modeView after p, got %v", got.mode)
	}
	if cmd == nil {
		t.Error("expected non-nil cmd (pauseToggle) after p")
	}
}

func TestUpdateViewMode_P_NoClient_StillReturnsCmd(t *testing.T) {
	// Even with a nil client the key press should return a cmd (it will fail when executed).
	m := tuiModel{mode: modeView, taskIDs: []string{"task-a"}}
	_, cmd := m.updateViewMode("p")
	if cmd == nil {
		t.Error("expected non-nil cmd from p even with nil client")
	}
}

func TestUpdate_PauseResultMsg_Success(t *testing.T) {
	// On success, should fetch fresh status (non-nil cmd) but not set a flash.
	m := tuiModel{}
	result, cmd := m.Update(pauseResultMsg{err: nil})
	got := result.(tuiModel)

	if got.flash != "" {
		t.Errorf("flash should be empty on success, got %q", got.flash)
	}
	if cmd == nil {
		t.Error("expected non-nil cmd (fetchStatus) after successful pauseResultMsg")
	}
}

func TestUpdate_PauseResultMsg_Failure(t *testing.T) {
	// On failure, should set a flash error and fetch status.
	m := tuiModel{}
	result, cmd := m.Update(pauseResultMsg{err: errors.New("daemon not running")})
	got := result.(tuiModel)

	if !strings.Contains(got.flash, "Pause failed") {
		t.Errorf("flash = %q, expected 'Pause failed'", got.flash)
	}
	if !strings.Contains(got.flash, "daemon not running") {
		t.Errorf("flash = %q, expected error message in flash", got.flash)
	}
	if got.flashAt.IsZero() {
		t.Error("flashAt should be set on failure")
	}
	if cmd == nil {
		t.Error("expected non-nil cmd after failed pauseResultMsg")
	}
}
