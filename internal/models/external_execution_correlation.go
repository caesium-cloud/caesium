package models

import (
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"time"
)

// ExternalExecutionCorrelation is a verified delegation to an existing local
// admission, not another external execution. Soft references keep this catalog
// metadata separate from hot run tables. One activity retains its admission and
// queue identity when promoted; no synthetic JobRun or ExternalExecution is made.
type ExternalExecutionCorrelation struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	OpaqueID       string         `gorm:"type:text;not null;uniqueIndex:idx_external_correlation_activity,priority:1" json:"opaque_id"`
	ConnectionID   string         `gorm:"type:text;not null;index" json:"connection_id"`
	ActivityID     string         `gorm:"type:text;not null;uniqueIndex:idx_external_correlation_activity,priority:2" json:"activity_id"`
	ActivityType   string         `gorm:"type:text;not null" json:"activity_type"`
	RelationType   string         `gorm:"type:text;not null" json:"relation_type"`
	BindingVersion string         `gorm:"type:text;not null" json:"binding_version"`
	AdmissionID    uuid.UUID      `gorm:"type:uuid;not null;index" json:"admission_id"`
	JobID          uuid.UUID      `gorm:"type:uuid;not null;index" json:"job_id"`
	IdempotencyKey string         `gorm:"type:text;not null" json:"idempotency_key"`
	Outcome        string         `gorm:"type:text;not null" json:"outcome"`
	QueueID        *uuid.UUID     `gorm:"type:uuid" json:"queue_id,omitempty"`
	RunID          *uuid.UUID     `gorm:"type:uuid;index" json:"run_id,omitempty"`
	SourceEventID  int64          `gorm:"not null" json:"source_event_id"`
	Evidence       datatypes.JSON `gorm:"type:json;not null" json:"evidence"`
	EvidenceDigest string         `gorm:"type:text;not null" json:"-"`
	VerifiedAt     time.Time      `gorm:"not null" json:"verified_at"`
	CreatedAt      time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt      time.Time      `gorm:"not null" json:"updated_at"`
}

func (ExternalExecutionCorrelation) TableName() string { return "external_execution_correlations" }
