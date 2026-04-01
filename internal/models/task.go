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
	Name         string            `gorm:"type:text;not null;default:''" json:"name"`
	Type         string            `gorm:"type:text;not null;default:'task'" json:"type"`
	NodeSelector datatypes.JSONMap `gorm:"type:json" json:"node_selector,omitempty"`
	Retries      int               `gorm:"not null;default:0" json:"retries"`
	RetryDelay   time.Duration     `gorm:"not null;default:0" json:"retry_delay"`
	RetryBackoff bool              `gorm:"not null;default:false" json:"retry_backoff"`
	TriggerRule  string            `gorm:"type:text;not null;default:'all_success'" json:"trigger_rule"`
	CacheConfig  datatypes.JSON `gorm:"type:json" json:"cache_config,omitempty"`
	// OutputSchema is a JSON Schema describing this task's expected output keys.
	OutputSchema datatypes.JSON `gorm:"type:json" json:"output_schema,omitempty"`
	// InputSchema maps predecessor task names to JSON Schema fragments describing
	// required keys from each predecessor's output.
	InputSchema datatypes.JSON `gorm:"type:json" json:"input_schema,omitempty"`
	CreatedAt   time.Time      `gorm:"not null"`
	UpdatedAt   time.Time      `gorm:"not null"`
}

type Tasks []*Task
