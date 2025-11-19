package app

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/caesium-cloud/caesium/cmd/console/ui/detail"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

var (
	barStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Padding(0, 1)
	boxStyle     = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	activeBox    = boxStyle.BorderForeground(lipgloss.Color("63"))
	tabActive    = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57")).Bold(true)
	tabInactive  = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("240"))
	sectionNames = map[section]string{
		sectionJobs:     "Jobs",
		sectionTriggers: "Triggers",
		sectionAtoms:    "Atoms",
	}
	logoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).PaddingRight(1)

	jobColumnTitles      = []string{"Alias", "Labels", "Annotations", "ID", "Created"}
	jobColumnWeights     = []int{3, 4, 4, 3, 3}
	triggerColumnTitles  = []string{"Alias", "Type", "ID"}
	triggerColumnWeights = []int{3, 2, 4}
	atomColumnTitles     = []string{"Image", "Engine", "ID"}
	atomColumnWeights    = []int{4, 2, 4}
)

// Model represents the Bubble Tea program state.
type Model struct {
	client        *api.Client
	spinner       spinner.Model
	state         status
	err           error
	active        section
	jobs          table.Model
	triggers      table.Model
	atoms         table.Model
	viewportWidth int
	selectedJobID string
	jobDetail     *api.JobDetail
	graph         *dag.Graph
	focusedNodeID string
	dagErr        error
	detailErr     error
	atomDetails   map[string]*api.Atom
	atomErr       error
	loadingAtomID string
	atomIndex     map[string]api.Atom
	showDetail    bool
	detailLoading bool
}

// New creates the root model with dependency references.
func New(client *api.Client) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

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
			m.graph = nil
			m.focusedNodeID = ""
			m.dagErr = nil
			m.detailErr = nil
			m.atomDetails = make(map[string]*api.Atom)
			m.atomErr = nil
			m.loadingAtomID = ""
			m.showDetail = false
			m.detailLoading = false
			cmds = append(cmds, m.spinner.Tick, fetchData(m.client))
		case "1":
			m = m.activate(sectionJobs)
		case "2":
			m = m.activate(sectionTriggers)
		case "3":
			m = m.activate(sectionAtoms)
		case "tab":
			m = m.activate(m.active.next())
		case "shift+tab":
			m = m.activate(m.active.prev())
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
		}
	case tea.WindowSizeMsg:
		height := max(5, msg.Height-7)
		width := max(20, msg.Width-8)
		m.viewportWidth = msg.Width
		m.jobs.SetHeight(height)
		m.triggers.SetHeight(height)
		m.atoms.SetHeight(height)
		m.jobs.SetWidth(width)
		m.triggers.SetWidth(width)
		m.atoms.SetWidth(width)
		m.resizeColumns(max(10, width-2))
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
		m.detailErr = nil
		m.atomErr = nil
		m.atomDetails = make(map[string]*api.Atom)
		m.loadingAtomID = ""
		m.atomIndex = make(map[string]api.Atom)
		for _, atom := range msg.atoms {
			m.atomIndex[atom.ID] = atom
		}
		m.showDetail = false
		m.detailLoading = false
		if id := m.currentJobID(); id != "" {
			m.selectedJobID = id
			cmds = append(cmds, fetchJobDetail(m.client, id, true))
		}
	case jobDetailLoadedMsg:
		m.jobDetail = msg.detail
		if msg.detail == nil {
			m.showDetail = false
		}
		m.detailLoading = false
		m.detailErr = nil
		m.dagErr = nil
		m.atomErr = nil
		m.atomDetails = make(map[string]*api.Atom)
		m.loadingAtomID = ""
		if msg.detail != nil && msg.detail.DAG != nil {
			graph, err := dag.FromJobDAG(msg.detail.DAG)
			if err != nil {
				m.graph = nil
				m.focusedNodeID = ""
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
	case jobDetailErrMsg:
		m.graph = nil
		m.focusedNodeID = ""
		m.dagErr = msg.err
		m.detailErr = msg.err
		m.atomDetails = make(map[string]*api.Atom)
		m.loadingAtomID = ""
		m.atomErr = nil
		m.showDetail = false
		m.detailLoading = false
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
				m.graph = nil
				m.focusedNodeID = ""
				m.dagErr = nil
				m.detailErr = nil
				m.atomDetails = make(map[string]*api.Atom)
				m.atomErr = nil
				m.loadingAtomID = ""
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

// View renders the interface.
func (m Model) View() string {
	tabs := renderTabsBar(m.active, m.viewportWidth)

	footerKeys := "[1/2/3] switch  [tab] cycle  [r] reload  [q] quit"
	if m.active == sectionJobs {
		if m.showDetail {
			footerKeys = "[esc/q] back  [←/→] traverse"
		} else {
			footerKeys += "  [enter] detail"
		}
	}
	footer := barStyle.Render(footerKeys)

	var body string

	switch m.state {
	case statusLoading:
		body = centerText(fmt.Sprintf("%s Loading data…", m.spinner.View()))
	case statusError:
		body = boxStyle.Render("Failed to load data: " + m.err.Error())
	case statusReady:
		switch m.active {
		case sectionJobs:
			if m.showDetail {
				body = m.renderJobDetailScreen()
			} else {
				body = m.renderJobsView()
			}
		case sectionTriggers:
			m.jobs.SetWidth(max(20, m.viewportWidth-8))
			body = renderPane(m.triggers, true)
		case sectionAtoms:
			m.jobs.SetWidth(max(20, m.viewportWidth-8))
			body = renderPane(m.atoms, true)
		default:
			activeTable := m.tableFor(m.active)
			body = renderPane(activeTable, true)
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, tabs, body, footer)
}

func (m Model) renderJobsView() string {
	width := max(20, m.viewportWidth-8)
	m.jobs.SetWidth(width)
	return renderPane(m.jobs, true)
}

func (m Model) renderJobDetailScreen() string {
	totalWidth := max(40, m.viewportWidth-6)

	var focusedAtom *api.Atom
	if m.graph != nil && m.focusedNodeID != "" {
		if node, ok := m.graph.Node(m.focusedNodeID); ok {
			if atomID := node.AtomID(); atomID != "" {
				focusedAtom = m.atomDetails[atomID]
			}
		}
	}

	labeler := m.nodeLabeler()
	vm := detail.ViewModel{
		Job:           m.jobDetail,
		Graph:         m.graph,
		FocusedNode:   m.focusedNodeID,
		FocusedAtom:   focusedAtom,
		DetailErr:     m.detailErr,
		DetailPending: m.detailLoading,
		GraphErr:      m.dagErr,
		AtomErr:       m.atomErr,
		AtomLoading:   m.loadingAtomID != "",
		AtomLookup:    m.atomIndex,
		Labeler:       labeler,
		ViewportWidth: max(20, totalWidth-4),
	}

	content := detail.Render(vm)
	body := boxStyle.Width(totalWidth).Render(content)

	return body
}

func (m Model) nodeLabeler() dag.LabelFunc {
	atoms := m.atomIndex
	return func(n *dag.Node) string {
		if n == nil {
			return ""
		}
		if atom, ok := atoms[n.AtomID()]; ok {
			return fmt.Sprintf("%s (%s)", shortImage(atom.Image), shortID(n.ID()))
		}
		return shortID(n.ID())
	}
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

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func shortImage(image string) string {
	if image == "" {
		return "unknown"
	}
	if idx := strings.LastIndex(image, "/"); idx >= 0 && idx < len(image)-1 {
		image = image[idx+1:]
	}
	return image
}

func renderPane(tbl table.Model, active bool) string {
	content := tbl.View()
	style := boxStyle
	if active {
		style = activeBox
	}

	return style.Render(content)
}

func (m Model) currentJobID() string {
	row := m.jobs.SelectedRow()
	if len(row) < 4 {
		return ""
	}
	return row[3]
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
		return nil
	}
	if _, ok := m.graph.Node(id); !ok {
		return nil
	}
	m.focusedNodeID = id
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

func jobsToRows(jobs []api.Job) []table.Row {
	rows := make([]table.Row, len(jobs))
	for i, job := range jobs {
		rows[i] = table.Row{
			job.Alias,
			formatStringMap(job.Labels),
			formatStringMap(job.Annotations),
			job.ID,
			job.CreatedAt.Format(time.RFC3339),
		}
	}
	return rows
}

func formatStringMap(values map[string]string) string {
	if len(values) == 0 {
		return "-"
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, values[key]))
	}

	return strings.Join(parts, ", ")
}

func triggersToRows(triggers []api.Trigger) []table.Row {
	rows := make([]table.Row, len(triggers))
	for i, trigger := range triggers {
		rows[i] = table.Row{trigger.Alias, trigger.Type, trigger.ID}
	}
	return rows
}

func atomsToRows(atoms []api.Atom) []table.Row {
	rows := make([]table.Row, len(atoms))
	for i, atom := range atoms {
		rows[i] = table.Row{atom.Image, atom.Engine, atom.ID}
	}
	return rows
}

func (m Model) tableFor(sec section) table.Model {
	switch sec {
	case sectionJobs:
		return m.jobs
	case sectionTriggers:
		return m.triggers
	case sectionAtoms:
		return m.atoms
	default:
		return m.jobs
	}
}

func (m *Model) resizeColumns(width int) {
	if width <= 0 {
		return
	}
	jobsCols := buildColumns(jobColumnTitles, distributeWidths(width, jobColumnWeights))
	m.jobs.SetColumns(jobsCols)

	triggerCols := buildColumns(triggerColumnTitles, distributeWidths(width, triggerColumnWeights))
	m.triggers.SetColumns(triggerCols)

	atomCols := buildColumns(atomColumnTitles, distributeWidths(width, atomColumnWeights))
	m.atoms.SetColumns(atomCols)
}

func fetchAtomDetail(client *api.Client, atomID string) tea.Cmd {
	return func() tea.Msg {
		atom, err := client.Atoms().Get(context.Background(), atomID)
		if err != nil {
			return atomDetailErrMsg{id: atomID, err: err}
		}
		return atomDetailLoadedMsg{id: atomID, atom: atom}
	}
}

func fetchJobDetail(client *api.Client, jobID string, includeDAG bool) tea.Cmd {
	return func() tea.Msg {
		var opts *api.JobDetailOptions
		if includeDAG {
			opts = &api.JobDetailOptions{IncludeDAG: true}
		}

		detail, err := client.Jobs().Detail(context.Background(), jobID, opts)
		if err != nil {
			return jobDetailErrMsg{err: err}
		}

		return jobDetailLoadedMsg{detail: detail}
	}
}

func fetchData(client *api.Client) tea.Cmd {
	return func() tea.Msg {
		params := url.Values{}
		params.Set("order_by", "created_at desc")

		jobs, err := client.Jobs().List(context.Background(), params)
		if err != nil {
			return errMsg(err)
		}

		triggers, err := client.Triggers().List(context.Background(), url.Values{})
		if err != nil {
			return errMsg(err)
		}

		atoms, err := client.Atoms().List(context.Background(), url.Values{})
		if err != nil {
			return errMsg(err)
		}

		return dataLoadedMsg{
			jobs:     jobs,
			triggers: triggers,
			atoms:    atoms,
		}
	}
}

type jobDetailLoadedMsg struct {
	detail *api.JobDetail
}

type jobDetailErrMsg struct {
	err error
}

type atomDetailLoadedMsg struct {
	id   string
	atom *api.Atom
}

type atomDetailErrMsg struct {
	id  string
	err error
}

type dataLoadedMsg struct {
	jobs     []api.Job
	triggers []api.Trigger
	atoms    []api.Atom
}

type errMsg error

func centerText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return lipgloss.NewStyle().Align(lipgloss.Center).Render(value)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func createTable(titles []string, widths []int, focused bool) table.Model {
	columns := buildColumns(titles, widths)
	tbl := table.New(
		table.WithColumns(columns),
		table.WithHeight(10),
	)

	styles := table.DefaultStyles()
	styles.Header = styles.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true)

	styles.Selected = styles.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("63")).
		Bold(false)

	tbl.SetStyles(styles)
	if focused {
		tbl.Focus()
	}
	return tbl
}

func buildColumns(titles []string, widths []int) []table.Column {
	columns := make([]table.Column, len(titles))
	for i, title := range titles {
		width := 12
		if i < len(widths) && widths[i] > 0 {
			width = widths[i]
		}
		columns[i] = table.Column{Title: title, Width: width}
	}

	return columns
}

func distributeWidths(total int, weights []int) []int {
	if len(weights) == 0 {
		return nil
	}

	if total <= 0 {
		total = len(weights) * 12
	}

	sum := 0
	for _, w := range weights {
		sum += w
	}

	minWidth := 10
	widths := make([]int, len(weights))
	remaining := total

	for i, weight := range weights {
		if i == len(weights)-1 {
			widths[i] = max(minWidth, remaining)
			break
		}

		portion := max(minWidth, weight*total/sum)
		minRemaining := minWidth * (len(weights) - i - 1)
		if remaining-portion < minRemaining {
			portion = max(minWidth, remaining-minRemaining)
		}

		widths[i] = portion
		remaining -= portion
	}

	return widths
}

func renderTabs(active section) string {
	sections := []section{sectionJobs, sectionTriggers, sectionAtoms}
	tabs := make([]string, len(sections))
	for i, sec := range sections {
		label := fmt.Sprintf("%d %s", i+1, sectionNames[sec])
		if sec == active {
			tabs[i] = tabActive.Render(label)
		} else {
			tabs[i] = tabInactive.Render(label)
		}
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
}

func renderTabsBar(active section, totalWidth int) string {
	tabs := renderTabs(active)
	logo := logoStyle.Render("┌────┐\n│ Cs │\n└────┘")
	if totalWidth <= 0 {
		return lipgloss.JoinHorizontal(lipgloss.Top, tabs, logo)
	}

	logoWidth := lipgloss.Width(logo)
	leftWidth := max(0, totalWidth-logoWidth)
	left := lipgloss.NewStyle().Width(leftWidth).MaxWidth(leftWidth).Render(tabs)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, logo)
}
