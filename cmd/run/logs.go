package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

const (
	defaultLogsMaxBytes     = 64 * 1024
	defaultLogsMaxInstances = 20
)

var (
	logsRunID  string
	logsJobID  string
	logsServer string
	logsAPIKey string
	logsJSON   bool

	// logsMaxBytes and logsMaxInstances bound what `run logs` prints. The API's
	// own redaction is left as returned; these caps only stop an unbounded dump.
	logsMaxBytes     = defaultLogsMaxBytes
	logsMaxInstances = defaultLogsMaxInstances
	logsFetchTimeout = 10 * time.Second
)

var logsCmd = &cobra.Command{
	Use:   "logs --run-id <run-id> --job-id <job-id>",
	Short: "Print task logs for a run",
	Long: "Print task logs for a run from GET /v1/jobs/:id/runs/:run_id/logs.\n\n" +
		"A fanned task is requested once per partition instance the API lists. " +
		"Output is bounded to 64KiB per log and 20 partition instances; a note on " +
		"stderr says when either cap is hit. Logs are printed as the API returned " +
		"them, including redaction the server already applied. " +
		"With --json, stdout is only the JSON document.",
	Args: cobra.NoArgs,
	RunE: runLogs,
}

type logEntry struct {
	TaskID    string `json:"task_id"`
	TaskRunID string `json:"task_run_id,omitempty"`
	Partition string `json:"partition,omitempty"`
	Status    string `json:"status,omitempty"`
	State     string `json:"state,omitempty"`
	Source    string `json:"source,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Text      string `json:"text"`
}

type logsDocument struct {
	RunID     string     `json:"run_id"`
	JobID     string     `json:"job_id"`
	JobAlias  string     `json:"job_alias,omitempty"`
	Entries   []logEntry `json:"entries"`
	Truncated bool       `json:"truncated"`
}

type logProblem struct {
	Message   string        `json:"message"`
	Instances []logInstance `json:"instances"`
}

type logInstance struct {
	TaskRunID string `json:"task_run_id"`
	Partition string `json:"partition"`
	Status    string `json:"status"`
}

func runLogs(cmd *cobra.Command, _ []string) error {
	if err := requireRunRef(logsJobID, logsRunID); err != nil {
		return err
	}
	cmd.SilenceUsage = true
	apiKey := resolveRunDiffAPIKey(cmd, logsAPIKey)
	view, err := fetchRun(cmd, inspectHTTPClient, logsServer, apiKey, logsJobID, logsRunID)
	if err != nil {
		return err
	}
	entries, truncated, err := collectLogs(cmd, apiKey, strings.TrimSpace(logsJobID), strings.TrimSpace(logsRunID), view)
	if err != nil {
		return err
	}
	if truncated {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: log output truncated")
	}
	if logsJSON {
		return writeLogsJSON(cmd, view, entries, truncated)
	}
	return writeLogsHuman(cmd, entries)
}

func collectLogs(cmd *cobra.Command, apiKey, jobID, runID string, view *runView) ([]logEntry, bool, error) {
	entries := make([]logEntry, 0)
	truncated := false
	if view == nil {
		return entries, false, nil
	}
	for _, task := range view.Tasks {
		got, cut, err := collectTaskLogs(cmd, apiKey, jobID, runID, task)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, got...)
		truncated = truncated || cut
	}
	return entries, truncated, nil
}

func collectTaskLogs(cmd *cobra.Command, apiKey, jobID, runID string, task taskView) ([]logEntry, bool, error) {
	taskID := task.catalogID()
	if taskID == "" {
		return nil, false, fmt.Errorf("run task is missing an id")
	}
	entry, problem, truncated, err := fetchTaskLog(cmd, apiKey, jobID, runID, taskID, "", task)
	if err != nil {
		return nil, false, err
	}
	if problem == nil {
		return []logEntry{entry}, truncated, nil
	}
	if len(problem.Instances) == 0 {
		msg := strings.TrimSpace(problem.Message)
		if msg == "" {
			msg = "task logs were not available"
		}
		return nil, false, fmt.Errorf("task %s logs: %s", taskID, msg)
	}
	limit := logsMaxInstances
	if limit <= 0 {
		limit = defaultLogsMaxInstances
	}
	instances := problem.Instances
	cut := false
	if len(instances) > limit {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: showing %d of %d partition logs for task %s\n", limit, len(instances), taskID)
		instances = instances[:limit]
		cut = true
	}
	entries := make([]logEntry, 0, len(instances))
	for _, inst := range instances {
		got, again, instTrunc, fetchErr := fetchTaskLog(cmd, apiKey, jobID, runID, taskID, inst.TaskRunID, task)
		if fetchErr != nil {
			return nil, false, fetchErr
		}
		if again != nil {
			return nil, false, fmt.Errorf("task %s partition %s logs: %s", taskID, inst.Partition, again.Message)
		}
		if got.Partition == "" {
			got.Partition = inst.Partition
		}
		if got.TaskRunID == "" {
			got.TaskRunID = inst.TaskRunID
		}
		if got.Status == "" {
			got.Status = inst.Status
		}
		entries = append(entries, got)
		cut = cut || instTrunc
	}
	return entries, cut, nil
}

// fetchTaskLog reads one log stream. It never adds a query parameter that would
// ask the API for an unredacted log; task_id and, for a selected partition,
// task_run_id are the only selectors.
func fetchTaskLog(cmd *cobra.Command, apiKey, jobID, runID, taskID, taskRunID string, task taskView) (logEntry, *logProblem, bool, error) {
	query := url.Values{}
	query.Set("task_id", taskID)
	if taskRunID != "" {
		query.Set("task_run_id", taskRunID)
	}
	rawURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?%s", serverBase(logsServer), url.PathEscape(jobID), url.PathEscape(runID), query.Encode())

	timeout := logsFetchTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return logEntry{}, nil, false, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := inspectHTTPClient.Do(req)
	if err != nil {
		return logEntry{}, nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return logEntry{}, nil, false, readErr
		}
		var problem logProblem
		if jsonErr := json.Unmarshal(body, &problem); jsonErr == nil && taskRunID == "" && resp.StatusCode == http.StatusBadRequest && len(problem.Instances) > 0 {
			return logEntry{}, &problem, false, nil
		}
		return logEntry{}, nil, false, statusError("task logs", resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return logEntry{}, nil, false, statusError("task logs", resp.StatusCode, body)
	}

	maxBytes := logsMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultLogsMaxBytes
	}
	text, cut, readErr := readBounded(resp.Body, maxBytes)
	if readErr != nil && text == "" {
		return logEntry{}, nil, false, fmt.Errorf("reading task %s logs: %w", taskID, readErr)
	}
	if readErr != nil {
		cut = true
	}
	if strings.EqualFold(resp.Header.Get("X-Caesium-Log-Truncated"), "true") {
		cut = true
	}
	entry := logEntry{
		TaskID:    taskID,
		TaskRunID: firstNonEmpty(resp.Header.Get("X-Caesium-Task-Run-ID"), taskRunID),
		Partition: resp.Header.Get("X-Caesium-Partition"),
		Status:    task.Status,
		State:     resp.Header.Get("X-Caesium-Log-State"),
		Source:    resp.Header.Get("X-Caesium-Log-Source"),
		Truncated: cut,
		Text:      text,
	}
	return entry, nil, cut, nil
}

func readBounded(r io.Reader, maxBytes int) (string, bool, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	buf, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if len(buf) > maxBytes {
		return string(buf[:maxBytes]), true, err
	}
	return string(buf), false, err
}

func writeLogsJSON(cmd *cobra.Command, view *runView, entries []logEntry, truncated bool) error {
	if entries == nil {
		entries = []logEntry{}
	}
	doc := logsDocument{
		RunID:     view.ID,
		JobID:     view.JobID,
		JobAlias:  view.JobAlias,
		Entries:   entries,
		Truncated: truncated,
	}
	if doc.RunID == "" {
		doc.RunID = strings.TrimSpace(logsRunID)
	}
	if doc.JobID == "" {
		doc.JobID = strings.TrimSpace(logsJobID)
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return cliutil.WritePrettyJSON(cmd, encoded, "run logs")
}

func writeLogsHuman(cmd *cobra.Command, entries []logEntry) error {
	out := cmd.OutOrStdout()
	for i, entry := range entries {
		if i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		header := entry.TaskID
		if entry.Partition != "" {
			header += " partition=" + entry.Partition
		}
		if entry.TaskRunID != "" {
			header += " task_run=" + entry.TaskRunID
		}
		if _, err := fmt.Fprintf(out, "== %s ==\n", header); err != nil {
			return err
		}
		switch {
		case entry.Text != "":
			if _, err := fmt.Fprint(out, entry.Text); err != nil {
				return err
			}
			if !strings.HasSuffix(entry.Text, "\n") {
				if _, err := fmt.Fprintln(out); err != nil {
					return err
				}
			}
		case entry.State != "":
			if _, err := fmt.Fprintf(out, "(no logs: %s)\n", entry.State); err != nil {
				return err
			}
		}
	}
	return nil
}

func init() {
	logsCmd.Flags().StringVar(&logsRunID, "run-id", "", "Run ID to read logs for (required)")
	logsCmd.Flags().StringVar(&logsJobID, "job-id", "", "Job ID that owns the run (required)")
	logsCmd.Flags().StringVar(&logsServer, "server", "http://localhost:8080", "Caesium server base URL")
	logsCmd.Flags().StringVar(&logsAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	logsCmd.Flags().BoolVar(&logsJSON, "json", false, "Print task logs as JSON on stdout")
	Cmd.AddCommand(logsCmd)
}
