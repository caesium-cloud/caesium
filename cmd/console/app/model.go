package app

import (
	"fmt"

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
	viewportWidth   int
	viewportHeight  int
	selectedJobID   string
	jobDetail       *api.JobDetail
	graph           *dag.Graph
	focusedNodeID   string
	dagLayout       string
	dagFocusPath    bool
	dagViewport     viewport.Model
	dagErr          error
	detailErr       error
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

	jobs := createTable(jobColumnTitles, []int{20, 24, 24, 20, 20}, true)
	triggers := createTable(triggerColumnTitles, []int{20, 12, 20}, false)
	atoms := createTable(atomColumnTitles, []int{24, 12, 20}, false)

	return Model{
		client:      client,
		spinner:     sp,
		state:       statusLoading,
		active:      sectionJobs,
		jobs:        jobs,
		triggers:    triggers,
		atoms:       atoms,
		dagViewport: dagViewport,
		atomDetails: make(map[string]*api.Atom),
		atomIndex:   make(map[string]api.Atom),
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
		switch msg.String() {
		case "ctrl+c", "q":
			if m.showDetail {
				m.showDetail = false
				m.detailLoading = false
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.showDetail {
				m.showDetail = false
				m.detailLoading = false
				return m, nil
			}
		case "r":
			m.state = statusLoading
			m.err = nil
			m.jobs.SetRows(nil)
			m.triggers.SetRows(nil)
			m.atoms.SetRows(nil)
			m.selectedJobID = ""
			m.jobDetail = nil
			m.resetDetailView(nil, nil, true)
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
		m.resizeColumns(max(10, width-2))
		m.resizeDAGViewport()
		if m.graph != nil {
			m.refreshDAGLayout(false)
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	case dataLoadedMsg:
		m.state = statusReady
		m.err = nil
		m.jobs.SetRows(jobsToRows(msg.jobs))
		m.triggers.SetRows(triggersToRows(msg.triggers))
		m.atoms.SetRows(atomsToRows(msg.atoms))
		m = m.activate(sectionJobs)
		m.resetDetailView(nil, nil, true)
		m.atomIndex = make(map[string]api.Atom)
		for _, atom := range msg.atoms {
			m.atomIndex[atom.ID] = atom
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
	}
	m.active = sec
	return m
}

func (m Model) currentJobID() string {
	row := m.jobs.SelectedRow()
	if len(row) < 4 {
		return ""
	}
	return row[3]
}

func (m Model) currentJobAlias() string {
	row := m.jobs.SelectedRow()
	if len(row) == 0 {
		return ""
	}
	return row[0]
}

func (m *Model) resetDetailView(detailErr, dagErr error, hideDetail bool) {
	m.graph = nil
	m.focusedNodeID = ""
	m.dagLayout = ""
	m.dagViewport.SetContent("")
	m.detailErr = detailErr
	m.dagErr = dagErr
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
