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
	jobColumnTitles      = []string{"Alias", "Status", "Last Run", "Duration", "Labels", "ID"}
	jobColumnWeights     = []int{3, 2, 3, 2, 3, 2}
	triggerColumnTitles  = []string{"Alias", "Type", "ID"}
	triggerColumnWeights = []int{3, 2, 4}
	atomColumnTitles     = []string{"Image", "Engine", "ID"}
	atomColumnWeights    = []int{4, 2, 4}
	runColumnTitles      = []string{"Run", "Status", "Started", "Completed"}
	runColumnWeights     = []int{3, 2, 4, 4}
)

func jobsToRows(jobs []api.Job, statuses map[string]*api.Run, spinnerFrame string) []table.Row {
	rows := make([]table.Row, len(jobs))
	for i, job := range jobs {
		run := statuses[job.ID]
		status := formatRunStatus(run, spinnerFrame)
		lastRun := "-"
		duration := "-"
		if run != nil {
			lastRun = relativeTime(run.StartedAt)
			duration = formatRunDuration(run)
		}
		rows[i] = table.Row{
			job.Alias,
			status,
			lastRun,
			duration,
			formatStringMap(job.Labels),
			shortID(job.ID),
		}
	}
	return rows
}

func formatRunStatus(run *api.Run, spinnerFrame string) string {
	if run == nil {
		return "-"
	}

	status := strings.ToLower(strings.TrimSpace(run.Status))
	switch status {
	case "running":
		if spinnerFrame != "" {
			return fmt.Sprintf("%s Running", spinnerFrame)
		}
		return "Running"
	case "pending":
		if spinnerFrame != "" {
			return fmt.Sprintf("%s Pending", spinnerFrame)
		}
		return "Pending"
	case "succeeded":
		return "✅ Succeeded"
	case "failed":
		return "❌ Failed"
	default:
		return titleCase(run.Status)
	}
}

func titleCase(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) == 1 {
		return strings.ToUpper(value)
	}
	return strings.ToUpper(value[:1]) + value[1:]
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

func runsToRows(runs []api.Run, spinnerFrame string) []table.Row {
	rows := make([]table.Row, len(runs))
	for i, run := range runs {
		completed := "-"
		if run.CompletedAt != nil {
			completed = run.CompletedAt.Format(time.RFC3339)
		}
		rows[i] = table.Row{
			run.ID,
			formatRunStatus(&run, spinnerFrame),
			run.StartedAt.Format(time.RFC3339),
			completed,
		}
	}
	return rows
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

	// Reserve one character gap between columns so assigned widths align
	// with table inner content width instead of overflowing by gap count.
	contentTotal := total - (len(weights) - 1)
	if contentTotal < len(weights)*10 {
		contentTotal = len(weights) * 10
	}

	sum := 0
	for _, w := range weights {
		sum += w
	}

	minWidth := 10
	widths := make([]int, len(weights))
	remaining := contentTotal

	for i, weight := range weights {
		if i == len(weights)-1 {
			widths[i] = max(remaining, minWidth)
			break
		}

		portion := max(weight*contentTotal/sum, minWidth)
		minRemaining := minWidth * (len(weights) - i - 1)
		if remaining-portion < minRemaining {
			portion = max(remaining-minRemaining, minWidth)
		}

		widths[i] = portion
		remaining -= portion
	}

	return widths
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func formatRunDuration(run *api.Run) string {
	if run == nil {
		return "-"
	}
	if run.CompletedAt != nil {
		return formatDuration(run.CompletedAt.Sub(run.StartedAt).Seconds())
	}
	if strings.ToLower(strings.TrimSpace(run.Status)) == "running" {
		return formatDuration(time.Since(run.StartedAt).Seconds())
	}
	return "-"
}
