package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// StatusResponse is the JSON payload returned by the status server.
type StatusResponse struct {
	Uptime           string           `json:"uptime"`
	TickCount        int64            `json:"tick_count"`
	Timestamp        string           `json:"timestamp"`
	ConfigValid      bool             `json:"config_valid"`
	ConfigLastReload string           `json:"config_last_reload,omitempty"`
	ConfigReloadOK   *bool            `json:"config_reload_ok,omitempty"`
	Paused           bool             `json:"paused"`
	Active           []ActiveJobInfo  `json:"active"`
	Queued           []QueuedJobInfo  `json:"queued"`
	Recent           []RecentRunInfo  `json:"recent"`
	Upcoming         []UpcomingInfo   `json:"upcoming"`
	OnDemand         []string         `json:"on_demand"`
}

// ActiveJobInfo describes a currently running job.
type ActiveJobInfo struct {
	TaskID string `json:"task_id"`
	StartedAt  string `json:"started_at"`
	Elapsed    string `json:"elapsed"`
	Attempt    int    `json:"attempt"`
	MaxAttempt int    `json:"max_attempt"`
}

// QueuedJobInfo describes a job waiting to run.
type QueuedJobInfo struct {
	TaskID string `json:"task_id"`
	Reason     string `json:"reason"`
}

// RecentRunInfo describes a recently completed task.
type RecentRunInfo struct {
	TaskID string `json:"task_id"`
	Status     string `json:"status"`
	RunAt      string `json:"run_at"`
	Duration   string `json:"duration"`
	Error      string `json:"error,omitempty"`
}

// UpcomingInfo describes a future scheduled firing.
type UpcomingInfo struct {
	TaskID string `json:"task_id"`
	NextRun    string `json:"next_run"`
	InDuration string `json:"in_duration"`
}

// statusRequest is the JSON request sent over the socket.
type statusRequest struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"`
}

// runResponse is the JSON response for a run request.
type runResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// JobOutputResponse is the JSON response for a job_output request.
type JobOutputResponse struct {
	TaskID  string   `json:"task_id"`
	Lines   []string `json:"lines"`
	Running bool     `json:"running"`
}

// StatusServer listens on a Unix socket and responds to status queries.
type StatusServer struct {
	socketPath       string
	listener         net.Listener
	scheduler        *Scheduler
	state            *StateStore
	pool             *WorkerPool
	clock            Clock
	liveOutputs      *LiveOutputRegistry // optional; nil = job_output not supported
	logFilePath      string              // path to protomolecule.log for historical log queries
	done             chan struct{}
	configLastReload time.Time
	configReloadOK   *bool
	configMu         sync.Mutex
}

// NewStatusServer creates a new StatusServer.
func NewStatusServer(socketPath string, scheduler *Scheduler, state *StateStore, pool *WorkerPool, clock Clock) *StatusServer {
	return &StatusServer{
		socketPath: socketPath,
		scheduler:  scheduler,
		state:      state,
		pool:       pool,
		clock:      clock,
		done:       make(chan struct{}),
	}
}

// TrackConfigReload records the result of a config reload attempt.
func (ss *StatusServer) TrackConfigReload(ok bool) {
	ss.configMu.Lock()
	defer ss.configMu.Unlock()
	ss.configLastReload = ss.clock.Now()
	b := ok
	ss.configReloadOK = &b
}

// Start begins listening on the Unix socket.
// It performs stale socket recovery: if the socket file exists, it tries to connect.
// If the connection is refused, the stale socket is removed. If the connection
// succeeds, another instance is running and an error is returned.
func (ss *StatusServer) Start() error {
	// Check for existing socket file
	if _, err := os.Stat(ss.socketPath); err == nil {
		// Socket file exists — try to connect to see if it's alive
		conn, err := net.DialTimeout("unix", ss.socketPath, 2*time.Second)
		if err != nil {
			// Connection refused — stale socket from dead instance
			os.Remove(ss.socketPath)
		} else {
			// Connection succeeded — another instance is running
			conn.Close()
			return fmt.Errorf("another protomolecule instance is already running (socket %s is active)", ss.socketPath)
		}
	}

	ln, err := net.Listen("unix", ss.socketPath)
	if err != nil {
		return fmt.Errorf("listening on unix socket %s: %w", ss.socketPath, err)
	}

	// Set socket permissions to 0600
	if err := os.Chmod(ss.socketPath, 0600); err != nil {
		ln.Close()
		os.Remove(ss.socketPath)
		return fmt.Errorf("setting socket permissions: %w", err)
	}

	ss.listener = ln

	go ss.acceptLoop()

	return nil
}

func (ss *StatusServer) acceptLoop() {
	defer close(ss.done)
	for {
		conn, err := ss.listener.Accept()
		if err != nil {
			// Listener was closed
			return
		}
		go ss.handleConnection(conn)
	}
}

func (ss *StatusServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Set read deadline — use real time, not Clock, since this is actual I/O
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}

	var req statusRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		return
	}

	switch req.Type {
	case "status":
		resp := ss.buildStatusResponse()
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		data = append(data, '\n')
		conn.Write(data)
	case "run":
		resp := runResponse{OK: true}
		if req.TaskID == "" {
			resp = runResponse{OK: false, Error: "task_id is required"}
		} else if err := ss.scheduler.ForceRunTask(req.TaskID); err != nil {
			resp = runResponse{OK: false, Error: err.Error()}
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		data = append(data, '\n')
		conn.Write(data)
	case "pause":
		ss.scheduler.SetPaused(!ss.scheduler.IsPaused())
		resp := runResponse{OK: true}
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		data = append(data, '\n')
		conn.Write(data)
	case "job_output":
		resp := JobOutputResponse{TaskID: req.TaskID}
		if ss.liveOutputs != nil && req.TaskID != "" {
			if buf, ok := ss.liveOutputs.Get(req.TaskID); ok {
				resp.Lines = buf.Lines()
				resp.Running = true
			}
		}
		if resp.Lines == nil {
			resp.Lines = []string{}
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		data = append(data, '\n')
		conn.Write(data)
	case "task_logs":
		// Read historical logs for a completed task from the log file.
		resp := JobOutputResponse{TaskID: req.TaskID}
		if req.TaskID != "" && ss.logFilePath != "" {
			lines, err := readTaskOutputFromLog(ss.logFilePath, req.TaskID)
			if err == nil {
				resp.Lines = lines
			}
		}
		// Check if the task is currently running
		resp.Running = ss.pool.IsActive(req.TaskID)
		if resp.Lines == nil {
			resp.Lines = []string{}
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return
		}
		data = append(data, '\n')
		conn.Write(data)
	}
}

func (ss *StatusServer) buildStatusResponse() StatusResponse {
	now := ss.clock.Now()
	cfg := ss.scheduler.GetConfig()

	resp := StatusResponse{
		Uptime:      formatDuration(now.Sub(ss.scheduler.GetStartedAt())),
		TickCount:   ss.scheduler.GetTickCount(),
		Timestamp:   now.Format(time.RFC3339),
		ConfigValid: cfg != nil,
		Paused:      ss.scheduler.IsPaused(),
	}

	// Populate config reload info
	ss.configMu.Lock()
	if !ss.configLastReload.IsZero() {
		resp.ConfigLastReload = ss.configLastReload.Format(time.RFC3339)
	}
	if ss.configReloadOK != nil {
		b := *ss.configReloadOK
		resp.ConfigReloadOK = &b
	}
	ss.configMu.Unlock()

	// Active jobs — sort by start time (oldest first), then task ID for stable order.
	activeJobs := ss.pool.ActiveJobs()
	sort.Slice(activeJobs, func(i, j int) bool {
		if !activeJobs[i].StartedAt.Equal(activeJobs[j].StartedAt) {
			return activeJobs[i].StartedAt.Before(activeJobs[j].StartedAt)
		}
		return activeJobs[i].Job.Task.ID < activeJobs[j].Job.Task.ID
	})
	activeIDs := make(map[string]bool)
	for _, aj := range activeJobs {
		activeIDs[aj.Job.Task.ID] = true
		resp.Active = append(resp.Active, ActiveJobInfo{
			TaskID: aj.Job.Task.ID,
			StartedAt:  aj.StartedAt.Format("2006-01-02 15:04:05"),
			Elapsed:    formatElapsed(now.Sub(aj.StartedAt)),
			Attempt:    aj.Job.Attempt,
			MaxAttempt: aj.Job.MaxAttempts,
		})
	}

	// Queued: tasks whose depends_on parent is currently running
	if cfg != nil {
		for _, wf := range cfg.Workflows {
			for _, act := range wf.Tasks {
				if act.DependsOn != "" && activeIDs[act.DependsOn] {
					resp.Queued = append(resp.Queued, QueuedJobInfo{
						TaskID: act.ID,
						Reason:     "waiting on: " + act.DependsOn,
					})
				}
			}
		}
	}

	// Build a set of workflow sentinel IDs — these are workflow-level IDs used only
	// for isDue() tracking (set by updateWorkflowStateOnLeafSuccess). They never
	// represent real task executions and should not appear in the Recent list.
	workflowSentinels := make(map[string]bool)
	if cfg != nil {
		for _, wf := range cfg.Workflows {
			workflowSentinels[wf.ID] = true
		}
	}

	// Recent runs: collect from per-task run history, show last 10 across all tasks.
	allState := ss.state.GetAll()
	type runItem struct {
		taskID string
		entry  RunEntry
		isFail bool
		consec int
	}
	var allRuns []runItem
	for id, st := range allState {
		// Skip workflow sentinel IDs — they track scheduling state, not real executions.
		if workflowSentinels[id] {
			continue
		}
		for _, r := range st.RecentRuns {
			allRuns = append(allRuns, runItem{
				taskID: id,
				entry:  r,
				isFail: st.LastRunStatus == "failed" && st.ConsecutiveFailures > 0 && r.RunAt.Equal(*st.LastRun),
				consec: st.ConsecutiveFailures,
			})
		}
		// Fall back to LastRun for tasks that ran before history was added.
		if len(st.RecentRuns) == 0 && st.LastRun != nil {
			allRuns = append(allRuns, runItem{
				taskID: id,
				entry:  RunEntry{Status: st.LastRunStatus, RunAt: *st.LastRun, Duration: st.LastRunDuration},
				isFail: st.LastRunStatus == "failed" && st.ConsecutiveFailures > 0,
				consec: st.ConsecutiveFailures,
			})
		}
	}
	sort.Slice(allRuns, func(i, j int) bool {
		if !allRuns[i].entry.RunAt.Equal(allRuns[j].entry.RunAt) {
			return allRuns[i].entry.RunAt.After(allRuns[j].entry.RunAt)
		}
		return allRuns[i].taskID < allRuns[j].taskID
	})
	const maxRecent = 10
	if len(allRuns) > maxRecent {
		allRuns = allRuns[:maxRecent]
	}
	for _, r := range allRuns {
		info := RecentRunInfo{
			TaskID:   r.taskID,
			Status:   r.entry.Status,
			RunAt:    r.entry.RunAt.Format("2006-01-02 15:04:05"),
		}
		if r.entry.Duration > 0 {
			info.Duration = formatDuration(r.entry.Duration)
		}
		if r.entry.Status == "fatal" {
			info.Error = "fatal: binary not found or infrastructure error (not retried)"
		} else if r.isFail {
			maxRetries := 3 // default display
			if cfg != nil {
				maxRetries = cfg.MaxRetries + 1
			}
			info.Error = fmt.Sprintf("exhausted %d/%d attempts", r.consec, maxRetries)
		}
		resp.Recent = append(resp.Recent, info)
	}

	// On-demand tasks: collect all tasks with on-demand or empty schedule
	if cfg != nil {
		seen := make(map[string]bool)
		for _, wf := range cfg.Workflows {
			if strings.EqualFold(wf.Schedule, "on-demand") || wf.Schedule == "" {
				for _, act := range wf.Tasks {
					if !seen[act.ID] {
						seen[act.ID] = true
						resp.OnDemand = append(resp.OnDemand, act.ID)
					}
				}
			}
		}
		for _, act := range cfg.Standalone {
			if strings.EqualFold(act.Schedule, "on-demand") || act.Schedule == "" {
				if !seen[act.ID] {
					seen[act.ID] = true
					resp.OnDemand = append(resp.OnDemand, act.ID)
				}
			}
		}
		sort.Strings(resp.OnDemand)
	}

	// Upcoming: compute next firing for each task
	if cfg != nil {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		loc := cfg.Location()

		type upcomingEntry struct {
			id      string
			nextRun time.Time
		}
		var entries []upcomingEntry

		// Workflow tasks use the workflow's schedule
		for _, wf := range cfg.Workflows {
			if strings.EqualFold(wf.Schedule, "on-demand") || wf.Schedule == "" {
				continue
			}
			sched, err := parser.Parse(wf.Schedule)
			if err != nil {
				continue
			}
			nextRun := sched.Next(now.In(loc))
			for _, act := range wf.Tasks {
				entries = append(entries, upcomingEntry{
					id:      act.ID,
					nextRun: nextRun,
				})
			}
		}

		// Standalone tasks
		for _, act := range cfg.Standalone {
			if strings.EqualFold(act.Schedule, "on-demand") || act.Schedule == "" {
				continue
			}
			sched, err := parser.Parse(act.Schedule)
			if err != nil {
				continue
			}
			nextRun := sched.Next(now.In(loc))
			entries = append(entries, upcomingEntry{
				id:      act.ID,
				nextRun: nextRun,
			})
		}

		// Sort by next run time, then by task ID for stable ordering when times are equal.
		sort.Slice(entries, func(i, j int) bool {
			if !entries[i].nextRun.Equal(entries[j].nextRun) {
				return entries[i].nextRun.Before(entries[j].nextRun)
			}
			return entries[i].id < entries[j].id
		})

		for _, e := range entries {
			resp.Upcoming = append(resp.Upcoming, UpcomingInfo{
				TaskID: e.id,
				NextRun:    e.nextRun.Format(time.RFC3339),
				InDuration: formatDuration(e.nextRun.Sub(now)),
			})
		}
	}

	return resp
}

// Stop closes the listener and removes the socket file.
func (ss *StatusServer) Stop() {
	if ss.listener != nil {
		ss.listener.Close()
		<-ss.done
	}
	os.Remove(ss.socketPath)
}

// StatusClient connects to a running StatusServer via Unix socket.
type StatusClient struct {
	socketPath string
}

// NewStatusClient creates a new StatusClient.
func NewStatusClient(socketPath string) *StatusClient {
	return &StatusClient{socketPath: socketPath}
}

// FetchStatus connects to the daemon, sends a status request, and returns the response.
func (sc *StatusClient) FetchStatus() (*StatusResponse, error) {
	conn, err := net.DialTimeout("unix", sc.socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := statusRequest{Type: "status"}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		return nil, fmt.Errorf("empty response from daemon")
	}

	var resp StatusResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}

// RunTask sends a run request to the daemon to force-run a task.
func (sc *StatusClient) RunTask(taskID string) error {
	conn, err := net.DialTimeout("unix", sc.socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := statusRequest{Type: "run", TaskID: taskID}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading response: %w", err)
		}
		return fmt.Errorf("empty response from daemon")
	}

	var resp runResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	return nil
}

// Pause sends a pause toggle request to the daemon.
func (sc *StatusClient) Pause() error {
	conn, err := net.DialTimeout("unix", sc.socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := statusRequest{Type: "pause"}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("sending request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading response: %w", err)
		}
		return fmt.Errorf("empty response from daemon")
	}

	var resp runResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// FetchJobOutput queries the daemon for the live output of a running task.
func (sc *StatusClient) FetchJobOutput(taskID string) (*JobOutputResponse, error) {
	conn, err := net.DialTimeout("unix", sc.socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	req := statusRequest{Type: "job_output", TaskID: taskID}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		return nil, fmt.Errorf("empty response from daemon")
	}

	var resp JobOutputResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}

// FetchTaskLogs queries the daemon for the historical log output of a completed task.
// It reuses JobOutputResponse since the shape is identical.
func (sc *StatusClient) FetchTaskLogs(taskID string) (*JobOutputResponse, error) {
	conn, err := net.DialTimeout("unix", sc.socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second)) // longer timeout — log file can be big

	req := statusRequest{Type: "task_logs", TaskID: taskID}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024) // large buffer for big log responses
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		return nil, fmt.Errorf("empty response from daemon")
	}

	var resp JobOutputResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}

// FormatStatus renders a StatusResponse as a human-readable string.
func FormatStatus(r *StatusResponse) string {
	var b strings.Builder

	// Header line
	if r.Paused {
		fmt.Fprintf(&b, "Protomolecule — uptime %s    [PAUSED]\n", r.Uptime)
	} else {
		fmt.Fprintf(&b, "Protomolecule — uptime %s\n", r.Uptime)
	}
	if r.ConfigLastReload != "" {
		reloadStatus := "ok"
		if r.ConfigReloadOK != nil && !*r.ConfigReloadOK {
			reloadStatus = "FAILED"
		}
		fmt.Fprintf(&b, "Config last reloaded: %s (%s)\n", r.ConfigLastReload, reloadStatus)
	}
	b.WriteString("\n")

	// ACTIVE section
	if len(r.Active) > 0 {
		b.WriteString("ACTIVE\n")
		for _, a := range r.Active {
			fmt.Fprintf(&b, "  %-28s running    started %s    %s    attempt %d/%d\n",
				a.TaskID, a.StartedAt, a.Elapsed, a.Attempt, a.MaxAttempt)
		}
		b.WriteString("\n")
	}

	// QUEUED section
	if len(r.Queued) > 0 {
		b.WriteString("QUEUED\n")
		for _, q := range r.Queued {
			fmt.Fprintf(&b, "  %-28s %s\n", q.TaskID, q.Reason)
		}
		b.WriteString("\n")
	}

	// UPCOMING section
	if len(r.Upcoming) > 0 {
		b.WriteString("UPCOMING\n")
		for _, u := range r.Upcoming {
			fmt.Fprintf(&b, "  %-28s in %s\n", u.TaskID, u.InDuration)
		}
		b.WriteString("\n")
	}

	// RECENT section
	if len(r.Recent) > 0 {
		b.WriteString("RECENT\n")
		for _, r := range r.Recent {
			line := fmt.Sprintf("  %-28s %-10s %s", r.TaskID, r.Status, r.RunAt)
			if r.Duration != "" {
				line += fmt.Sprintf("    duration %s", r.Duration)
			}
			if r.Error != "" {
				line += fmt.Sprintf("    (%s)", r.Error)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// formatDuration renders a duration in a human-friendly form like "4h 23m" or "2d 14h".
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	if days > 0 {
		if hours > 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}
	if hours > 0 {
		if minutes > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// formatElapsed renders elapsed time as M:SS.
func formatElapsed(d time.Duration) string {
	totalSeconds := int(d.Seconds())
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}
