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
	ID          uuid.UUID         `gorm:"type:uuid;primaryKey"`
	CallbackID  uuid.UUID         `gorm:"type:uuid;index;not null"`
	JobID       uuid.UUID         `gorm:"type:uuid;index;not null"`
	JobRunID    uuid.UUID         `gorm:"type:uuid;index;not null"`
	Status      CallbackRunStatus `gorm:"type:text;index;not null"`
	Error       string            `gorm:"type:text"`
	StartedAt   time.Time         `gorm:"not null"`
	CompletedAt *time.Time        `gorm:""`
	CreatedAt   time.Time         `gorm:"not null"`
	UpdatedAt   time.Time         `gorm:"not null"`
}

type CallbackRuns []*CallbackRun
