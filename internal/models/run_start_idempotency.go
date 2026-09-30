package models

import (
	"time"

	"github.com/google/uuid"
)

func (RunStartIdempotency) TableName() string { return "run_start_idempotency" }

// RunStartIdempotency records the admission outcome of a run start that carried
// an Idempotency-Key (POST /v1/jobs/:id/run), so a retried request returns the
// original outcome instead of admitting a second run. At-least-once callers — a
// Temporal activity, a CI job, any client that retries on timeout — depend on it.
//
// The row is written in the same transaction that admits the run, and the
// (job_id, idempotency_key) unique index is what makes two concurrent retries
// resolve to one admission. A queued start is updated in place when the
// dequeuer promotes it, in the transaction that creates the run.
type RunStartIdempotency struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	JobID          uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_run_start_idempotency_job_key,priority:1" json:"job_id"`
	Job            Job       `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	IdempotencyKey string    `gorm:"type:text;not null;uniqueIndex:idx_run_start_idempotency_job_key,priority:2" json:"idempotency_key"`
	// RequestHash fingerprints the request body (params + priority) so a key
	// reused for a different request is refused instead of silently answered
	// with another request's run.
	RequestHash string `gorm:"type:text;not null" json:"request_hash"`
	// Outcome is created, queued, or skipped.
	Outcome string `gorm:"type:text;not null" json:"outcome"`
	// Reason explains a skipped outcome: max_concurrency or dataset_hold.
	Reason string `gorm:"type:text;not null;default:''" json:"reason,omitempty"`
	// RunID is the created run, or the terminal skipped run a dataset hold
	// wrote. A queued start gains it when it is promoted.
	RunID *uuid.UUID `gorm:"type:uuid;index" json:"run_id,omitempty"`
	// QueueID is the run_queue row a queued start is waiting in.
	QueueID   *uuid.UUID `gorm:"type:uuid;index" json:"queue_id,omitempty"`
	CreatedAt time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"not null" json:"updated_at"`
}
