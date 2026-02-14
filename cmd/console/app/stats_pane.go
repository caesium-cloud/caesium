package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/charmbracelet/lipgloss"
)

func renderStatsView(stats *api.StatsResponse, width int) string {
	if stats == nil {
		return placeholder.Render("No statistics available. Press r to reload.")
	}

	contentWidth := max(width-8, 40)
	sections := make([]string, 0, 4)

	// Overview section
	overview := renderStatsOverview(stats, contentWidth)
	sections = append(sections, overview)

	// Top failing jobs
	if len(stats.TopFailing) > 0 {
		sections = append(sections, renderTopFailing(stats.TopFailing, contentWidth))
	}

	// Slowest jobs
	if len(stats.SlowestJobs) > 0 {
		sections = append(sections, renderSlowestJobs(stats.SlowestJobs, contentWidth))
	}

	body := strings.Join(sections, "\n\n")
	border := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(1, 2).
		Width(max(width-2, 40))

	return border.Render(body)
}

func renderStatsOverview(stats *api.StatsResponse, width int) string {
	title := modalTitle.Render("Overview")

	successPct := fmt.Sprintf("%.0f%%", stats.Jobs.SuccessRate*100)
	avgDur := formatDuration(stats.Jobs.AvgDurationSeconds)

	bar := renderProgressBar(stats.Jobs.SuccessRate, max(width/2, 20))

	lines := []string{
		title,
		"",
		fmt.Sprintf("  Total Jobs:        %d", stats.Jobs.Total),
		fmt.Sprintf("  Recent Runs (24h): %d", stats.Jobs.RecentRuns),
		fmt.Sprintf("  Success Rate:      %s  %s", successPct, bar),
		fmt.Sprintf("  Avg Duration:      %s", avgDur),
	}

	return strings.Join(lines, "\n")
}

func renderTopFailing(jobs []api.FailingJob, width int) string {
	title := modalTitle.Render("Top Failing Jobs")

	lines := []string{title, ""}
	header := fmt.Sprintf("  %-24s %-8s %s", "Job", "Fails", "Last Failure")
	lines = append(lines, modalHint.Render(header))

	for _, job := range jobs {
		name := job.Alias
		if name == "" {
			name = shortID(job.JobID)
		}
		if len(name) > 24 {
			name = name[:21] + "..."
		}
		lastFail := "-"
		if job.LastFailure != nil {
			lastFail = job.LastFailure.Format(time.RFC3339)
		}
		lines = append(lines, fmt.Sprintf("  %-24s %-8d %s", name, job.FailureCount, lastFail))
	}

	return strings.Join(lines, "\n")
}

func renderSlowestJobs(jobs []api.SlowestJob, width int) string {
	title := modalTitle.Render("Slowest Jobs")

	lines := []string{title, ""}
	header := fmt.Sprintf("  %-24s %s", "Job", "Avg Duration")
	lines = append(lines, modalHint.Render(header))

	for _, job := range jobs {
		name := job.Alias
		if name == "" {
			name = shortID(job.JobID)
		}
		if len(name) > 24 {
			name = name[:21] + "..."
		}
		lines = append(lines, fmt.Sprintf("  %-24s %s", name, formatDuration(job.AvgDurationSeconds)))
	}

	return strings.Join(lines, "\n")
}

func renderProgressBar(ratio float64, width int) string {
	if width < 4 {
		width = 4
	}
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	empty := width - filled

	bar := strings.Repeat("█", filled) + strings.Repeat("░", empty)
	return bar
}

func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds * float64(time.Second))
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}
