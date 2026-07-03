package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// maxRetriesLimit is the hard upper bound on retries for any task, global or per-task.
// This prevents runaway retry loops from burning tokens indefinitely.
const maxRetriesLimit = 10

// Config represents the top-level protomolecule.yaml configuration.
type Config struct {
	Timezone          string           `yaml:"timezone"`
	WorkingDir        string           `yaml:"working_dir"`
	WorkerSlots       int              `yaml:"worker_slots"`
	MaxRetries        int              `yaml:"max_retries"`
	RetryDelaySeconds int              `yaml:"retry_delay_seconds"`
	TimeoutMinutes    int              `yaml:"timeout_minutes"`
	Workflows         []WorkflowConfig `yaml:"workflows"`
	Standalone        []TaskConfig `yaml:"standalone"`
	// internal
	location *time.Location
}

// WorkflowConfig defines a group of tasks with dependencies and a shared schedule.
type WorkflowConfig struct {
	ID          string       `yaml:"id"`
	Description string       `yaml:"description,omitempty"`
	Schedule    string       `yaml:"schedule"`
	Catchup     bool         `yaml:"catchup"`
	Tasks       []TaskConfig `yaml:"tasks"`
}

// TaskConfig defines a single task, either standalone or within a workflow.
type TaskConfig struct {
	ID                string `yaml:"id"`
	Description       string `yaml:"description,omitempty"`
	Schedule          string `yaml:"schedule,omitempty"`
	Catchup           bool   `yaml:"catchup"`
	DependsOn         string `yaml:"depends_on,omitempty"`
	RequiresNetwork   *bool  `yaml:"requires_network"`
	Agent             string `yaml:"agent,omitempty"`
	Model             string `yaml:"model,omitempty"`
	Prompt            string `yaml:"prompt,omitempty"`
	TimeoutMinutes    *int   `yaml:"timeout_minutes,omitempty"`
	MaxRetries        *int   `yaml:"max_retries,omitempty"`
	RetryDelaySeconds *int   `yaml:"retry_delay_seconds,omitempty"`
}

// Location returns the parsed timezone location for this config.
// If no timezone was specified, it returns time.Local.
func (c *Config) Location() *time.Location {
	if c.location != nil {
		return c.location
	}
	return time.Local
}

// ResolveWorkingDir returns the absolute agent working directory — the
// directory in which `claude -p` runs for each task. It expands a leading "~"
// and, when working_dir is unset, falls back to the current directory. The
// returned bool reports whether working_dir was explicitly configured.
func (c *Config) ResolveWorkingDir() (string, bool, error) {
	if c.WorkingDir == "" {
		cwd, err := os.Getwd()
		return cwd, false, err
	}
	abs, err := filepath.Abs(expandTilde(c.WorkingDir))
	if err != nil {
		return "", true, err
	}
	return abs, true, nil
}

// expandTilde expands a leading "~" or "~/" to the user's home directory.
// It returns the input unchanged if the home directory cannot be resolved.
func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// IsNetworkRequired returns true if RequiresNetwork is nil (default) or explicitly true.
func (a *TaskConfig) IsNetworkRequired() bool {
	if a.RequiresNetwork == nil {
		return true
	}
	return *a.RequiresNetwork
}

// GetTimeoutMinutes returns the per-task timeout override, or the global default.
func (a *TaskConfig) GetTimeoutMinutes(globalDefault int) int {
	if a.TimeoutMinutes != nil {
		return *a.TimeoutMinutes
	}
	return globalDefault
}

// GetMaxRetries returns the per-task max retries override, or the global default.
func (a *TaskConfig) GetMaxRetries(globalDefault int) int {
	if a.MaxRetries != nil {
		return *a.MaxRetries
	}
	return globalDefault
}

// GetRetryDelaySeconds returns the per-task retry delay override, or the global default.
func (a *TaskConfig) GetRetryDelaySeconds(globalDefault int) int {
	if a.RetryDelaySeconds != nil {
		return *a.RetryDelaySeconds
	}
	return globalDefault
}

// LoadConfig reads and parses a protomolecule.yaml file, applies defaults, and validates.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	// Apply defaults
	if cfg.WorkerSlots == 0 {
		cfg.WorkerSlots = 5
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.RetryDelaySeconds == 0 {
		cfg.RetryDelaySeconds = 30
	}
	if cfg.TimeoutMinutes == 0 {
		cfg.TimeoutMinutes = 10
	}

	// Parse timezone
	if cfg.Timezone != "" {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			return nil, fmt.Errorf("invalid timezone %q: %w", cfg.Timezone, err)
		}
		cfg.location = loc
	} else {
		log.Printf("warning: no timezone configured, defaulting to system local time; recommend setting timezone in protomolecule.yaml")
	}

	// Enforce hard limits
	if cfg.MaxRetries > maxRetriesLimit {
		return nil, fmt.Errorf("max_retries %d exceeds hard limit of %d", cfg.MaxRetries, maxRetriesLimit)
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validateConfig checks the config for structural and semantic errors.
func validateConfig(c *Config) error {
	// If a working directory is configured, it must exist and be a directory.
	if c.WorkingDir != "" {
		wd := expandTilde(c.WorkingDir)
		info, err := os.Stat(wd)
		if err != nil {
			return fmt.Errorf("working_dir %q: %w", c.WorkingDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("working_dir %q is not a directory", c.WorkingDir)
		}
	}

	// Collect all task IDs to check for duplicates and cross-section conflicts.
	allIDs := make(map[string]string) // id -> location description

	// Track standalone IDs for cross-section check.
	standaloneIDs := make(map[string]bool)
	for _, a := range c.Standalone {
		standaloneIDs[a.ID] = true
	}

	// Validate workflows
	for _, wf := range c.Workflows {
		// Validate workflow schedule
		if err := validateCronExpression(wf.Schedule); err != nil {
			return fmt.Errorf("workflow %q: invalid schedule %q: %w", wf.ID, wf.Schedule, err)
		}

		// Build set of task IDs within this workflow for depends_on resolution.
		wfTaskIDs := make(map[string]bool)
		for _, a := range wf.Tasks {
			wfTaskIDs[a.ID] = true
		}

		for _, a := range wf.Tasks {
			loc := fmt.Sprintf("workflow %q", wf.ID)

			// Check for duplicate IDs across all workflows and standalone
			if prev, exists := allIDs[a.ID]; exists {
				return fmt.Errorf("duplicate task ID %q: found in %s and %s", a.ID, prev, loc)
			}
			allIDs[a.ID] = loc

			// Check task not also in standalone
			if standaloneIDs[a.ID] {
				return fmt.Errorf("task %q appears in both workflow %q and standalone", a.ID, wf.ID)
			}

			// Workflow-nested tasks must not have a schedule field
			if a.Schedule != "" {
				return fmt.Errorf("task %q in workflow %q must not have a schedule field (schedule is set on the workflow)", a.ID, wf.ID)
			}

			// Reject explicit timeout_minutes: 0 — infinite timeouts are not allowed.
			// Omitting the field inherits the global default.
			if a.TimeoutMinutes != nil && *a.TimeoutMinutes == 0 {
				return fmt.Errorf("task %q in workflow %q: timeout_minutes must be > 0 (omit to inherit global default)", a.ID, wf.ID)
			}

			// Reject per-task max_retries exceeding the hard limit.
			if a.MaxRetries != nil && *a.MaxRetries > maxRetriesLimit {
				return fmt.Errorf("task %q in workflow %q: max_retries %d exceeds hard limit of %d", a.ID, wf.ID, *a.MaxRetries, maxRetriesLimit)
			}

			// Validate depends_on references
			if a.DependsOn != "" {
				if a.DependsOn == a.ID {
					return fmt.Errorf("task %q in workflow %q references itself in depends_on", a.ID, wf.ID)
				}
				if !wfTaskIDs[a.DependsOn] {
					return fmt.Errorf("task %q in workflow %q depends on nonexistent task %q", a.ID, wf.ID, a.DependsOn)
				}
			}
		}

		// Check for DAG cycles using Kahn's algorithm
		if err := checkDAGCycles(wf); err != nil {
			return err
		}
	}

	// Validate standalone tasks
	for _, a := range c.Standalone {
		loc := "standalone"
		if prev, exists := allIDs[a.ID]; exists {
			return fmt.Errorf("duplicate task ID %q: found in %s and %s", a.ID, prev, loc)
		}
		allIDs[a.ID] = loc

		// Reject explicit timeout_minutes: 0 — infinite timeouts are not allowed.
		if a.TimeoutMinutes != nil && *a.TimeoutMinutes == 0 {
			return fmt.Errorf("standalone task %q: timeout_minutes must be > 0 (omit to inherit global default)", a.ID)
		}

		// Reject per-task max_retries exceeding the hard limit.
		if a.MaxRetries != nil && *a.MaxRetries > maxRetriesLimit {
			return fmt.Errorf("standalone task %q: max_retries %d exceeds hard limit of %d", a.ID, *a.MaxRetries, maxRetriesLimit)
		}

		// Validate standalone schedule
		if a.Schedule != "" {
			if err := validateCronExpression(a.Schedule); err != nil {
				return fmt.Errorf("standalone task %q: invalid schedule %q: %w", a.ID, a.Schedule, err)
			}
		}
	}

	return nil
}

// validateCronExpression validates a cron expression string.
// "on-demand" is accepted as a special non-cron value.
func validateCronExpression(expr string) error {
	if expr == "" {
		return nil
	}
	if strings.EqualFold(expr, "on-demand") {
		return nil
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	_, err := parser.Parse(expr)
	if err != nil {
		return fmt.Errorf("cron parse error: %w", err)
	}
	return nil
}

// checkDAGCycles uses Kahn's algorithm to detect cycles in workflow dependencies.
func checkDAGCycles(wf WorkflowConfig) error {
	// Build adjacency list and in-degree count.
	inDegree := make(map[string]int)
	dependents := make(map[string][]string) // parent -> list of children

	for _, a := range wf.Tasks {
		if _, exists := inDegree[a.ID]; !exists {
			inDegree[a.ID] = 0
		}
		if a.DependsOn != "" {
			inDegree[a.ID]++
			dependents[a.DependsOn] = append(dependents[a.DependsOn], a.ID)
		}
	}

	// Find all nodes with in-degree 0.
	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	visited := 0
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		visited++

		for _, child := range dependents[node] {
			inDegree[child]--
			if inDegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	if visited != len(inDegree) {
		return fmt.Errorf("workflow %q contains a dependency cycle", wf.ID)
	}

	return nil
}

// ConfigWatcher watches a config file for changes and calls a callback on reload.
type ConfigWatcher struct {
	watcher  *fsnotify.Watcher
	path     string
	onReload func(*Config, error)
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewConfigWatcher creates a watcher on the given config file path.
// When the file changes, it debounces for 500ms, then parses and validates.
// On valid config: calls onReload(config, nil). On error: calls onReload(nil, err).
func NewConfigWatcher(path string, onReload func(*Config, error)) (*ConfigWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	if err := w.Add(path); err != nil {
		w.Close()
		return nil, fmt.Errorf("watching path %q: %w", path, err)
	}

	cw := &ConfigWatcher{
		watcher:  w,
		path:     path,
		onReload: onReload,
		stopCh:   make(chan struct{}),
	}

	cw.wg.Add(1)
	go cw.loop()

	return cw, nil
}

// Close stops the config watcher.
func (cw *ConfigWatcher) Close() error {
	close(cw.stopCh)
	cw.wg.Wait()
	return cw.watcher.Close()
}

func (cw *ConfigWatcher) loop() {
	defer cw.wg.Done()

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-cw.stopCh:
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.NewTimer(500 * time.Millisecond)
				debounceCh = debounceTimer.C
			}

		case _, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			// Watcher errors are logged but not fatal.

		case <-debounceCh:
			debounceCh = nil
			cfg, err := LoadConfig(cw.path)
			if err != nil {
				cw.onReload(nil, err)
			} else {
				cw.onReload(cfg, nil)
			}
		}
	}
}
