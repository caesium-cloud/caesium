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

	// RunID is the run this decision points at: the run it derived for a
	// `derived` decision, or — for `skipped_active_run` — the already-running run
	// that is covering this dataset, so "why didn't this derive" answers itself.
	// Nil for every other skip. A soft reference (no FK) so run pruning never
	// cascades into the audit trail.
	RunID *uuid.UUID `gorm:"type:uuid;index" json:"run_id,omitempty"`

	// ProducerJobID and JobNamespace record which producer the decision was
	// about and that job's tenancy namespace at decision time (for a derived
	// run, the run's persisted namespace), so derivation history is owned by
	// the namespace that made the decision even after the job moves
	// (identity-and-access B6). Most skips have no run, so the run cannot
	// stand in for them. Both are NULL on rows written before B6: a reader
	// treats a row as legacy only when BOTH are NULL. They are not serialised:
	// the response shape is unchanged for every caller.
	ProducerJobID *uuid.UUID `gorm:"type:uuid;index" json:"-"`
	JobNamespace  *string    `gorm:"type:text;index" json:"-"`

	// CreatedAt is the decision time (append-only; rows are never updated).
	CreatedAt time.Time `gorm:"not null;index:idx_dataset_deriv_created" json:"created_at"`
}
