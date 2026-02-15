package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type JobRun struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	JobID       uuid.UUID `gorm:"type:uuid;index;not null"`
	Status      string    `gorm:"type:text;index;not null"`
	Error       string
	StartedAt   time.Time `gorm:"not null"`
	CompletedAt *time.Time
	CreatedAt   time.Time  `gorm:"not null"`
	UpdatedAt   time.Time  `gorm:"not null"`
	Tasks       []*TaskRun `gorm:"foreignKey:JobRunID;constraint:OnDelete:CASCADE"`
}

type TaskRun struct {
	ID                      uuid.UUID         `gorm:"type:uuid;primaryKey"`
	JobRunID                uuid.UUID         `gorm:"type:uuid;index;not null"`
	TaskID                  uuid.UUID         `gorm:"type:uuid;index;not null"`
	AtomID                  uuid.UUID         `gorm:"type:uuid;index;not null"`
	Engine                  AtomEngine        `gorm:"type:text;not null"`
	Image                   string            `gorm:"not null"`
	Command                 string            `gorm:"not null"`
	Status                  string            `gorm:"type:text;index;not null"`
	ClaimedBy               string            `gorm:"type:text;index;not null;default:''"`
	ClaimExpiresAt          *time.Time        `gorm:"index"`
	ClaimAttempt            int               `gorm:"not null;default:0"`
	NodeSelector            datatypes.JSONMap `gorm:"type:json" json:"node_selector,omitempty"`
	Result                  string
	Error                   string
	RuntimeID               string
	OutstandingPredecessors int `gorm:"not null"`
	StartedAt               *time.Time
	CompletedAt             *time.Time
	CreatedAt               time.Time `gorm:"not null"`
	UpdatedAt               time.Time `gorm:"not null"`
}
