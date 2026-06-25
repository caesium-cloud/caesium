package run

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const runDiffAPIKeyEnvVar = "CAESIUM_API_KEY"

var (
	diffJobID  string
	diffServer string
	diffAPIKey string
	diffJSON   bool
)

type runDiffResponse struct {
	JobID      string `json:"jobId"`
	LeftRunID  string `json:"leftRunId"`
	RightRunID string `json:"rightRunId"`

	LeftStatus  string `json:"leftStatus"`
	RightStatus string `json:"rightStatus"`

	LeftTrigger  runDiffTrigger `json:"leftTrigger"`
	RightTrigger runDiffTrigger `json:"rightTrigger"`

	TriggerChanges []runDiffFieldChange `json:"triggerChanges,omitempty"`
	ParamChanges   []runDiffFieldChange `json:"paramChanges,omitempty"`
	Tasks          []runDiffTask        `json:"tasks"`
	TasksAdded     []string             `json:"tasksAdded,omitempty"`
	TasksRemoved   []string             `json:"tasksRemoved,omitempty"`
}

type runDiffTrigger struct {
	Type   string            `json:"type"`
	Alias  string            `json:"alias"`
	Params map[string]string `json:"params"`
}

type runDiffTask struct {
	TaskName string `json:"taskName"`

	LeftStatus   string `json:"leftStatus"`
	RightStatus  string `json:"rightStatus"`
	LeftAttempt  int    `json:"leftAttempt"`
	RightAttempt int    `json:"rightAttempt"`
	LeftHash     string `json:"leftHash,omitempty"`
	RightHash    string `json:"rightHash,omitempty"`

	Verdict   string               `json:"verdict"`
	HashEqual bool                 `json:"hashEqual"`
	Changes   []runDiffFieldChange `json:"changes,omitempty"`
	Degraded  string               `json:"degraded,omitempty"`
}

type runDiffFieldChange struct {
	Field    string `json:"field"`
	Kind     string `json:"kind"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Added    bool   `json:"added"`
	Removed  bool   `json:"removed"`
	Redacted bool   `json:"redacted"`
}

var diffCmd = &cobra.Command{
	Use:   "diff <left-run> <right-run> --job-id <job-id>",
	Short: "Diff two runs of the same job",
	Long: "Diff two runs of the same job by asking the job-scoped run-diff REST " +
		"endpoint for cache-bust attribution. Prints a per-task table by default, " +
		"or machine-readable JSON with --json.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		leftRunID := strings.TrimSpace(args[0])
		rightRunID := strings.TrimSpace(args[1])
		if diffJobID == "" {
			return fmt.Errorf("--job-id is required")
		}

		server := strings.TrimSuffix(diffServer, "/")
		params := url.Values{}
		params.Set("left", leftRunID)
		params.Set("right", rightRunID)
		reqURL := fmt.Sprintf("%s/v1/jobs/%s/runs/diff?%s", server, url.PathEscape(diffJobID), params.Encode())

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
		if apiKey := resolveRunDiffAPIKey(cmd, diffAPIKey); apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading run diff response: %w", err)
		}
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("run diff failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		stdout := cmd.OutOrStdout()
		if diffJSON {
			var out interface{}
			if err := json.Unmarshal(body, &out); err != nil {
				return fmt.Errorf("run diff response was not valid JSON (status %d): %w", resp.StatusCode, err)
			}
			pretty, _ := json.MarshalIndent(out, "", "  ")
			_, _ = fmt.Fprintln(stdout, string(pretty))
			return nil
		}

		var diff runDiffResponse
		if err := json.Unmarshal(body, &diff); err != nil {
			return fmt.Errorf("run diff response was not valid JSON (status %d): %w", resp.StatusCode, err)
		}
		renderRunDiffTable(cmd, &diff)
		return nil
	},
}

func renderRunDiffTable(cmd *cobra.Command, diff *runDiffResponse) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Run diff for job %s\n\n", diff.JobID)

	summary := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(summary, "LEFT RUN\t%s\t%s\n", diff.LeftRunID, diff.LeftStatus)
	_, _ = fmt.Fprintf(summary, "RIGHT RUN\t%s\t%s\n", diff.RightRunID, diff.RightStatus)
	if trigger := formatRunDiffTrigger(diff.LeftTrigger); trigger != "" {
		_, _ = fmt.Fprintf(summary, "LEFT TRIGGER\t%s\n", trigger)
	}
	if trigger := formatRunDiffTrigger(diff.RightTrigger); trigger != "" {
		_, _ = fmt.Fprintf(summary, "RIGHT TRIGGER\t%s\n", trigger)
	}
	_ = summary.Flush()

	renderRunDiffChanges(out, "Trigger changes", diff.TriggerChanges)
	renderRunDiffChanges(out, "Run parameter changes", diff.ParamChanges)

	if len(diff.TasksAdded) > 0 {
		_, _ = fmt.Fprintf(out, "\nTasks added: %s\n", strings.Join(diff.TasksAdded, ", "))
	}
	if len(diff.TasksRemoved) > 0 {
		_, _ = fmt.Fprintf(out, "\nTasks removed: %s\n", strings.Join(diff.TasksRemoved, ", "))
	}

	_, _ = fmt.Fprintln(out, "\nTasks:")
	tasks := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tasks, "TASK\tVERDICT\tLEFT\tRIGHT\tCHANGES")
	for _, task := range diff.Tasks {
		_, _ = fmt.Fprintf(tasks, "%s\t%s\t%s\t%s\t%d\n",
			task.TaskName,
			task.Verdict,
			formatRunDiffTaskStatus(task.LeftStatus, task.LeftAttempt),
			formatRunDiffTaskStatus(task.RightStatus, task.RightAttempt),
			len(task.Changes),
		)
	}
	_ = tasks.Flush()

	for _, task := range diff.Tasks {
		if task.Degraded != "" {
			_, _ = fmt.Fprintf(out, "\n%s degraded: %s\n", task.TaskName, task.Degraded)
			continue
		}
		renderRunDiffChanges(out, task.TaskName+" changes", task.Changes)
	}
}

func renderRunDiffChanges(out io.Writer, title string, changes []runDiffFieldChange) {
	if len(changes) == 0 {
		return
	}

	_, _ = fmt.Fprintf(out, "\n%s (%d):\n", title, len(changes))
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FIELD\tCHANGE\tBEFORE\tAFTER")
	for _, ch := range changes {
		before, after := ch.Before, ch.After
		if ch.Redacted {
			if before != "" {
				before += " (redacted)"
			}
			if after != "" {
				after += " (redacted)"
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			ch.Field,
			runDiffChangeLabel(ch),
			dashRunDiffEmpty(before),
			dashRunDiffEmpty(after),
		)
	}
	_ = tw.Flush()
}

func runDiffChangeLabel(ch runDiffFieldChange) string {
	switch {
	case ch.Added:
		return "added"
	case ch.Removed:
		return "removed"
	case ch.Kind == "structural":
		return "changed (structural)"
	default:
		return "changed"
	}
}

func formatRunDiffTrigger(trigger runDiffTrigger) string {
	if trigger.Type == "" {
		return ""
	}
	if trigger.Alias != "" {
		return fmt.Sprintf("%s (%s)", trigger.Type, trigger.Alias)
	}
	return trigger.Type
}

func formatRunDiffTaskStatus(status string, attempt int) string {
	if attempt <= 0 {
		return status
	}
	return fmt.Sprintf("%s (attempt %d)", status, attempt)
}

func dashRunDiffEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func resolveRunDiffAPIKey(cmd *cobra.Command, flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --api-key is visible in process listings; prefer %s\n", runDiffAPIKeyEnvVar)
		return strings.TrimSpace(flagValue)
	}
	return strings.TrimSpace(os.Getenv(runDiffAPIKeyEnvVar))
}

func init() {
	diffCmd.Flags().StringVar(&diffJobID, "job-id", "", "Job ID that owns both runs (required)")
	diffCmd.Flags().StringVar(&diffServer, "server", "http://localhost:8080", "Caesium server base URL")
	diffCmd.Flags().StringVar(&diffAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	diffCmd.Flags().BoolVar(&diffJSON, "json", false, "Emit machine-readable JSON instead of a table")

	Cmd.AddCommand(diffCmd)
}
