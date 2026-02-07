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

type actionKind int

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

const (
	actionNone actionKind = iota
	actionTrigger
	actionRerun
)

type actionRequest struct {
	kind  actionKind
	jobID string
	runID string
	label string
}

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
	runsTable       table.Model
	runs            []api.Run
	runsJobID       string
	runsLoading     bool
	runsErr         error
	showRunsModal   bool
	activeRunID     string
	followLatestRun bool
	logCtx          context.Context
	logCancel       context.CancelFunc
	logStream       io.ReadCloser
	logTaskID       string
	logRunID        string
	showLogsModal   bool
	logsViewport    viewport.Model
	logsLoading     bool
	logsErr         error
	logContent      string
	logCache        map[string]string
	logSince        map[string]time.Time
	logLastLine     map[string]string
	logsFollow      bool
	logFilter       string
	logFilterInput  bool
	atomDetails     map[string]*api.Atom
	atomErr         error
	loadingAtomID   string
	atomIndex       map[string]api.Atom
	showDetail      bool
	detailLoading   bool
	showHelp        bool
	themeIndex      int
	themeName       string
	apiHealthy      bool
	apiHealthErr    error
	apiLatency      time.Duration
	apiCheckedAt    time.Time
	lastLoadAt      time.Time
	lastLoadLatency time.Duration
	lastLoadRetries int
	confirmAction   *actionRequest
	actionPending   *actionRequest
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
	runsTable := createTable(runColumnTitles, []int{16, 12, 22, 22}, false)

	m := Model{
		client:          client,
		spinner:         sp,
		state:           statusLoading,
		active:          sectionJobs,
		jobs:            jobs,
		triggers:        triggers,
		atoms:           atoms,
		runsTable:       runsTable,
		dagViewport:     dagViewport,
		logsViewport:    logsViewport,
		jobRunStatus:    make(map[string]*api.Run),
		logCache:        make(map[string]string),
		logSince:        make(map[string]time.Time),
		logLastLine:     make(map[string]string),
		logsFollow:      true,
		atomDetails:     make(map[string]*api.Atom),
		atomIndex:       make(map[string]api.Atom),
		taskRunStatus:   make(map[string]api.RunTask),
		followLatestRun: true,
	}
	m.setTheme(0)
	return m
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
		if m.confirmAction != nil {
			switch msg.String() {
			case "enter", "y":
				cmd := m.confirmActionNow()
				return m, cmd
			case "ctrl+c", "q", "esc", "n":
				m.confirmAction = nil
				return m, nil
			}
			return m, nil
		}
		if m.showHelp {
			switch msg.String() {
			case "ctrl+c", "q", "esc", "?":
				m.showHelp = false
				return m, nil
			}
			return m, nil
		}
		if !m.logFilterInput {
			switch msg.String() {
			case "?":
				m.showHelp = true
				return m, nil
			case "T":
				m.cycleTheme()
				return m, nil
			case "p":
				if m.client != nil {
					return m, pingHealth(m.client)
				}
				m.setActionStatus("Health check failed", fmt.Errorf("api client not configured"))
				return m, nil
			}
		}
		if m.showRunsModal {
			switch msg.String() {
			case "ctrl+c", "q", "esc":
				m.showRunsModal = false
				return m, nil
			case "enter":
				m.selectRunFromModal()
				m.showRunsModal = false
				return m, nil
			case "R":
				if cmd := m.requestRerunSelectedRun(); cmd != nil {
					return m, cmd
				}
				return m, nil
			case "r":
				if cmd := m.reloadRuns(); cmd != nil {
					return m, cmd
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.runsTable, cmd = m.runsTable.Update(msg)
			return m, cmd
		}
		if m.showLogsModal {
			if m.logFilterInput {
				switch msg.String() {
				case "enter":
					m.logFilterInput = false
					m.refreshLogsViewport()
				case "esc":
					m.logFilterInput = false
				case "backspace", "ctrl+h":
					if m.logFilter != "" {
						runes := []rune(m.logFilter)
						m.logFilter = string(runes[:len(runes)-1])
						m.refreshLogsViewport()
					}
				case "ctrl+u":
					m.logFilter = ""
					m.refreshLogsViewport()
				default:
					if msg.Type == tea.KeyRunes {
						m.logFilter += string(msg.Runes)
						m.refreshLogsViewport()
					}
				}
				return m, nil
			}
			switch msg.String() {
			case "ctrl+c", "q", "esc", "g":
				m.stopLogStream(true)
				m.showLogsModal = false
				return m, nil
			case "/":
				m.logFilterInput = true
				return m, nil
			case "c":
				m.logFilter = ""
				m.logFilterInput = false
				m.refreshLogsViewport()
				return m, nil
			case "e":
				if cmd := m.exportFilteredLogs(); cmd != nil {
					return m, cmd
				}
				return m, nil
			case " ":
				m.logsFollow = !m.logsFollow
				if m.logsFollow {
					m.logsViewport.GotoBottom()
				}
				return m, nil
			case "up", "k":
				m.logsViewport.ScrollUp(1)
				m.logsFollow = false
				return m, nil
			case "down", "j":
				m.logsViewport.ScrollDown(1)
				if m.logsViewport.AtBottom() {
					m.logsFollow = true
				}
				return m, nil
			case "pgup":
				m.logsViewport.ScrollUp(m.logsViewport.Height / 2)
				m.logsFollow = false
				return m, nil
			case "pgdown":
				m.logsViewport.ScrollDown(m.logsViewport.Height / 2)
				if m.logsViewport.AtBottom() {
					m.logsFollow = true
				}
				return m, nil
			}
		}
		switch msg.String() {
		case "ctrl+c", "q":
			if m.showDetail {
				m.showDetail = false
				m.detailLoading = false
				m.stopLogStream(false)
				m.showRunsModal = false
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.showDetail {
				m.showDetail = false
				m.detailLoading = false
				m.stopLogStream(false)
				m.showRunsModal = false
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
			m.runs = nil
			m.runsTable.SetRows(nil)
			m.runsJobID = ""
			m.runsLoading = false
			m.runsErr = nil
			m.showRunsModal = false
			m.activeRunID = ""
			m.followLatestRun = true
			m.resetDetailView(nil, nil, true)
			m.taskRunStatus = make(map[string]api.RunTask)
			m.stopLogStream(false)
			m.confirmAction = nil
			m.actionPending = nil
			m.showHelp = false
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
				} else if cmd := m.cycleFocusedNode(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
		case "left", "h":
			if m.showDetail {
				if id := m.firstPredecessor(); id != "" {
					if cmd := m.setFocusedNode(id); cmd != nil {
						cmds = append(cmds, cmd)
					}
				} else if cmd := m.cycleFocusedNode(-1); cmd != nil {
					cmds = append(cmds, cmd)
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
		case "u":
			if m.showDetail {
				if cmd := m.toggleRuns(); cmd != nil {
					cmds = append(cmds, cmd)
				}
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
		if m.showRunsModal && len(m.runs) > 0 {
			m.runsTable.SetRows(runsToRows(m.runs, m.spinner.View()))
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dataLoadedMsg:
		m.state = statusReady
		m.err = nil
		m.updateDiagnostics(msg.healthOK, msg.healthErr, msg.healthLatency, msg.healthCheckedAt, msg.fetchDuration, msg.attempts)
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
		previousJobID := ""
		if m.jobDetail != nil {
			previousJobID = m.jobDetail.Job.ID
		}
		m.jobDetail = msg.detail
		if msg.detail.Job.ID != previousJobID {
			m.runsJobID = msg.detail.Job.ID
			m.runs = nil
			m.runsTable.SetRows(nil)
			m.runsErr = nil
			m.showRunsModal = false
			m.activeRunID = ""
			m.followLatestRun = true
		} else if m.runsJobID == "" {
			m.runsJobID = msg.detail.Job.ID
		}
		if m.runsJobID != "" {
			m.runsLoading = true
			cmds = append(cmds, fetchRuns(m.client, m.runsJobID))
		}
		m.resetDetailView(nil, nil, false)
		m.setJobStatus(msg.detail.Job.ID, msg.detail.LatestRun)
		if msg.detail.LatestRun != nil && (m.followLatestRun || m.activeRunID == "") {
			m.activeRunID = msg.detail.LatestRun.ID
			m.followLatestRun = true
		}
		m.applyRun(m.activeRun())

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
		prevRunID := m.activeRunID
		m.setJobStatus(msg.jobID, msg.run)
		if msg.run != nil && m.jobDetail != nil && m.jobDetail.Job.ID == msg.jobID {
			m.jobDetail.LatestRun = msg.run
			if m.followLatestRun || m.activeRunID == "" {
				m.activeRunID = msg.run.ID
				m.followLatestRun = true
				m.applyRun(msg.run)
				if m.graph != nil {
					m.refreshDAGLayout(false)
				}
			}
		}
		if cmd := m.maybeRestartLogStream(prevRunID, m.activeRunID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case jobStatusErrMsg:
		m.setJobStatus(msg.jobID, nil)
	case runsLoadedMsg:
		if msg.jobID != m.runsJobID {
			break
		}
		m.runsLoading = false
		m.runsErr = nil
		m.runs = orderRunsDesc(msg.runs)
		m.runsTable.SetRows(runsToRows(m.runs, m.spinner.View()))
		m.syncRunCursor()
		prevRunID := m.activeRunID
		if m.followLatestRun || m.activeRunID == "" {
			if latest := m.latestRun(); latest != nil {
				m.activeRunID = latest.ID
				m.followLatestRun = true
				m.applyRun(latest)
				if m.graph != nil {
					m.refreshDAGLayout(false)
				}
			}
		} else {
			if run := m.runByID(m.activeRunID); run != nil {
				m.applyRun(run)
				if m.graph != nil {
					m.refreshDAGLayout(false)
				}
			} else if latest := m.latestRun(); latest != nil {
				m.activeRunID = latest.ID
				m.followLatestRun = true
				m.applyRun(latest)
				if m.graph != nil {
					m.refreshDAGLayout(false)
				}
			}
		}
		if cmd := m.maybeRestartLogStream(prevRunID, m.activeRunID); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case runsErrMsg:
		if msg.jobID != m.runsJobID {
			break
		}
		m.runsLoading = false
		m.runsErr = msg.err
	case logsOpenedMsg:
		m.logsLoading = false
		m.logsErr = nil
		m.logStream = msg.reader
		if m.logCtx == nil {
			m.logCtx = msg.ctx
		}
		m.refreshLogsViewport()
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
	case logsExportedMsg:
		m.setActionStatus(fmt.Sprintf("Logs exported: %s", msg.path), nil)
	case logsExportErrMsg:
		m.setActionStatus("Log export failed", msg.err)
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
		req := m.clearPendingAction(msg.jobID)
		notice := actionAcceptedNotice(req, msg.run)
		m.setJobStatus(msg.jobID, msg.run)
		m.setActionStatus(notice, nil)
		if m.state == statusReady {
			if id := m.currentJobID(); id != "" && id == msg.jobID {
				m.detailLoading = true
				cmds = append(cmds, fetchJobDetail(m.client, id, true))
			}
		}
	case jobTriggerErrMsg:
		req := m.clearPendingAction(msg.jobID)
		m.setActionStatus(actionFailureTitle(req), msg.err)
	case healthCheckedMsg:
		m.apiHealthy = msg.ok
		m.apiHealthErr = msg.err
		m.apiLatency = msg.latency
		m.apiCheckedAt = msg.checkedAt
		if msg.ok {
			m.setActionStatus("Health check passed", nil)
		} else {
			m.setActionStatus("Health check failed", msg.err)
		}
	case dataLoadErrMsg:
		m.state = statusError
		m.err = msg.err
		m.updateDiagnostics(msg.healthOK, msg.healthErr, msg.healthLatency, msg.healthCheckedAt, msg.fetchDuration, msg.attempts)
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
		m.showRunsModal = false
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

func (m Model) jobAliasByID(jobID string) string {
	if jobID == "" {
		return ""
	}
	if m.jobDetail != nil && m.jobDetail.Job.ID == jobID && m.jobDetail.Job.Alias != "" {
		return m.jobDetail.Job.Alias
	}
	for _, job := range m.jobRecords {
		if job.ID == jobID && job.Alias != "" {
			return job.Alias
		}
	}
	return ""
}

func (m Model) jobLabel(jobID string) string {
	row := m.jobs.SelectedRow()
	if len(row) >= 5 && row[4] == jobID {
		if alias := strings.TrimSpace(row[0]); alias != "" {
			return alias
		}
	}
	if alias := m.jobAliasByID(jobID); alias != "" {
		return alias
	}
	return shortID(jobID)
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

func (m *Model) clearPendingAction(jobID string) *actionRequest {
	if m.actionPending == nil {
		return nil
	}
	if jobID == "" || m.actionPending.jobID == jobID {
		req := m.actionPending
		m.actionPending = nil
		return req
	}
	return nil
}

func (m *Model) setActionStatus(notice string, err error) {
	m.actionNotice = notice
	m.actionErr = err
}

func (m *Model) updateDiagnostics(healthy bool, healthErr error, healthLatency time.Duration, checkedAt time.Time, fetchLatency time.Duration, attempts int) {
	m.apiHealthy = healthy
	m.apiHealthErr = healthErr
	m.apiLatency = healthLatency
	m.apiCheckedAt = checkedAt
	m.lastLoadAt = checkedAt
	m.lastLoadLatency = fetchLatency
	if attempts > 0 {
		m.lastLoadRetries = attempts - 1
	}
}

func actionVerb(kind actionKind) string {
	switch kind {
	case actionTrigger:
		return "Triggering"
	case actionRerun:
		return "Re-running"
	default:
		return "Working"
	}
}

func actionFailureTitle(req *actionRequest) string {
	if req == nil {
		return "Action failed"
	}
	switch req.kind {
	case actionTrigger:
		return "Trigger failed"
	case actionRerun:
		return "Re-run failed"
	default:
		return "Action failed"
	}
}

func actionAcceptedNotice(req *actionRequest, run *api.Run) string {
	if run == nil {
		if req != nil && req.kind == actionRerun {
			return "Re-run accepted"
		}
		return "Run accepted"
	}
	if req != nil && req.kind == actionRerun {
		source := ""
		if req.runID != "" {
			source = shortID(req.runID)
		}
		if source != "" {
			return fmt.Sprintf("Re-run from %s accepted as %s", source, shortID(run.ID))
		}
		return fmt.Sprintf("Re-run %s accepted", shortID(run.ID))
	}
	return fmt.Sprintf("Run %s accepted", shortID(run.ID))
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

	run := m.activeRun()
	if m.jobDetail == nil || run == nil {
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
	m.logRunID = run.ID
	m.logsLoading = true
	m.logsErr = nil
	key := logKey(run.ID, taskID)
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
	m.refreshLogsViewport()
	m.logsFollow = true
	m.logsViewport.GotoBottom()
	m.showLogsModal = true
	m.resizeLogViewport()

	var since time.Time
	if ts, ok := m.logSince[key]; ok {
		since = ts
	} else if ts := extractLastTimestamp(m.logContent); !ts.IsZero() {
		since = ts.Add(time.Nanosecond)
		m.logSince[key] = since
	}

	return openLogStream(ctx, m.client, m.jobDetail.Job.ID, run.ID, taskID, since)
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
		m.logRunID = ""
		m.logsErr = nil
		m.logContent = ""
		m.refreshLogsViewport()
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
	key := m.logCacheKey()

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
	m.refreshLogsViewport()
}

func (m *Model) storeLogCache() {
	key := m.logCacheKey()
	if key == "" {
		return
	}
	if m.logCache == nil {
		m.logCache = make(map[string]string)
	}
	m.logCache[key] = m.logContent
}

func (m *Model) updateLogCursor(chunk string) {
	key := m.logCacheKey()
	if key == "" {
		return
	}
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

func (m *Model) logCacheKey() string {
	if m.jobDetail == nil || m.logTaskID == "" {
		return ""
	}
	runID := m.logRunID
	if runID == "" {
		if run := m.activeRun(); run != nil {
			runID = run.ID
		}
	}
	if runID == "" {
		return ""
	}
	return logKey(runID, m.logTaskID)
}

func (m *Model) maybeRestartLogStream(prevID, nextID string) tea.Cmd {
	if prevID == "" || nextID == "" || prevID == nextID {
		return nil
	}
	if !m.showLogsModal || !m.followLatestRun || m.logTaskID == "" {
		return nil
	}
	if m.logStream == nil && !m.logsLoading {
		return nil
	}
	return m.startLogStream(m.logTaskID)
}

func (m *Model) triggerSelectedJob() tea.Cmd {
	if m.state != statusReady || m.active != sectionJobs {
		return nil
	}
	if m.actionPending != nil || m.confirmAction != nil {
		return nil
	}
	jobID := m.currentJobID()
	if jobID == "" {
		return nil
	}
	label := m.jobLabel(jobID)
	m.confirmAction = &actionRequest{
		kind:  actionTrigger,
		jobID: jobID,
		label: label,
	}
	return nil
}

func (m *Model) requestRerunSelectedRun() tea.Cmd {
	if m.actionPending != nil || m.confirmAction != nil {
		return nil
	}
	run := m.selectedRun()
	if run == nil {
		return nil
	}
	jobID := run.JobID
	if jobID == "" && m.jobDetail != nil {
		jobID = m.jobDetail.Job.ID
	}
	if jobID == "" {
		return nil
	}
	label := fmt.Sprintf("run %s", shortID(run.ID))
	m.confirmAction = &actionRequest{
		kind:  actionRerun,
		jobID: jobID,
		runID: run.ID,
		label: label,
	}
	return nil
}

func (m *Model) confirmActionNow() tea.Cmd {
	if m.confirmAction == nil {
		return nil
	}
	req := *m.confirmAction
	m.confirmAction = nil
	return m.startAction(req)
}

func (m *Model) startAction(req actionRequest) tea.Cmd {
	if m.client == nil || req.jobID == "" {
		return nil
	}
	if m.actionPending != nil {
		return nil
	}
	m.actionPending = &req
	m.actionErr = nil
	if req.label != "" {
		m.actionNotice = fmt.Sprintf("%s %s…", actionVerb(req.kind), req.label)
	} else {
		m.actionNotice = fmt.Sprintf("%s…", actionVerb(req.kind))
	}
	switch req.kind {
	case actionRerun:
		return rerunJob(m.client, req.jobID, req.runID)
	default:
		return triggerJob(m.client, req.jobID)
	}
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
	m.refreshLogsViewport()
}

func (m *Model) refreshLogsViewport() {
	m.logsViewport.SetContent(strings.TrimSuffix(m.filteredLogContent(), "\n"))
	if m.logsFollow {
		m.logsViewport.GotoBottom()
	}
}

func (m Model) filteredLogContent() string {
	content := strings.TrimPrefix(m.logContent, "\n")
	query := strings.ToLower(strings.TrimSpace(m.logFilter))
	if query == "" {
		return content
	}
	if strings.TrimSpace(content) == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), query) {
			filtered = append(filtered, line)
		}
	}
	return strings.Join(filtered, "\n")
}

func (m *Model) exportFilteredLogs() tea.Cmd {
	content := strings.TrimSpace(m.filteredLogContent())
	if content == "" {
		m.setActionStatus("Log export failed", fmt.Errorf("no log content to export"))
		return nil
	}
	return exportLogSnippet(content, m.logRunID, m.logTaskID, m.logFilter)
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

func (m *Model) toggleRuns() tea.Cmd {
	if m.client == nil || m.jobDetail == nil {
		return nil
	}
	if m.showRunsModal {
		m.showRunsModal = false
		return nil
	}
	m.showRunsModal = true
	m.runsTable.Focus()
	if m.runsJobID != m.jobDetail.Job.ID {
		m.runsJobID = m.jobDetail.Job.ID
		m.runsLoading = true
		m.runsErr = nil
		return fetchRuns(m.client, m.runsJobID)
	}
	if len(m.runs) == 0 && !m.runsLoading {
		m.runsLoading = true
		m.runsErr = nil
		return fetchRuns(m.client, m.runsJobID)
	}
	m.syncRunCursor()
	return nil
}

func (m *Model) reloadRuns() tea.Cmd {
	if m.client == nil || m.runsJobID == "" {
		return nil
	}
	m.runsLoading = true
	m.runsErr = nil
	return fetchRuns(m.client, m.runsJobID)
}

func (m *Model) selectRunFromModal() {
	row := m.runsTable.SelectedRow()
	if len(row) == 0 {
		return
	}
	m.setActiveRunID(row[0])
}

func (m *Model) selectedRun() *api.Run {
	row := m.runsTable.SelectedRow()
	if len(row) == 0 {
		return nil
	}
	return m.runByID(row[0])
}

func (m *Model) setActiveRunID(runID string) {
	if runID == "" {
		return
	}
	run := m.runByID(runID)
	if run == nil {
		return
	}
	m.activeRunID = run.ID
	m.followLatestRun = m.isLatestRunID(run.ID)
	m.applyRun(run)
	m.syncRunCursor()
	if m.graph != nil {
		m.refreshDAGLayout(false)
	}
	m.stopLogStream(false)
	m.showLogsModal = false
}

func (m *Model) syncRunCursor() {
	if len(m.runs) == 0 {
		m.runsTable.SetCursor(0)
		return
	}
	target := m.activeRunID
	if target == "" {
		if latest := m.latestRun(); latest != nil {
			target = latest.ID
		}
	}
	if target == "" {
		return
	}
	for i, run := range m.runs {
		if run.ID == target {
			m.runsTable.SetCursor(i)
			return
		}
	}
}

func (m *Model) applyRun(run *api.Run) {
	m.taskRunStatus = make(map[string]api.RunTask)
	if run == nil {
		return
	}
	for _, task := range run.Tasks {
		m.taskRunStatus[task.ID] = task
	}
}

func (m *Model) activeRun() *api.Run {
	if m.activeRunID != "" {
		if run := m.runByID(m.activeRunID); run != nil {
			return run
		}
	}
	return m.latestRun()
}

func (m *Model) latestRun() *api.Run {
	if m.jobDetail == nil {
		return nil
	}
	return m.jobDetail.LatestRun
}

func (m *Model) isLatestRunID(runID string) bool {
	latest := m.latestRun()
	if latest == nil {
		return false
	}
	return latest.ID == runID
}

func (m *Model) runByID(runID string) *api.Run {
	if runID == "" {
		return nil
	}
	for i := range m.runs {
		if m.runs[i].ID == runID {
			return &m.runs[i]
		}
	}
	if m.jobDetail != nil && m.jobDetail.LatestRun != nil && m.jobDetail.LatestRun.ID == runID {
		return m.jobDetail.LatestRun
	}
	return nil
}

func orderRunsDesc(runs []api.Run) []api.Run {
	if len(runs) < 2 {
		return runs
	}
	ordered := make([]api.Run, len(runs))
	for i := range runs {
		ordered[len(runs)-1-i] = runs[i]
	}
	return ordered
}
