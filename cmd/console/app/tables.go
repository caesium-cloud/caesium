package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

var (
	jobColumnTitles      = []string{"Alias", "Labels", "Annotations", "ID", "Created"}
	jobColumnWeights     = []int{3, 4, 4, 3, 3}
	triggerColumnTitles  = []string{"Alias", "Type", "ID"}
	triggerColumnWeights = []int{3, 2, 4}
	atomColumnTitles     = []string{"Image", "Engine", "ID"}
	atomColumnWeights    = []int{4, 2, 4}
)

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
			widths[i] = max(remaining, minWidth)
			break
		}

		portion := max(weight*total/sum, minWidth)
		minRemaining := minWidth * (len(weights) - i - 1)
		if remaining-portion < minRemaining {
			portion = max(remaining-minRemaining, minWidth)
		}

		widths[i] = portion
		remaining -= portion
	}

	return widths
}
