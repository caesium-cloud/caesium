package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/caesium-cloud/caesium/cmd/console/ui/detail"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	barStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Padding(0, 1)
	boxStyle     = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	placeholder  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	tabActive    = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57")).Bold(true)
	tabInactive  = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("240"))
	tabBarStyle  = lipgloss.NewStyle().BorderBottom(true).BorderStyle(lipgloss.NormalBorder()).BorderBottomForeground(lipgloss.Color("240")).PaddingBottom(0).MarginBottom(0)
	summaryStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Padding(0, 2)
	filterStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Padding(0, 2)
	modalStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63")).Padding(1, 2)
	modalTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	modalHint    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	sectionNames = map[section]string{
		sectionJobs:     "Jobs",
		sectionTriggers: "Triggers",
		sectionAtoms:    "Atoms",
		sectionStats:    "Stats",
	}
	logoStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true).PaddingRight(1)
	logoDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

type tableKind int

const (
	tableKindJobs tableKind = iota
	tableKindTriggers
	tableKindAtoms
)

// View renders the interface.
func (m Model) View() string {
	tabs := renderTabsBar(m.active, m.viewportWidth)

	footerKeys := globalFooterKeys()
	if m.active == sectionJobs {
		if m.showDetail {
			footerKeys = jobsDetailFooterKeys(m.showLogsModal)
		} else {
			footerKeys = append(footerKeys, "[enter] detail", "[t] trigger", "[/] filter")
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
			body = m.renderTablePane(&m.triggers, true, tableKindTriggers)
		case sectionAtoms:
			body = m.renderTablePane(&m.atoms, true, tableKindAtoms)
		case sectionStats:
			body = m.renderStatsSection()
		default:
			activeTable := m.tableFor(m.active)
			body = m.renderTablePane(&activeTable, true, tableKindForSection(m.active))
		}
	}

	screen := lipgloss.JoinVertical(lipgloss.Left, tabs, body, footer)
	if m.showNodeDetail {
		screen = m.renderNodeDetailModal(screen)
	}
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

func globalFooterKeys() []string {
	return []string{"[1/2/3/4] switch", "[tab] cycle", "[r] reload", "[p] ping", "[q] quit", "[T] theme", "[?] help"}
}

func (m Model) renderStatsSection() string {
	if m.statsLoading {
		return centerText(fmt.Sprintf("%s Loading stats…", m.spinner.View()))
	}
	if m.statsErr != nil {
		return boxStyle.Render("Failed to load stats: " + m.statsErr.Error())
	}
	return renderStatsView(m.statsData, m.viewportWidth)
}

func jobsDetailFooterKeys(showLogs bool) []string {
	keys := []string{
		"[esc/q] back",
		"[enter] node info",
		"[←/→] traverse",
		"[tab/shift+tab] cycle",
		"[↑/↓] scroll",
		"[f] focus path",
		"[u] runs",
		"[t] trigger",
		"[g] logs",
		"[p] ping",
		"[q] quit",
		"[T] theme",
		"[?] help",
	}
	if showLogs {
		keys = append(keys,
			"[space] follow",
			"[/] filter",
			"[c] clear",
			"[e] export",
			"[pgup/pgdn] log scroll",
		)
	}
	return keys
}

func (m Model) renderJobsView() string {
	summary := m.renderJobsSummaryStrip()
	parts := []string{summary}

	if m.jobFilterInput || m.jobFilter != "" {
		var filterLine string
		if m.jobFilterInput {
			filterLine = filterStyle.Render(fmt.Sprintf("🔍 %s▏", m.jobFilter))
		} else {
			filterLine = filterStyle.Render(fmt.Sprintf("🔍 %q  (/ edit, esc clear)", m.jobFilter))
		}
		parts = append(parts, filterLine)
	}

	if len(m.jobRecords) == 0 {
		msg := placeholder.Padding(1, 2).Render("No jobs found. Create a job definition to get started.")
		parts = append(parts, msg)
		return lipgloss.JoinVertical(lipgloss.Left, parts...)
	}

	tbl := m.renderTablePane(&m.jobs, true, tableKindJobs)
	parts = append(parts, tbl)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m Model) renderJobsSummaryStrip() string {
	total := len(m.jobRecords)
	var running, succeeded, failed, pending, noRuns int
	for _, job := range m.jobRecords {
		run, ok := m.jobRunStatus[job.ID]
		if !ok || run == nil {
			noRuns++
			continue
		}
		switch strings.ToLower(strings.TrimSpace(run.Status)) {
		case "running":
			running++
		case "succeeded":
			succeeded++
		case "failed":
			failed++
		case "pending":
			pending++
		default:
			noRuns++
		}
	}

	sc := CurrentStatusColors()
	totalLabel := lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("%d jobs", total))
	parts := []string{totalLabel}

	divider := logoDimStyle.Render("│")

	if succeeded > 0 {
		parts = append(parts, divider,
			lipgloss.NewStyle().Foreground(lipgloss.Color(sc.Success)).Render(fmt.Sprintf("✓ %d succeeded", succeeded)))
	}
	if running > 0 {
		parts = append(parts, divider,
			lipgloss.NewStyle().Foreground(lipgloss.Color(sc.Running)).Render(fmt.Sprintf("%s %d running", m.spinner.View(), running)))
	}
	if failed > 0 {
		parts = append(parts, divider,
			lipgloss.NewStyle().Foreground(lipgloss.Color(sc.Error)).Render(fmt.Sprintf("✗ %d failed", failed)))
	}
	if pending > 0 {
		parts = append(parts, divider,
			lipgloss.NewStyle().Foreground(lipgloss.Color(sc.Pending)).Render(fmt.Sprintf("· %d pending", pending)))
	}
	if noRuns > 0 {
		parts = append(parts, divider,
			logoDimStyle.Render(fmt.Sprintf("– %d idle", noRuns)))
	}
	return summaryStyle.Render(strings.Join(parts, " "))
}

func (m Model) renderJobDetailScreen() string {
	totalWidth := max(m.viewportWidth-6, 40)

	labeler := m.nodeLabeler()
	vm := detail.ViewModel{
		Job:           m.jobDetail,
		ActiveRun:     m.activeRun(),
		Graph:         m.graph,
		GraphLayout:   m.dagLayout,
		GraphViewport: &m.dagViewport,
		FocusPath:     m.dagFocusPath,
		DetailErr:     m.detailErr,
		DetailPending: m.detailLoading,
		GraphErr:      m.dagErr,
		Labeler:       labeler,
		Spinner:       m.spinner.View(),
		ViewportWidth: max(totalWidth-4, 20),
	}

	content := detail.Render(vm)
	body := boxStyle.Width(totalWidth).Render(content)

	return body
}

func (m Model) renderTablePane(tbl *table.Model, active bool, kind tableKind) string {
	available := m.viewportWidth
	if available <= 0 {
		available = 80
	}
	available = max(20, available-4) // leave buffer for margins

	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		MarginLeft(1).MarginRight(1)
	if active {
		border = border.BorderForeground(lipgloss.Color("63"))
	}

	frame := border.GetHorizontalFrameSize()
	outerWidth := available
	innerWidth := max(outerWidth-frame, 20)

	m.resizeColumnsToWidth(innerWidth, tbl, kind)
	tbl.SetWidth(innerWidth)

	content := lipgloss.NewStyle().
		Width(innerWidth).
		MaxWidth(innerWidth).
		Render(tbl.View())

	return border.Width(outerWidth).Render(content)
}

func (m *Model) resizeColumnsToWidth(width int, tbl *table.Model, kind tableKind) {
	if width <= 0 || tbl == nil {
		return
	}
	switch kind {
	case tableKindJobs:
		m.jobs.SetColumns(adjustColumnsToWidth(buildColumns(jobColumnTitles, distributeWidths(width, jobColumnWeights)), width))
		tbl.SetColumns(adjustColumnsToWidth(buildColumns(jobColumnTitles, distributeWidths(width, jobColumnWeights)), width))
	case tableKindTriggers:
		m.triggers.SetColumns(adjustColumnsToWidth(buildColumns(triggerColumnTitles, distributeWidths(width, triggerColumnWeights)), width))
		tbl.SetColumns(adjustColumnsToWidth(buildColumns(triggerColumnTitles, distributeWidths(width, triggerColumnWeights)), width))
	case tableKindAtoms:
		m.atoms.SetColumns(adjustColumnsToWidth(buildColumns(atomColumnTitles, distributeWidths(width, atomColumnWeights)), width))
		tbl.SetColumns(adjustColumnsToWidth(buildColumns(atomColumnTitles, distributeWidths(width, atomColumnWeights)), width))
	}
}

func tableKindForSection(sec section) tableKind {
	switch sec {
	case sectionTriggers:
		return tableKindTriggers
	case sectionAtoms:
		return tableKindAtoms
	default:
		return tableKindJobs
	}
}

func adjustColumnsToWidth(cols []table.Column, target int) []table.Column {
	if target <= 0 || len(cols) == 0 {
		return cols
	}

	// Bubble table cells are rendered with horizontal padding on each column
	// (`table.DefaultStyles().Cell` and Header both use Padding(0, 1)),
	// so we must reserve that frame width before distributing content widths.
	const perColumnFrame = 2 // left+right padding
	targetContent := target - (len(cols) * perColumnFrame)
	if targetContent < len(cols) {
		targetContent = len(cols)
	}

	sum := 0
	for _, c := range cols {
		sum += c.Width
	}

	if sum < targetContent {
		extra := targetContent - sum
		last := len(cols) - 1
		cols[last].Width += extra
		return cols
	}
	if sum == targetContent {
		return cols
	}

	// Shrink from right to left first so primary identifiers (left columns)
	// retain readability when space is constrained.
	reduce := sum - targetContent
	const minColumnWidth = 6
	for i := len(cols) - 1; i >= 0 && reduce > 0; i-- {
		available := cols[i].Width - minColumnWidth
		if available <= 0 {
			continue
		}
		delta := available
		if delta > reduce {
			delta = reduce
		}
		cols[i].Width -= delta
		reduce -= delta
	}

	// If the minimum width guard prevented full fit, force down to width 1.
	for i := len(cols) - 1; i >= 0 && reduce > 0; i-- {
		available := cols[i].Width - 1
		if available <= 0 {
			continue
		}
		delta := available
		if delta > reduce {
			delta = reduce
		}
		cols[i].Width -= delta
		reduce -= delta
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
	sections := []section{sectionJobs, sectionTriggers, sectionAtoms, sectionStats}
	tabs := make([]string, len(sections))
	for i, sec := range sections {
		label := fmt.Sprintf(" %d %s ", i+1, sectionNames[sec])
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
	logo := logoStyle.Render("Cs") + logoDimStyle.Render("·caesium")
	if totalWidth <= 0 {
		bar := lipgloss.JoinHorizontal(lipgloss.Top, tabs, "  ", logo)
		return tabBarStyle.Render(bar)
	}

	logoWidth := lipgloss.Width(logo)
	leftWidth := max(totalWidth-logoWidth-2, 0)
	left := lipgloss.NewStyle().Width(leftWidth).MaxWidth(leftWidth).Render(tabs)
	bar := lipgloss.JoinHorizontal(lipgloss.Top, left, logo)
	return tabBarStyle.Width(totalWidth).Render(bar)
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
	hint := renderModalHint([]string{
		fmt.Sprintf("follow: %s", followState),
		filterState,
		"/ edit",
		"c clear",
		"e export",
	}, contentWidth)

	var body string
	switch {
	case m.logsErr != nil:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, boxStyle.Render(fmt.Sprintf("Error: %s", m.logsErr.Error())))
	case m.logsLoading:
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, centerText(fmt.Sprintf("%s Streaming logs…", m.spinner.View())))
	case strings.TrimSpace(m.logContent) == "":
		msg := "No log lines received yet for this task."
		if strings.TrimSpace(m.logTaskID) == "" {
			msg = "Select a task to stream logs."
		}
		body = fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, placeholder.Render(msg))
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

	title, prompt := actionConfirmCopy(req, m.jobLabel(req.jobID))
	modalWidth, modalHeight := confirmModalDimensions(width, height, title, prompt)
	contentWidth := max(modalWidth-6, 16)
	hint := renderModalHint([]string{"enter/y confirm", "esc/n cancel"}, contentWidth)
	promptBody := lipgloss.NewStyle().
		Width(contentWidth).
		MaxWidth(contentWidth).
		Align(lipgloss.Center).
		Render(prompt)

	body := fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, promptBody)
	modal := modalStyle.Width(modalWidth).Height(modalHeight).Render(body)

	return overlayCentered(background, modal, width, height)
}

func confirmModalDimensions(viewWidth, viewHeight int, title, prompt string) (int, int) {
	if viewWidth <= 0 {
		viewWidth = 80
	}
	if viewHeight <= 0 {
		viewHeight = 24
	}

	// Keep confirm dialogs intentionally compact; they should feel like a prompt,
	// not a full-screen page.
	const minModalWidth = 36
	const maxModalWidth = 72
	maxAllowed := viewWidth - 4
	if maxAllowed < minModalWidth {
		maxAllowed = minModalWidth
	}

	base := max(lipgloss.Width(title), lipgloss.Width(prompt))
	width := base + 8 // border/padding breathing room
	if width < minModalWidth {
		width = minModalWidth
	}
	if width > maxModalWidth {
		width = maxModalWidth
	}
	if width > maxAllowed {
		width = maxAllowed
	}

	contentWidth := max(width-6, 12)
	promptLines := wrapTokens([]string{prompt}, contentWidth, " ")
	lineCount := len(promptLines)
	if lineCount == 0 {
		lineCount = 1
	}
	height := 7 + lineCount // title + hint + spacing + prompt + frame/padding
	if height < 8 {
		height = 8
	}
	if height > 12 {
		height = 12
	}
	maxHeight := viewHeight - 2
	if maxHeight < 8 {
		maxHeight = 8
	}
	if height > maxHeight {
		height = maxHeight
	}

	return width, height
}

func overlayCentered(background, overlay string, width, height int) string {
	if width <= 0 {
		width = lipgloss.Width(background)
	}
	if height <= 0 {
		height = lipgloss.Height(background)
	}
	if width <= 0 || height <= 0 {
		return background
	}

	canvas := normalizeCanvas(background, width, height)
	overlayLines := strings.Split(overlay, "\n")
	if len(overlayLines) == 0 {
		return strings.Join(canvas, "\n")
	}

	overlayWidth := 0
	for _, line := range overlayLines {
		if w := ansi.StringWidth(line); w > overlayWidth {
			overlayWidth = w
		}
	}
	if overlayWidth <= 0 {
		return strings.Join(canvas, "\n")
	}

	startX := max((width-overlayWidth)/2, 0)
	startY := max((height-len(overlayLines))/2, 0)

	for idx, line := range overlayLines {
		row := startY + idx
		if row < 0 || row >= len(canvas) {
			continue
		}
		if startX >= width {
			continue
		}

		segment := line
		segmentWidth := ansi.StringWidth(segment)
		if segmentWidth <= 0 {
			continue
		}
		if startX+segmentWidth > width {
			segment = ansi.Cut(segment, 0, width-startX)
			segmentWidth = ansi.StringWidth(segment)
		}
		if segmentWidth <= 0 {
			continue
		}

		base := canvas[row]
		prefix := ansi.Cut(base, 0, startX)
		suffix := ansi.Cut(base, startX+segmentWidth, width)
		canvas[row] = prefix + segment + suffix
	}

	return strings.Join(canvas, "\n")
}

func normalizeCanvas(background string, width, height int) []string {
	lines := strings.Split(background, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i, line := range lines {
		lineWidth := ansi.StringWidth(line)
		switch {
		case lineWidth > width:
			lines[i] = ansi.Cut(line, 0, width)
		case lineWidth < width:
			lines[i] = line + strings.Repeat(" ", width-lineWidth)
		}
	}
	return lines
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
		"  1/2/3/4 switch tabs  •  tab/shift+tab cycle tabs  •  r reload",
		"  p health ping  •  T cycle theme  •  ? help  •  q quit",
		"",
		"Jobs List",
		"  enter detail  •  t trigger  •  / filter jobs  •  esc clear filter",
		"",
		"Jobs Detail",
		"  enter node info  •  t trigger  •  u runs  •  g logs",
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

func (m Model) renderNodeDetailModal(background string) string {
	if m.graph == nil || m.focusedNodeID == "" {
		return background
	}
	node, ok := m.graph.Node(m.focusedNodeID)
	if !ok {
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

	modalWidth := max(50, width-12)
	if modalWidth > width-4 {
		modalWidth = width - 4
	}
	modalHeight := max(14, height-6)
	if modalHeight > height-2 {
		modalHeight = height - 2
	}

	label := strings.TrimSpace(m.nodeLabeler()(node))
	if label == "" {
		label = node.ID()
	}
	title := fmt.Sprintf("Node: %s (%s)", label, shortID(node.ID()))

	var lines []string

	// Status
	if task, ok := m.taskRunStatus[node.ID()]; ok {
		badge := statusBadge(task.Status, m.spinner.View(), false)
		lines = append(lines, fmt.Sprintf("  Status      %s", badge))
		if strings.TrimSpace(task.ClaimedBy) != "" {
			lines = append(lines, fmt.Sprintf("  Claimed By  %s", task.ClaimedBy))
		}

		if task.Image != "" {
			lines = append(lines, fmt.Sprintf("  Image       %s", task.Image))
		}
		if task.Engine != "" {
			lines = append(lines, fmt.Sprintf("  Engine      %s", task.Engine))
		}
		if len(task.Command) > 0 {
			lines = append(lines, fmt.Sprintf("  Command     %s", strings.Join(task.Command, " ")))
		}
		if task.StartedAt != nil {
			lines = append(lines, fmt.Sprintf("  Started     %s", task.StartedAt.Format(time.RFC3339)))
		}
		dur := formatTaskDuration(task)
		if dur != "" {
			lines = append(lines, fmt.Sprintf("  Duration    %s", dur))
		}
	} else {
		lines = append(lines, "  Status      pending")
	}

	lines = append(lines, "")

	// Predecessors
	preds := node.Predecessors()
	if len(preds) == 0 {
		lines = append(lines, "  Predecessors  (none)")
	} else {
		predLabels := make([]string, len(preds))
		for i, p := range preds {
			predLabels[i] = fmt.Sprintf("%s (%s)", m.nodeDisplayLabel(p), shortID(p.ID()))
		}
		lines = append(lines, fmt.Sprintf("  Predecessors  %s", strings.Join(predLabels, ", ")))
	}

	// Successors
	succs := node.Successors()
	if len(succs) == 0 {
		lines = append(lines, "  Successors    (none)")
	} else {
		succLabels := make([]string, len(succs))
		for i, s := range succs {
			succLabels[i] = fmt.Sprintf("%s (%s)", m.nodeDisplayLabel(s), shortID(s.ID()))
		}
		lines = append(lines, fmt.Sprintf("  Successors    %s", strings.Join(succLabels, ", ")))
	}

	lines = append(lines, "")

	// Atom info
	atomID := node.AtomID()
	if atomID != "" {
		lines = append(lines, fmt.Sprintf("  Atom ID     %s", atomID))
		if atom, ok := m.atomDetails[atomID]; ok && atom != nil {
			if atom.ProvenanceSourceID != "" {
				lines = append(lines, fmt.Sprintf("  Source      %s", atom.ProvenanceSourceID))
			}
		} else if cached, ok := m.atomIndex[atomID]; ok {
			if cached.ProvenanceSourceID != "" {
				lines = append(lines, fmt.Sprintf("  Source      %s", cached.ProvenanceSourceID))
			}
		}
	}

	hint := modalHint.Render("[g] logs  [esc] close")

	body := fmt.Sprintf("%s\n%s\n\n%s", modalTitle.Render(title), hint, strings.Join(lines, "\n"))
	modal := modalStyle.Width(modalWidth).Height(modalHeight).Render(body)

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		modal,
		lipgloss.WithWhitespaceChars(" "),
		lipgloss.WithWhitespaceForeground(lipgloss.Color("235")))
}

func (m Model) nodeDisplayLabel(node *dag.Node) string {
	if node == nil {
		return ""
	}
	labeler := m.nodeLabeler()
	label := strings.TrimSpace(labeler(node))
	if label == "" {
		return shortID(node.ID())
	}
	// Strip status prefix for display
	label = stripNodeStatusPrefix(label)
	return label
}

func stripNodeStatusPrefix(label string) string {
	prefixes := []string{"✓ ", "✗ ", "· "}
	for _, prefix := range prefixes {
		if strings.HasPrefix(label, prefix) {
			return label[len(prefix):]
		}
	}
	if len(label) > 0 {
		r := []rune(label)
		if len(r) >= 2 && r[0] >= 0x2800 && r[0] <= 0x28FF && r[1] == ' ' {
			return string(r[2:])
		}
	}
	return label
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
	contentWidth := footerContentWidth(width)
	lines := fitWrappedKeys(keys, contentWidth, 2)
	footer := styleFooterLines(lines)
	status = strings.TrimSpace(status)
	if status != "" {
		status = truncateStatus(status, contentWidth)
		statusLine := logoDimStyle.Padding(0, 1).Render(status)
		if footer != "" {
			footer = lipgloss.JoinVertical(lipgloss.Left, footer, statusLine)
		} else {
			footer = statusLine
		}
	}
	return footer
}

func footerContentWidth(width int) int {
	if width <= 0 {
		return 78
	}
	return max(width-barStyle.GetHorizontalFrameSize(), 1)
}

func fitWrappedKeys(keys []string, width, maxLines int) []string {
	if len(keys) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if width > 0 && lipgloss.Width(key) > width {
			key = truncateText(key, width)
		}
		filtered = append(filtered, key)
	}
	if len(filtered) == 0 {
		return nil
	}
	for len(filtered) > 0 {
		lines := wrapTokens(filtered, width, "  ")
		if maxLines <= 0 || len(lines) <= maxLines {
			return lines
		}
		filtered = filtered[:len(filtered)-1]
	}
	return nil
}

func styleFooterLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, modalHint.Padding(0, 1).Render(line))
	}
	return strings.Join(parts, "\n")
}

func truncateStatus(value string, width int) string {
	value = strings.TrimSpace(value)
	if width <= 0 || lipgloss.Width(value) <= width {
		return value
	}
	parts := strings.Split(value, "  ")
	if len(parts) <= 1 {
		return truncateText(value, width)
	}
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		candidate := part
		if len(kept) > 0 {
			candidate = strings.Join(append(append([]string{}, kept...), part), "  ")
		}
		if lipgloss.Width(candidate) > width {
			break
		}
		kept = append(kept, part)
	}
	if len(kept) == 0 {
		return truncateText(value, width)
	}
	result := strings.Join(kept, "  ")
	if len(kept) < len(parts) {
		withEllipsis := result + "  ..."
		if lipgloss.Width(withEllipsis) <= width {
			return withEllipsis
		}
	}
	return result
}

func truncateText(value string, width int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if width <= 0 || len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func renderModalHint(tokens []string, width int) string {
	lines := wrapTokens(tokens, width, "  •  ")
	if len(lines) == 0 {
		return ""
	}
	styled := make([]string, 0, len(lines))
	for _, line := range lines {
		styled = append(styled, modalHint.Render(line))
	}
	return strings.Join(styled, "\n")
}

func wrapTokens(tokens []string, width int, sep string) []string {
	if len(tokens) == 0 {
		return nil
	}
	if width <= 0 {
		width = 80
	}
	lines := make([]string, 0, 2)
	line := ""
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if lipgloss.Width(token) > width {
			token = truncateText(token, width)
		}
		if line == "" {
			line = token
			continue
		}
		candidate := line + sep + token
		if lipgloss.Width(candidate) > width {
			lines = append(lines, line)
			line = token
			continue
		}
		line = candidate
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
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
	rerunHint := "R re-run"
	if run := m.selectedRun(); run != nil && !runIsRerunnable(run.Status) {
		rerunHint = "R re-run (terminal only)"
	}
	hint := renderModalHint([]string{"enter select", "r refresh", rerunHint, "esc close"}, contentWidth)

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
