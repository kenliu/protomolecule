package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// logEntry is a single parsed line from the protomolecule JSON log.
// The slog JSON handler writes flat key-value pairs alongside the standard
// "time", "level", and "msg" fields.
type logEntry struct {
	Time   string `json:"time"`
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Type   string `json:"type"`
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Line   string `json:"line"`
	Error  string `json:"error,omitempty"`
}

// logsCmd implements the "logs" subcommand.
// It reads protomolecule.log (line-delimited JSON), parses each entry,
// and filters/formats output according to the provided flags.
func logsCmd(args []string) {
	logPath := filepath.Join(runtimeDir(), "logs", "protomolecule.log")
	taskFilter := ""
	runFilter := ""
	outputOnly := false
	statusFilter := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--task":
			if i+1 < len(args) {
				taskFilter = args[i+1]
				i++
			}
		case "--run":
			if i+1 < len(args) {
				runFilter = args[i+1]
				i++
			}
		case "--output":
			outputOnly = true
		case "--status":
			if i+1 < len(args) {
				statusFilter = args[i+1]
				i++
			}
		case "--log":
			if i+1 < len(args) {
				logPath = args[i+1]
				i++
			}
		case "--help", "-h":
			fmt.Println(`protomolecule logs — filter and display log entries

Usage:
  protomolecule logs [flags]

Flags:
  --task <id>        Filter to entries for this task ID
  --run last         Show only the most recent run of the task
  --output           Show only raw claude output lines (the "line" field)
  --status <status>  Filter task_end entries by status (failed|success|timeout)
  --log <path>       Log file path (default: <runtime-dir>/logs/protomolecule.log)

Examples:
  # Clean claude output from the most recent slack-digest run
  protomolecule logs --task slack-digest --run last --output

  # List all failed task_end entries
  protomolecule logs --status failed

  # Show all log entries for a specific task
  protomolecule logs --task weekly-exec-report`)
			return
		}
	}

	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening log file %s: %v\n", logPath, err)
		os.Exit(1)
	}
	defer f.Close()

	// Parse all entries from the log file.
	var entries []logEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			// Skip unparseable lines silently.
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error reading log file: %v\n", err)
		os.Exit(1)
	}

	// Apply --task filter.
	if taskFilter != "" {
		var filtered []logEntry
		for _, e := range entries {
			if e.TaskID == taskFilter {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	// Apply --run last: find the last task_start for this task and keep
	// everything from that point forward.
	if runFilter == "last" {
		entries = filterLastRun(entries)
	}

	// Apply --status filter: keep only task_end entries with matching status.
	if statusFilter != "" {
		var filtered []logEntry
		for _, e := range entries {
			if e.Type == "task_end" && e.Status == statusFilter {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	// Output entries.
	for _, e := range entries {
		if outputOnly {
			if e.Type == "claude_output" && e.Line != "" {
				fmt.Println(e.Line)
			}
		} else {
			data, _ := json.Marshal(e)
			fmt.Println(string(data))
		}
	}
}

// filterLastRun returns entries from the most recent run.
// A run begins at the last task_start entry and ends at the subsequent
// task_end entry (inclusive). If no task_start is found, all entries
// are returned unchanged.
func filterLastRun(entries []logEntry) []logEntry {
	// Find the index of the last task_start.
	lastStartIdx := -1
	for i, e := range entries {
		if e.Type == "task_start" {
			lastStartIdx = i
		}
	}
	if lastStartIdx == -1 {
		return entries
	}
	return entries[lastStartIdx:]
}

// readTaskOutputFromLog reads the protomolecule log file and extracts the
// claude output lines from the last run of the given task ID.
// Returns the output lines (plain text, not JSON) and any error.
func readTaskOutputFromLog(logPath, taskID string) ([]string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []logEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.TaskID == taskID {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Get last run only
	entries = filterLastRun(entries)

	// Extract output lines
	var lines []string
	for _, e := range entries {
		if e.Type == "claude_output" && e.Line != "" {
			lines = append(lines, e.Line)
		}
	}
	return lines, nil
}
