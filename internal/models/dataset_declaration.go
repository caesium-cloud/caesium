package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Dataset declaration direction values. They mirror the jobdef constants
// (pkg/jobdef.DatasetDirection*) and are duplicated here so the models package
// stays free of a jobdef import.
const (
	DatasetDirectionProduces = "produces"
	DatasetDirectionConsumes = "consumes"
	DatasetDirectionSource   = "source"
)

// DatasetDeclaration is the persisted *declared* dataset graph — the
// complement to the *observed* LineageDataset graph (which requires
// OpenLineage). One row is written per (job, step, dataset, direction)
// relationship declared in a job's `datasets` surface, rebuilt from the
// manifest on every apply so a removed declaration is pruned. It stores only
// scheduling metadata (SLOs, watermark key, arrival binding); it never enters
// the cache identity and is not a hot per-run table.
//
// Dataset identity is keyed on Name in v1. Namespace is nullable and carried
// from day one (per the design's Non-goals) so cross-instance namespacing can
// be added later without a migration rewrite; it is not populated in v1.
type DatasetDeclaration struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// JobID links this declaration to its job. Rows are cascade-deleted when the
	// job is hard-deleted; the importer also rebuilds a job's declarations on
	// every apply and prunes them when the job is retired.
	JobID uuid.UUID `gorm:"type:uuid;not null;index:idx_dataset_decl_job" json:"job_id"`
	Job   Job       `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// JobAlias is denormalized so the cross-job lint can resolve producers and
	// build the derivation graph without joining back to jobs.
	JobAlias string `gorm:"type:text;not null;index:idx_dataset_decl_alias" json:"job_alias"`

	// StepName is the producing/consuming step. It is empty for a
	// metadata-level source declaration (direction=source).
	StepName string `gorm:"type:text;not null;default:''" json:"step_name"`

	// Namespace is nullable (reserved for cross-instance datasets, unused in v1).
	// Name is the dataset identity the whole feature keys on.
	Namespace *string `gorm:"type:text;index:idx_dataset_decl_identity,priority:1" json:"namespace,omitempty"`
	Name      string  `gorm:"type:text;not null;index:idx_dataset_decl_identity,priority:2" json:"name"`

	// Direction is one of DatasetDirectionProduces / Consumes / Source.
	Direction string `gorm:"type:text;not null;index:idx_dataset_decl_identity,priority:3" json:"direction"`

	// Freshness / MaxStaleness are the produced dataset's SLO (Go duration
	// strings). ExpectedEvery is the source dataset's cadence expectation. All
	// empty when not applicable to the direction.
	Freshness     string `gorm:"type:text;not null;default:''" json:"freshness,omitempty"`
	MaxStaleness  string `gorm:"type:text;not null;default:''" json:"max_staleness,omitempty"`
	ExpectedEvery string `gorm:"type:text;not null;default:''" json:"expected_every,omitempty"`

	// WatermarkKey is the ##caesium::output key a producing step emits to
	// advance the dataset. Empty in degraded mode (no declared watermark).
	WatermarkKey string `gorm:"type:text;not null;default:''" json:"watermark_key,omitempty"`

	// SkipWhenFresh carries metadata.datasets.skipWhenFresh after defaulting.
	// It is evaluated at the cron scheduling seam only and never affects a task's
	// cache identity. Pointer form preserves an explicit false across GORM's
	// default:true create path.
	SkipWhenFresh *bool `gorm:"not null;default:true" json:"skip_when_fresh,omitempty"`

	// External marks a source dataset nobody in Caesium produces.
	External bool `gorm:"not null;default:false" json:"external"`

	// ArrivalBinding is the source dataset's arrival event pattern + watermark
	// JSONPath, stored as JSON. Empty for produced/consumed declarations.
	ArrivalBinding datatypes.JSON `gorm:"type:json" json:"arrival_binding,omitempty"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}
