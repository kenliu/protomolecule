package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTempLog writes lines to a temp log file and returns its path.
func writeTempLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "protomolecule.log")
	var content strings.Builder
	for _, l := range lines {
		content.WriteString(l)
		content.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(content.String()), 0644); err != nil {
		t.Fatalf("writing temp log: %v", err)
	}
	return path
}

func TestReadTaskOutputFromLog_ExtractsOutputLines(t *testing.T) {
	path := writeTempLog(t,
		`{"type":"task_start","task_id":"foo"}`,
		`{"type":"claude_output","task_id":"foo","line":"line one"}`,
		`{"type":"claude_output","task_id":"foo","line":"line two"}`,
		`{"type":"task_end","task_id":"foo","status":"success"}`,
	)

	got, err := readTaskOutputFromLog(path, "foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"line one", "line two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Only the most recent run's output is returned when a task has run multiple times.
func TestReadTaskOutputFromLog_LastRunOnly(t *testing.T) {
	path := writeTempLog(t,
		`{"type":"task_start","task_id":"foo"}`,
		`{"type":"claude_output","task_id":"foo","line":"old run"}`,
		`{"type":"task_end","task_id":"foo","status":"success"}`,
		`{"type":"task_start","task_id":"foo"}`,
		`{"type":"claude_output","task_id":"foo","line":"new run"}`,
		`{"type":"task_end","task_id":"foo","status":"success"}`,
	)

	got, err := readTaskOutputFromLog(path, "foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"new run"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Entries for other tasks are ignored.
func TestReadTaskOutputFromLog_FiltersByTaskID(t *testing.T) {
	path := writeTempLog(t,
		`{"type":"task_start","task_id":"foo"}`,
		`{"type":"claude_output","task_id":"foo","line":"mine"}`,
		`{"type":"claude_output","task_id":"bar","line":"not mine"}`,
		`{"type":"task_end","task_id":"foo","status":"success"}`,
	)

	got, err := readTaskOutputFromLog(path, "foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"mine"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Malformed JSON lines and blank lines are skipped, not fatal.
func TestReadTaskOutputFromLog_SkipsMalformedLines(t *testing.T) {
	path := writeTempLog(t,
		`{"type":"task_start","task_id":"foo"}`,
		`not valid json`,
		``,
		`{"type":"claude_output","task_id":"foo","line":"survived"}`,
	)

	got, err := readTaskOutputFromLog(path, "foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"survived"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A task with no matching entries yields no output (and no error).
func TestReadTaskOutputFromLog_NoMatch(t *testing.T) {
	path := writeTempLog(t,
		`{"type":"claude_output","task_id":"other","line":"nope"}`,
	)

	got, err := readTaskOutputFromLog(path, "foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no output, got %v", got)
	}
}

// A missing log file surfaces an error rather than panicking.
func TestReadTaskOutputFromLog_MissingFile(t *testing.T) {
	_, err := readTaskOutputFromLog(filepath.Join(t.TempDir(), "does-not-exist.log"), "foo")
	if err == nil {
		t.Error("expected an error for a missing log file, got nil")
	}
}

// ============================================================================
// filterLastRun Tests
// ============================================================================

func TestFilterLastRun_KeepsFromLastStart(t *testing.T) {
	entries := []logEntry{
		{Type: "task_start"},
		{Type: "claude_output", Line: "old"},
		{Type: "task_start"},
		{Type: "claude_output", Line: "new"},
	}
	got := filterLastRun(entries)
	want := []logEntry{
		{Type: "task_start"},
		{Type: "claude_output", Line: "new"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// With no task_start marker, all entries are returned unchanged.
func TestFilterLastRun_NoStartReturnsAll(t *testing.T) {
	entries := []logEntry{
		{Type: "claude_output", Line: "a"},
		{Type: "claude_output", Line: "b"},
	}
	got := filterLastRun(entries)
	if !reflect.DeepEqual(got, entries) {
		t.Errorf("got %v, want %v", got, entries)
	}
}
