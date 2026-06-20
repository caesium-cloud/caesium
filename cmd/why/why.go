// Package why implements `caesium why <run> --task <t>` (data-plane-memory A3):
// the causal explainer for why a task in a run executed, hit the cache, or
// re-ran. It calls the server's
// GET /v1/jobs/:id/runs/:run_id/why?task=<t> endpoint and renders either a
// human-readable summary table (default) or the raw machine-readable JSON
// (--json), so the explanation can be both eyeballed and asserted in a harness.
package why

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

const apiKeyEnvVar = "CAESIUM_API_KEY"

var (
	whyJobID  string
	whyTask   string
	whyServer string
	whyAPIKey string
	whyJSON   bool
)

// explanation mirrors the server's run.WhyExplanation JSON so the CLI can render
// a table. Only the fields the table renders are typed; the rest round-trips via
// --json (which prints the server body verbatim).
type explanation struct {
	RunID    string `json:"runId"`
	JobID    string `json:"jobId"`
	TaskID   string `json:"taskId"`
	TaskName string `json:"taskName"`
	Verdict  string `json:"verdict"`
	Status   string `json:"status"`
	Hash     string `json:"hash"`
	Summary  string `json:"summary"`
	Trigger  struct {
		Type   string            `json:"type"`
		Alias  string            `json:"alias"`
		Params map[string]string `json:"params"`
	} `json:"trigger"`
	Baseline struct {
		Kind  string `json:"kind"`
		RunID string `json:"runId"`
	} `json:"baseline"`
	Diff *struct {
		HashEqual    bool   `json:"hashEqual"`
		SubjectHash  string `json:"subjectHash"`
		BaselineHash string `json:"baselineHash"`
		Degraded     string `json:"degraded"`
		Changes      []struct {
			Field    string `json:"field"`
			Kind     string `json:"kind"`
			Before   string `json:"before"`
			After    string `json:"after"`
			Added    bool   `json:"added"`
			Removed  bool   `json:"removed"`
			Redacted bool   `json:"redacted"`
		} `json:"changes"`
	} `json:"diff"`
}

// Cmd is the `caesium why` command.
var Cmd = &cobra.Command{
	Use:   "why <run-id> --task <task> --job-id <job-id>",
	Short: "Explain why a task ran, hit the cache, or re-ran",
	Long: "Explain why a specific task in a run executed, was served from cache, " +
		"or re-ran — by diffing the task's persisted identity-hash inputs against " +
		"the prior/cached run and naming the discriminating field(s). Prints a " +
		"human-readable summary by default, or machine-readable JSON with --json.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		runID := strings.TrimSpace(args[0])
		if whyJobID == "" {
			return fmt.Errorf("--job-id is required")
		}
		if whyTask == "" {
			return fmt.Errorf("--task is required")
		}

		server := strings.TrimSuffix(whyServer, "/")
		reqURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/why?task=%s",
			server, whyJobID, runID, url.QueryEscape(whyTask))

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
		if apiKey := resolveAPIKey(cmd, whyAPIKey); apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("why failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		// NOTE: write machine-readable output via cmd.OutOrStdout(), NOT
		// cmd.Print/Println — cobra's Print* helpers go to stderr, which would
		// leave `--json` output unusable for piping (e.g. into `caesium verify`).
		stdout := cmd.OutOrStdout()
		if whyJSON {
			// Re-indent for readability; fall back to the raw body if it isn't
			// JSON (it always should be).
			var out interface{}
			if err := json.Unmarshal(body, &out); err != nil {
				_, _ = fmt.Fprint(stdout, string(body))
				return nil
			}
			pretty, _ := json.MarshalIndent(out, "", "  ")
			_, _ = fmt.Fprintln(stdout, string(pretty))
			return nil
		}

		var exp explanation
		if err := json.Unmarshal(body, &exp); err != nil {
			// Unknown shape — just print what we got.
			_, _ = fmt.Fprint(stdout, string(body))
			return nil
		}
		renderTable(cmd, &exp)
		return nil
	},
}

func renderTable(cmd *cobra.Command, exp *explanation) {
	// All rendered output goes to stdout (cobra's cmd.Print* would route to
	// stderr, splitting the report across streams).
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, exp.Summary)
	_, _ = fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "TASK\t%s\n", exp.TaskName)
	_, _ = fmt.Fprintf(tw, "VERDICT\t%s\n", exp.Verdict)
	_, _ = fmt.Fprintf(tw, "STATUS\t%s\n", exp.Status)
	if exp.Hash != "" {
		_, _ = fmt.Fprintf(tw, "HASH\t%s\n", exp.Hash)
	}
	trigger := exp.Trigger.Type
	if exp.Trigger.Alias != "" {
		trigger = fmt.Sprintf("%s (%s)", exp.Trigger.Type, exp.Trigger.Alias)
	}
	if trigger != "" {
		_, _ = fmt.Fprintf(tw, "TRIGGER\t%s\n", trigger)
	}
	if exp.Baseline.Kind != "" {
		baseline := exp.Baseline.Kind
		if exp.Baseline.RunID != "" {
			baseline = fmt.Sprintf("%s (run %s)", exp.Baseline.Kind, exp.Baseline.RunID)
		}
		_, _ = fmt.Fprintf(tw, "COMPARED-TO\t%s\n", baseline)
	}
	_ = tw.Flush()

	if exp.Diff == nil {
		return
	}
	if exp.Diff.Degraded != "" {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "note: %s\n", exp.Diff.Degraded)
		return
	}
	if len(exp.Diff.Changes) == 0 {
		_, _ = fmt.Fprintln(out)
		if exp.Diff.HashEqual {
			_, _ = fmt.Fprintln(out, "All hashed inputs are identical (no discriminating field).")
		} else {
			_, _ = fmt.Fprintln(out, "No discriminating input field found.")
		}
		return
	}

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Discriminating fields (%d):\n", len(exp.Diff.Changes))
	dw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(dw, "FIELD\tCHANGE\tBEFORE\tAFTER")
	for _, ch := range exp.Diff.Changes {
		before, after := ch.Before, ch.After
		if ch.Redacted {
			if before != "" {
				before += " (redacted)"
			}
			if after != "" {
				after += " (redacted)"
			}
		}
		change := "changed"
		switch {
		case ch.Added:
			change = "added"
		case ch.Removed:
			change = "removed"
		case ch.Kind == "structural":
			change = "changed (structural)"
		}
		_, _ = fmt.Fprintf(dw, "%s\t%s\t%s\t%s\n", ch.Field, change, dashIfEmpty(before), dashIfEmpty(after))
	}
	_ = dw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func resolveAPIKey(cmd *cobra.Command, flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		cmd.PrintErrln(fmt.Sprintf("warning: --api-key is visible in process listings; prefer %s", apiKeyEnvVar))
		return strings.TrimSpace(flagValue)
	}
	return strings.TrimSpace(os.Getenv(apiKeyEnvVar))
}

func init() {
	Cmd.Flags().StringVar(&whyJobID, "job-id", "", "Job ID that owns the run (required)")
	Cmd.Flags().StringVar(&whyTask, "task", "", "Task name or ID to explain (required)")
	Cmd.Flags().StringVar(&whyServer, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.Flags().StringVar(&whyAPIKey, "api-key", "", "API key for authentication (prefer "+apiKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.Flags().BoolVar(&whyJSON, "json", false, "Emit machine-readable JSON instead of a table")
}
