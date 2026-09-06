package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Task struct {
	// ID, JobID and AtomID (and CreatedAt/UpdatedAt below) carry explicit
	// snake_case json tags so GET /v1/jobs/:id/tasks — which serialises this
	// model directly — matches every other endpoint's casing. Without them Go
	// emitted the Go field names ("ID", "JobID", "AtomID", …), which every
	// consumer had to shim.
	ID           uuid.UUID         `gorm:"type:uuid;primaryKey" json:"id"`
	JobID        uuid.UUID         `gorm:"type:uuid;index;not null" json:"job_id"`
	Job          Job               `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	AtomID       uuid.UUID         `gorm:"type:uuid;index;not null" json:"atom_id"`
	Atom         Atom              `gorm:"constraint:OnDelete:RESTRICT" json:"-"`
	Name         string            `gorm:"type:text;not null;default:''" json:"name"`
	Position     int               `gorm:"not null;default:0" json:"-"`
	Type         string            `gorm:"type:text;not null;default:'task'" json:"type"`
	NodeSelector datatypes.JSONMap `gorm:"type:json" json:"node_selector,omitempty"`
	Retries      int               `gorm:"not null;default:0" json:"retries"`
	RetryDelay   time.Duration     `gorm:"not null;default:0" json:"retry_delay"`
	RetryBackoff bool              `gorm:"not null;default:false" json:"retry_backoff"`
	TriggerRule  string            `gorm:"type:text;not null;default:'all_success'" json:"trigger_rule"`
	ReplaySafe   bool              `gorm:"not null;default:false" json:"replay_safe"`
	// RateLimitResource and RateLimitUnits carry step scheduling metadata from
	// the job definition into the durable task catalog.
	RateLimitResource string `gorm:"type:text;not null;default:''" json:"rate_limit_resource,omitempty"`
	RateLimitUnits    int    `gorm:"not null;default:0" json:"rate_limit_units,omitempty"`
	// FanOutConfig carries step scheduling metadata from the job definition
	// into the durable task catalog. It is JSON of pkg/jobdef.FanOut and is
	// deliberately excluded from the cache identity hash.
	FanOutConfig datatypes.JSON `gorm:"type:json" json:"fan_out_config,omitempty"`
	CacheConfig  datatypes.JSON `gorm:"type:json" json:"cache_config,omitempty"`
	// OutputSchema is a JSON Schema describing this task's expected output keys.
	OutputSchema datatypes.JSON `gorm:"type:json" json:"output_schema,omitempty"`
	// InputSchema maps predecessor task names to JSON Schema fragments describing
	// required keys from each predecessor's output.
	InputSchema datatypes.JSON `gorm:"type:json" json:"input_schema,omitempty"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt   time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"not null" json:"updated_at"`
}

type Tasks []*Task
