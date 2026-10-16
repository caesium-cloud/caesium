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
	// Namespace is the tenancy namespace the audited decision was about: "*"
	// for a cluster route or a decision spanning several namespaces, "" for an
	// identity route or when it could not be resolved, the namespace
	// otherwise. Rows written before the column existed are NULL. Nullable and
	// deliberately without a default: tag (dqlite re-tightens default-tagged
	// columns on every AutoMigrate).
	Namespace string `gorm:"type:text;index" json:"namespace,omitempty"`
}
