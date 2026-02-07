package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/caesium-cloud/caesium/cmd/console/ui/detail"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

var (
	barStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Padding(0, 1)
	boxStyle     = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	placeholder  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	tabActive    = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57")).Bold(true)
	tabInactive  = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("240"))
	modalStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63")).Padding(1, 2)
	modalTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	modalHint    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	sectionNames = map[section]string{
		sectionJobs:     "Jobs",
		sectionTriggers: "Triggers",
		sectionAtoms:    "Atoms",
	}
	logoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).PaddingRight(1)
)

// View renders the interface.
func (m Model) View() string {
	tabs := renderTabsBar(m.active, m.viewportWidth, m.themeName)

	footerKeys := []string{"[1/2/3] switch", "[tab] cycle", "[r] reload", "[p] ping", "[T] theme", "[?] help", "[q] quit"}
	if m.active == sectionJobs {
		if m.showDetail {
			footerKeys = []string{
				"[esc/q] back",
				"[←/→] traverse",
				"[tab/shift+tab] cycle",
				"[↑/↓] scroll",
				"[f] focus path",
				"[u] runs",
				"[t] trigger",
				"[g] logs",
				"[space] follow",
				"[/] filter",
				"[c] clear",
				"[e] export",
				"[pgup/pgdn] log scroll",
				"[p] ping",
				"[T] theme",
				"[?] help",
			}
		} else {
			footerKeys = append(footerKeys, "[enter] detail", "[t] trigger")
		}
	}
	footer := renderFooter(footerKeys, m.actionStatusText(), m.viewportWidth)

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
			body = m.renderTablePane(&m.triggers, true)
		case sectionAtoms:
			body = m.renderTablePane(&m.atoms, true)
		default:
			activeTable := m.tableFor(m.active)
			body = m.renderTablePane(&activeTable, true)
		}
	}

	screen := lipgloss.JoinVertical(lipgloss.Left, tabs, body, footer)
	if m.showRunsModal {
		screen = m.renderRunsModal(screen)
	}
	if m.showLogsModal {
		screen = m.renderLogsModal(screen)
	}
	if m.confirmAction != nil {
		screen = m.renderConfirmModal(screen)
	}
	if m.showHelp {
		screen = m.renderHelpModal(screen)
	}
	return screen
}

func (m Model) renderJobsView() string {
	return m.renderTablePane(&m.jobs, true)
}

func (m Model) renderJobDetailScreen() string {
	totalWidth := max(m.viewportWidth-6, 40)

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
		ActiveRun:     m.activeRun(),
		Graph:         m.graph,
		GraphLayout:   m.dagLayout,
		GraphViewport: &m.dagViewport,
		FocusPath:     m.dagFocusPath,
		FocusedNode:   m.focusedNodeID,
		FocusedAtom:   focusedAtom,
		TaskStatus:    m.taskRunStatus,
		DetailErr:     m.detailErr,
		DetailPending: m.detailLoading,
		GraphErr:      m.dagErr,
		AtomErr:       m.atomErr,
		AtomLoading:   m.loadingAtomID != "",
		AtomLookup:    m.atomIndex,
		Labeler:       labeler,
		Spinner:       m.spinner.View(),
		ViewportWidth: max(totalWidth-4, 20),
	}

	content := detail.Render(vm)
	body := boxStyle.Width(totalWidth).Render(content)

	return body
}

func (m Model) renderTablePane(tbl *table.Model, active bool) string {
	available := m.viewportWidth
	if available <= 0 {
		available = 80
	}
	available = max(20, available-2) // leave a small buffer but stay wide

	border := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240"))
	if active {
		border = border.BorderForeground(lipgloss.Color("63"))
	}

	frame := border.GetHorizontalFrameSize()
	outerWidth := available
	innerWidth := max(outerWidth-frame, 20)

	m.resizeColumnsToWidth(innerWidth, tbl)
	tbl.SetWidth(innerWidth)

	content := lipgloss.NewStyle().
		Width(innerWidth).
		MaxWidth(innerWidth).
		Render(tbl.View())

	return border.Width(outerWidth).Render(content)
}

func (m *Model) resizeColumnsToWidth(width int, tbl *table.Model) {
	if width <= 0 || tbl == nil {
		return
	}
	switch tbl {
	case &m.jobs:
		m.jobs.SetColumns(adjustColumnsToWidth(buildColumns(jobColumnTitles, distributeWidths(width, jobColumnWeights)), width))
	case &m.triggers:
		m.triggers.SetColumns(adjustColumnsToWidth(buildColumns(triggerColumnTitles, distributeWidths(width, triggerColumnWeights)), width))
	case &m.atoms:
		m.atoms.SetColumns(adjustColumnsToWidth(buildColumns(atomColumnTitles, distributeWidths(width, atomColumnWeights)), width))
	}
}

func adjustColumnsToWidth(cols []table.Column, target int) []table.Column {
	if target <= 0 || len(cols) == 0 {
		return cols
	}
	sum := 0
	for _, c := range cols {
		sum += c.Width
	}
	// account for spacing between columns (one space per gap)
	sum += len(cols) - 1
	if sum < target {
		extra := target - sum
		last := len(cols) - 1
		cols[last].Width += extra
	}
	return cols
}

func (m Model) nodeLabeler() dag.LabelFunc {
	atoms := m.atomIndex
	taskStatus := m.taskRunStatus
	spin := m.spinner.View()
	return func(n *dag.Node) string {
		if n == nil {
			return ""
		}

		status := ""
		if taskStatus != nil {
			if task, ok := taskStatus[n.ID()]; ok {
				status = statusBadge(task.Status, spin, true)
			}
		}

		if atom, ok := atoms[n.AtomID()]; ok {
			label := fmt.Sprintf("%s (%s)", shortImage(atom.Image), shortID(n.ID()))
			if status != "" {
				return fmt.Sprintf("%s %s", status, label)
			}
			return label
		}
		label := shortID(n.ID())
		if status != "" {
			return fmt.Sprintf("%s %s", status, label)
		}
		return label
	}
}

func (m Model) actionStatusText() string {
	diagnostics := m.diagnosticsStatusText()
	if m.actionErr != nil {
		title := strings.TrimSpace(m.actionNotice)
		if title == "" {
			title = "Action failed"
		}
		msg := fmt.Sprintf("%s: %s", title, m.actionErr.Error())
		if diagnostics != "" {
			return msg + "  |  " + diagnostics
		}
		return msg
	}
	notice := strings.TrimSpace(m.actionNotice)
	if notice == "" {
		return diagnostics
	}
	if diagnostics == "" {
		return notice
	}
	return notice + "  |  " + diagnostics
}

func (m Model) diagnosticsStatusText() string {
	if m.apiCheckedAt.IsZero() {
		return ""
	}
	health := "api:healthy"
	if !m.apiHealthy {
		health = "api:degraded"
	}
	parts := []string{
		health,
		fmt.Sprintf("ping:%s", m.apiLatency.Round(time.Millisecond)),
		fmt.Sprintf("load:%s", m.lastLoadLatency.Round(time.Millisecond)),
		fmt.Sprintf("retries:%d", m.lastLoadRetries),
		fmt.Sprintf("checked:%s", m.apiCheckedAt.Format("15:04:05")),
	}
	if m.apiHealthErr != nil {
		parts = append(parts, fmt.Sprintf("err:%s", m.apiHealthErr.Error()))
	}
	return strings.Join(parts, "  ")
}

func statusBadge(status, spinnerFrame string, compact bool) string {
	normalized := strings.ToLower(strings.TrimSpace(status))
	label := titleCase(normalized)
	switch normalized {
	case "running", "pending":
		if spinnerFrame != "" {
			return fmt.Sprintf("%s %s", spinnerFrame, label)
		}
		return label
	case "succeeded":
		if compact {
			return "✓"
		}
		return "✓ Succeeded"
	case "failed":
		if compact {
			return "✗"
		}
		return "✗ Failed"
	default:
		if label == "" {
			return "-"
		}
		if compact {
			return label
		}
		return strings.ToUpper(status)
	}
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

func renderTabsBar(active section, totalWidth int, themeName string) string {
	tabs := renderTabs(active)
	logo := logoStyle.Render("┌────┐\n│ Cs │\n└────┘")
	if trimmed := strings.TrimSpace(themeName); trimmed != "" {
		tag := truncateText(trimmed, 4)
		logo = logoStyle.Render(fmt.Sprintf("┌────┐\n│%4s│\n└────┘", tag))
	}
	if totalWidth <= 0 {
		return lipgloss.JoinHorizontal(lipgloss.Top, tabs, logo)
	}

	logoWidth := lipgloss.Width(logo)
	leftWidth := max(totalWidth-logoWidth, 0)
	left := lipgloss.NewStyle().Width(leftWidth).MaxWidth(leftWidth).Render(tabs)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, logo)
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

func centerText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return lipgloss.NewStyle().Align(lipgloss.Center).Render(value)
}

func (m Model) renderLogsModal(background string) string {
	width := m.viewportWidth
	height := m.viewportHeight
	if width <= 0 {
		width = lipgloss.Width(background)
	}
	if height <= 0 {
		height = lipgloss.Height(background)
	}

	modalWidth := max(40, width-10)
	if modalWidth > width-4 {
		modalWidth = width - 4
	}
	modalHeight := max(12, height-6)
	if modalHeight > height-2 {
		modalHeight = height - 2
	}

	contentWidth := max(modalWidth-4, 20)
	contentHeight := max(modalHeight-4, 8)
	m.logsViewport.Width = contentWidth
	m.logsViewport.Height = contentHeight
	m.logsViewport.SetContent(strings.TrimSuffix(m.filteredLogContent(), "\n"))

	title := "Task Logs"
	var labels []string
	runID := m.logRunID
	if runID == "" {
		if run := m.activeRun(); run != nil {
			runID = run.ID
		}
	}
	if runID != "" {
		labels = append(labels, fmt.Sprintf("run %s", shortID(runID)))
	}
	if m.focusedNodeID != "" {
		labels = append(labels, fmt.Sprintf("task %s", shortID(m.focusedNodeID)))
	}
	if len(labels) > 0 {
		title = fmt.Sprintf("Task Logs (%s)", strings.Join(labels, ", "))
	}
	followState := "following"
	if !m.logsFollow {
		followState = "paused"
	}
	filterState := "filter off"
	if query := strings.TrimSpace(m.logFilter); query != "" {
		filterState = fmt.Sprintf("filter: %q", query)
	}
	if m.logFilterInput {
		filterState = fmt.Sprintf("filter: %q (editing)", m.logFilter)
	}
	hint := modalHint.Render(fmt.Sprintf("follow: %s  •  %s  •  / edit  •  c clear  •  e export", followState, filterState))

	var body string
	switch {
	case m.logsErr != nil:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, boxStyle.Render(fmt.Sprintf("Error: %s", m.logsErr.Error())))
	case m.logsLoading:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, centerText(fmt.Sprintf("%s Streaming logs…", m.spinner.View())))
	case strings.TrimSpace(m.logContent) == "":
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, placeholder.Render("Press g to stream logs for the focused task."))
	case strings.TrimSpace(m.filteredLogContent()) == "":
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, placeholder.Render(fmt.Sprintf("No log lines match %q.", m.logFilter)))
	default:
		filterPrompt := ""
		if m.logFilterInput {
			filterPrompt = modalHint.Render(fmt.Sprintf("Filter query: %s_", m.logFilter))
		}
		if filterPrompt != "" {
			body = fmt.Sprintf("%s\n%s\n%s\n\n%s", modalTitle.Render(title), hint, filterPrompt, m.logsViewport.View())
		} else {
			body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, m.logsViewport.View())
		}
	}

	modal := modalStyle.Width(modalWidth).Height(modalHeight).Render(body)

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("235")))
}

func (m Model) renderConfirmModal(background string) string {
	req := m.confirmAction
	if req == nil {
		return background
	}
	width := m.viewportWidth
	height := m.viewportHeight
	if width <= 0 {
		width = lipgloss.Width(background)
	}
	if height <= 0 {
		height = lipgloss.Height(background)
	}

	modalWidth := max(40, width-14)
	if modalWidth > width-4 {
		modalWidth = width - 4
	}
	modalHeight := max(10, height-8)
	if modalHeight > height-2 {
		modalHeight = height - 2
	}

	title, prompt := actionConfirmCopy(req, m.jobLabel(req.jobID))
	hint := modalHint.Render("enter/y confirm  •  esc/n cancel")

	body := fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, boxStyle.Render(prompt))
	modal := modalStyle.Width(modalWidth).Height(modalHeight).Render(body)

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("235")))
}

func (m Model) renderHelpModal(background string) string {
	width := m.viewportWidth
	height := m.viewportHeight
	if width <= 0 {
		width = lipgloss.Width(background)
	}
	if height <= 0 {
		height = lipgloss.Height(background)
	}

	modalWidth := max(64, width-12)
	if modalWidth > width-4 {
		modalWidth = width - 4
	}
	modalHeight := max(18, height-6)
	if modalHeight > height-2 {
		modalHeight = height - 2
	}

	body := strings.Join([]string{
		modalTitle.Render("Console Help"),
		modalHint.Render("esc / q / ? close"),
		"",
		"Global",
		"  1/2/3 switch tabs  •  tab/shift+tab cycle tabs  •  r reload",
		"  p health ping  •  T cycle theme  •  ? help  •  q quit",
		"",
		"Jobs Detail",
		"  enter detail  •  t trigger  •  u runs  •  g logs",
		"  left/right traverse DAG  •  f focus path",
		"",
		"Runs + Logs",
		"  Runs: R re-run  •  Logs: space follow/pause  •  / filter",
		"  Logs: c clear filter  •  e export snippet  •  pgup/pgdn scroll",
	}, "\n")

	modal := modalStyle.Width(modalWidth).Height(modalHeight).Render(body)
	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("235")))
}

func actionConfirmCopy(req *actionRequest, jobLabel string) (string, string) {
	if req == nil {
		return "Confirm Action", "Proceed?"
	}
	label := strings.TrimSpace(req.label)
	if jobLabel == "" {
		jobLabel = "job"
	}
	switch req.kind {
	case actionTrigger:
		title := "Trigger Job"
		if label == "" {
			label = jobLabel
		}
		return title, fmt.Sprintf("Trigger %s now?", label)
	case actionRerun:
		title := "Re-run Job"
		if label == "" {
			label = "selected run"
		}
		return title, fmt.Sprintf("Re-run %s for %s?", label, jobLabel)
	default:
		return "Confirm Action", "Proceed?"
	}
}

func renderFooter(keys []string, status string, width int) string {
	lines := wrapKeys(keys, width)
	footer := styleFooterLines(lines)
	status = strings.TrimSpace(status)
	if status != "" {
		status = truncateText(status, width)
		if footer != "" {
			footer = lipgloss.JoinVertical(lipgloss.Left, footer, barStyle.Render(status))
		} else {
			footer = barStyle.Render(status)
		}
	}
	return footer
}

func wrapKeys(keys []string, width int) string {
	if len(keys) == 0 {
		return ""
	}
	if width <= 0 {
		return strings.Join(keys, "  ")
	}
	lines := []string{}
	line := ""
	for _, key := range keys {
		if key == "" {
			continue
		}
		if line == "" {
			line = key
			continue
		}
		candidate := line + "  " + key
		if lipgloss.Width(candidate) > width {
			lines = append(lines, line)
			line = key
			continue
		}
		line = candidate
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func styleFooterLines(lines string) string {
	if lines == "" {
		return ""
	}
	parts := strings.Split(lines, "\n")
	for i, part := range parts {
		parts[i] = barStyle.Render(part)
	}
	return strings.Join(parts, "\n")
}

func truncateText(value string, width int) string {
	value = strings.TrimSpace(value)
	if width <= 0 || lipgloss.Width(value) <= width {
		return value
	}
	if width <= 3 {
		return value[:width]
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	return string(runes[:width-3]) + "..."
}

func (m Model) renderRunsModal(background string) string {
	width := m.viewportWidth
	height := m.viewportHeight
	if width <= 0 {
		width = lipgloss.Width(background)
	}
	if height <= 0 {
		height = lipgloss.Height(background)
	}

	modalWidth := max(50, width-10)
	if modalWidth > width-4 {
		modalWidth = width - 4
	}
	modalHeight := max(12, height-6)
	if modalHeight > height-2 {
		modalHeight = height - 2
	}

	contentWidth := max(modalWidth-4, 20)
	contentHeight := max(modalHeight-6, 6)

	title := "Select Run"
	hint := modalHint.Render("enter select  •  r refresh  •  R re-run  •  esc close")

	var body string
	switch {
	case m.runsErr != nil:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, boxStyle.Render(fmt.Sprintf("Error: %s", m.runsErr.Error())))
	case m.runsLoading:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, centerText(fmt.Sprintf("%s Loading runs…", m.spinner.View())))
	case len(m.runs) == 0:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, placeholder.Render("No runs recorded yet."))
	default:
		table := m.runsTable
		table.SetColumns(adjustColumnsToWidth(buildColumns(runColumnTitles, distributeWidths(contentWidth, runColumnWeights)), contentWidth))
		table.SetWidth(contentWidth)
		table.SetHeight(contentHeight)
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, table.View())
	}

	modal := modalStyle.Width(modalWidth).Height(modalHeight).Render(body)

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("235")))
}
