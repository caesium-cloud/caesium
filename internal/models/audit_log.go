package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// AuditLog records every state-changing operation and auth rejection.
type AuditLog struct {
	ID           uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Timestamp    time.Time      `gorm:"not null;index" json:"timestamp"`
	Actor        string         `gorm:"type:text;not null;index" json:"actor"`
	Action       string         `gorm:"type:text;not null;index" json:"action"`
	ResourceType string         `gorm:"type:text" json:"resource_type,omitempty"`
	ResourceID   string         `gorm:"type:text" json:"resource_id,omitempty"`
	SourceIP     string         `gorm:"type:text" json:"source_ip,omitempty"`
	Outcome      string         `gorm:"type:text;not null" json:"outcome"`
	Metadata     datatypes.JSON `gorm:"type:json" json:"metadata,omitempty"`
}
