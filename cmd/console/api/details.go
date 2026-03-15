package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// JobDetailOptions configures job detail fetch behaviour.
type JobDetailOptions struct {
	IncludeDAG bool
}

// JobDetail aggregates job metadata, trigger configuration,
// latest run snapshot, and optional DAG topology data.
type JobDetail struct {
	Job       JobDescriptor
	Trigger   *TriggerDetail
	LatestRun *Run
	Tasks     []JobTask
	DAG       *JobDAG
}

// JobDescriptor mirrors the job payload returned by the API.
type JobDescriptor struct {
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

// JobTask represents persisted task definition metadata for a job.
type JobTask struct {
	ID           string            `json:"id"`
	JobID        string            `json:"job_id"`
	AtomID       string            `json:"atom_id"`
	NextID       *string           `json:"next_id"`
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	Retries      int               `json:"retries"`
	RetryDelay   time.Duration     `json:"retry_delay"`
	RetryBackoff bool              `json:"retry_backoff"`
	TriggerRule  string            `json:"trigger_rule"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// TriggerDetail mirrors the trigger payload returned alongside job detail.
type TriggerDetail struct {
	ID                 string    `json:"id"`
	Alias              string    `json:"alias"`
	Type               string    `json:"type"`
	Configuration      string    `json:"configuration"`
	ProvenanceSourceID string    `json:"provenance_source_id"`
	ProvenanceRepo     string    `json:"provenance_repo"`
	ProvenanceRef      string    `json:"provenance_ref"`
	ProvenanceCommit   string    `json:"provenance_commit"`
	ProvenancePath     string    `json:"provenance_path"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// JobDAG captures the job DAG topology payload.
type JobDAG struct {
	JobID string       `json:"job_id"`
	Nodes []JobDAGNode `json:"nodes"`
	Edges []JobDAGEdge `json:"edges"`
}

// JobDAGNode represents a DAG node returned by the API.
type JobDAGNode struct {
	ID         string   `json:"id"`
	AtomID     string   `json:"atom_id"`
	Successors []string `json:"successors"`
}

// JobDAGEdge represents a DAG edge returned by the API.
type JobDAGEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type jobDetailPayload struct {
	JobDescriptor
	Trigger   *TriggerDetail `json:"trigger"`
	LatestRun *Run           `json:"latest_run"`
}

// Detail fetches job metadata, trigger detail, latest run snapshot, and optionally the DAG.
func (s *JobsService) Detail(ctx context.Context, jobID string, opts *JobDetailOptions) (*JobDetail, error) {
	if jobID == "" {
		return nil, fmt.Errorf("job id is required")
	}

	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s", jobID))

	var payload jobDetailPayload
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("job detail: %w", err)
	}

	detail := &JobDetail{
		Job:       payload.JobDescriptor,
		Trigger:   payload.Trigger,
		LatestRun: payload.LatestRun,
	}

	if opts != nil && opts.IncludeDAG {
		dag, err := s.fetchDAG(ctx, jobID)
		if err != nil {
			return nil, err
		}
		detail.DAG = dag

		tasks, err := s.fetchTasks(ctx, jobID)
		if err != nil {
			return nil, err
		}
		detail.Tasks = tasks
	}

	return detail, nil
}

func (s *JobsService) fetchDAG(ctx context.Context, jobID string) (*JobDAG, error) {
	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/dag", jobID))

	var payload JobDAG
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("job dag: %w", err)
	}

	return &payload, nil
}

func (s *JobsService) fetchTasks(ctx context.Context, jobID string) ([]JobTask, error) {
	endpoint := s.client.resolve(fmt.Sprintf("/v1/jobs/%s/tasks", jobID))

	var payload []JobTask
	if err := s.client.do(ctx, http.MethodGet, endpoint, &payload); err != nil {
		return nil, fmt.Errorf("job tasks: %w", err)
	}

	return payload, nil
}
