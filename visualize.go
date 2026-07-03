package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenerateVisualization creates a Markdown document with a Mermaid flowchart
// of the configured workflows, color-coded by current task state.
// If workflowFilter is non-empty, only that workflow is included.
func GenerateVisualization(cfg *Config, state *StateStore, clock Clock, pool *WorkerPool, workflowFilter string) string {
	now := clock.Now()

	var b strings.Builder
	b.WriteString("# Protomolecule Workflows\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", now.Format("2006-01-02 15:04:05"))

	// Gather active job IDs for "running" state detection
	activeIDs := make(map[string]bool)
	if pool != nil {
		for _, aj := range pool.ActiveJobs() {
			activeIDs[aj.Job.Task.ID] = true
		}
	}

	// Collect all task IDs and their CSS classes
	classes := make(map[string]string) // task ID -> CSS class name

	// Determine class for a task
	classFor := func(taskID string, schedule string) string {
		if activeIDs[taskID] {
			return "running"
		}
		st := state.GetTask(taskID)
		if st != nil && st.LastRunStatus == "success" {
			return "success"
		}
		if st != nil && st.LastRunStatus == "failed" {
			return "failed"
		}
		if strings.EqualFold(schedule, "on-demand") {
			return "ondemand"
		}
		return "never"
	}

	b.WriteString("```mermaid\n")
	b.WriteString("flowchart TD\n")

	// Track standalone tasks (those not in any workflow)
	workflowTaskIDs := make(map[string]bool)

	// Render workflows
	for _, wf := range cfg.Workflows {
		if workflowFilter != "" && wf.ID != workflowFilter {
			continue
		}

		scheduleLabel := wf.Schedule
		if scheduleLabel == "" {
			scheduleLabel = "on-demand"
		}

		fmt.Fprintf(&b, "    subgraph %s [\"%s — %s\"]\n", sanitizeMermaidID(wf.ID), wf.ID, scheduleLabel)

		for _, act := range wf.Tasks {
			workflowTaskIDs[act.ID] = true
			cls := classFor(act.ID, wf.Schedule)
			classes[act.ID] = cls

			if act.DependsOn != "" {
				fmt.Fprintf(&b, "        %s --> %s\n", sanitizeMermaidID(act.DependsOn), sanitizeMermaidID(act.ID))
			} else {
				fmt.Fprintf(&b, "        %s\n", sanitizeMermaidID(act.ID))
			}
		}

		b.WriteString("    end\n\n")
	}

	// Render standalone tasks (skip if filtering to a specific workflow)
	if workflowFilter == "" {
		var standaloneTasks []TaskConfig
		for _, act := range cfg.Standalone {
			if !workflowTaskIDs[act.ID] {
				standaloneTasks = append(standaloneTasks, act)
			}
		}

		if len(standaloneTasks) > 0 {
			b.WriteString("    subgraph standalone [\"Standalone\"]\n")
			for _, act := range standaloneTasks {
				cls := classFor(act.ID, act.Schedule)
				classes[act.ID] = cls
				fmt.Fprintf(&b, "        %s\n", sanitizeMermaidID(act.ID))
			}
			b.WriteString("    end\n\n")
		}
	}

	// Emit class assignments
	for id, cls := range classes {
		fmt.Fprintf(&b, "class %s %s\n", sanitizeMermaidID(id), cls)
	}

	// Emit classDef declarations after all nodes and class assignments
	b.WriteString("classDef success fill:#27AE60,color:#fff\n")
	b.WriteString("classDef failed fill:#E74C3C,color:#fff\n")
	b.WriteString("classDef running fill:#F39C12,color:#fff\n")
	b.WriteString("classDef never fill:#95A5A6,color:#fff\n")
	b.WriteString("classDef ondemand fill:#BDC3C7,color:#333\n")

	b.WriteString("```\n")

	return b.String()
}

// sanitizeMermaidID replaces characters that are problematic in Mermaid node IDs.
func sanitizeMermaidID(id string) string {
	// Mermaid node IDs are fine with hyphens and alphanumerics.
	// Replace any other problematic characters with underscores.
	return strings.ReplaceAll(id, " ", "_")
}

// WriteVisualization generates a visualization and writes it to the specified output path.
// If outputPath is empty, it defaults to "output/workflows/protomolecule.md".
func WriteVisualization(cfg *Config, state *StateStore, clock Clock, pool *WorkerPool, workflowFilter string, outputPath string) error {
	if outputPath == "" {
		outputPath = "output/workflows/protomolecule.md"
	}

	content := GenerateVisualization(cfg, state, clock, pool, workflowFilter)

	// Ensure the output directory exists
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating output directory %s: %w", dir, err)
	}

	if err := os.WriteFile(outputPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing visualization to %s: %w", outputPath, err)
	}

	return nil
}
