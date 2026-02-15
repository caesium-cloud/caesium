package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Run represents a job execution run.
type Run struct {
	ID          string        `json:"id"`
	JobID       string        `json:"job_id"`
	Status      string        `json:"status"`
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt *time.Time    `json:"completed_at"`
	Error       string        `json:"error"`
	Tasks       []RunTask     `json:"tasks"`
	Callbacks   []RunCallback `json:"callbacks"`
}

// RunTask contains per-task execution detail.
type RunTask struct {
	ID           string     `json:"id"`
	AtomID       string     `json:"atom_id"`
	Engine       string     `json:"engine"`
	Image        string     `json:"image"`
	Command      []string   `json:"command"`
	RuntimeID    string     `json:"runtime_id"`
	ClaimedBy    string     `json:"claimed_by,omitempty"`
	ClaimAttempt int        `json:"claim_attempt,omitempty"`
	Status       string     `json:"status"`
	Result       string     `json:"result"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	Error        string     `json:"error"`
	// OutstandingPredecessors captures how many predecessor tasks were incomplete when this task started.
	OutstandingPredecessors int `json:"outstanding_predecessors,omitempty"`
}

// RunCallback contains per-callback execution detail.
type RunCallback struct {
	ID          string     `json:"id"`
	CallbackID  string     `json:"callback_id"`
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// RunsResponse wraps a list of runs.
type RunsResponse []Run

// RunsService exposes run history operations.
type RunsService struct {
	client *Client
}

// List fetches run history for the specified job.
func (s *RunsService) List(ctx context.Context, jobID string, params url.Values) (RunsResponse, error) {
	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/runs", jobID), params.Encode())

	var payload RunsResponse
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	return payload, nil
}

// Get retrieves a single run detail for a job.
func (s *RunsService) Get(ctx context.Context, jobID, runID string) (*Run, error) {
	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID))

	var payload Run
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}

	return &payload, nil
}

// Logs streams task logs for a given run and task.
func (s *RunsService) Logs(ctx context.Context, jobID, runID, taskID string, since time.Time) (io.ReadCloser, error) {
	if jobID == "" || runID == "" || taskID == "" {
		return nil, fmt.Errorf("job id, run id, and task id are required")
	}

	params := url.Values{}
	params.Set("task_id", taskID)
	if !since.IsZero() {
		params.Set("since", since.Format(time.RFC3339Nano))
	}
	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/runs/%s/logs", jobID, runID), params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			_ = resp.Body.Close()
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		} else {
			msg = fmt.Sprintf("%s: %s", resp.Status, msg)
		}
		return nil, fmt.Errorf("stream logs: %s", msg)
	}

	return resp.Body, nil
}

// Trigger manually starts a run for the specified job.
func (s *RunsService) Trigger(ctx context.Context, jobID string) (*Run, error) {
	if jobID == "" {
		return nil, fmt.Errorf("job id is required")
	}

	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/run", jobID))

	var payload Run
	if err := s.client.do(ctx, http.MethodPost, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("trigger run: %w", err)
	}

	return &payload, nil
}
