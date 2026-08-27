package run

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	partitionsJobID  string
	partitionsTask   string
	partitionsStatus string
	partitionsJSON   bool
	partitionsServer string
	partitionsAPIKey string

	partitionsHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

// partitionRow mirrors the server's partitionRow JSON
// (api/rest/controller/job/run/partitions.go) so the CLI can render a table.
// --json prints the server response body verbatim (re-indented), which is the
// only place `fingerprint`/`depends_on` need to round-trip losslessly.
type partitionRow struct {
	Value       string   `json:"value"`
	Index       int      `json:"index"`
	Status      string   `json:"status"`
	Attempt     int      `json:"attempt"`
	CacheHit    bool     `json:"cache_hit"`
	Duration    string   `json:"duration,omitempty"`
	Error       string   `json:"error,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

var partitionsCmd = &cobra.Command{
	Use:   "partitions <run-id> --job-id <job-id> --task <name>",
	Short: "List fan-out partition instances for a task in a run",
	Long: "List the materialized TaskRun instances of a fanned step. Prints a " +
		"human-readable table by default (value, index, status, attempt, " +
		"cache-hit, duration, error, plus fingerprint/depends_on columns when " +
		"any instance carries them), or the server's machine-readable JSON " +
		"verbatim with --json.",
	Args: cobra.ExactArgs(1),
	RunE: runPartitions,
}

func runPartitions(cmd *cobra.Command, args []string) error {
	if partitionsJobID == "" || partitionsTask == "" {
		return fmt.Errorf("--job-id and --task are required")
	}
	runID := strings.TrimSpace(args[0])

	body, err := fetchPartitions(cmd, partitionsJobID, runID, partitionsTask, partitionsStatus)
	if err != nil {
		return err
	}

	if partitionsJSON {
		return cliutil.WritePrettyJSON(cmd, body, "partitions response")
	}

	var parsed struct {
		Partitions []partitionRow `json:"partitions"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("partitions response was not valid JSON: %w", err)
	}
	renderPartitionsTable(cmd, parsed.Partitions)
	return nil
}

func fetchPartitions(cmd *cobra.Command, jobID, runID, task, status string) ([]byte, error) {
	server := strings.TrimSuffix(partitionsServer, "/")
	endpoint := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions",
		server, url.PathEscape(jobID), url.PathEscape(runID), url.PathEscape(task))
	if status != "" {
		endpoint += "?" + url.Values{"status": {status}}.Encode()
	}

	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveRunDiffAPIKey(cmd, partitionsAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := partitionsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading partitions response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("partitions failed (%d): %s", resp.StatusCode, replayErrorMessage(body))
	}
	return body, nil
}

func renderPartitionsTable(cmd *cobra.Command, rows []partitionRow) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No partitions found.")
		return
	}

	var showFingerprint, showDependsOn bool
	for _, r := range rows {
		if r.Fingerprint != "" {
			showFingerprint = true
		}
		if len(r.DependsOn) > 0 {
			showDependsOn = true
		}
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "VALUE\tINDEX\tSTATUS\tATTEMPT\tCACHE HIT\tDURATION\tERROR"
	if showFingerprint {
		header += "\tFINGERPRINT"
	}
	if showDependsOn {
		header += "\tDEPENDS ON"
	}
	_, _ = fmt.Fprintln(tw, header)

	for _, r := range rows {
		line := fmt.Sprintf("%s\t%d\t%s\t%d\t%s\t%s\t%s",
			r.Value,
			r.Index,
			r.Status,
			r.Attempt,
			strconv.FormatBool(r.CacheHit),
			dashIfEmptyPartitions(r.Duration),
			dashIfEmptyPartitions(r.Error),
		)
		if showFingerprint {
			line += "\t" + dashIfEmptyPartitions(r.Fingerprint)
		}
		if showDependsOn {
			line += "\t" + dashIfEmptyPartitions(strings.Join(r.DependsOn, ","))
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

func dashIfEmptyPartitions(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	partitionsCmd.Flags().StringVar(&partitionsJobID, "job-id", "", "Job ID (required)")
	partitionsCmd.Flags().StringVar(&partitionsTask, "task", "", "Task ID or name (required)")
	partitionsCmd.Flags().StringVar(&partitionsStatus, "status", "", "Filter by instance status")
	partitionsCmd.Flags().BoolVar(&partitionsJSON, "json", false, "Write the server's machine-readable JSON to stdout")
	partitionsCmd.Flags().StringVar(&partitionsServer, "server", "http://localhost:8080", "Caesium server base URL")
	partitionsCmd.Flags().StringVar(&partitionsAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.AddCommand(partitionsCmd)
}
