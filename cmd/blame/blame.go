// Package blame implements `caesium blame <job>`: a read-side attribution view
// over dag_snapshot history.
package blame

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const (
	apiKeyEnvVar = "CAESIUM_API_KEY"
	coverageNote = "Coverage: topology + image + command only. env/spec/retries/cache/schema/sla/triggerRules changes are not tracked."
)

var (
	blameTask   string
	blameFrom   string
	blameTo     string
	blameServer string
	blameAPIKey string
	blameJSON   bool
)

type result struct {
	JobID      string            `json:"job_id"`
	Coverage   string            `json:"coverage"`
	FromCommit string            `json:"from_commit,omitempty"`
	ToCommit   string            `json:"to_commit,omitempty"`
	Tasks      []taskAttribution `json:"tasks"`
	Edges      []edgeAttribution `json:"edges"`
}

type taskAttribution struct {
	Element           taskElement `json:"element"`
	IntroducingCommit string      `json:"introducing_commit"`
	SnapshotID        string      `json:"snapshot_id"`
}

type taskElement struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

type edgeAttribution struct {
	Element           edgeElement `json:"element"`
	IntroducingCommit string      `json:"introducing_commit"`
	SnapshotID        string      `json:"snapshot_id"`
	ProvenanceCommit  string      `json:"provenance_commit,omitempty"`
}

type edgeElement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type jobSummary struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

// Cmd is the `caesium blame` command.
var Cmd = &cobra.Command{
	Use:   "blame <job-id-or-alias>",
	Short: "Attribute DAG tasks and edges to their introducing snapshot",
	Long: "Attribute each persisted DAG task and edge to the commit/snapshot that introduced its current form. " +
		"By default prints a per-element table; with --json prints the server's machine-readable JSON result. " +
		coverageNote,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(blameServer, "/")
		jobID, err := resolveJobID(cmd, server, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		params := url.Values{}
		if blameTask != "" {
			params.Set("task", blameTask)
		}
		if blameFrom != "" {
			params.Set("from", blameFrom)
		}
		if blameTo != "" {
			params.Set("to", blameTo)
		}

		reqURL := fmt.Sprintf("%s/v1/jobs/%s/blame", server, url.PathEscape(jobID))
		if encoded := params.Encode(); encoded != "" {
			reqURL += "?" + encoded
		}
		body, err := get(cmd, reqURL, "blame")
		if err != nil {
			return err
		}

		stdout := cmd.OutOrStdout()
		if blameJSON {
			var out any
			if err := json.Unmarshal(body, &out); err != nil {
				_, writeErr := stdout.Write(body)
				return writeErr
			}
			pretty, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				_, writeErr := stdout.Write(body)
				return writeErr
			}
			_, _ = stdout.Write(pretty)
			_, _ = fmt.Fprintln(stdout)
			return nil
		}

		var res result
		if err := json.Unmarshal(body, &res); err != nil {
			_, writeErr := stdout.Write(body)
			return writeErr
		}
		renderTable(cmd, &res)
		return nil
	},
}

func resolveJobID(cmd *cobra.Command, server, idOrAlias string) (string, error) {
	if idOrAlias == "" {
		return "", fmt.Errorf("job id or alias is required")
	}
	if _, err := uuid.Parse(idOrAlias); err == nil {
		return idOrAlias, nil
	}

	body, err := get(cmd, server+"/v1/jobs", "job lookup")
	if err != nil {
		return "", err
	}
	var jobs []jobSummary
	if err := json.Unmarshal(body, &jobs); err != nil {
		return "", fmt.Errorf("job lookup returned invalid JSON: %w", err)
	}
	for _, job := range jobs {
		if job.Alias == idOrAlias {
			return job.ID, nil
		}
	}
	return "", fmt.Errorf("job alias %q not found", idOrAlias)
}

func get(cmd *cobra.Command, reqURL, label string) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveAPIKey(cmd, blameAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s failed (%d): %s", label, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func renderTable(cmd *cobra.Command, res *result) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, coverageNote)
	if res.Coverage != "" {
		_, _ = fmt.Fprintf(out, "coverage: %s\n", res.Coverage)
	}
	if res.FromCommit != "" || res.ToCommit != "" {
		_, _ = fmt.Fprintf(out, "range: %s..%s\n", dashIfEmpty(res.FromCommit), dashIfEmpty(res.ToCommit))
	}
	_, _ = fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TYPE\tELEMENT\tIMAGE/COMMAND\tINTRODUCING COMMIT\tSNAPSHOT\tPROVENANCE")
	for _, task := range res.Tasks {
		_, _ = fmt.Fprintf(tw, "task\t%s\t%s %s\t%s\t%s\t-\n",
			task.Element.Name,
			dashIfEmpty(task.Element.Image),
			commandString(task.Element.Command),
			dashIfEmpty(task.IntroducingCommit),
			task.SnapshotID,
		)
	}
	for _, edge := range res.Edges {
		_, _ = fmt.Fprintf(tw, "edge\t%s -> %s\t-\t%s\t%s\t%s\n",
			edge.Element.From,
			edge.Element.To,
			dashIfEmpty(edge.IntroducingCommit),
			edge.SnapshotID,
			dashIfEmpty(edge.ProvenanceCommit),
		)
	}
	_ = tw.Flush()
}

func commandString(command []string) string {
	if len(command) == 0 {
		return "-"
	}
	return strings.Join(command, " ")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func resolveAPIKey(cmd *cobra.Command, explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		cmd.PrintErrln(fmt.Sprintf("warning: --api-key is visible in process listings; prefer %s", apiKeyEnvVar))
		return v
	}
	return strings.TrimSpace(os.Getenv(apiKeyEnvVar))
}

func init() {
	Cmd.Flags().StringVar(&blameTask, "task", "", "Limit blame to a task and adjacent edges")
	Cmd.Flags().StringVar(&blameFrom, "from", "", "Start commit for attribution range")
	Cmd.Flags().StringVar(&blameTo, "to", "", "End commit for attribution range")
	Cmd.Flags().BoolVar(&blameJSON, "json", false, "Print machine-readable JSON")
	Cmd.Flags().StringVar(&blameServer, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.Flags().StringVar(&blameAPIKey, "api-key", "", "API key bearer token (default: CAESIUM_API_KEY)")
}
