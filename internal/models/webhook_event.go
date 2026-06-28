package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type WebhookEvent struct {
	ID                   uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Path                 string         `gorm:"type:text;index;not null" json:"path"`
	Source               string         `gorm:"type:text;index" json:"source,omitempty"`
	ReceivedAt           time.Time      `gorm:"not null;index" json:"received_at"`
	Status               string         `gorm:"type:text;index;not null" json:"status"`
	EventID              uuid.UUID      `gorm:"type:uuid;index" json:"event_id,omitempty"`
	EventMatchedTriggers int            `gorm:"not null;default:0" json:"event_matched_triggers"`
	EventRunsStarted     int            `gorm:"not null;default:0" json:"event_runs_started"`
	HTTPTriggersAccepted int            `gorm:"not null;default:0" json:"http_triggers_accepted"`
	HTTPRunsStarted      int            `gorm:"not null;default:0" json:"http_runs_started"`
	HTTPTriggerIDs       datatypes.JSON `gorm:"type:json" json:"http_trigger_ids,omitempty"`
	HTTPJobIDs           datatypes.JSON `gorm:"type:json" json:"http_job_ids,omitempty"`
	AuthFailures         datatypes.JSON `gorm:"type:json" json:"auth_failures,omitempty"`
	Error                string         `gorm:"type:text" json:"error,omitempty"`
}
