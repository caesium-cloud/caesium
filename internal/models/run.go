package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type JobRun struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	JobID        uuid.UUID `gorm:"type:uuid;index;not null" json:"job_id"`
	TriggerID    uuid.UUID `gorm:"type:uuid;index" json:"trigger_id"`
	TriggerType  string    `gorm:"type:text" json:"trigger_type"`
	TriggerAlias string    `gorm:"type:text" json:"trigger_alias"`
	Status       string    `gorm:"type:text;index;not null" json:"status"`
	Error        string    `json:"error,omitempty"`
	StartedAt   time.Time `gorm:"not null" json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"not null" json:"updated_at"`
	Tasks       []*TaskRun `gorm:"foreignKey:JobRunID;constraint:OnDelete:CASCADE" json:"tasks,omitempty"`
}

type TaskRun struct {
	ID                      uuid.UUID         `gorm:"type:uuid;primaryKey" json:"id"`
	JobRunID                uuid.UUID         `gorm:"type:uuid;index;not null" json:"job_run_id"`
	TaskID                  uuid.UUID         `gorm:"type:uuid;index;not null" json:"task_id"`
	AtomID                  uuid.UUID         `gorm:"type:uuid;index;not null" json:"atom_id"`
	Engine                  AtomEngine        `gorm:"type:text;not null" json:"engine"`
	Image                   string            `gorm:"not null" json:"image"`
	Command                 string            `gorm:"not null" json:"command"`
	Status                  string            `gorm:"type:text;index;not null" json:"status"`
	ClaimedBy               string            `gorm:"type:text;index;not null;default:''" json:"claimed_by"`
	ClaimExpiresAt          *time.Time        `gorm:"index" json:"claim_expires_at,omitempty"`
	ClaimAttempt            int               `gorm:"not null;default:0" json:"claim_attempt"`
	NodeSelector            datatypes.JSONMap `gorm:"type:json" json:"node_selector,omitempty"`
	Result                  string            `json:"result,omitempty"`
	Error                   string            `json:"error,omitempty"`
	RuntimeID               string            `json:"runtime_id,omitempty"`
	OutstandingPredecessors int               `gorm:"not null" json:"outstanding_predecessors"`
	StartedAt               *time.Time        `json:"started_at,omitempty"`
	CompletedAt             *time.Time        `json:"completed_at,omitempty"`
	CreatedAt               time.Time         `gorm:"not null" json:"created_at"`
	UpdatedAt               time.Time         `gorm:"not null" json:"updated_at"`
}
