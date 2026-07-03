package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStateStore_WriteAndReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "task-state.json")

	store := NewStateStore(path)

	now := time.Now().UTC().Truncate(time.Second)
	err := store.UpdateTask("task-1", func(s *TaskState) {
		s.LastRun = &now
		s.LastRunStatus = "success"
		s.LastSuccess = &now
		s.ConsecutiveFailures = 0
		s.LastCycleID = "wf:2026-03-08T09:00:00Z"
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// Load into a fresh store
	store2 := NewStateStore(path)
	if err := store2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	state := store2.GetTask("task-1")
	if state == nil {
		t.Fatal("GetTask returned nil after round-trip")
	}
	if state.LastRunStatus != "success" {
		t.Errorf("LastRunStatus = %q, want %q", state.LastRunStatus, "success")
	}
	if !state.LastRun.Equal(now) {
		t.Errorf("LastRun = %v, want %v", state.LastRun, now)
	}
	if !state.LastSuccess.Equal(now) {
		t.Errorf("LastSuccess = %v, want %v", state.LastSuccess, now)
	}
	if state.LastCycleID != "wf:2026-03-08T09:00:00Z" {
		t.Errorf("LastCycleID = %q, want %q", state.LastCycleID, "wf:2026-03-08T09:00:00Z")
	}
}

func TestStateStore_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	store := NewStateStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}

	state := store.GetTask("anything")
	if state != nil {
		t.Errorf("expected nil for nonexistent task, got %+v", state)
	}

	all := store.GetAll()
	if len(all) != 0 {
		t.Errorf("expected empty state, got %d entries", len(all))
	}
}

func TestStateStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	// Write corrupt JSON
	if err := os.WriteFile(path, []byte("{not valid json!!!"), 0644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	store := NewStateStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load on corrupt file should not return error, got: %v", err)
	}

	// State should be empty
	if state := store.GetTask("anything"); state != nil {
		t.Errorf("expected nil state after corrupt recovery, got %+v", state)
	}

	// Backup file should exist
	entries, _ := os.ReadDir(dir)
	foundBackup := false
	for _, e := range entries {
		if len(e.Name()) > len("task-state.json") && e.Name() != "task-state.json" {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Error("expected .corrupt backup file after corrupt recovery")
	}
}

func TestStateStore_ZeroLengthFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	// Write empty file
	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatalf("writing empty file: %v", err)
	}

	store := NewStateStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load on zero-length file should not return error, got: %v", err)
	}

	// State should be empty
	if state := store.GetTask("anything"); state != nil {
		t.Errorf("expected nil state after zero-length recovery, got %+v", state)
	}

	// Backup file should exist
	entries, _ := os.ReadDir(dir)
	foundBackup := false
	for _, e := range entries {
		if len(e.Name()) > len("task-state.json") && e.Name() != "task-state.json" {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Error("expected .corrupt backup file after zero-length recovery")
	}
}

func TestStateStore_ConcurrentReadsAndWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	store := NewStateStore(path)

	var wg sync.WaitGroup
	const numGoroutines = 20

	// Half writers, half readers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		if i%2 == 0 {
			go func(id int) {
				defer wg.Done()
				now := time.Now().UTC()
				_ = store.UpdateTask("task-concurrent", func(s *TaskState) {
					s.LastRun = &now
					s.ConsecutiveFailures = id
				})
			}(i)
		} else {
			go func() {
				defer wg.Done()
				_ = store.GetTask("task-concurrent")
				_ = store.GetAll()
			}()
		}
	}

	wg.Wait()

	// Verify state is not corrupted
	state := store.GetTask("task-concurrent")
	if state == nil {
		t.Fatal("expected non-nil state after concurrent access")
	}
}

func TestStateStore_ConcurrentWritesDifferentIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	store := NewStateStore(path)

	var wg sync.WaitGroup
	ids := []string{"task-a", "task-b", "task-c", "task-d", "task-e"}

	for _, id := range ids {
		wg.Add(1)
		go func(actID string) {
			defer wg.Done()
			now := time.Now().UTC()
			_ = store.UpdateTask(actID, func(s *TaskState) {
				s.LastRun = &now
				s.LastRunStatus = "success"
			})
		}(id)
	}

	wg.Wait()

	// All IDs should be present
	all := store.GetAll()
	for _, id := range ids {
		if _, exists := all[id]; !exists {
			t.Errorf("missing task %q after concurrent writes", id)
		}
	}

	// Verify persistence by loading into a fresh store
	store2 := NewStateStore(path)
	if err := store2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	all2 := store2.GetAll()
	for _, id := range ids {
		if _, exists := all2[id]; !exists {
			t.Errorf("missing task %q after reload", id)
		}
	}
}

func TestStateStore_UpdateSingleTaskOthersUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	store := NewStateStore(path)

	now := time.Now().UTC().Truncate(time.Second)
	earlier := now.Add(-1 * time.Hour)

	// Set up two tasks
	_ = store.UpdateTask("task-1", func(s *TaskState) {
		s.LastRun = &earlier
		s.LastRunStatus = "success"
		s.ConsecutiveFailures = 0
	})
	_ = store.UpdateTask("task-2", func(s *TaskState) {
		s.LastRun = &earlier
		s.LastRunStatus = "success"
		s.ConsecutiveFailures = 0
	})

	// Update only task-1
	_ = store.UpdateTask("task-1", func(s *TaskState) {
		s.LastRun = &now
		s.LastRunStatus = "failed"
		s.ConsecutiveFailures = 1
	})

	// task-2 should be unchanged
	state2 := store.GetTask("task-2")
	if state2 == nil {
		t.Fatal("task-2 state is nil after updating task-1")
	}
	if state2.LastRunStatus != "success" {
		t.Errorf("task-2 LastRunStatus = %q, want %q", state2.LastRunStatus, "success")
	}
	if !state2.LastRun.Equal(earlier) {
		t.Errorf("task-2 LastRun changed unexpectedly")
	}
}

func TestStateStore_CycleIDPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	store := NewStateStore(path)
	cycleID := "daily-pipeline:2026-03-08T09:00:00Z"

	now := time.Now().UTC().Truncate(time.Second)
	_ = store.UpdateTask("task-1", func(s *TaskState) {
		s.LastRun = &now
		s.LastRunStatus = "success"
		s.LastCycleID = cycleID
	})

	// Reload
	store2 := NewStateStore(path)
	if err := store2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	state := store2.GetTask("task-1")
	if state == nil {
		t.Fatal("GetTask returned nil after reload")
	}
	if state.LastCycleID != cycleID {
		t.Errorf("LastCycleID = %q, want %q", state.LastCycleID, cycleID)
	}
}

func TestStateStore_GetTaskReturnsCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	store := NewStateStore(path)
	now := time.Now().UTC()
	_ = store.UpdateTask("task-1", func(s *TaskState) {
		s.LastRun = &now
		s.LastRunStatus = "success"
	})

	// Get a copy and mutate it
	copy1 := store.GetTask("task-1")
	copy1.LastRunStatus = "modified"

	// Original should be unaffected
	copy2 := store.GetTask("task-1")
	if copy2.LastRunStatus != "success" {
		t.Errorf("mutating copy affected internal state: LastRunStatus = %q, want %q", copy2.LastRunStatus, "success")
	}
}

func TestStateStore_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-state.json")

	store := NewStateStore(path)
	now := time.Now().UTC()
	_ = store.UpdateTask("task-1", func(s *TaskState) {
		s.LastRun = &now
		s.LastRunStatus = "success"
	})

	// Verify the file is valid JSON
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading state file: %v", err)
	}

	var data map[string]*TaskState
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}

	// No .tmp file should remain
	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file should not exist after successful write")
	}
}
