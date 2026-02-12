package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// StatsResponse mirrors the /v1/stats API response.
type StatsResponse struct {
	Jobs        JobStats     `json:"jobs"`
	TopFailing  []FailingJob `json:"top_failing"`
	SlowestJobs []SlowestJob `json:"slowest_jobs"`
}

// JobStats contains aggregate job statistics.
type JobStats struct {
	Total              int64   `json:"total"`
	RecentRuns         int64   `json:"recent_runs"`
	SuccessRate        float64 `json:"success_rate"`
	AvgDurationSeconds float64 `json:"avg_duration_seconds"`
}

// FailingJob describes a frequently failing job.
type FailingJob struct {
	JobID        string     `json:"job_id"`
	Alias        string     `json:"alias"`
	FailureCount int64      `json:"failure_count"`
	LastFailure  *time.Time `json:"last_failure"`
}

// SlowestJob describes a job with high average duration.
type SlowestJob struct {
	JobID              string  `json:"job_id"`
	Alias              string  `json:"alias"`
	AvgDurationSeconds float64 `json:"avg_duration_seconds"`
}

// StatsService exposes stats-related API helpers.
type StatsService struct {
	client *Client
}

// Stats exposes the stats service.
func (c *Client) Stats() *StatsService {
	return &StatsService{client: c}
}

// Get fetches aggregated statistics.
func (s *StatsService) Get(ctx context.Context) (*StatsResponse, error) {
	endpoint := s.client.resolve("/v1/stats")
	var payload StatsResponse
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("fetch stats: %w", err)
	}
	return &payload, nil
}
