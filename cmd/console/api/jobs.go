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
	ID          string            `json:"id"`
	Alias       string            `json:"alias"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	CreatedAt   time.Time         `json:"created_at"`
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
