package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// RunDeadlineError identifies expiry of metadata.runTimeout. It unwraps to
// context.DeadlineExceeded so ordinary cancellation handling still recognizes
// it, while IsRunDeadlineError lets persistence distinguish this narrow case
// from process shutdown or claim cancellation.
type RunDeadlineError struct {
	Timeout time.Duration
}

func (e *RunDeadlineError) Error() string {
	return fmt.Sprintf("run timed out after %s", e.Timeout)
}

func (e *RunDeadlineError) Unwrap() error {
	return context.DeadlineExceeded
}

func NewRunDeadlineError(timeout time.Duration) error {
	return &RunDeadlineError{Timeout: timeout}
}

func IsRunDeadlineError(err error) bool {
	var target *RunDeadlineError
	return errors.As(err, &target)
}

// TaskExecutionDeadline is the immutable timeout policy captured when the
// TaskRun was registered plus the durable anchor for this run execution
// window. The worker reads this instead of the mutable jobs row.
type TaskExecutionDeadline struct {
	TaskTimeout time.Duration
	RunTimeout  time.Duration
	RunStarted  time.Time
}

// TaskExecutionDeadlineForRun loads deadline inputs for one exact TaskRun.
// taskRef follows TaskExecutionDescriptor's primary-key-or-unique-task-id
// contract; workers holding a TaskRun always pass its primary key.
func (s *Store) TaskExecutionDeadlineForRun(ctx context.Context, runID, taskRef uuid.UUID) (TaskExecutionDeadline, error) {
	taskRun, err := loadTaskRunByIDOrUnique(s.db.WithContext(ctx), runID, taskRef)
	if err != nil {
		return TaskExecutionDeadline{}, err
	}
	// Legacy TaskRuns can predate execution descriptors. Preserve their prior
	// worker behavior (environment task-timeout fallback, no invented run
	// timeout) instead of failing the task during a rolling upgrade. Every newly
	// registered row has a descriptor and therefore takes the immutable path.
	var timing models.TaskExecutionTiming
	if len(taskRun.ExecutionDescriptor) > 0 {
		var descriptor models.TaskExecutionDescriptor
		if err := json.Unmarshal(taskRun.ExecutionDescriptor, &descriptor); err != nil {
			return TaskExecutionDeadline{}, fmt.Errorf("run: decode task execution descriptor for run %s task %s: %w", runID, taskRef, err)
		}
		if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
			return TaskExecutionDeadline{}, fmt.Errorf("run: unsupported task execution descriptor version %d for run %s task %s", descriptor.SchemaVersion, runID, taskRef)
		}
		timing = descriptor.Timing
	}

	var jobRun models.JobRun
	if err := s.db.WithContext(ctx).
		Select("started_at", "timeout_started_at").
		First(&jobRun, "id = ?", runID).Error; err != nil {
		return TaskExecutionDeadline{}, err
	}
	anchor := jobRun.StartedAt
	if jobRun.TimeoutStartedAt != nil && !jobRun.TimeoutStartedAt.IsZero() {
		anchor = *jobRun.TimeoutStartedAt
	}
	return TaskExecutionDeadline{
		TaskTimeout: timing.TaskTimeout,
		RunTimeout:  timing.RunTimeout,
		RunStarted:  anchor,
	}, nil
}

// RunExecutionDeadline returns the timeout and durable anchor for the current
// execution window. Once TaskRuns exist, their frozen descriptor is
// authoritative; fallback applies only before a new run has registered tasks.
func (s *Store) RunExecutionDeadline(ctx context.Context, runID uuid.UUID, fallback time.Duration) (time.Duration, time.Time, error) {
	var jobRun models.JobRun
	if err := s.db.WithContext(ctx).
		Select("started_at", "timeout_started_at").
		First(&jobRun, "id = ?", runID).Error; err != nil {
		return 0, time.Time{}, err
	}
	anchor := jobRun.StartedAt
	if jobRun.TimeoutStartedAt != nil && !jobRun.TimeoutStartedAt.IsZero() {
		anchor = *jobRun.TimeoutStartedAt
	}

	var rows []models.TaskRun
	if err := s.db.WithContext(ctx).
		Select("execution_descriptor").
		Where("job_run_id = ?", runID).
		Order("created_at ASC").
		Limit(1).
		Find(&rows).Error; err != nil {
		return 0, time.Time{}, err
	}
	if len(rows) == 0 {
		return fallback, anchor, nil
	}
	if len(rows[0].ExecutionDescriptor) == 0 {
		// Compatibility for a run registered by a pre-descriptor binary. Keep
		// its original StartedAt anchor, so even the fallback cannot grant a new
		// timeout window on takeover.
		return fallback, anchor, nil
	}
	var descriptor models.TaskExecutionDescriptor
	if err := json.Unmarshal(rows[0].ExecutionDescriptor, &descriptor); err != nil {
		return 0, time.Time{}, fmt.Errorf("run: decode task execution descriptor for run %s: %w", runID, err)
	}
	if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
		return 0, time.Time{}, fmt.Errorf("run: unsupported task execution descriptor version %d for run %s", descriptor.SchemaVersion, runID)
	}
	return descriptor.Timing.RunTimeout, anchor, nil
}
