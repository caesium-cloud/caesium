package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Run represents a job execution run.
type Run struct {
	ID          string     `json:"id"`
	JobID       string     `json:"job_id"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Error       string     `json:"error"`
	Tasks       []RunTask  `json:"tasks"`
}

// RunTask contains per-task execution detail.
type RunTask struct {
	ID          string     `json:"id"`
	AtomID      string     `json:"atom_id"`
	Engine      string     `json:"engine"`
	Image       string     `json:"image"`
	Command     []string   `json:"command"`
	RuntimeID   string     `json:"runtime_id"`
	Status      string     `json:"status"`
	Result      string     `json:"result"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Error       string     `json:"error"`
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
