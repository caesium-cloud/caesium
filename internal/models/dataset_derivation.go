package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// Dataset derivation decision values — the "why did/didn't this run" audit the
// leader-gated evaluator (Stream C) appends. A `derived` decision carries the
// resulting run id; the `skipped_*` decisions record why no run was started.
const (
	DatasetDecisionDerived          = "derived"
	DatasetDecisionSkippedFresh     = "skipped_fresh"
	DatasetDecisionSkippedUpstream  = "skipped_upstream"
	DatasetDecisionSkippedAdmission = "skipped_admission"
	DatasetDecisionSkippedActiveRun = "skipped_active_run"
)

// DatasetDerivation is the append-only audit of every evaluator decision for a
// dataset: whether it derived a run, and if not, why. It is the row behind the
// "why did/didn't this run" operator surface. Never updated — one row per
// decision. Not a hot per-run table.
//
// The consumed-watermark snapshot records what the decision saw for the
// dataset's inputs, so a later reader can reconstruct the derivation without
// re-deriving state. RunID is the resulting run for a `derived` decision and
// nil for a skip.
type DatasetDerivation struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// Namespace is nullable (unused in v1); Name is the dataset identity.
	Namespace *string `gorm:"type:text;index:idx_dataset_deriv_identity,priority:1" json:"namespace,omitempty"`
	Name      string  `gorm:"type:text;not null;index:idx_dataset_deriv_identity,priority:2" json:"name"`

	// Decision is one of DatasetDecision* above.
	Decision string `gorm:"type:text;not null" json:"decision"`

	// Reason is a human-readable elaboration of the decision (e.g. "fresh
	// (2h/6h)", "waiting on raw.vendor_x"). Empty when the decision is
	// self-explanatory.
	Reason string `gorm:"type:text;not null;default:''" json:"reason,omitempty"`

	// ConsumedWatermarks snapshots the consumed dataset watermarks the decision
	// evaluated against, as a JSON object keyed by consumed dataset name.
	ConsumedWatermarks datatypes.JSON `gorm:"type:json" json:"consumed_watermarks,omitempty"`

	// RunID is the derived run for a `derived` decision; nil for a skip. A soft
	// reference (no FK) so run pruning never cascades into the audit trail.
	RunID *uuid.UUID `gorm:"type:uuid;index" json:"run_id,omitempty"`

	// CreatedAt is the decision time (append-only; rows are never updated).
	CreatedAt time.Time `gorm:"not null;index:idx_dataset_deriv_created" json:"created_at"`
}
