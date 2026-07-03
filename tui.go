package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
)

// TUI message types.

type tickMsg time.Time

type statusMsg struct {
	status *StatusResponse
}

type errMsg struct {
	err error
}

type runResultMsg struct {
	taskID string
	err    error
}

type pauseResultMsg struct {
	err error
}

type jobOutputMsg struct {
	taskID  string
	lines   []string
	running bool
	err     error
}

// tuiMode represents the current interaction mode.
type tuiMode int

const (
	modeView         tuiMode = iota // Normal viewing mode
	modeRun                         // Selecting a task to run
	modeSelectActive                // Pick which active job to drill into
	modeLiveOutput                  // View live output of one job
	modeSelectRecent                // Pick which recent job to view logs for
)

// tuiModel holds the TUI state.
type tuiModel struct {
	client     *StatusClient
	status     *StatusResponse
	err        error
	lastUpdate time.Time
	quitting   bool
	width      int
	height     int

	// Modal state
	mode    tuiMode
	cursor  int      // index into taskIDs (only used in modeRun)
	taskIDs []string // flat list of all task IDs from status

	// Live output drill-down state
	liveOutputTaskID  string
	liveOutputLines   []string
	liveOutputRunning bool
	liveOutputScroll  int  // 0 = follow (bottom), >0 = lines scrolled up from bottom
	liveOutputSummary bool // true = show parsed summary, false = show raw output
	drillCursor       int  // cursor for modeSelectActive
	recentCursor      int  // cursor for modeSelectRecent

	// Flash message
	flash   string
	flashAt time.Time
}

// TUI styles.
var (
	headerStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("63")).
			Padding(0, 1)

	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("255"))

	activeIcon   = lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Render("●") // yellow
	queuedIcon   = lipgloss.NewStyle().Faint(true).Render("○")
	upcomingIcon = lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Render("◇") // blue
	successIcon  = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓") // green
	failedIcon   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗") // red
	fatalIcon    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render("☠") // red bold

	dimStyle      = lipgloss.NewStyle().Faint(true)
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	hintStyle     = lipgloss.NewStyle().Faint(true)
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true)
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	flashStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	flashErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	modeBanner    = lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true)
)

func doTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func fetchStatus(client *StatusClient) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.FetchStatus()
		if err != nil {
			return errMsg{err: err}
		}
		return statusMsg{status: resp}
	}
}

func runTask(client *StatusClient, taskID string) tea.Cmd {
	return func() tea.Msg {
		err := client.RunTask(taskID)
		return runResultMsg{taskID: taskID, err: err}
	}
}

func pauseToggle(client *StatusClient) tea.Cmd {
	return func() tea.Msg {
		err := client.Pause()
		return pauseResultMsg{err: err}
	}
}

func fetchJobOutput(client *StatusClient, taskID string) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.FetchJobOutput(taskID)
		if err != nil {
			return jobOutputMsg{taskID: taskID, err: err}
		}
		return jobOutputMsg{taskID: taskID, lines: resp.Lines, running: resp.Running}
	}
}

func fetchTaskLogs(client *StatusClient, taskID string) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.FetchTaskLogs(taskID)
		if err != nil {
			return jobOutputMsg{taskID: taskID, err: err}
		}
		return jobOutputMsg{taskID: taskID, lines: resp.Lines, running: resp.Running}
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(fetchStatus(m.client), doTick())
}

// buildTaskIDs extracts all task IDs from the status response in display order.
// On-demand tasks are appended at the end (they are only shown in run mode).
func buildTaskIDs(s *StatusResponse) []string {
	if s == nil {
		return nil
	}
	seen := make(map[string]bool)
	var ids []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, a := range s.Active {
		add(a.TaskID)
	}
	for _, q := range s.Queued {
		add(q.TaskID)
	}
	for _, u := range s.Upcoming {
		add(u.TaskID)
	}
	for _, r := range s.Recent {
		add(r.TaskID)
	}
	for _, id := range s.OnDemand {
		add(id)
	}
	return ids
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		key := msg.String()

		switch m.mode {
		case modeRun:
			return m.updateRunMode(key)
		case modeSelectActive:
			return m.updateSelectActiveMode(key)
		case modeSelectRecent:
			return m.updateSelectRecentMode(key)
		case modeLiveOutput:
			return m.updateLiveOutputMode(key)
		default:
			return m.updateViewMode(key)
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		// Clear flash after 5 seconds
		if m.flash != "" && time.Since(m.flashAt) > 5*time.Second {
			m.flash = ""
		}
		cmds := []tea.Cmd{fetchStatus(m.client), doTick()}
		if m.mode == modeLiveOutput && m.liveOutputTaskID != "" && m.liveOutputRunning {
			cmds = append(cmds, fetchJobOutput(m.client, m.liveOutputTaskID))
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.status = msg.status
		m.err = nil
		m.lastUpdate = time.Now()
		m.taskIDs = buildTaskIDs(msg.status)
		// Clamp cursor if list shrank
		if m.cursor >= len(m.taskIDs) && len(m.taskIDs) > 0 {
			m.cursor = len(m.taskIDs) - 1
		}

	case jobOutputMsg:
		if msg.err == nil {
			m.liveOutputLines = msg.lines
			m.liveOutputRunning = msg.running
		}

	case runResultMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Failed to run %s: %s", msg.taskID, msg.err)
		} else {
			m.flash = fmt.Sprintf("Started %s", msg.taskID)
		}
		m.flashAt = time.Now()
		return m, fetchStatus(m.client)

	case pauseResultMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Pause failed: %s", msg.err)
			m.flashAt = time.Now()
		}
		return m, fetchStatus(m.client)

	case errMsg:
		m.err = msg.err
	}

	return m, nil
}

func (m tuiModel) updateViewMode(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "r":
		if len(m.taskIDs) > 0 {
			m.mode = modeRun
			m.cursor = 0
		}
	case "p":
		return m, pauseToggle(m.client)
	case "l":
		if m.status == nil || len(m.status.Recent) == 0 {
			m.flash = "No recent runs"
			m.flashAt = time.Now()
		} else if len(m.status.Recent) == 1 {
			m.liveOutputTaskID = m.status.Recent[0].TaskID
			m.liveOutputLines = nil
			m.liveOutputRunning = false
			m.liveOutputScroll = 0
			m.liveOutputSummary = true // default to summary for completed runs
			m.mode = modeLiveOutput
			return m, fetchTaskLogs(m.client, m.liveOutputTaskID)
		} else {
			m.recentCursor = 0
			m.mode = modeSelectRecent
		}
	case "d":
		if m.status == nil || len(m.status.Active) == 0 {
			m.flash = "No active jobs"
			m.flashAt = time.Now()
		} else if len(m.status.Active) == 1 {
			m.liveOutputTaskID = m.status.Active[0].TaskID
			m.liveOutputLines = nil
			m.liveOutputRunning = true
			m.liveOutputScroll = 0
			m.mode = modeLiveOutput
			return m, fetchJobOutput(m.client, m.liveOutputTaskID)
		} else {
			m.drillCursor = 0
			m.mode = modeSelectActive
		}
	}
	return m, nil
}

func (m tuiModel) updateSelectActiveMode(key string) (tea.Model, tea.Cmd) {
	active := m.status.Active
	switch key {
	case "esc", "escape", "q":
		m.mode = modeView
	case "j", "down":
		if len(active) > 0 {
			m.drillCursor = (m.drillCursor + 1) % len(active)
		}
	case "k", "up":
		if len(active) > 0 {
			m.drillCursor = (m.drillCursor - 1 + len(active)) % len(active)
		}
	case "enter":
		if m.drillCursor < len(active) {
			m.liveOutputTaskID = active[m.drillCursor].TaskID
			m.liveOutputLines = nil
			m.liveOutputRunning = true
			m.liveOutputScroll = 0
			m.mode = modeLiveOutput
			return m, fetchJobOutput(m.client, m.liveOutputTaskID)
		}
	}
	return m, nil
}

func (m tuiModel) updateSelectRecentMode(key string) (tea.Model, tea.Cmd) {
	recent := m.status.Recent
	switch key {
	case "esc", "escape", "q":
		m.mode = modeView
	case "j", "down":
		if len(recent) > 0 {
			m.recentCursor = (m.recentCursor + 1) % len(recent)
		}
	case "k", "up":
		if len(recent) > 0 {
			m.recentCursor = (m.recentCursor - 1 + len(recent)) % len(recent)
		}
	case "enter":
		if m.recentCursor < len(recent) {
			m.liveOutputTaskID = recent[m.recentCursor].TaskID
			m.liveOutputLines = nil
			m.liveOutputRunning = false
			m.liveOutputScroll = 0
			m.liveOutputSummary = true // default to summary for completed runs
			m.mode = modeLiveOutput
			return m, fetchTaskLogs(m.client, m.liveOutputTaskID)
		}
	}
	return m, nil
}

func (m tuiModel) updateLiveOutputMode(key string) (tea.Model, tea.Cmd) {
	maxLines := m.height - 6
	if maxLines < 1 {
		maxLines = 1
	}
	switch key {
	case "esc", "escape", "q":
		m.mode = modeView
		m.liveOutputScroll = 0
		m.liveOutputSummary = false
	case "s":
		// Toggle summary view (only for completed runs)
		if !m.liveOutputRunning {
			m.liveOutputSummary = !m.liveOutputSummary
			m.liveOutputScroll = 0
		}
	case "k", "up":
		// Scroll up — increase offset from bottom
		viewLines := m.summaryOrOutputLines()
		maxScroll := len(viewLines) - maxLines
		if maxScroll < 0 {
			maxScroll = 0
		}
		if m.liveOutputScroll < maxScroll {
			m.liveOutputScroll++
		}
	case "j", "down":
		// Scroll down — decrease offset from bottom
		if m.liveOutputScroll > 0 {
			m.liveOutputScroll--
		}
	case "g":
		// Jump to top
		viewLines := m.summaryOrOutputLines()
		maxScroll := len(viewLines) - maxLines
		if maxScroll > 0 {
			m.liveOutputScroll = maxScroll
		}
	case "G":
		// Jump to bottom (follow)
		m.liveOutputScroll = 0
	}
	return m, nil
}

// summaryOrOutputLines returns the lines to display based on the current view mode.
func (m tuiModel) summaryOrOutputLines() []string {
	if m.liveOutputSummary && !m.liveOutputRunning {
		summary := parseRunSummary(m.liveOutputLines)
		text := formatRunSummary(summary, m.width)
		return strings.Split(text, "\n")
	}
	return m.liveOutputLines
}

func (m tuiModel) updateRunMode(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "escape", "q":
		m.mode = modeView
	case "j", "down":
		if len(m.taskIDs) > 0 {
			m.cursor = (m.cursor + 1) % len(m.taskIDs)
		}
	case "k", "up":
		if len(m.taskIDs) > 0 {
			m.cursor = (m.cursor - 1 + len(m.taskIDs)) % len(m.taskIDs)
		}
	case "enter":
		if m.cursor < len(m.taskIDs) {
			taskID := m.taskIDs[m.cursor]
			m.flash = fmt.Sprintf("Running %s...", taskID)
			m.flashAt = time.Now()
			m.mode = modeView
			return m, runTask(m.client, taskID)
		}
	}
	return m, nil
}

// selectedTaskID returns the task ID under the cursor in run mode.
func (m tuiModel) selectedTaskID() string {
	if m.mode == modeRun && m.cursor < len(m.taskIDs) {
		return m.taskIDs[m.cursor]
	}
	return ""
}

func (m tuiModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}

	if m.mode == modeLiveOutput {
		return m.viewLiveOutput()
	}
	if m.mode == modeSelectActive {
		return m.viewSelectActive()
	}
	if m.mode == modeSelectRecent {
		return m.viewSelectRecent()
	}

	var b strings.Builder

	if m.err != nil && m.status == nil {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("  Cannot connect to daemon") + "\n")
		if m.client != nil {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  socket: %s", m.client.socketPath)) + "\n")
		}
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  Retrying...") + "\n")
		b.WriteString("\n")
		b.WriteString(hintStyle.Render("  q to quit") + "\n")
		v := tea.NewView(b.String())
		v.AltScreen = true
		return v
	}

	if m.status == nil {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  Connecting to daemon...") + "\n")
		b.WriteString("\n")
		b.WriteString(hintStyle.Render("  q to quit") + "\n")
		v := tea.NewView(b.String())
		v.AltScreen = true
		return v
	}

	s := m.status
	selected := m.selectedTaskID()

	// cursorRendered ensures only the first occurrence of a task ID across all sections
	// gets the cursor highlight. Without this, tasks appearing in both UPCOMING and RECENT
	// (e.g. slack-digest) would show the cursor in both places simultaneously.
	cursorRendered := false
	renderRow := func(line, taskID string) string {
		isSelected := taskID == selected && !cursorRendered
		if isSelected {
			cursorRendered = true
		}
		return m.renderLine(line, isSelected)
	}

	// Header box
	lastUpdateAgo := "just now"
	if !m.lastUpdate.IsZero() {
		elapsed := time.Since(m.lastUpdate)
		if elapsed >= time.Second {
			lastUpdateAgo = fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
		}
	}

	headerContent := fmt.Sprintf("Protomolecule    uptime %s    tick #%d    last update: %s",
		s.Uptime, s.TickCount, lastUpdateAgo)

	if s.Paused {
		headerContent += "    " + lipgloss.NewStyle().Foreground(lipgloss.Color("226")).Bold(true).Render("⏸ PAUSED")
	}
	if m.err != nil {
		headerContent += "    " + errorStyle.Render("[connection error]")
	}

	headerWidth := m.width - 4
	if headerWidth < 60 {
		headerWidth = 60
	}
	if headerWidth > 120 {
		headerWidth = 120
	}

	b.WriteString(headerStyle.Width(headerWidth).Render(headerContent))
	b.WriteString("\n\n")

	// Run mode banner
	if m.mode == modeRun {
		b.WriteString(modeBanner.Render("  ▸ SELECT TASK TO RUN") + "\n\n")
	}

	// Flash message
	if m.flash != "" {
		style := flashStyle
		if strings.HasPrefix(m.flash, "Failed") {
			style = flashErrStyle
		}
		b.WriteString("  " + style.Render(m.flash) + "\n\n")
	}

	// ACTIVE section
	if len(s.Active) > 0 {
		b.WriteString(sectionStyle.Render(" ACTIVE") + "\n")
		for _, a := range s.Active {
			line := fmt.Sprintf("  %s %-28s %s    attempt %d/%d",
				activeIcon, a.TaskID, a.Elapsed, a.Attempt, a.MaxAttempt)
			b.WriteString(renderRow(line, a.TaskID))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// QUEUED section
	if len(s.Queued) > 0 {
		b.WriteString(sectionStyle.Render(" QUEUED") + "\n")
		for _, q := range s.Queued {
			line := fmt.Sprintf("  %s %-28s %s",
				queuedIcon, q.TaskID, dimStyle.Render(q.Reason))
			b.WriteString(renderRow(line, q.TaskID))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// UPCOMING section
	if len(s.Upcoming) > 0 {
		b.WriteString(sectionStyle.Render(" UPCOMING") + "\n")
		for _, u := range s.Upcoming {
			nextAt := u.NextRun
			if t, err := time.Parse(time.RFC3339, u.NextRun); err == nil {
				nextAt = t.Local().Format("Mon Jan 2 15:04")
			}
			line := fmt.Sprintf("  %s %-28s %s %s",
				upcomingIcon, u.TaskID, dimStyle.Render(nextAt), dimStyle.Render("(in "+u.InDuration+")"))
			b.WriteString(renderRow(line, u.TaskID))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// RECENT section
	if len(s.Recent) > 0 {
		b.WriteString(sectionStyle.Render(" RECENT") + "\n")
		for _, r := range s.Recent {
			icon := successIcon
			switch r.Status {
			case "failed", "timeout":
				icon = failedIcon
			case "fatal":
				icon = fatalIcon
			}
			line := fmt.Sprintf("  %s %-28s %-10s %s", icon, r.TaskID, r.Status, r.RunAt)
			if r.Duration != "" {
				line += fmt.Sprintf("    %s", r.Duration)
			}
			if r.Error != "" {
				line += "    " + dimStyle.Render("("+r.Error+")")
			}
			b.WriteString(renderRow(line, r.TaskID))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// ON-DEMAND section — only shown in run mode
	if m.mode == modeRun && len(s.OnDemand) > 0 {
		b.WriteString(sectionStyle.Render(" ON-DEMAND") + "\n")
		for _, id := range s.OnDemand {
			line := fmt.Sprintf("  %s %-28s %s",
				upcomingIcon, id, dimStyle.Render("manual trigger"))
			b.WriteString(renderRow(line, id))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// No data indicator
	if len(s.Active) == 0 && len(s.Queued) == 0 && len(s.Upcoming) == 0 && len(s.Recent) == 0 {
		b.WriteString(dimStyle.Render("  No task data") + "\n\n")
	}

	// Hint bar
	if m.mode == modeRun {
		b.WriteString(hintStyle.Render("  ↑/↓ select    enter run    esc cancel") + "\n")
	} else {
		pauseHint := "p pause"
		if s.Paused {
			pauseHint = "p unpause"
		}
		hints := fmt.Sprintf("  r run task    %s    q quit", pauseHint)
		if len(s.Active) > 0 {
			hints = fmt.Sprintf("  r run task    d drill-down    %s    q quit", pauseHint)
		}
		if len(s.Recent) > 0 {
			hints = strings.Replace(hints, "q quit", "l logs    q quit", 1)
		}
		b.WriteString(hintStyle.Render(hints) + "\n")
	}

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

// renderLine renders a task line, highlighting it if isSelected is true.
func (m tuiModel) renderLine(line string, isSelected bool) string {
	if isSelected {
		return cursorStyle.Render("▸") + selectedStyle.Render(line[1:])
	}
	return line
}

// viewSelectActive renders the active-job selection overlay for drill-down.
func (m tuiModel) viewSelectActive() tea.View {
	var b strings.Builder

	b.WriteString(modeBanner.Render("  ▸ SELECT JOB TO DRILL INTO") + "\n\n")

	if m.status != nil {
		for i, a := range m.status.Active {
			line := fmt.Sprintf("  %s %-28s %s    attempt %d/%d",
				activeIcon, a.TaskID, a.Elapsed, a.Attempt, a.MaxAttempt)
			b.WriteString(m.renderLine(line, i == m.drillCursor))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("  ↑/↓ select    enter drill-down    esc back") + "\n")

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

// viewSelectRecent renders the recent-job selection overlay for log viewing.
func (m tuiModel) viewSelectRecent() tea.View {
	var b strings.Builder

	b.WriteString(modeBanner.Render("  ▸ SELECT TASK TO VIEW LOGS") + "\n\n")

	if m.status != nil {
		for i, r := range m.status.Recent {
			icon := successIcon
			switch r.Status {
			case "failed", "timeout":
				icon = failedIcon
			case "fatal":
				icon = fatalIcon
			}
			line := fmt.Sprintf("  %s %-28s %-10s %s", icon, r.TaskID, r.Status, r.RunAt)
			if r.Duration != "" {
				line += fmt.Sprintf("    %s", r.Duration)
			}
			b.WriteString(m.renderLine(line, i == m.recentCursor))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("  ↑/↓ select    enter view logs    esc back") + "\n")

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

// viewLiveOutput renders the live output view for a running job.
func (m tuiModel) viewLiveOutput() tea.View {
	var b strings.Builder

	// Header
	headerLabel := "Live output"
	if m.liveOutputSummary {
		headerLabel = "Run summary"
	} else if !m.liveOutputRunning {
		headerLabel = "Log output"
	}
	header := fmt.Sprintf("%s: %s", headerLabel, m.liveOutputTaskID)
	if m.status != nil {
		for _, a := range m.status.Active {
			if a.TaskID == m.liveOutputTaskID {
				header += fmt.Sprintf("    elapsed %s    attempt %d/%d", a.Elapsed, a.Attempt, a.MaxAttempt)
				break
			}
		}
	}
	if !m.liveOutputRunning {
		header += "    [completed]"
	}

	headerWidth := m.width - 4
	if headerWidth < 60 {
		headerWidth = 60
	}
	if headerWidth > 120 {
		headerWidth = 120
	}
	b.WriteString(headerStyle.Width(headerWidth).Render(header))
	b.WriteString("\n\n")

	// Output lines — show a viewport of N lines with scroll support
	allLines := m.summaryOrOutputLines()
	maxLines := m.height - 6 // reserve space for header + hint bar
	if maxLines < 1 {
		maxLines = 1
	}

	if len(allLines) == 0 {
		b.WriteString(dimStyle.Render("  Waiting for output...") + "\n")
	} else {
		// Calculate the window to display based on scroll offset
		endIdx := len(allLines) - m.liveOutputScroll
		if endIdx < 0 {
			endIdx = 0
		}
		startIdx := endIdx - maxLines
		if startIdx < 0 {
			startIdx = 0
		}
		for _, line := range allLines[startIdx:endIdx] {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")

	// Hint bar with scroll indicator
	scrollIndicator := "[FOLLOW]"
	if m.liveOutputScroll > 0 {
		visibleEnd := len(allLines) - m.liveOutputScroll
		scrollIndicator = fmt.Sprintf("[line %d/%d]", visibleEnd, len(allLines))
	}
	summaryHint := ""
	if !m.liveOutputRunning {
		if m.liveOutputSummary {
			summaryHint = "s raw output    "
		} else {
			summaryHint = "s summary    "
		}
	}
	b.WriteString(hintStyle.Render(fmt.Sprintf("  ↑/↓ scroll    g top    G bottom    %sesc back    %s", summaryHint, scrollIndicator)) + "\n")

	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

// RunTUI starts the live TUI, connecting to the daemon at socketPath.
func RunTUI(socketPath string) error {
	client := NewStatusClient(socketPath)
	model := tuiModel{
		client: client,
	}

	p := tea.NewProgram(model)
	_, err := p.Run()
	return err
}
