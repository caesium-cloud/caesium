// Package reproduce exposes the read-only execution descriptor surface used by
// the reproduce client feature.
package reproduce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ErrDescriptorUnavailable is returned when an addressed task run exists but
// predates execution-descriptor capture or otherwise lacks a stored descriptor.
var ErrDescriptorUnavailable = errors.New("reproduce: descriptor unavailable")

// Service loads task execution descriptors for the REST controller.
type Service struct {
	ctx context.Context
	db  *gorm.DB
}

// DescriptorResponse is the small response wrapper around the stored raw
// TaskRun.ExecutionDescriptor JSON.
type DescriptorResponse struct {
	JobID uuid.UUID `json:"-"`

	TaskRunID  uuid.UUID           `json:"task_run_id"`
	Status     string              `json:"status"`
	Result     string              `json:"result"`
	Output     json.RawMessage     `json:"output"`
	ReplaySafe bool                `json:"replay_safe"`
	LogExcerpt LogExcerptReference `json:"log_excerpt"`
	Descriptor json.RawMessage     `json:"descriptor"`
}

// LogExcerptReference points callers at the existing task-log endpoint instead
// of embedding log text in the descriptor response.
type LogExcerptReference struct {
	Path string `json:"path"`
}

type taskDescriptorRow struct {
	ID                  uuid.UUID
	JobID               uuid.UUID
	TaskID              uuid.UUID
	Status              string
	Result              string
	Output              datatypes.JSON
	ReplaySafe          bool
	ExecutionDescriptor datatypes.JSON
}

// New creates a Service with the default DB connection.
func New(ctx context.Context) *Service {
	conn := db.Connection()
	return NewWithDatabase(ctx, conn)
}

// NewWithDatabase creates a Service backed by the supplied database connection.
func NewWithDatabase(ctx context.Context, conn *gorm.DB) *Service {
	return &Service{ctx: ctx, db: conn}
}

// WithDatabase returns a copy of the Service using the supplied connection.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return NewWithDatabase(s.ctx, conn)
}

// Descriptor returns the raw stored descriptor for taskRef in runID. taskRef is
// resolved by task name first, then as a task UUID for callers that already have
// the durable task identifier.
func (s *Service) Descriptor(runID uuid.UUID, taskRef string) (*DescriptorResponse, error) {
	taskRef = strings.TrimSpace(taskRef)
	if taskRef == "" {
		return nil, runstorage.ErrTaskRunNotFound
	}

	row, err := s.resolveTaskRun(taskRef, runID)
	if err != nil {
		return nil, err
	}
	if len(row.ExecutionDescriptor) == 0 {
		return nil, ErrDescriptorUnavailable
	}

	// The raw stored bytes are returned verbatim — deliberately NO typed decode
	// or schema-version gate here: a future schemaVersion bump must not hide a
	// perfectly readable descriptor from the CLI (which owns version handling).
	return &DescriptorResponse{
		JobID:      row.JobID,
		TaskRunID:  row.ID,
		Status:     row.Status,
		Result:     row.Result,
		Output:     cloneRawJSON(row.Output),
		ReplaySafe: row.ReplaySafe,
		LogExcerpt: LogExcerptReference{
			Path: fmt.Sprintf("/v1/jobs/%s/runs/%s/logs?task_id=%s", row.JobID, runID, row.TaskID),
		},
		Descriptor: cloneRawJSON(row.ExecutionDescriptor),
	}, nil
}

func (s *Service) resolveTaskRun(taskRef string, runID uuid.UUID) (*taskDescriptorRow, error) {
	if row, err := s.resolveTaskRunByName(taskRef, runID); err == nil {
		return row, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	taskID, err := uuid.Parse(taskRef)
	if err != nil {
		return nil, runstorage.ErrTaskRunNotFound
	}

	row, err := s.resolveTaskRunByID(taskID, runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, runstorage.ErrTaskRunNotFound
		}
		return nil, err
	}
	return row, nil
}

func (s *Service) resolveTaskRunByName(taskName string, runID uuid.UUID) (*taskDescriptorRow, error) {
	var row taskDescriptorRow
	err := s.baseDescriptorQuery(runID).
		Where("tasks.name = ?", taskName).
		Take(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Service) resolveTaskRunByID(taskID, runID uuid.UUID) (*taskDescriptorRow, error) {
	var row taskDescriptorRow
	err := s.baseDescriptorQuery(runID).
		Where("task_runs.task_id = ?", taskID).
		Take(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *Service) baseDescriptorQuery(runID uuid.UUID) *gorm.DB {
	return s.db.WithContext(s.ctx).
		Table("task_runs").
		Select(
			"task_runs.id AS id, "+
				"job_runs.job_id AS job_id, "+
				"task_runs.task_id AS task_id, "+
				"task_runs.status AS status, "+
				"task_runs.result AS result, "+
				"task_runs.output AS output, "+
				"task_runs.replay_safe AS replay_safe, "+
				"task_runs.execution_descriptor AS execution_descriptor",
		).
		Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
		Joins("JOIN tasks ON tasks.id = task_runs.task_id").
		Where("task_runs.job_run_id = ?", runID)
}

func cloneRawJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return json.RawMessage(cp)
}
