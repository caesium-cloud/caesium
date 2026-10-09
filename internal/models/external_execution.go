package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// ExternalExecution is one opaque external execution identity.
//
// The primary key is the digest from connector.ExecutionReference.OpaqueID.
// Provider coordinates stay in Coordinates. This table has no workflow id,
// run id, or namespace column, and no foreign key to job_runs or task_runs.
//
// It is catalog metadata, not a hot execution table: low volume, absent from
// pkg/db hotPathModels()/hotTables, like DatasetHold and DatasetMetric.
// Recording a snapshot does not set Referenced. A verified relation or an
// admitted operation does. Retention may delete unreferenced snapshots; it
// never deletes this row.
//
// CASVersion plus the Latest* fields are the compare-and-swap token for an
// append-only snapshot. An advance updates this row and inserts a new
// snapshot. It does not update the previous snapshot.
type ExternalExecution struct {
	OpaqueID     string         `gorm:"type:text;primaryKey" json:"opaque_id"`
	ConnectionID string         `gorm:"type:text;not null;index" json:"connection_id"`
	Coordinates  datatypes.JSON `gorm:"type:json;not null" json:"coordinates"`
	Referenced   bool           `gorm:"not null" json:"referenced"`

	// CASVersion increments only when a new snapshot row is inserted.
	CASVersion           int64      `gorm:"not null" json:"-"`
	LatestGeneration     *int64     `json:"-"`
	LatestEvidenceDigest string     `gorm:"type:text;not null" json:"-"`
	LatestTerminal       bool       `gorm:"not null" json:"-"`
	LatestSourceKind     string     `gorm:"type:text;not null" json:"-"`
	LatestObservedAt     *time.Time `json:"-"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}

// TableName is the catalog table. Explicit so GORM does not pluralize it
// into a hot-path name.
func (ExternalExecution) TableName() string { return "external_executions" }

// ExternalExecutionSnapshot is one append-only direct or discovery observation.
// Rows are inserted, never updated. Unreferenced rows may be evicted; a
// snapshot whose identity is referenced is kept.
type ExternalExecutionSnapshot struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	OpaqueID       string         `gorm:"type:text;not null;index" json:"opaque_id"`
	ConnectionID   string         `gorm:"type:text;not null;index:idx_external_snapshot_retention,priority:1" json:"connection_id"`
	Generation     int64          `gorm:"not null" json:"generation"`
	SourceEventID  string         `gorm:"type:text;not null" json:"source_event_id"`
	NativeStatus   string         `gorm:"type:text;not null" json:"native_status"`
	DisplayStatus  string         `gorm:"type:text;not null" json:"display_status"`
	Availability   string         `gorm:"type:text;not null" json:"availability"`
	Completeness   string         `gorm:"type:text;not null" json:"completeness"`
	Metadata       datatypes.JSON `gorm:"type:json" json:"metadata,omitempty"`
	SourceKind     string         `gorm:"type:text;not null" json:"source_kind"`
	Terminal       bool           `gorm:"not null" json:"terminal"`
	EvidenceDigest string         `gorm:"type:text;not null" json:"-"`
	ObservedAt     time.Time      `gorm:"not null;index:idx_external_snapshot_retention,priority:2" json:"observed_at"`
	CreatedAt      time.Time      `gorm:"not null" json:"created_at"`
}

// TableName is the catalog snapshot table.
func (ExternalExecutionSnapshot) TableName() string { return "external_execution_snapshots" }

// ExternalExecutionRelation is one piece of typed relation evidence between
// two identities that already exist. The same from/to/type with the same
// evidence digest is one row. A different digest inserts another row.
// There is no foreign key to job_runs or task_runs.
type ExternalExecutionRelation struct {
	ID             uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	FromOpaqueID   string         `gorm:"type:text;not null;uniqueIndex:idx_external_relation_evidence,priority:1" json:"from_opaque_id"`
	ToOpaqueID     string         `gorm:"type:text;not null;uniqueIndex:idx_external_relation_evidence,priority:2" json:"to_opaque_id"`
	RelationType   string         `gorm:"column:relation_type;type:text;not null;uniqueIndex:idx_external_relation_evidence,priority:3" json:"relation_type"`
	Evidence       datatypes.JSON `gorm:"type:json" json:"evidence,omitempty"`
	EvidenceDigest string         `gorm:"type:text;not null;uniqueIndex:idx_external_relation_evidence,priority:4" json:"-"`
	CreatedAt      time.Time      `gorm:"not null" json:"created_at"`
}

// TableName is the catalog relation table.
func (ExternalExecutionRelation) TableName() string { return "external_execution_relations" }
