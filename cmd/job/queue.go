package job

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/spf13/cobra"
)

var (
	queueServer string
	queueAPIKey string
	queueJSON   bool

	queueHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

type queueJobSummary struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

type queueItem struct {
	ID         string            `json:"id"`
	Position   int               `json:"position"`
	Priority   int               `json:"priority"`
	Params     map[string]string `json:"params,omitempty"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
}

var queueCmd = &cobra.Command{
	Use:   "queue <alias>",
	Short: "List pending queued runs for a job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		alias := strings.TrimSpace(args[0])
		if alias == "" {
			return fmt.Errorf("job alias is required")
		}

		server := strings.TrimSuffix(queueServer, "/")
		apiKey := cliutil.ResolveAPIKey(cmd, queueAPIKey, cliutil.APIKeyEnvVar)
		jobID, err := resolveQueueJobID(cmd, server, apiKey, alias)
		if err != nil {
			return err
		}

		body, rows, err := fetchQueue(cmd, server, apiKey, jobID)
		if err != nil {
			return err
		}
		if queueJSON {
			return cliutil.WritePrettyJSON(cmd, body, "job queue response")
		}

		renderQueueTable(cmd, rows)
		return nil
	},
}

func resolveQueueJobID(cmd *cobra.Command, server, apiKey, alias string) (string, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, server+"/v1/jobs", nil)
	if err != nil {
		return "", err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := queueHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading jobs response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("list jobs failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var jobs []queueJobSummary
	if err := json.Unmarshal(body, &jobs); err != nil {
		return "", fmt.Errorf("jobs response was not valid JSON (status %d): %w", resp.StatusCode, err)
	}
	for _, job := range jobs {
		if job.Alias == alias {
			return job.ID, nil
		}
	}
	return "", fmt.Errorf("job %q not found", alias)
}

func fetchQueue(cmd *cobra.Command, server, apiKey, jobID string) ([]byte, []queueItem, error) {
	reqURL := fmt.Sprintf("%s/v1/jobs/%s/queue", server, url.PathEscape(jobID))
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := queueHTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("reading job queue response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, nil, fmt.Errorf("job queue failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rows []queueItem
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, nil, fmt.Errorf("job queue response was not valid JSON (status %d): %w", resp.StatusCode, err)
	}
	return body, rows, nil
}

func renderQueueTable(cmd *cobra.Command, rows []queueItem) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No queued runs.")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "POSITION\tPRIORITY\tENQUEUED_AT\tPARAMS")
	for _, row := range rows {
		_, _ = fmt.Fprintf(
			tw,
			"%d\t%s\t%s\t%s\n",
			row.Position,
			runstorage.PriorityLabel(row.Priority),
			row.EnqueuedAt.UTC().Format(time.RFC3339),
			formatQueueParams(row.Params),
		)
	}
	_ = tw.Flush()
}

func formatQueueParams(params map[string]string) string {
	if len(params) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	replacer := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	for _, key := range keys {
		parts = append(parts, replacer.Replace(key)+"="+replacer.Replace(params[key]))
	}
	return strings.Join(parts, ",")
}

func init() {
	queueCmd.Flags().StringVar(&queueServer, "server", "http://localhost:8080", "Caesium server base URL")
	queueCmd.Flags().StringVar(&queueAPIKey, "api-key", "", "API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	queueCmd.Flags().BoolVar(&queueJSON, "json", false, "Emit machine-readable JSON instead of a table")

	Cmd.AddCommand(queueCmd)
}
