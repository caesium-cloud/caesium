package models

import (
	"time"

	"github.com/google/uuid"
)

type CallbackRunStatus string

const (
	CallbackRunStatusPending   CallbackRunStatus = "pending"
	CallbackRunStatusRunning   CallbackRunStatus = "running"
	CallbackRunStatusSucceeded CallbackRunStatus = "succeeded"
	CallbackRunStatusFailed    CallbackRunStatus = "failed"
)

type CallbackRun struct {
	ID         uuid.UUID         `gorm:"type:uuid;primaryKey"`
	CallbackID uuid.UUID         `gorm:"type:uuid;index;not null;uniqueIndex:idx_callback_runs_completion,priority:1"`
	Callback   Callback          `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	JobID      uuid.UUID         `gorm:"type:uuid;index;not null"`
	JobRunID   uuid.UUID         `gorm:"type:uuid;index;not null;uniqueIndex:idx_callback_runs_completion,priority:2"`
	JobRun     JobRun            `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Status     CallbackRunStatus `gorm:"type:text;index;not null"`
	Error      string            `gorm:"type:text"`
	// HTTPStatus is the status code the callback target answered with. It is 0
	// when the attempt never produced a response at all (DNS/connect/TLS
	// failure, timeout, a malformed configuration) or when the handler is not
	// HTTP-based, which is exactly the distinction an operator needs to tell a
	// transient network failure from a permanent 4xx.
	HTTPStatus int `gorm:"not null;default:0"`
	// ResponseBody is the target's response body, truncated to
	// maxResponseBodyBytes and passed through the secret scrubber before it is
	// persisted. Empty when there was no response or the body was empty.
	ResponseBody string `gorm:"type:text"`
	// RetryCount is the number of delivery attempts that preceded the one this
	// row records: 0 for the run-completion dispatch, 1 for the first
	// `retry-callbacks` retry of the same callback on the same run, and so on.
	// It is per (callback, job run), so it survives being read back out of a
	// single row without joining the callback's other attempts.
	RetryCount int `gorm:"not null;default:0"`
	// CompletionKey names the run completion this row delivered for, so the
	// unique index lets one callback be delivered once per completion however
	// many processes finish the same run. It is NULL on an operator's re-send
	// and on rows from before the column, which the index never conflicts on.
	CompletionKey *string   `gorm:"type:text;uniqueIndex:idx_callback_runs_completion,priority:3"`
	StartedAt     time.Time `gorm:"not null"`
	CompletedAt   *time.Time
	CreatedAt     time.Time `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null"`
}

type CallbackRuns []*CallbackRun
