package models

import (
	"time"

	"github.com/google/uuid"
)

type ExecutionEvent struct {
	Sequence           uint64     `gorm:"primaryKey;autoIncrement" json:"sequence"`
	Type               string     `gorm:"type:text;index;not null" json:"type"`
	JobID              *uuid.UUID `gorm:"type:uuid;index" json:"job_id,omitempty"`
	RunID              *uuid.UUID `gorm:"type:uuid;index" json:"run_id,omitempty"`
	TaskID             *uuid.UUID `gorm:"type:uuid;index" json:"task_id,omitempty"`
	Payload            []byte     `gorm:"type:json" json:"payload,omitempty"`
	BusDispatchPending bool       `gorm:"not null;default:false;index" json:"bus_dispatch_pending"`
	BusDispatchedAt    *time.Time `gorm:"index" json:"bus_dispatched_at,omitempty"`
	CreatedAt          time.Time  `gorm:"not null;index" json:"created_at"`
}
