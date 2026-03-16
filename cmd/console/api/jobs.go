package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Job represents the API projection of a job record.
type Job struct {
	ID                 string            `json:"id"`
	Alias              string            `json:"alias"`
	TriggerID          string            `json:"trigger_id"`
	Paused             bool              `json:"paused"`
	Labels             map[string]string `json:"labels"`
	Annotations        map[string]string `json:"annotations"`
	ProvenanceSourceID string            `json:"provenance_source_id"`
	ProvenanceRepo     string            `json:"provenance_repo"`
	ProvenanceRef      string            `json:"provenance_ref"`
	ProvenanceCommit   string            `json:"provenance_commit"`
	ProvenancePath     string            `json:"provenance_path"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

// JobsResponse wraps the job list payload.
type JobsResponse []Job

// JobsService exposes job-related operations.
type JobsService struct {
	client *Client
}

// List fetches jobs ordered by creation date descending.
func (s *JobsService) List(ctx context.Context, params url.Values) (JobsResponse, error) {
	endpoint := s.client.resolve("/v1/jobs", params.Encode())

	var payload JobsResponse
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}

	return payload, nil
}

func (s *JobsService) Pause(ctx context.Context, jobID string) (*Job, error) {
	return s.setPaused(ctx, jobID, true)
}

func (s *JobsService) Unpause(ctx context.Context, jobID string) (*Job, error) {
	return s.setPaused(ctx, jobID, false)
}

func (s *JobsService) setPaused(ctx context.Context, jobID string, paused bool) (*Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("job id is required")
	}

	action := "pause"
	if !paused {
		action = "unpause"
	}

	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/%s", jobID, action))

	var payload Job
	if err := s.client.do(ctx, http.MethodPut, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("%s job: %w", action, err)
	}

	return &payload, nil
}
