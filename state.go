package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RunEntry records a single completed execution of a task.
type RunEntry struct {
	Status   string        `json:"status"`
	RunAt    time.Time     `json:"run_at"`
	Duration time.Duration `json:"duration,omitempty"`
}

// TaskState tracks the execution state for a single task.
type TaskState struct {
	LastSuccess         *time.Time    `json:"last_success"`
	LastFailure         *time.Time    `json:"last_failure"`
	LastRun             *time.Time    `json:"last_run"`
	LastRunStatus       string        `json:"last_run_status"`
	LastRunDuration     time.Duration `json:"last_run_duration,omitempty"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
	LastCycleID         string        `json:"last_cycle_id,omitempty"`
	RetryAt             *time.Time    `json:"retry_at,omitempty"`
	RetryAttempt        int           `json:"retry_attempt,omitempty"`
	LastAttemptedFiring *time.Time    `json:"last_attempted_firing,omitempty"`
	RecentRuns          []RunEntry    `json:"recent_runs,omitempty"`
}

// maxRunHistory is the maximum number of recent run entries kept per task.
const maxRunHistory = 20

// appendRunEntry appends entry to history and trims to maxRunHistory.
func appendRunEntry(history []RunEntry, entry RunEntry) []RunEntry {
	history = append(history, entry)
	if len(history) > maxRunHistory {
		history = history[len(history)-maxRunHistory:]
	}
	return history
}

// StateStore provides thread-safe read/write access to the task state file.
type StateStore struct {
	mu   sync.RWMutex
	path string
	data map[string]*TaskState
}

// NewStateStore creates a new StateStore for the given file path.
func NewStateStore(path string) *StateStore {
	return &StateStore{
		path: path,
		data: make(map[string]*TaskState),
	}
}

// Load reads state from the file on disk. If the file is missing, state starts empty.
// If the file is corrupt or zero-length, it is copied to a .corrupt.<timestamp> backup
// and state starts empty.
func (s *StateStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.data = make(map[string]*TaskState)
			return s.saveAtomic()
		}
		return fmt.Errorf("reading state file: %w", err)
	}

	if len(raw) == 0 {
		s.handleCorruptFile("zero-length file")
		return nil
	}

	var data map[string]*TaskState
	if err := json.Unmarshal(raw, &data); err != nil {
		s.handleCorruptFile(fmt.Sprintf("JSON parse error: %v", err))
		return nil
	}

	s.data = data
	return nil
}

// handleCorruptFile copies the state file to a .corrupt.<timestamp> backup,
// logs a warning, and resets state to empty. Must be called with s.mu held.
func (s *StateStore) handleCorruptFile(reason string) {
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	backupPath := s.path + ".corrupt." + timestamp
	// Best-effort copy
	if raw, err := os.ReadFile(s.path); err == nil {
		os.WriteFile(backupPath, raw, 0644)
	}
	log.Printf("WARNING: corrupt state file %s (%s), backed up to %s, starting with empty state", s.path, reason, backupPath)
	s.data = make(map[string]*TaskState)
}

// GetTask returns a copy of the state for the given task ID.
// Returns nil if the task has no state.
func (s *StateStore) GetTask(id string) *TaskState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state, exists := s.data[id]
	if !exists {
		return nil
	}

	// Return a copy
	cp := *state
	return &cp
}

// GetAll returns a copy of all task state.
func (s *StateStore) GetAll() map[string]*TaskState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]*TaskState, len(s.data))
	for k, v := range s.data {
		cp := *v
		result[k] = &cp
	}
	return result
}

// UpdateTask applies a mutation function to the state for the given task ID,
// then atomically persists the state file.
func (s *StateStore) UpdateTask(id string, fn func(*TaskState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, exists := s.data[id]
	if !exists {
		state = &TaskState{}
		s.data[id] = state
	}
	fn(state)
	return s.saveAtomic()
}

// saveAtomic marshals the state to JSON, writes to a temp file, fsyncs, and renames.
// Must be called with s.mu held.
func (s *StateStore) saveAtomic() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	tmpPath := s.path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp state file: %w", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp state file: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp state file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp state file: %w", err)
	}

	return nil
}
