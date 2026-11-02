package run

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

// inspectHTTPClient is the client for run get, wait, and logs. Start keeps
// its own client so its tests can replace that one without affecting these.
var inspectHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

const inspectJSONLimit = 4 << 20

type runView struct {
	ID       string     `json:"id"`
	JobID    string     `json:"job_id"`
	JobAlias string     `json:"job_alias"`
	Status   string     `json:"status"`
	Error    string     `json:"error"`
	Tasks    []taskView `json:"tasks"`
}

type taskView struct {
	ID                    string         `json:"id"`
	TaskID                string         `json:"task_id"`
	Status                string         `json:"status"`
	Error                 string         `json:"error,omitempty"`
	PartitionValue        string         `json:"partition_value,omitempty"`
	PartitionCount        int            `json:"partition_count,omitempty"`
	PartitionStatusCounts map[string]int `json:"partition_status_counts,omitempty"`
}

func (t taskView) catalogID() string {
	if strings.TrimSpace(t.ID) != "" {
		return t.ID
	}
	return t.TaskID
}

type taskSummaryCounts struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Running   int `json:"running,omitempty"`
	Pending   int `json:"pending,omitempty"`
	Skipped   int `json:"skipped,omitempty"`
	Cancelled int `json:"cancelled,omitempty"`
	Cached    int `json:"cached,omitempty"`
	Other     int `json:"other,omitempty"`
}

type runStatusDocument struct {
	ID          string            `json:"id"`
	JobID       string            `json:"job_id"`
	JobAlias    string            `json:"job_alias,omitempty"`
	Status      string            `json:"status"`
	Error       string            `json:"error,omitempty"`
	TimedOut    bool              `json:"timed_out,omitempty"`
	Tasks       []taskView        `json:"tasks"`
	TaskSummary taskSummaryCounts `json:"task_summary"`
}

func serverBase(server string) string {
	return strings.TrimSuffix(strings.TrimSpace(server), "/")
}

func lookupJobIDByAlias(cmd *cobra.Command, client *http.Client, server, apiKey, alias string) (string, error) {
	alias = strings.TrimSpace(alias)
	query := url.Values{}
	query.Set("alias", alias)
	rawURL := serverBase(server) + "/v1/jobs?" + query.Encode()
	body, status, err := getJSON(cmd, client, rawURL, apiKey)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", statusError(fmt.Sprintf("lookup job %q", alias), status, body)
	}
	var jobs []struct {
		ID    string `json:"id"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(body, &jobs); err != nil {
		return "", fmt.Errorf("lookup job %q: response was not a job list: %w", alias, err)
	}
	var match string
	for _, job := range jobs {
		if job.Alias != alias || strings.TrimSpace(job.ID) == "" {
			continue
		}
		if match != "" && match != job.ID {
			return "", fmt.Errorf("alias %q matched more than one job", alias)
		}
		match = job.ID
	}
	if match == "" {
		return "", fmt.Errorf("no job with alias %q", alias)
	}
	return match, nil
}

func fetchRun(cmd *cobra.Command, client *http.Client, server, apiKey, jobID, runID string) (*runView, error) {
	rawURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s", serverBase(server), url.PathEscape(strings.TrimSpace(jobID)), url.PathEscape(strings.TrimSpace(runID)))
	body, status, err := getJSON(cmd, client, rawURL, apiKey)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError("run get", status, body)
	}
	var view runView
	if err := json.Unmarshal(body, &view); err != nil {
		return nil, fmt.Errorf("run get response was not valid JSON: %w", err)
	}
	if view.Tasks == nil {
		view.Tasks = []taskView{}
	}
	return &view, nil
}

func getJSON(cmd *cobra.Command, client *http.Client, rawURL, apiKey string) ([]byte, int, error) {
	if client == nil {
		client = inspectHTTPClient
	}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, inspectJSONLimit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > inspectJSONLimit {
		return nil, resp.StatusCode, fmt.Errorf("response exceeded %d bytes", inspectJSONLimit)
	}
	return body, resp.StatusCode, nil
}

func statusError(action string, status int, body []byte) error {
	msg := replayErrorMessage(body)
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return fmt.Errorf("%s failed (%d): %s", action, status, msg)
}

func requireRunRef(jobID, runID string) error {
	if strings.TrimSpace(jobID) == "" {
		return fmt.Errorf("--job-id is required")
	}
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("--run-id is required")
	}
	return nil
}

func runStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "failed", "cancelled", "canceled", "skipped":
		return true
	default:
		return false
	}
}

func summarizeTasks(tasks []taskView) taskSummaryCounts {
	var counts taskSummaryCounts
	for _, task := range tasks {
		if task.PartitionCount > 1 && len(task.PartitionStatusCounts) > 0 {
			for status, n := range task.PartitionStatusCounts {
				addTaskStatus(&counts, status, n)
			}
			continue
		}
		if strings.TrimSpace(task.Status) == "" {
			continue
		}
		addTaskStatus(&counts, task.Status, 1)
	}
	return counts
}

func addTaskStatus(counts *taskSummaryCounts, status string, n int) {
	if n <= 0 {
		return
	}
	counts.Total += n
	switch status {
	case "succeeded":
		counts.Succeeded += n
	case "failed":
		counts.Failed += n
	case "running":
		counts.Running += n
	case "pending":
		counts.Pending += n
	case "skipped":
		counts.Skipped += n
	case "cancelled", "canceled":
		counts.Cancelled += n
	case "cached":
		counts.Cached += n
	default:
		counts.Other += n
	}
}

func runSucceeded(view *runView) bool {
	return view != nil && view.Status == "succeeded" && summarizeTasks(view.Tasks).Failed == 0
}

func writeRunJSON(cmd *cobra.Command, view *runView, timedOut bool) error {
	if view == nil {
		view = &runView{}
	}
	tasks := view.Tasks
	if tasks == nil {
		tasks = []taskView{}
	}
	doc := runStatusDocument{
		ID:          view.ID,
		JobID:       view.JobID,
		JobAlias:    view.JobAlias,
		Status:      view.Status,
		Error:       view.Error,
		TimedOut:    timedOut,
		Tasks:       tasks,
		TaskSummary: summarizeTasks(tasks),
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return cliutil.WritePrettyJSON(cmd, encoded, "run status")
}

func formatTaskLine(task taskView) string {
	id := task.catalogID()
	if id == "" {
		id = "-"
	}
	if task.PartitionCount > 1 && len(task.PartitionStatusCounts) > 0 {
		return fmt.Sprintf("%s %s partitions=%d %s", id, task.Status, task.PartitionCount, formatStatusCounts(task.PartitionStatusCounts))
	}
	if task.PartitionValue != "" {
		return fmt.Sprintf("%s %s partition=%s", id, task.Status, task.PartitionValue)
	}
	return fmt.Sprintf("%s %s", id, task.Status)
}

func formatStatusCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for status := range counts {
		keys = append(keys, status)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, status := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", status, counts[status]))
	}
	return strings.Join(parts, " ")
}

func jobLabel(view *runView) string {
	if view == nil {
		return ""
	}
	if view.JobAlias != "" && view.JobID != "" {
		return fmt.Sprintf("%s (%s)", view.JobAlias, view.JobID)
	}
	if view.JobAlias != "" {
		return view.JobAlias
	}
	return view.JobID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
