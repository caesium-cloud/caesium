package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type status int

type section int

const (
	statusLoading status = iota
	statusReady
	statusError
)

const (
	sectionJobs section = iota
	sectionTriggers
	sectionAtoms
)

func (s section) next() section {
	return section((int(s) + 1) % 3)
}

func (s section) prev() section {
	return section((int(s) + 2) % 3)
}

// Model represents the Bubble Tea program state.
type Model struct {
	client          *api.Client
	spinner         spinner.Model
	state           status
	err             error
	active          section
	jobs            table.Model
	triggers        table.Model
	atoms           table.Model
	jobRecords      []api.Job
	jobRunStatus    map[string]*api.Run
	viewportWidth   int
	viewportHeight  int
	selectedJobID   string
	jobDetail       *api.JobDetail
	graph           *dag.Graph
	taskRunStatus   map[string]api.RunTask
	focusedNodeID   string
	dagLayout       string
	dagFocusPath    bool
	dagViewport     viewport.Model
	dagErr          error
	detailErr       error
	logCtx          context.Context
	logCancel       context.CancelFunc
	logStream       io.ReadCloser
	logTaskID       string
	showLogsModal   bool
	logsViewport    viewport.Model
	logsLoading     bool
	logsErr         error
	logContent      string
	logCache        map[string]string
	logSince        map[string]time.Time
	logLastLine     map[string]string
	atomDetails     map[string]*api.Atom
	atomErr         error
	loadingAtomID   string
	atomIndex       map[string]api.Atom
	showDetail      bool
	detailLoading   bool
	triggeringJobID string
	actionNotice    string
	actionErr       error
}

// New creates the root model with dependency references.
func New(client *api.Client) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	dagViewport := viewport.New(60, 12)
	logsViewport := viewport.New(60, 10)

	jobs := createTable(jobColumnTitles, []int{18, 12, 22, 22, 20, 20}, true)
	triggers := createTable(triggerColumnTitles, []int{20, 12, 20}, false)
	atoms := createTable(atomColumnTitles, []int{24, 12, 20}, false)

	return Model{
		client:        client,
		spinner:       sp,
		state:         statusLoading,
		active:        sectionJobs,
		jobs:          jobs,
		triggers:      triggers,
		atoms:         atoms,
		dagViewport:   dagViewport,
		logsViewport:  logsViewport,
		jobRunStatus:  make(map[string]*api.Run),
		logCache:      make(map[string]string),
		logSince:      make(map[string]time.Time),
		logLastLine:   make(map[string]string),
		atomDetails:   make(map[string]*api.Atom),
		atomIndex:     make(map[string]api.Atom),
		taskRunStatus: make(map[string]api.RunTask),
	}
}

// Init bootstraps async fetch and spinner tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchData(m.client))
}

// Update handles Bubble Tea messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.showLogsModal {
			switch msg.String() {
			case "ctrl+c", "q", "esc", "g":
				m.stopLogStream(true)
				m.showLogsModal = false
				return m, nil
			case "up", "k":
				m.logsViewport.ScrollUp(1)
				return m, nil
			case "down", "j":
				m.logsViewport.ScrollDown(1)
				return m, nil
			case "pgup":
				m.logsViewport.ScrollUp(m.logsViewport.Height / 2)
				return m, nil
			case "pgdown":
				m.logsViewport.ScrollDown(m.logsViewport.Height / 2)
				m.logsViewport.GotoBottom()
				return m, nil
			}
		}
		switch msg.String() {
		case "ctrl+c", "q":
			if m.showDetail {
				m.showDetail = false
				m.detailLoading = false
				m.stopLogStream(false)
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.showDetail {
				m.showDetail = false
				m.detailLoading = false
				m.stopLogStream(false)
				return m, nil
			}
		case "r":
			m.state = statusLoading
			m.err = nil
			m.jobs.SetRows(nil)
			m.triggers.SetRows(nil)
			m.atoms.SetRows(nil)
			m.jobRecords = nil
			m.jobRunStatus = make(map[string]*api.Run)
			m.selectedJobID = ""
			m.jobDetail = nil
			m.resetDetailView(nil, nil, true)
			m.taskRunStatus = make(map[string]api.RunTask)
			m.stopLogStream(false)
			m.clearTriggeringJob("")
			m.setActionStatus("", nil)
			cmds = append(cmds, m.spinner.Tick, fetchData(m.client))
		case "1":
			m = m.activate(sectionJobs)
		case "2":
			m = m.activate(sectionTriggers)
		case "3":
			m = m.activate(sectionAtoms)
		case "tab":
			if m.showDetail {
				if cmd := m.cycleFocusedNode(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
			} else {
				m = m.activate(m.active.next())
			}
		case "shift+tab":
			if m.showDetail {
				if cmd := m.cycleFocusedNode(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
			} else {
				m = m.activate(m.active.prev())
			}
		case "right", "l":
			if m.showDetail {
				if id := m.nextSuccessor(); id != "" {
					if cmd := m.setFocusedNode(id); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		case "left", "h":
			if m.showDetail {
				if id := m.firstPredecessor(); id != "" {
					if cmd := m.setFocusedNode(id); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		case "up", "k":
			if m.showDetail {
				m.dagViewport.ScrollUp(1)
			}
		case "down", "j":
			if m.showDetail {
				m.dagViewport.ScrollDown(1)
			}
		case "f":
			if m.showDetail {
				m.dagFocusPath = !m.dagFocusPath
				m.refreshDAGLayout(false)
			}
		case "g":
			if m.showDetail {
				if cmd := m.toggleLogs(); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		case "enter":
			if m.active == sectionJobs && m.state == statusReady {
				m.showDetail = true
				if m.jobDetail == nil {
					m.detailLoading = true
					if id := m.currentJobID(); id != "" {
						cmds = append(cmds, fetchJobDetail(m.client, id, true))
					}
				} else {
					m.detailLoading = false
				}
			}
		case "t":
			if cmd := m.triggerSelectedJob(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case tea.WindowSizeMsg:
		height := max(5, msg.Height-7)
		width := max(20, msg.Width-8)
		m.viewportWidth = msg.Width
		m.viewportHeight = msg.Height
		m.jobs.SetHeight(height)
		m.triggers.SetHeight(height)
		m.atoms.SetHeight(height)
		m.jobs.SetWidth(width)
		m.triggers.SetWidth(width)
		m.atoms.SetWidth(width)
		m.resizeDAGViewport()
		m.resizeLogViewport()
		if m.graph != nil {
			m.refreshDAGLayout(false)
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.hasAnimatedJobStatus() {
			m.refreshJobRows()
		}
		if m.hasAnimatedTaskStatus() && m.graph != nil {
			m.refreshDAGLayout(false)
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dataLoadedMsg:
		m.state = statusReady
		m.err = nil
		m.jobRecords = msg.jobs
		m.jobRunStatus = make(map[string]*api.Run)
		m.refreshJobRows()
		m.triggers.SetRows(triggersToRows(msg.triggers))
		m.atoms.SetRows(atomsToRows(msg.atoms))
		m = m.activate(sectionJobs)
		m.resetDetailView(nil, nil, true)
		m.atomIndex = make(map[string]api.Atom)
		for _, atom := range msg.atoms {
			m.atomIndex[atom.ID] = atom
		}
		for _, job := range msg.jobs {
			cmds = append(cmds, fetchLatestRun(m.client, job.ID))
		}
		if id := m.currentJobID(); id != "" {
			m.selectedJobID = id
			cmds = append(cmds, fetchJobDetail(m.client, id, true))
		}
	case jobDetailLoadedMsg:
		m.detailLoading = false
		if msg.detail == nil {
			m.jobDetail = nil
			m.resetDetailView(nil, nil, true)
			break
		}
		m.jobDetail = msg.detail
		m.resetDetailView(nil, nil, false)
		m.setJobStatus(msg.detail.Job.ID, msg.detail.LatestRun)
		m.taskRunStatus = make(map[string]api.RunTask)
		if msg.detail.LatestRun != nil {
			for _, task := range msg.detail.LatestRun.Tasks {
				m.taskRunStatus[task.ID] = task
			}
		}

		if msg.detail.DAG != nil {
			graph, err := dag.FromJobDAG(msg.detail.DAG)
			if err != nil {
				m.dagErr = err
			} else {
				m.graph = graph
				if root := graph.First(); root != nil {
					if cmd := m.setFocusedNode(root.ID()); cmd != nil {
						cmds = append(cmds, cmd)
					}
				} else {
					m.focusedNodeID = ""
				}
			}
		} else {
			m.graph = nil
			m.focusedNodeID = ""
		}
		m.refreshDAGLayout(true)
	case jobDetailErrMsg:
		m.resetDetailView(msg.err, msg.err, true)
	case jobStatusLoadedMsg:
		m.setJobStatus(msg.jobID, msg.run)
		if msg.run != nil && m.jobDetail != nil && m.jobDetail.Job.ID == msg.jobID {
			m.taskRunStatus = make(map[string]api.RunTask)
			for _, task := range msg.run.Tasks {
				m.taskRunStatus[task.ID] = task
			}
			if m.graph != nil {
				m.refreshDAGLayout(false)
			}
		}
	case jobStatusErrMsg:
		m.setJobStatus(msg.jobID, nil)
	case logsOpenedMsg:
		m.logsLoading = false
		m.logsErr = nil
		m.logStream = msg.reader
		if m.logCtx == nil {
			m.logCtx = msg.ctx
		}
		if msg.reader != nil && m.logCtx != nil {
			cmds = append(cmds, readLogChunk(m.logCtx, msg.reader))
		}
	case logChunkMsg:
		if msg.reader != nil && msg.reader == m.logStream {
			m.appendLogs(msg.data)
			if m.logCtx != nil {
				cmds = append(cmds, readLogChunk(m.logCtx, msg.reader))
			}
		}
	case logsClosedMsg:
		m.logsLoading = false
		if msg.err != nil && strings.TrimSpace(m.logContent) == "" {
			m.logsErr = msg.err
		}
		m.stopLogStream(true)
	case atomDetailLoadedMsg:
		if msg.atom != nil {
			if m.atomDetails == nil {
				m.atomDetails = make(map[string]*api.Atom)
			}
			m.atomDetails[msg.atom.ID] = msg.atom
		}
		if m.loadingAtomID == msg.id {
			m.loadingAtomID = ""
		}
		m.atomErr = nil
	case atomDetailErrMsg:
		if m.loadingAtomID == msg.id {
			m.loadingAtomID = ""
		}
		m.atomErr = msg.err
	case jobTriggeredMsg:
		m.clearTriggeringJob(msg.jobID)
		notice := "Run accepted"
		if msg.run != nil {
			notice = fmt.Sprintf("Run %s accepted", shortID(msg.run.ID))
		}
		m.setJobStatus(msg.jobID, msg.run)
		m.setActionStatus(notice, nil)
		if m.state == statusReady {
			if id := m.currentJobID(); id != "" && id == msg.jobID {
				m.detailLoading = true
				cmds = append(cmds, fetchJobDetail(m.client, id, true))
			}
		}
	case jobTriggerErrMsg:
		m.clearTriggeringJob(msg.jobID)
		m.setActionStatus("", msg.err)
	case errMsg:
		m.state = statusError
		m.err = msg
	}

	if m.state != statusReady {
		return m, tea.Batch(cmds...)
	}

	switch m.active {
	case sectionJobs:
		if !m.showDetail {
			var tableCmd tea.Cmd
			m.jobs, tableCmd = m.jobs.Update(msg)
			if tableCmd != nil {
				cmds = append(cmds, tableCmd)
			}
			if id := m.currentJobID(); id != "" && id != m.selectedJobID {
				m.selectedJobID = id
				m.jobDetail = nil
				m.resetDetailView(nil, nil, false)
				cmds = append(cmds, fetchJobDetail(m.client, id, true))
			}
		} else if m.jobDetail == nil {
			if id := m.currentJobID(); id != "" {
				cmds = append(cmds, fetchJobDetail(m.client, id, true))
			}
		}
	case sectionTriggers:
		var tableCmd tea.Cmd
		m.triggers, tableCmd = m.triggers.Update(msg)
		if tableCmd != nil {
			cmds = append(cmds, tableCmd)
		}
	case sectionAtoms:
		var tableCmd tea.Cmd
		m.atoms, tableCmd = m.atoms.Update(msg)
		if tableCmd != nil {
			cmds = append(cmds, tableCmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m Model) activate(sec section) Model {
	m.jobs.Blur()
	m.triggers.Blur()
	m.atoms.Blur()
	switch sec {
	case sectionJobs:
		m.jobs.Focus()
	case sectionTriggers:
		m.triggers.Focus()
	case sectionAtoms:
		m.atoms.Focus()
	}
	if sec != sectionJobs {
		m.showDetail = false
		m.detailLoading = false
		m.stopLogStream(false)
		m.showLogsModal = false
	}
	m.active = sec
	return m
}

func (m Model) currentJobID() string {
	row := m.jobs.SelectedRow()
	if len(row) < 5 {
		return ""
	}
	return row[4]
}

func (m Model) currentJobAlias() string {
	row := m.jobs.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

func (m *Model) setJobStatus(jobID string, run *api.Run) {
	if jobID == "" {
		return
	}
	if m.jobRunStatus == nil {
		m.jobRunStatus = make(map[string]*api.Run)
	}
	if run == nil {
		delete(m.jobRunStatus, jobID)
	} else {
		m.jobRunStatus[jobID] = run
	}
	if len(m.jobRecords) > 0 {
		m.refreshJobRows()
	}
}

func (m *Model) refreshJobRows() {
	m.jobs.SetRows(jobsToRows(m.jobRecords, m.jobRunStatus, m.spinner.View()))
}

func (m Model) hasAnimatedJobStatus() bool {
	for _, run := range m.jobRunStatus {
		if run == nil {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(run.Status))
		if status == "running" || status == "pending" {
			return true
		}
	}
	return false
}

func (m Model) hasAnimatedTaskStatus() bool {
	for _, task := range m.taskRunStatus {
		status := strings.ToLower(strings.TrimSpace(task.Status))
		if status == "running" || status == "pending" {
			return true
		}
	}
	return false
}

func logKey(runID, taskID string) string {
	return fmt.Sprintf("%s:%s", runID, taskID)
}

func extractLastTimestamp(chunk string) time.Time {
	lines := strings.Split(chunk, "\n")
	var latest time.Time
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		token := strings.Trim(fields[0], "/[]")
		if ts, err := time.Parse(time.RFC3339Nano, token); err == nil {
			if ts.After(latest) {
				latest = ts
			}
			continue
		}
		if ts, err := time.Parse(time.RFC3339, token); err == nil {
			if ts.After(latest) {
				latest = ts
			}
		}
	}
	return latest
}

func (m *Model) resetDetailView(detailErr, dagErr error, hideDetail bool) {
	m.graph = nil
	m.focusedNodeID = ""
	m.dagLayout = ""
	m.taskRunStatus = make(map[string]api.RunTask)
	m.dagViewport.SetContent("")
	m.detailErr = detailErr
	m.dagErr = dagErr
	m.stopLogStream(false)
	m.showLogsModal = false
	m.atomDetails = make(map[string]*api.Atom)
	m.atomErr = nil
	m.loadingAtomID = ""
	if hideDetail {
		m.showDetail = false
		m.detailLoading = false
	}
}

func (m *Model) clearTriggeringJob(jobID string) {
	if jobID == "" || m.triggeringJobID == jobID {
		m.triggeringJobID = ""
	}
}

func (m *Model) setActionStatus(notice string, err error) {
	m.actionNotice = notice
	m.actionErr = err
}

func (m *Model) toggleLogs() tea.Cmd {
	if m.client == nil {
		return nil
	}
	if m.focusedNodeID == "" {
		m.logsErr = fmt.Errorf("select a DAG node to stream logs")
		m.logsLoading = false
		m.showLogsModal = true
		return nil
	}
	if m.logTaskID == m.focusedNodeID && (m.logStream != nil || m.logsLoading) {
		m.stopLogStream(true)
		m.showLogsModal = false
		return nil
	}
	return m.startLogStream(m.focusedNodeID)
}

func (m *Model) startLogStream(taskID string) tea.Cmd {
	m.stopLogStream(false)

	if m.jobDetail == nil || m.jobDetail.LatestRun == nil {
		m.logsErr = fmt.Errorf("no runs available for this job")
		m.showLogsModal = true
		return nil
	}

	if task, ok := m.taskRunStatus[taskID]; ok && strings.TrimSpace(task.RuntimeID) == "" {
		m.logsErr = fmt.Errorf("task has no runtime logs yet")
		m.showLogsModal = true
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.logCtx = ctx
	m.logCancel = cancel
	m.logTaskID = taskID
	m.logsLoading = true
	m.logsErr = nil
	key := logKey(m.jobDetail.LatestRun.ID, taskID)
	if cached, ok := m.logCache[key]; ok {
		m.logContent = cached
	} else {
		m.logContent = ""
	}
	if trimmed := strings.TrimSuffix(m.logContent, "\n"); trimmed != "" {
		parts := strings.Split(trimmed, "\n")
		m.logLastLine[key] = parts[len(parts)-1]
	} else {
		delete(m.logLastLine, key)
	}
	m.logsViewport.SetContent(m.logContent)
	m.logsViewport.GotoTop()
	m.showLogsModal = true
	m.resizeLogViewport()

	var since time.Time
	if ts, ok := m.logSince[key]; ok {
		since = ts
	} else if ts := extractLastTimestamp(m.logContent); !ts.IsZero() {
		since = ts.Add(time.Nanosecond)
		m.logSince[key] = since
	}

	return openLogStream(ctx, m.client, m.jobDetail.Job.ID, m.jobDetail.LatestRun.ID, taskID, since)
}

func (m *Model) stopLogStream(preserveContent bool) {
	m.storeLogCache()
	if m.logCancel != nil {
		m.logCancel()
		m.logCancel = nil
	}
	if m.logStream != nil {
		_ = m.logStream.Close()
		m.logStream = nil
	}
	m.logCtx = nil
	m.logsLoading = false
	if !preserveContent {
		m.logTaskID = ""
		m.logsErr = nil
		m.logContent = ""
		m.logsViewport.SetContent("")
		m.logLastLine = make(map[string]string)
	}
	if !preserveContent {
		m.showLogsModal = false
	}
}

func (m *Model) appendLogs(chunk string) {
	if chunk == "" {
		return
	}
	key := ""
	if m.jobDetail != nil && m.jobDetail.LatestRun != nil && m.logTaskID != "" {
		key = logKey(m.jobDetail.LatestRun.ID, m.logTaskID)
	}

	// Deduplicate consecutive identical lines to avoid replays when resubscribing.
	if key != "" {
		last := m.logLastLine[key]
		lines := strings.Split(chunk, "\n")
		dedup := make([]string, 0, len(lines))
		for idx, line := range lines {
			// Preserve trailing newline via empty last element only if meaningful.
			if idx == len(lines)-1 && line == "" {
				continue
			}
			if line == "" {
				dedup = append(dedup, line)
				continue
			}
			if line == last {
				continue
			}
			last = line
			dedup = append(dedup, line)
		}
		if len(dedup) == 0 {
			return
		}
		m.logLastLine[key] = last
		chunk = strings.Join(dedup, "\n") + "\n"
	}

	m.logContent += chunk
	m.storeLogCache()
	m.updateLogCursor(chunk)
	m.logsViewport.SetContent(strings.TrimPrefix(m.logContent, "\n"))
	m.logsViewport.GotoBottom()
}

func (m *Model) storeLogCache() {
	if m.jobDetail == nil || m.jobDetail.LatestRun == nil || m.logTaskID == "" {
		return
	}
	key := logKey(m.jobDetail.LatestRun.ID, m.logTaskID)
	if m.logCache == nil {
		m.logCache = make(map[string]string)
	}
	m.logCache[key] = m.logContent
}

func (m *Model) updateLogCursor(chunk string) {
	if m.jobDetail == nil || m.jobDetail.LatestRun == nil || m.logTaskID == "" {
		return
	}
	key := logKey(m.jobDetail.LatestRun.ID, m.logTaskID)
	ts := extractLastTimestamp(chunk)
	if ts.IsZero() {
		return
	}
	if m.logSince == nil {
		m.logSince = make(map[string]time.Time)
	}
	if existing, ok := m.logSince[key]; !ok || ts.After(existing) {
		m.logSince[key] = ts.Add(time.Nanosecond)
	}
}

func (m *Model) triggerSelectedJob() tea.Cmd {
	if m.state != statusReady || m.active != sectionJobs {
		return nil
	}
	if m.triggeringJobID != "" {
		return nil
	}
	jobID := m.currentJobID()
	if jobID == "" {
		return nil
	}
	alias := m.currentJobAlias()
	if alias == "" && m.jobDetail != nil && m.jobDetail.Job.ID == jobID {
		alias = m.jobDetail.Job.Alias
	}
	if alias == "" {
		alias = shortID(jobID)
	}
	m.triggeringJobID = jobID
	m.actionErr = nil
	m.actionNotice = fmt.Sprintf("Triggering %s…", alias)
	return triggerJob(m.client, jobID)
}

func (m Model) nextSuccessor() string {
	if m.graph == nil {
		return ""
	}
	if m.focusedNodeID == "" {
		if root := m.graph.First(); root != nil {
			return root.ID()
		}
		return ""
	}
	node, ok := m.graph.Node(m.focusedNodeID)
	if !ok {
		return ""
	}
	successors := node.Successors()
	if len(successors) == 0 {
		return ""
	}
	return successors[0].ID()
}

func (m Model) firstPredecessor() string {
	if m.graph == nil || m.focusedNodeID == "" {
		return ""
	}
	node, ok := m.graph.Node(m.focusedNodeID)
	if !ok {
		return ""
	}
	predecessors := node.Predecessors()
	if len(predecessors) == 0 {
		return ""
	}
	return predecessors[0].ID()
}

func (m *Model) setFocusedNode(id string) tea.Cmd {
	if m.graph == nil || id == "" {
		m.focusedNodeID = ""
		m.refreshDAGLayout(false)
		return nil
	}
	if _, ok := m.graph.Node(id); !ok {
		return nil
	}
	m.focusedNodeID = id
	m.refreshDAGLayout(false)
	return m.preloadAtomMetadata(id)
}

func (m *Model) preloadAtomMetadata(id string) tea.Cmd {
	if m.client == nil || m.graph == nil {
		return nil
	}

	node, ok := m.graph.Node(id)
	if !ok {
		return nil
	}

	atomID := node.AtomID()
	if atomID == "" {
		m.loadingAtomID = ""
		m.atomErr = nil
		return nil
	}

	if m.atomDetails == nil {
		m.atomDetails = make(map[string]*api.Atom)
	}

	if _, ok := m.atomDetails[atomID]; ok {
		return nil
	}

	if m.loadingAtomID == atomID {
		return nil
	}

	m.atomErr = nil
	m.loadingAtomID = atomID
	return fetchAtomDetail(m.client, atomID)
}

func (m *Model) refreshDAGLayout(resetScroll bool) {
	if m.graph == nil {
		m.dagLayout = ""
		m.dagViewport.SetContent("")
		if resetScroll {
			m.dagViewport.GotoTop()
		}
		return
	}

	opts := dag.RenderOptions{
		FocusedID: m.focusedNodeID,
		Labeler:   m.nodeLabeler(),
		FocusPath: m.dagFocusPath,
		MaxWidth:  m.dagViewport.Width,
	}

	layout := dag.Render(m.graph, opts)
	m.dagLayout = layout
	m.dagViewport.SetContent(layout)
	if resetScroll {
		m.dagViewport.GotoTop()
	}
}

func (m *Model) resizeDAGViewport() {
	width := m.dagViewport.Width
	if m.viewportWidth > 0 {
		width = max(m.viewportWidth-12, 30)
	}
	height := m.dagViewport.Height
	if m.viewportHeight > 0 {
		height = max(8, m.viewportHeight/3)
	}
	m.dagViewport.Width = width
	m.dagViewport.Height = height
	m.dagViewport.SetContent(m.dagLayout)
}

func (m *Model) resizeLogViewport() {
	width := m.logsViewport.Width
	if m.viewportWidth > 0 {
		width = max(m.viewportWidth-12, 30)
	}
	height := m.logsViewport.Height
	if m.viewportHeight > 0 {
		height = max(10, m.viewportHeight/3)
	}
	m.logsViewport.Width = width
	m.logsViewport.Height = height
	m.logsViewport.SetContent(m.logContent)
}

func (m Model) orderedNodeIDs() []string {
	if m.graph == nil {
		return nil
	}
	levels := m.graph.Levels()
	count := 0
	for _, level := range levels {
		count += len(level)
	}
	ids := make([]string, 0, count)
	for _, level := range levels {
		for _, node := range level {
			if node != nil {
				ids = append(ids, node.ID())
			}
		}
	}
	return ids
}

func (m *Model) cycleFocusedNode(delta int) tea.Cmd {
	ids := m.orderedNodeIDs()
	if len(ids) == 0 {
		return nil
	}
	if m.focusedNodeID == "" {
		if delta < 0 {
			return m.setFocusedNode(ids[len(ids)-1])
		}
		return m.setFocusedNode(ids[0])
	}

	index := 0
	for i, id := range ids {
		if id == m.focusedNodeID {
			index = i
			break
		}
	}
	index = (index + delta + len(ids)) % len(ids)
	return m.setFocusedNode(ids[index])
}
