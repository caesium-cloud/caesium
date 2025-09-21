package app

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
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

	jobColumnTitles      = []string{"Alias", "ID", "Created"}
	jobColumnWeights     = []int{3, 4, 3}
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
}

// New creates the root model with dependency references.
func New(client *api.Client) Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	jobs := createTable(jobColumnTitles, []int{20, 20, 20}, true)
	triggers := createTable(triggerColumnTitles, []int{20, 12, 20}, false)
	atoms := createTable(atomColumnTitles, []int{24, 12, 20}, false)

	return Model{
		client:   client,
		spinner:  sp,
		state:    statusLoading,
		active:   sectionJobs,
		jobs:     jobs,
		triggers: triggers,
		atoms:    atoms,
	}
}

// Init bootstraps async fetch and spinner tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchData(m.client))
}

// Update handles Bubble Tea messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "r":
			m.state = statusLoading
			m.err = nil
			return m, tea.Batch(m.spinner.Tick, fetchData(m.client))
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
		return m, cmd
	case dataLoadedMsg:
		m.state = statusReady
		m.err = nil
		m.jobs.SetRows(jobsToRows(msg.jobs))
		m.triggers.SetRows(triggersToRows(msg.triggers))
		m.atoms.SetRows(atomsToRows(msg.atoms))
		m = m.activate(sectionJobs)
	case errMsg:
		m.state = statusError
		m.err = msg
	}

	if m.state != statusReady {
		return m, nil
	}

	var cmd tea.Cmd
	switch m.active {
	case sectionJobs:
		m.jobs, cmd = m.jobs.Update(msg)
	case sectionTriggers:
		m.triggers, cmd = m.triggers.Update(msg)
	case sectionAtoms:
		m.atoms, cmd = m.atoms.Update(msg)
	}

	return m, cmd
}

// View renders the interface.
func (m Model) View() string {
	tabs := renderTabsBar(m.active, m.viewportWidth)
	footer := barStyle.Render("[1/2/3] switch  [tab] cycle  [r] reload  [q] quit")

	var body string

	switch m.state {
	case statusLoading:
		body = centerText(fmt.Sprintf("%s Loading data…", m.spinner.View()))
	case statusError:
		body = boxStyle.Render("Failed to load data: " + m.err.Error())
	case statusReady:
		activeTable := m.tableFor(m.active)
		body = renderPane(activeTable, true)
	}

	return lipgloss.JoinVertical(lipgloss.Left, tabs, body, footer)
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
	m.active = sec
	return m
}

func renderPane(tbl table.Model, active bool) string {
	content := tbl.View()
	style := boxStyle
	if active {
		style = activeBox
	}

	return style.Render(content)
}

func jobsToRows(jobs []api.Job) []table.Row {
	rows := make([]table.Row, len(jobs))
	for i, job := range jobs {
		rows[i] = table.Row{job.Alias, job.ID, job.CreatedAt.Format(time.RFC3339)}
	}
	return rows
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
