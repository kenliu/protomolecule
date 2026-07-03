package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// runSummary holds parsed analysis of a completed run's stream-json output.
type runSummary struct {
	Model      string
	SessionID  string
	StartTime  string
	Duration   string
	DurationAPI string
	NumTurns   int
	StopReason string
	Status     string // success, error, or empty if no result event found
	Result     string // final result text
	Cost       float64

	// Usage
	InputTokens              int
	OutputTokens             int
	CacheReadTokens          int
	CacheCreationTokens      int

	// Tool usage
	ToolCalls  []toolCallInfo
	ToolErrors []toolErrorInfo

	// API retries
	APIRetries []apiRetryInfo
}

// toolCallInfo records a tool invocation.
type toolCallInfo struct {
	ID    string
	Name  string
	Input string // truncated input summary
}

// toolErrorInfo records a tool error.
type toolErrorInfo struct {
	ToolName  string
	ToolUseID string
	Error     string
	Context   string // e.g. the command that failed for Bash
}

// apiRetryInfo records an API retry event.
type apiRetryInfo struct {
	Attempt    int
	MaxRetries int
	Error      string
	Time       string
}

// parseRunSummary parses stream-json lines (from claude --output-format stream-json)
// and extracts a structured summary for display.
func parseRunSummary(lines []string) runSummary {
	var s runSummary
	toolNames := make(map[string]string) // tool_use_id -> tool name
	toolInputs := make(map[string]string) // tool_use_id -> input summary

	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		subtype, _ := event["subtype"].(string)

		switch {
		case eventType == "system" && subtype == "init":
			if model, ok := event["model"].(string); ok {
				s.Model = model
			}
			if sid, ok := event["session_id"].(string); ok {
				s.SessionID = sid
			}

		case eventType == "system" && subtype == "api_retry":
			ri := apiRetryInfo{
				Error: fmt.Sprintf("%v", event["error"]),
			}
			if a, ok := event["attempt"].(float64); ok {
				ri.Attempt = int(a)
			}
			if m, ok := event["max_retries"].(float64); ok {
				ri.MaxRetries = int(m)
			}
			s.APIRetries = append(s.APIRetries, ri)

		case eventType == "assistant":
			msg, ok := event["message"].(map[string]any)
			if !ok {
				continue
			}
			content, _ := msg["content"].([]any)
			for _, c := range content {
				block, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] == "tool_use" {
					name, _ := block["name"].(string)
					id, _ := block["id"].(string)
					if id != "" {
						toolNames[id] = name
					}
					tc := toolCallInfo{ID: id, Name: name}
					// Summarize input
					if inp, ok := block["input"].(map[string]any); ok {
						tc.Input = summarizeToolInput(name, inp)
						toolInputs[id] = tc.Input
					}
					s.ToolCalls = append(s.ToolCalls, tc)
				}
			}

		case eventType == "user":
			msg, ok := event["message"].(map[string]any)
			if !ok {
				continue
			}
			content, _ := msg["content"].([]any)
			for _, c := range content {
				block, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if block["type"] == "tool_result" {
					isError, _ := block["is_error"].(bool)
					if isError {
						toolUseID, _ := block["tool_use_id"].(string)
						errContent, _ := block["content"].(string)
						te := toolErrorInfo{
							ToolUseID: toolUseID,
							ToolName:  toolNames[toolUseID],
							Error:     errContent,
							Context:   toolInputs[toolUseID],
						}
						s.ToolErrors = append(s.ToolErrors, te)
					}
				}
			}

		case eventType == "result":
			if subtype == "success" {
				s.Status = "success"
			} else {
				s.Status = subtype
			}
			if r, ok := event["result"].(string); ok {
				s.Result = r
			}
			if d, ok := event["duration_ms"].(float64); ok {
				s.Duration = formatDuration(time.Duration(d) * time.Millisecond)
			}
			if d, ok := event["duration_api_ms"].(float64); ok {
				s.DurationAPI = formatDuration(time.Duration(d) * time.Millisecond)
			}
			if n, ok := event["num_turns"].(float64); ok {
				s.NumTurns = int(n)
			}
			if sr, ok := event["stop_reason"].(string); ok {
				s.StopReason = sr
			}
			if c, ok := event["total_cost_usd"].(float64); ok {
				s.Cost = c
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				if v, ok := usage["input_tokens"].(float64); ok {
					s.InputTokens = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					s.OutputTokens = int(v)
				}
				if v, ok := usage["cache_read_input_tokens"].(float64); ok {
					s.CacheReadTokens = int(v)
				}
				if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
					s.CacheCreationTokens = int(v)
				}
			}
		}
	}

	return s
}

// summarizeToolInput returns a short description of a tool call's input.
func summarizeToolInput(toolName string, input map[string]any) string {
	switch toolName {
	case "Read":
		if fp, ok := input["file_path"].(string); ok {
			return shortPath(fp)
		}
	case "Write":
		if fp, ok := input["file_path"].(string); ok {
			return shortPath(fp)
		}
	case "Edit":
		if fp, ok := input["file_path"].(string); ok {
			return shortPath(fp)
		}
	case "Glob":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			if len(cmd) > 60 {
				return cmd[:60] + "..."
			}
			return cmd
		}
	case "Agent", "Task":
		if desc, ok := input["description"].(string); ok {
			return desc
		}
	}
	return ""
}

// shortPath trims the working directory (the daemon's project root) or the
// home directory prefix to show a concise, relative path. It falls back to the
// absolute path if neither matches.
func shortPath(p string) string {
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		if rel := stripPrefixDir(p, cwd); rel != "" {
			return rel
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel := stripPrefixDir(p, home); rel != "" {
			return "~/" + rel
		}
	}
	return p
}

// stripPrefixDir returns p relative to dir if p is under dir, or "" otherwise.
func stripPrefixDir(p, dir string) string {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	if strings.HasPrefix(p, dir) {
		return p[len(dir):]
	}
	return ""
}

// formatRunSummary renders a runSummary as a human-readable multi-line string.
func formatRunSummary(s runSummary, width int) string {
	var b strings.Builder

	// Header
	b.WriteString("  RUN SUMMARY\n")
	b.WriteString("  " + strings.Repeat("─", 40) + "\n\n")

	// Basic info
	if s.Model != "" {
		fmt.Fprintf(&b, "  Model:       %s\n", s.Model)
	}
	if s.Status != "" {
		fmt.Fprintf(&b, "  Status:      %s\n", s.Status)
	}
	if s.Duration != "" {
		dur := s.Duration
		if s.DurationAPI != "" {
			dur += fmt.Sprintf(" (API: %s)", s.DurationAPI)
		}
		fmt.Fprintf(&b, "  Duration:    %s\n", dur)
	}
	if s.NumTurns > 0 {
		fmt.Fprintf(&b, "  Turns:       %d\n", s.NumTurns)
	}
	if s.StopReason != "" {
		fmt.Fprintf(&b, "  Stop reason: %s\n", s.StopReason)
	}
	if s.Cost > 0 {
		fmt.Fprintf(&b, "  Cost:        $%.4f\n", s.Cost)
	}

	// Token usage
	if s.InputTokens > 0 || s.OutputTokens > 0 {
		b.WriteString("\n  TOKENS\n")
		b.WriteString("  " + strings.Repeat("─", 40) + "\n")
		fmt.Fprintf(&b, "  Input:          %s\n", formatTokenCount(s.InputTokens))
		fmt.Fprintf(&b, "  Output:         %s\n", formatTokenCount(s.OutputTokens))
		if s.CacheReadTokens > 0 {
			fmt.Fprintf(&b, "  Cache read:     %s\n", formatTokenCount(s.CacheReadTokens))
		}
		if s.CacheCreationTokens > 0 {
			fmt.Fprintf(&b, "  Cache creation: %s\n", formatTokenCount(s.CacheCreationTokens))
		}
	}

	// Tool usage summary
	if len(s.ToolCalls) > 0 {
		counts := make(map[string]int)
		for _, tc := range s.ToolCalls {
			counts[tc.Name]++
		}
		b.WriteString("\n  TOOL USAGE\n")
		b.WriteString("  " + strings.Repeat("─", 40) + "\n")
		// Sort by count descending
		type toolCount struct {
			name  string
			count int
		}
		var sorted []toolCount
		for name, count := range counts {
			sorted = append(sorted, toolCount{name, count})
		}
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].count > sorted[i].count {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		for _, tc := range sorted {
			// Count errors for this tool
			errCount := 0
			for _, te := range s.ToolErrors {
				if te.ToolName == tc.name {
					errCount++
				}
			}
			if errCount > 0 {
				fmt.Fprintf(&b, "  %-18s %3d calls (%d errors)\n", tc.name, tc.count, errCount)
			} else {
				fmt.Fprintf(&b, "  %-18s %3d calls\n", tc.name, tc.count)
			}
		}
	}

	// Tool errors
	if len(s.ToolErrors) > 0 {
		b.WriteString("\n  ERRORS\n")
		b.WriteString("  " + strings.Repeat("─", 40) + "\n")
		for i, te := range s.ToolErrors {
			label := te.ToolName
			if label == "" {
				label = "unknown"
			}
			errText := te.Error
			maxLen := width - 10
			if maxLen < 40 {
				maxLen = 40
			}
			if len(errText) > maxLen {
				errText = errText[:maxLen] + "..."
			}
			if te.Context != "" {
				fmt.Fprintf(&b, "  %d. [%s] %s\n     %s\n", i+1, label, errText, te.Context)
			} else {
				fmt.Fprintf(&b, "  %d. [%s] %s\n", i+1, label, errText)
			}
		}
	}

	// API retries
	if len(s.APIRetries) > 0 {
		b.WriteString("\n  API RETRIES\n")
		b.WriteString("  " + strings.Repeat("─", 40) + "\n")
		for _, r := range s.APIRetries {
			fmt.Fprintf(&b, "  attempt %d/%d: %s\n", r.Attempt, r.MaxRetries, r.Error)
		}
	}

	// Result
	if s.Result != "" {
		b.WriteString("\n  RESULT\n")
		b.WriteString("  " + strings.Repeat("─", 40) + "\n")
		// Word-wrap the result text
		maxWidth := width - 4
		if maxWidth < 40 {
			maxWidth = 40
		}
		for _, line := range wrapText(s.Result, maxWidth) {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	return b.String()
}

// formatTokenCount formats a token count with thousands separators.
func formatTokenCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1000000)
}

// wrapText wraps text at word boundaries to fit within maxWidth.
func wrapText(text string, maxWidth int) []string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		current := words[0]
		for _, w := range words[1:] {
			if len(current)+1+len(w) > maxWidth {
				lines = append(lines, current)
				current = w
			} else {
				current += " " + w
			}
		}
		lines = append(lines, current)
	}
	return lines
}
