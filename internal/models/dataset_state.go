package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Dataset freshness status values. The state store (internal/freshness) sets a
// neutral default; the leader-gated evaluator (Stream C) owns the transitions
// between fresh/stale/stale-upstream/violated/quarantined.
const (
	DatasetStatusUnknown       = "unknown"
	DatasetStatusFresh         = "fresh"
	DatasetStatusStale         = "stale"
	DatasetStatusStaleUpstream = "stale-upstream"
	DatasetStatusViolated      = "violated"
	DatasetStatusQuarantined   = "quarantined"
)

// DatasetState is the durable truth every scheduling decision reads: one row per
// dataset (natural key namespace+name). It distinguishes "a run succeeded" from
// "the output advanced" — Watermark/AdvancedAt move only on a real watermark
// change (see internal/freshness.Store.Advance), while VerifiedAt records a
// successful run that merely confirmed the current value. Freshness is evaluated
// against max(AdvancedAt, VerifiedAt).
//
// Namespace is nullable and unused in v1 (mirrors DatasetDeclaration); dataset
// identity keys on Name today. This is NOT a hot per-run table: it is written at
// run completion and by the evaluator, not on the per-task hot path.
type DatasetState struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// Namespace is nullable (reserved for cross-instance datasets, unused in v1).
	// Name is the dataset identity the whole feature keys on. The (namespace,
	// name) pair is the natural key; the store find-or-creates on it (SQLite
	// treats NULLs as distinct in a UNIQUE index, so the index below documents
	// intent while the store enforces single-row-per-dataset in a transaction).
	Namespace *string `gorm:"type:text;index:idx_dataset_state_identity,priority:1" json:"namespace,omitempty"`
	Name      string  `gorm:"type:text;not null;index:idx_dataset_state_identity,priority:2" json:"name"`

	// Watermark is the current high-water value emitted by the producing step
	// (or an arrival binding). Empty until the dataset first advances.
	Watermark string `gorm:"type:text;not null;default:''" json:"watermark"`

	// WatermarkRunAt orders the run that set the current Watermark. It gates
	// opaque-string watermarks (git SHAs/UUIDs have no orderable relation): a
	// later-finishing older run must not clobber a newer value, so an opaque
	// write only overwrites when the incoming run is newer than this. Nullable
	// until the first advance.
	WatermarkRunAt *time.Time `json:"watermark_run_at,omitempty"`

	// AdvancedAt is the time the watermark last CHANGED (increased, for
	// orderable values). VerifiedAt is the time a successful run last CONFIRMED
	// the current watermark without changing it. Both nullable until observed.
	AdvancedAt *time.Time `json:"advanced_at,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`

	// Status / Reason are owned by the evaluator (Stream C). The state store
	// leaves them at their defaults.
	Status string `gorm:"type:text;not null;default:'unknown'" json:"status"`
	Reason string `gorm:"type:text;not null;default:''" json:"reason,omitempty"`

	// LastRunID is the run that most recently advanced or verified this dataset.
	// It is a soft reference (no FK constraint) so run pruning never cascades
	// into dataset state.
	LastRunID *uuid.UUID `gorm:"type:uuid;index" json:"last_run_id,omitempty"`

	// ConsumedWatermarks snapshots the watermarks of this dataset's declared
	// inputs at the time LastRunID produced it, so "is my output up to date with
	// my inputs" is a pure row comparison, not a heuristic. JSON object keyed by
	// consumed dataset name.
	ConsumedWatermarks datatypes.JSON `gorm:"type:json" json:"consumed_watermarks,omitempty"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}
