package localrun

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// TaskProgress represents the status of a single task during a local run.
type TaskProgress struct {
	Name     string
	Status   string // pending, running, succeeded, failed, skipped
	Duration time.Duration
	Error    string
}

// Display renders task progress to a terminal.
type Display struct {
	Writer io.Writer
}

// RenderProgress prints a simple task status table.
func (d *Display) RenderProgress(tasks []TaskProgress) {
	maxName := 0
	for _, t := range tasks {
		if len(t.Name) > maxName {
			maxName = len(t.Name)
		}
	}

	for _, t := range tasks {
		icon := statusIcon(t.Status)
		dur := ""
		if t.Duration > 0 {
			dur = fmt.Sprintf("  (%s)", t.Duration.Truncate(time.Millisecond))
		}
		errMsg := ""
		if t.Error != "" {
			errMsg = fmt.Sprintf("  error: %s", t.Error)
		}
		_, _ = fmt.Fprintf(d.Writer, "  %s  %-*s  %s%s%s\n", icon, maxName, t.Name, t.Status, dur, errMsg)
	}
}

// RenderHeader prints a header line for a dev run.
func (d *Display) RenderHeader(alias string, path string) {
	_, _ = fmt.Fprintf(d.Writer, "caesium dev %s  %s\n", alias, path)
	_, _ = fmt.Fprintln(d.Writer, strings.Repeat("-", 60))
}

func statusIcon(status string) string {
	switch status {
	case "succeeded":
		return "OK"
	case "failed":
		return "FAIL"
	case "running":
		return "...."
	case "skipped":
		return "SKIP"
	default:
		return "    "
	}
}
