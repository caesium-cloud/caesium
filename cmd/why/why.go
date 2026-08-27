// Package why implements `caesium why <run> --task <t>` (data-plane-memory A3):
// the causal explainer for why a task in a run executed, hit the cache, or
// re-ran. It calls the server's
// GET /v1/jobs/:id/runs/:run_id/why?task=<t> endpoint and renders either a
// human-readable summary table (default) or the raw machine-readable JSON
// (--json), so the explanation can be both eyeballed and asserted in a harness.
//
// On a FANNED step (dynamic fan-out) `--task` alone names N task instances, not
// one, so the server answers with a group summary — partition count, status
// histogram, the first failed partition's cause, and the aggregate timing — and
// `--partition <value>` selects a single instance for the full per-instance
// explanation.
package why

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const apiKeyEnvVar = "CAESIUM_API_KEY"

var (
	whyJobID     string
	whyTask      string
	whyPartition string
	whyServer    string
	whyAPIKey    string
	whyJSON      bool
)

// explanation mirrors the server's run.WhyExplanation JSON so the CLI can render
// a table. Only the fields the table renders are typed; the rest round-trips via
// --json (which prints the server body verbatim).
type explanation struct {
	RunID     string `json:"runId"`
	JobID     string `json:"jobId"`
	TaskID    string `json:"taskId"`
	TaskName  string `json:"taskName"`
	TaskRunID string `json:"taskRunId"`
	Partition string `json:"partition"`
	Verdict   string `json:"verdict"`
	Status    string `json:"status"`
	Hash      string `json:"hash"`
	Summary   string `json:"summary"`
	Trigger   struct {
		Type   string            `json:"type"`
		Alias  string            `json:"alias"`
		Params map[string]string `json:"params"`
	} `json:"trigger"`
	Baseline struct {
		Kind  string `json:"kind"`
		RunID string `json:"runId"`
	} `json:"baseline"`
	Group *groupSummary `json:"group"`
	Diff  *struct {
		HashEqual    bool   `json:"hashEqual"`
		SubjectHash  string `json:"subjectHash"`
		BaselineHash string `json:"baselineHash"`
		Degraded     string `json:"degraded"`
		// Notes are qualifiers about how the key was computed rather than which
		// input differed — today, the `chain: values` predecessor-hash exclusion,
		// which the table must render or a values-mode skip is unexplainable.
		Notes   []string `json:"notes"`
		Changes []struct {
			Field    string `json:"field"`
			Kind     string `json:"kind"`
			Before   string `json:"before"`
			After    string `json:"after"`
			Added    bool   `json:"added"`
			Removed  bool   `json:"removed"`
			Redacted bool   `json:"redacted"`
			Note     string `json:"note"`
		} `json:"changes"`
	} `json:"diff"`
}

// groupSummary mirrors run.WhyGroup: the aggregate answer the server returns for
// a fanned step addressed without a --partition selector.
type groupSummary struct {
	PartitionCount      int            `json:"partitionCount"`
	StatusCounts        map[string]int `json:"statusCounts"`
	CacheHits           int            `json:"cacheHits"`
	Partitions          []string       `json:"partitions"`
	PartitionsTruncated bool           `json:"partitionsTruncated"`
	FirstFailure        *groupFailure  `json:"firstFailure"`
	DurationMS          int64          `json:"durationMs"`
	// Notes mirrors run.WhyGroup.Notes: the group-level channel for
	// key-construction qualifiers, which a group needs because it carries no
	// diff to hang them on.
	Notes []string `json:"notes"`
}

// explanationNotes collects the key-construction notes from whichever channel
// this answer shape uses — a single instance carries them on its diff, a fanned
// group on the group summary — de-duplicated so a future shape carrying both
// does not print the same line twice. It mirrors run.explanationNotes.
func explanationNotes(exp *explanation) []string {
	var notes []string
	seen := make(map[string]struct{}, 2)
	add := func(candidates []string) {
		for _, n := range candidates {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			notes = append(notes, n)
		}
	}
	if exp.Diff != nil {
		add(exp.Diff.Notes)
	}
	if exp.Group != nil {
		add(exp.Group.Notes)
	}
	return notes
}

// groupFailure mirrors run.WhyGroupFailure.
type groupFailure struct {
	Partition      string `json:"partition"`
	PartitionIndex int    `json:"partitionIndex"`
	TaskRunID      string `json:"taskRunId"`
	Status         string `json:"status"`
	Attempt        int    `json:"attempt"`
	Error          string `json:"error"`
}

// Cmd is the `caesium why` command.
var Cmd = &cobra.Command{
	Use:   "why <run-id> --task <task> --job-id <job-id> [--partition <value>]",
	Short: "Explain why a task ran, hit the cache, or re-ran",
	Long: "Explain why a specific task in a run executed, was served from cache, " +
		"or re-ran — by diffing the task's persisted identity-hash inputs against " +
		"the prior/cached run and naming the discriminating field(s). Prints a " +
		"human-readable summary by default, or machine-readable JSON with --json. " +
		"A fanned step answers with the group summary (partition count, status " +
		"histogram, first failure); pass --partition <value> for one instance.",
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
		query := url.Values{}
		query.Set("task", whyTask)
		if partition := strings.TrimSpace(whyPartition); partition != "" {
			query.Set("partition", partition)
		}
		reqURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/why?%s",
			server, whyJobID, runID, query.Encode())

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
				_, _ = stdout.Write(body)
				return nil
			}
			pretty, _ := json.MarshalIndent(out, "", "  ")
			_, _ = stdout.Write(pretty)
			_, _ = fmt.Fprintln(stdout)
			return nil
		}

		var exp explanation
		if err := json.Unmarshal(body, &exp); err != nil {
			// Unknown shape — just print what we got.
			_, _ = stdout.Write(body)
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
	if exp.Partition != "" {
		_, _ = fmt.Fprintf(tw, "PARTITION\t%s\n", exp.Partition)
	}
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

	// Notes are printed on EVERY path — degraded blobs and fanned groups
	// included, and BEFORE either early return below. The chain: values exclusion
	// is the reason a task can stay cached while its predecessor visibly changed,
	// so hiding it behind a `return` would leave exactly the skips spec §4.3 says
	// must be explainable unexplained. A fanned group carries no Diff (N hashes,
	// N baselines), so its note rides on group.notes.
	for _, note := range explanationNotes(exp) {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "note: %s\n", note)
	}

	if exp.Group != nil {
		renderGroup(out, exp.Group)
		return
	}

	if exp.Diff == nil {
		return
	}

	if exp.Diff.Degraded != "" {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "note: %s\n", exp.Diff.Degraded)
		return
	}

	// An excluded entry records an input that was deliberately kept OUT of the
	// key; it is rendered above as a note, and counting it as a discriminating
	// field would report a cache hit as having a changed field.
	changes := exp.Diff.Changes[:0:0]
	for _, ch := range exp.Diff.Changes {
		if ch.Kind == "excluded" {
			continue
		}
		changes = append(changes, ch)
	}

	if len(changes) == 0 {
		_, _ = fmt.Fprintln(out)
		if exp.Diff.HashEqual {
			_, _ = fmt.Fprintln(out, "All hashed inputs are identical (no discriminating field).")
		} else {
			_, _ = fmt.Fprintln(out, "No discriminating input field found.")
		}
		return
	}

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Discriminating fields (%d):\n", len(changes))
	dw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(dw, "FIELD\tCHANGE\tBEFORE\tAFTER")
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

// renderGroup prints the fanned-step aggregate: the status histogram, the
// cache-hit count, the first failure, and the selector hint. A group has no
// single identity hash or baseline, so no field diff is printed — that is what
// --partition is for.
func renderGroup(out io.Writer, group *groupSummary) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Fan-out group (%d partitions):\n", group.PartitionCount)

	gw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	statuses := make([]string, 0, len(group.StatusCounts))
	for status := range group.StatusCounts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		_, _ = fmt.Fprintf(gw, "%s\t%d\n", strings.ToUpper(status), group.StatusCounts[status])
	}
	if group.CacheHits > 0 {
		_, _ = fmt.Fprintf(gw, "CACHE-HITS\t%d\n", group.CacheHits)
	}
	if group.DurationMS > 0 {
		_, _ = fmt.Fprintf(gw, "DURATION\t%dms\n", group.DurationMS)
	}
	if len(group.Partitions) > 0 {
		list := strings.Join(group.Partitions, ", ")
		if group.PartitionsTruncated {
			list += " …"
		}
		_, _ = fmt.Fprintf(gw, "PARTITIONS\t%s\n", list)
	}
	_ = gw.Flush()

	if group.FirstFailure != nil {
		_, _ = fmt.Fprintln(out)
		cause := group.FirstFailure.Error
		if cause == "" {
			cause = group.FirstFailure.Status
		}
		_, _ = fmt.Fprintf(out, "First failure: partition %q (index %d, attempt %d): %s\n",
			group.FirstFailure.Partition, group.FirstFailure.PartitionIndex, group.FirstFailure.Attempt, cause)
	}

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Select one instance for the field-level explanation: --partition <value>")
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
	Cmd.Flags().StringVar(&whyPartition, "partition", "", "Explain ONE fan-out instance of --task by its partition value (default: the group summary)")
	Cmd.Flags().StringVar(&whyServer, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.Flags().StringVar(&whyAPIKey, "api-key", "", "API key for authentication (prefer "+apiKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.Flags().BoolVar(&whyJSON, "json", false, "Emit machine-readable JSON instead of a table")
}
