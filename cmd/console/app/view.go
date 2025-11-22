package app

import (
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/caesium-cloud/caesium/cmd/console/ui/detail"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

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
)

// View renders the interface.
func (m Model) View() string {
	tabs := renderTabsBar(m.active, m.viewportWidth)

	footerKeys := "[1/2/3] switch  [tab] cycle  [r] reload  [q] quit"
	if m.active == sectionJobs {
		if m.showDetail {
			footerKeys = "[esc/q] back  [←/→] traverse  [t] trigger"
		} else {
			footerKeys += "  [enter] detail  [t] trigger"
		}
	}
	footer := barStyle.Render(footerKeys)
	if status := strings.TrimSpace(m.actionStatusText()); status != "" {
		footer = lipgloss.JoinHorizontal(lipgloss.Top, footer, barStyle.Render(status))
	}

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
			m.jobs.SetWidth(max(m.viewportWidth-8, 20))
			body = renderPane(m.triggers, true)
		case sectionAtoms:
			m.jobs.SetWidth(max(m.viewportWidth-8, 20))
			body = renderPane(m.atoms, true)
		default:
			activeTable := m.tableFor(m.active)
			body = renderPane(activeTable, true)
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, tabs, body, footer)
}

func (m Model) renderJobsView() string {
	width := max(m.viewportWidth-8, 20)
	m.jobs.SetWidth(width)
	return renderPane(m.jobs, true)
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
		ViewportWidth: max(totalWidth-4, 20),
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

func renderPane(tbl table.Model, active bool) string {
	content := tbl.View()
	style := boxStyle
	if active {
		style = activeBox
	}

	return style.Render(content)
}

func (m Model) actionStatusText() string {
	if m.actionErr != nil {
		return fmt.Sprintf("Trigger failed: %s", m.actionErr.Error())
	}
	return m.actionNotice
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
