package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type RunQueue struct {
	ID        uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	JobID     uuid.UUID      `gorm:"type:uuid;not null;index:idx_run_queue_job_priority_created,priority:1" json:"job_id"`
	Job       Job            `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Params    datatypes.JSON `gorm:"type:json" json:"params,omitempty"`
	Priority  int            `gorm:"not null;default:2;index:idx_run_queue_job_priority_created,priority:2,sort:desc" json:"priority"`
	ClaimedBy string         `gorm:"type:text;not null;default:'';index" json:"claimed_by"`
	ClaimedAt *time.Time     `gorm:"index" json:"claimed_at,omitempty"`
	CreatedAt time.Time      `gorm:"not null;index:idx_run_queue_job_priority_created,priority:3,sort:asc" json:"created_at"`
}
