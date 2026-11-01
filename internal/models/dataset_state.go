package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Dataset freshness status values. The state store (internal/freshness) sets a
// neutral default; the leader-gated evaluator (Stream C) owns the transitions
// between fresh/stale/stale-upstream/violated/quarantined/held.
const (
	DatasetStatusUnknown       = "unknown"
	DatasetStatusFresh         = "fresh"
	DatasetStatusStale         = "stale"
	DatasetStatusStaleUpstream = "stale-upstream"
	DatasetStatusViolated      = "violated"
	DatasetStatusQuarantined   = "quarantined"
	// DatasetStatusHeld means the data circuit breaker has an ACTIVE DatasetHold
	// on the dataset: its latest partition broke a declared assertion, so it is
	// recent but untrustworthy and must never satisfy a freshness trigger or
	// advance downstream scheduling (docs/design/data-circuit-breaker.md,
	// "Interplay"). It is its own value rather than an overload of an existing
	// one: Violated means a freshness SLO breach (and fires the violation
	// notification), Stale invites a derivation, StaleUpstream describes a
	// CONSUMER waiting on a held input, Unknown means "never observed", and
	// Quarantined means a non-authoritative replay run. The evaluator sets it
	// only while CAESIUM_DATA_ASSERTIONS_ENABLED is on; the reason names the
	// hold ("dataset_hold:<namespace>/<name> hold=<id>"), and the next
	// evaluation after release restores the computed status.
	DatasetStatusHeld = "held"
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

	// Namespace is reserved for cross-instance datasets (unused in v1) and Name
	// is the dataset identity the whole feature keys on. Together they are the
	// natural key, enforced by a real UNIQUE index — Namespace is NOT NULL with a
	// '' default (rather than nullable) precisely so the unique index is reliable
	// under SQLite, which treats NULLs as distinct in a UNIQUE index and would
	// otherwise let two rows share one dataset identity. The store maps a nil
	// namespace to '' on every query.
	Namespace string `gorm:"type:text;not null;default:'';uniqueIndex:idx_dataset_state_identity,priority:1" json:"namespace,omitempty"`
	Name      string `gorm:"type:text;not null;uniqueIndex:idx_dataset_state_identity,priority:2" json:"name"`

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

	// ReasonNamespace is the tenancy namespace of the producer whose
	// evaluation wrote Reason (identity-and-access B6). The reason is the
	// evaluator's elaboration for ONE producer and can name that producer's
	// inputs, so a namespace-confined reader sees it only when this namespace
	// is one it may read. NULL (rows written before B6, or a producer that
	// could not be resolved) hides the reason from every confined reader. Not
	// serialised.
	ReasonNamespace *string `gorm:"type:text" json:"-"`

	// LastRunID is the run that most recently advanced or verified this dataset.
	// It is a soft reference (no FK constraint) so run pruning never cascades
	// into dataset state.
	LastRunID *uuid.UUID `gorm:"type:uuid;index" json:"last_run_id,omitempty"`

	// ConsumedWatermarks snapshots the watermarks of this dataset's declared
	// inputs as LastRunID STARTED — the view that run actually consumed, not the
	// values current when it finished — so "is my output up to date with my
	// inputs" is a pure row comparison, not a heuristic. JSON object keyed by
	// consumed dataset name. See internal/freshness.Capturer.consumedForRun for
	// the three sources of that start-time view.
	ConsumedWatermarks datatypes.JSON `gorm:"type:json" json:"consumed_watermarks,omitempty"`

	// ConsumedRunID is the run whose start-time view ConsumedWatermarks is
	// (identity-and-access B6). It usually equals LastRunID, but an accepted
	// observation with no input snapshot (a manual advance, or a run of a
	// producer that no longer declares inputs) moves LastRunID and leaves the
	// snapshot in place, so the snapshot's owner is recorded on its own: a
	// namespace-confined reader sees the snapshot only when this run is
	// persisted in a namespace it may read. NULL (written before B6, or by a
	// run-less writer) hides it from every confined reader. Not serialised.
	ConsumedRunID *uuid.UUID `gorm:"type:uuid" json:"-"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}
