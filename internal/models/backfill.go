package models

import (
	"time"

	"github.com/google/uuid"
)

type BackfillStatus string

const (
	BackfillStatusRunning   BackfillStatus = "running"
	BackfillStatusSucceeded BackfillStatus = "succeeded"
	BackfillStatusFailed    BackfillStatus = "failed"
	BackfillStatusCancelled BackfillStatus = "cancelled"
)

type ReprocessPolicy string

const (
	ReprocessNone   ReprocessPolicy = "none"
	ReprocessFailed ReprocessPolicy = "failed"
	ReprocessAll    ReprocessPolicy = "all"
)

type Backfill struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	JobID         uuid.UUID  `gorm:"type:uuid;index;not null" json:"job_id"`
	Job           Job        `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Status        string     `gorm:"type:text;not null" json:"status"`
	Start         time.Time  `gorm:"not null" json:"start"`
	End           time.Time  `gorm:"not null" json:"end"`
	MaxConcurrent int        `gorm:"not null;default:1" json:"max_concurrent"`
	Reprocess     string     `gorm:"type:text;not null" json:"reprocess"`
	TotalRuns     int        `gorm:"not null;default:0" json:"total_runs"`
	CompletedRuns int        `gorm:"not null;default:0" json:"completed_runs"`
	FailedRuns    int        `gorm:"not null;default:0" json:"failed_runs"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt     time.Time  `gorm:"not null" json:"updated_at"`
}
