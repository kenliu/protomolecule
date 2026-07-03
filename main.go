package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

// configDirName is the per-user protomolecule home directory under $HOME.
const configDirName = ".protomolecule"

// defaultConfigPath returns the default config file location,
// ~/.protomolecule/protomolecule.yaml. If the home directory cannot be
// determined, it falls back to protomolecule.yaml in the current directory.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "protomolecule.yaml"
	}
	return filepath.Join(home, configDirName, "protomolecule.yaml")
}

// runtimeDir returns the directory where protomolecule stores its runtime
// files (state, socket, logs, output): the directory containing the config
// file. With the default config path this resolves to ~/.protomolecule. All
// subcommands derive runtime paths this way so they locate the same socket,
// state, and logs regardless of the current working directory.
func runtimeDir(configPath string) string {
	if abs, err := filepath.Abs(configPath); err == nil {
		return filepath.Dir(abs)
	}
	return filepath.Dir(configPath)
}

func main() {
	// Parse global flags before subcommand dispatch
	args := os.Args[1:]
	configPath := defaultConfigPath()
	verbose := false

	// Extract --config and --verbose flags from args
	var filteredArgs []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			configPath = args[i+1]
			i++ // skip the value
		} else if strings.HasPrefix(args[i], "--config=") {
			configPath = strings.TrimPrefix(args[i], "--config=")
		} else if args[i] == "--debug" {
			verbose = true
		} else {
			filteredArgs = append(filteredArgs, args[i])
		}
	}
	args = filteredArgs

	// Determine subcommand
	if len(args) == 0 {
		printUsage()
		return
	}

	switch args[0] {
	case "server":
		runDaemon(configPath, verbose)
	case "run":
		runCmd(configPath, args[1:], verbose)
	case "status":
		statusCmd(configPath, args[1:])
	case "watch":
		watchCmd(configPath)
	case "visualize":
		visualizeCmd(configPath, args[1:])
	case "logs":
		logsCmd(configPath, args[1:])
	case "install-plist":
		if err := installPlist(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println("protomolecule " + version)
	case "--version":
		fmt.Println("protomolecule " + version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`protomolecule — scheduler and execution system for AI agent tasks

Usage:
  protomolecule server                 Start daemon (foreground)
  protomolecule run --task <id>        Force run a specific task
  protomolecule status                 Show daemon status
  protomolecule status --json          Show daemon status as JSON
  protomolecule watch                  Live-updating TUI dashboard (d=drill-down, l=logs, s=summary)
  protomolecule logs                   Filter and display log entries
  protomolecule logs --task <id>       Filter to a specific task
  protomolecule logs --run last        Show only the most recent run
  protomolecule logs --output          Show raw claude output lines only
  protomolecule logs --status failed   Filter to task_end entries by status
  protomolecule visualize              Generate workflow visualization
  protomolecule visualize --workflow <id>  Visualize a specific workflow
  protomolecule install-plist          Install launchd plist
  protomolecule version                Print version
  protomolecule help                   Print this help

Global flags:
  --config <path>    Path to protomolecule.yaml (default: ~/.protomolecule/protomolecule.yaml)
  --debug            Enable debug logging (shows exact claude commands being run)`)
}

// projectRoot returns the current working directory as the project root.
func projectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// runDaemon starts the scheduler in daemon (foreground) mode.
func runDaemon(configPath string, verbose bool) {
	logger := NewJSONLogger(os.Stderr, verbose)

	// Pre-flight: verify the claude binary is available.
	// Without it every task will fail with "fatal" status immediately.
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		logger.Error("claude binary not found in PATH — all tasks will fail",
			"error", err.Error(),
			"hint", "install claude CLI or add it to PATH before starting the daemon",
		)
		os.Exit(1)
	}
	logger.Info("pre-flight ok", "claude", claudePath)

	root := runtimeDir(configPath)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err.Error())
		os.Exit(1)
	}

	// Ensure runtime directories exist before writing state, socket, or logs.
	for _, sub := range []string{"state", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0755); err != nil {
			logger.Error("failed to create runtime directory", "dir", sub, "error", err.Error())
			os.Exit(1)
		}
	}

	// Resolve the agent working directory (where claude -p runs).
	workingDir, explicit, err := cfg.ResolveWorkingDir()
	if err != nil {
		logger.Error("failed to resolve working_dir", "error", err.Error())
		os.Exit(1)
	}
	if !explicit {
		logger.Warn("no working_dir configured; running agent tasks in the current directory",
			"working_dir", workingDir,
			"hint", "set working_dir in protomolecule.yaml to run agents in a specific project",
		)
	}
	logger.Info("paths resolved", "runtime_dir", root, "working_dir", workingDir)

	statePath := filepath.Join(root, "state", "task-state.json")
	state := NewStateStore(statePath)
	if err := state.Load(); err != nil {
		logger.Error("failed to load state", "error", err.Error())
		os.Exit(1)
	}

	clock := RealClock{}
	network := RealNetworkChecker{}
	liveOutputs := NewLiveOutputRegistry()
	executor := &ClaudeExecutor{ProjectRoot: workingDir, Logger: logger, LiveOutputs: liveOutputs}
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	scheduler := NewScheduler(cfg, state, pool, clock, network, logger)

	// Start status server
	socketPath := filepath.Join(root, "state", "protomolecule.sock")
	statusServer := NewStatusServer(socketPath, scheduler, state, pool, clock)
	statusServer.liveOutputs = liveOutputs
	statusServer.logFilePath = filepath.Join(root, "logs", "protomolecule.log")
	if err := statusServer.Start(); err != nil {
		logger.Error("failed to start status server", "error", err.Error())
		os.Exit(1)
	}

	// Start config watcher
	absConfigPath, _ := filepath.Abs(configPath)
	configWatcher, err := NewConfigWatcher(absConfigPath, func(newCfg *Config, err error) {
		if err != nil {
			logger.Error("config reload error (keeping current config)", "error", err.Error())
			scheduler.TrackConfigReload(false)
			return
		}
		// Log what changed and seed state for genuinely new tasks.
		oldCfg := scheduler.GetConfig()
		added := logConfigDiff(oldCfg, newCfg, logger)
		// Seed state for new tasks so isDue doesn't permanently skip them.
		// Without this, new tasks have nil state and isDue returns false forever.
		// Only seed tasks that are actually new in the config, not all tasks.
		now := clock.Now()
		seedNewTaskState(added, state, now, logger)
		scheduler.ReloadConfig(newCfg)
		scheduler.TrackConfigReload(true)
		logger.Info("config reloaded successfully")
	})
	if err != nil {
		logger.Warn("could not start config watcher", "error", err.Error())
	}

	// Count tasks
	taskCount := len(cfg.Standalone)
	for _, wf := range cfg.Workflows {
		taskCount += len(wf.Tasks)
	}

	logger.Info("started",
		"version", version,
		"config", configPath,
		"tasks", taskCount,
		"workers", cfg.WorkerSlots,
	)

	// Start scheduler in a goroutine
	ctx, cancel := context.WithCancel(context.Background())
	go scheduler.Start(ctx)

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("received shutdown signal, shutting down...")
	cancel()

	if configWatcher != nil {
		configWatcher.Close()
	}
	statusServer.Stop()
	pool.Shutdown()
	logger.Info("shutdown complete")
}

// runCmd handles the "run" subcommand.
func runCmd(configPath string, args []string, verbose bool) {
	if len(args) == 0 || args[0] != "--task" {
		fmt.Fprintf(os.Stderr, "error: 'run' requires --task <id>\n")
		os.Exit(1)
	}

	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "error: --task requires an task ID\n")
		os.Exit(1)
	}

	root := runtimeDir(configPath)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load config: %v\n", err)
		os.Exit(1)
	}

	workingDir, _, err := cfg.ResolveWorkingDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to resolve working_dir: %v\n", err)
		os.Exit(1)
	}

	statePath := filepath.Join(root, "state", "task-state.json")
	state := NewStateStore(statePath)
	if err := state.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load state: %v\n", err)
		os.Exit(1)
	}

	clock := RealClock{}
	network := RealNetworkChecker{}
	logger := NewJSONLogger(os.Stderr, verbose)
	executor := &ClaudeExecutor{ProjectRoot: workingDir, Logger: logger}
	pool := NewWorkerPool(cfg.WorkerSlots, executor, clock)
	scheduler := NewScheduler(cfg, state, pool, clock, network, DiscardLogger())

	taskID := args[1]
	fmt.Printf("Force running task %q...\n", taskID)
	if err := scheduler.ForceRunTask(taskID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		pool.Shutdown()
		os.Exit(1)
	}
	// Collect single result
	result := <-pool.Results()
	if result.Result.Status == "success" {
		fmt.Printf("Task %q completed successfully\n", taskID)
	} else {
		fmt.Fprintf(os.Stderr, "Task %q %s: %s\n", taskID, result.Result.Status, result.Result.Error)
		pool.Shutdown()
		os.Exit(1)
	}
	pool.Shutdown()
}

// watchCmd handles the "watch" subcommand — live-updating TUI dashboard.
func watchCmd(configPath string) {
	socketPath := filepath.Join(runtimeDir(configPath), "state", "protomolecule.sock")
	if err := RunTUI(socketPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// statusCmd handles the "status" subcommand.
func statusCmd(configPath string, args []string) {
	socketPath := filepath.Join(runtimeDir(configPath), "state", "protomolecule.sock")
	asJSON := false

	for _, arg := range args {
		switch arg {
		case "--json":
			asJSON = true
		}
	}

	client := NewStatusClient(socketPath)
	resp, err := client.FetchStatus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the protomolecule daemon running?\n")
		os.Exit(1)
	}

	if asJSON {
		data, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	} else {
		fmt.Print(FormatStatus(resp))
	}
}

// configTaskIDs extracts all task IDs from a config, keyed by ID with schedule as value.
func configTaskIDs(cfg *Config) map[string]string {
	ids := make(map[string]string)
	for _, wf := range cfg.Workflows {
		for _, t := range wf.Tasks {
			ids[t.ID] = wf.Schedule + " (workflow:" + wf.ID + ")"
		}
	}
	for _, t := range cfg.Standalone {
		ids[t.ID] = t.Schedule + " (standalone)"
	}
	return ids
}

// logConfigDiff compares old and new configs and logs added, removed, and
// modified tasks so operators can see exactly what a config reload changed.
// Returns the list of newly added task IDs (for state seeding).
func logConfigDiff(oldCfg, newCfg *Config, logger *slog.Logger) []string {
	oldTasks := configTaskIDs(oldCfg)
	newTasks := configTaskIDs(newCfg)

	// Find added tasks
	var added []string
	for id := range newTasks {
		if _, exists := oldTasks[id]; !exists {
			added = append(added, id)
		}
	}

	// Find removed tasks
	var removed []string
	for id := range oldTasks {
		if _, exists := newTasks[id]; !exists {
			removed = append(removed, id)
		}
	}

	// Find modified tasks (schedule or location changed)
	var modified []string
	for id, newSched := range newTasks {
		if oldSched, exists := oldTasks[id]; exists && oldSched != newSched {
			modified = append(modified, id)
		}
	}

	// Log global setting changes
	if oldCfg.WorkerSlots != newCfg.WorkerSlots {
		logger.Info("config change", "type", "config_change", "field", "worker_slots", "old", oldCfg.WorkerSlots, "new", newCfg.WorkerSlots)
	}
	if oldCfg.MaxRetries != newCfg.MaxRetries {
		logger.Info("config change", "type", "config_change", "field", "max_retries", "old", oldCfg.MaxRetries, "new", newCfg.MaxRetries)
	}
	if oldCfg.RetryDelaySeconds != newCfg.RetryDelaySeconds {
		logger.Info("config change", "type", "config_change", "field", "retry_delay_seconds", "old", oldCfg.RetryDelaySeconds, "new", newCfg.RetryDelaySeconds)
	}
	if oldCfg.TimeoutMinutes != newCfg.TimeoutMinutes {
		logger.Info("config change", "type", "config_change", "field", "timeout_minutes", "old", oldCfg.TimeoutMinutes, "new", newCfg.TimeoutMinutes)
	}
	if oldCfg.Timezone != newCfg.Timezone {
		logger.Info("config change", "type", "config_change", "field", "timezone", "old", oldCfg.Timezone, "new", newCfg.Timezone)
	}

	for _, id := range added {
		logger.Info("config change", "type", "config_change", "change", "task_added", "task_id", id, "schedule", newTasks[id])
	}
	for _, id := range removed {
		logger.Info("config change", "type", "config_change", "change", "task_removed", "task_id", id)
	}
	for _, id := range modified {
		logger.Info("config change", "type", "config_change", "change", "task_modified", "task_id", id, "old_schedule", oldTasks[id], "new_schedule", newTasks[id])
	}

	if len(added) == 0 && len(removed) == 0 && len(modified) == 0 {
		logger.Info("config change", "type", "config_change", "change", "none", "msg", "no task changes detected")
	}

	return added
}

// seedNewTaskState seeds state for newly added task IDs so that isDue
// will fire them on their next natural cron window rather than permanently
// skipping them. Only tasks in the addedIDs list are seeded.
//
// This is needed because isDue returns false when state is nil (a safety
// measure to prevent mass-firing on fresh startup). Without seeding, new
// tasks added to a running daemon via config reload would never fire.
func seedNewTaskState(addedIDs []string, state *StateStore, now time.Time, logger *slog.Logger) {
	for _, id := range addedIDs {
		state.UpdateTask(id, func(ts *TaskState) {
			ts.LastSuccess = &now
			ts.LastRunStatus = "seeded"
		})
		logger.Info("seeded state for new task",
			"type", "task_seeded",
			"task_id", id,
			"seeded_at", now.Format(time.RFC3339),
		)
	}
}

// visualizeCmd handles the "visualize" subcommand.
func visualizeCmd(configPath string, args []string) {
	root := runtimeDir(configPath)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load config: %v\n", err)
		os.Exit(1)
	}

	statePath := filepath.Join(root, "state", "task-state.json")
	state := NewStateStore(statePath)
	if err := state.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load state: %v\n", err)
		os.Exit(1)
	}

	clock := RealClock{}
	workflowFilter := ""

	for i := 0; i < len(args); i++ {
		if args[i] == "--workflow" && i+1 < len(args) {
			workflowFilter = args[i+1]
			i++
		}
	}

	outputPath := filepath.Join(root, "output", "workflows", "protomolecule.md")
	if err := WriteVisualization(cfg, state, clock, nil, workflowFilter, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Visualization written to %s\n", outputPath)
}
