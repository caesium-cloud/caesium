package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type EventTriggerMatch struct {
	ID          uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	EventID     uuid.UUID      `gorm:"type:uuid;index;not null" json:"event_id"`
	Event       IngestedEvent  `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	TriggerID   uuid.UUID      `gorm:"type:uuid;index;not null" json:"trigger_id"`
	MatchedAt   time.Time      `gorm:"not null;index" json:"matched_at"`
	RunsStarted datatypes.JSON `gorm:"type:json" json:"runs_started,omitempty"`
	Skipped     bool           `gorm:"not null;default:false" json:"skipped"`
	SkipReason  string         `gorm:"type:text" json:"skip_reason,omitempty"`
	Error       string         `gorm:"type:text" json:"error,omitempty"`
}
