package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type Task struct {
	ID           uuid.UUID         `gorm:"type:uuid;primaryKey"`
	JobID        uuid.UUID         `gorm:"type:uuid;index;not null"`
	Job          Job               `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	AtomID       uuid.UUID         `gorm:"type:uuid;index;not null"`
	Atom         Atom              `gorm:"constraint:OnDelete:RESTRICT" json:"-"`
	NodeSelector datatypes.JSONMap `gorm:"type:json" json:"node_selector,omitempty"`
	Retries      int               `gorm:"not null;default:0" json:"retries"`
	RetryDelay   time.Duration     `gorm:"not null;default:0" json:"retry_delay"`
	RetryBackoff bool              `gorm:"not null;default:false" json:"retry_backoff"`
	CreatedAt    time.Time         `gorm:"not null"`
	UpdatedAt    time.Time         `gorm:"not null"`
}

type Tasks []*Task
