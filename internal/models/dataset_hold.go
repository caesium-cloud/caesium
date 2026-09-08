package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// DatasetHold status values.
const (
	// DatasetHoldStatusActive is an open hold: the dataset is broken, and every
	// downstream consumer admitted while it is set is skipped with reason.
	DatasetHoldStatusActive = "active"
	// DatasetHoldStatusReleased is a closed hold, kept for history.
	DatasetHoldStatusReleased = "released"
)

// DatasetHold release reasons.
const (
	// DatasetHoldReleaseCleanRun is the automatic release: the holder job
	// re-produced the dataset and every declared assertion passed. Only applies
	// to a dataset declared `release: auto` (the default).
	DatasetHoldReleaseCleanRun = "clean_run"
	// DatasetHoldReleaseManualAck is a human acknowledgement through
	// POST /v1/datasets/holds/:id/release.
	DatasetHoldReleaseManualAck = "manual_ack"
)

// DatasetHold is the circuit breaker's state: one row per time a declared
// dataset broke its data contract under `onViolation: hold`
// (docs/design-data-circuit-breaker.md "The hold model").
//
// The producing task SUCCEEDS — the work is done, and failing it would only
// invite a retry of the same data — and the DATASET is held instead. While a
// hold is active, every run of a job that declares the dataset under
// `datasets.consumes` is admitted straight to terminal `skipped`
// (run.Store.admit), so bad data stops at the boundary instead of propagating.
//
// # One active hold per dataset
//
// ActiveKey holds "<namespace>/<name>" while Status is active and is set NULL
// on release. Its unique index therefore constrains only active rows
// (dqlite/SQLite and Postgres both ignore NULLs in a unique index), which makes
// hold-open an ATOMIC conditional insert with ON CONFLICT DO NOTHING — exactly
// the shape incident.Store.OpenOrAppend uses, and for exactly the same reason:
// a select-then-insert is not correct under READ COMMITTED (Postgres) and would
// open twins under concurrent fanned verdicts. Repeat violations increment
// OccurrenceCount instead of opening a second hold, which is what makes
// alert-once structural rather than a notification-side heuristic: `dataset_held`
// is emitted only by the insert that wins.
//
// A fanned producer evaluates its contract per partition (design Open Question
// 5), so N breaching partitions produce N verdicts that collapse into ONE hold
// with N occurrences.
//
// It is a low-volume catalog table — written at most once per dataset break,
// never on the dispatch path — so it is deliberately absent from
// pkg/db hotPathModels()/hotTables, like DatasetMetric and Incident.
type DatasetHold struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// Namespace/Name are the dataset identity, matching DatasetDeclaration and
	// DatasetMetric. Namespace is unused in v1 and stored as the empty string
	// (NOT nullable here, because it participates in the active-hold key and in
	// equality joins against both sibling tables).
	Namespace string `gorm:"type:text;not null;default:'';index:idx_dataset_hold_identity,priority:1" json:"namespace"`
	Name      string `gorm:"type:text;not null;index:idx_dataset_hold_identity,priority:2" json:"name"`

	// Status is DatasetHoldStatusActive or DatasetHoldStatusReleased.
	Status string `gorm:"type:text;not null;index" json:"status"`

	// ActiveKey is the partial-unique guard described above: DatasetHoldKey()
	// while active, NULL once released.
	ActiveKey *string `gorm:"type:text;uniqueIndex:idx_dataset_hold_active" json:"-"`

	// PartitionKey is reserved (design open question 4): a future per-partition
	// hold would key on it and widen ActiveKey to include it. Always NULL in v1
	// — a fanned producer's N verdicts collapse into one dataset-level hold.
	PartitionKey *string `gorm:"type:text" json:"partition_key,omitempty"`

	// Tenant is reserved for multi-tenant deployments (design open question 3),
	// nullable from day one so adding it later needs no migration rewrite.
	Tenant *string `gorm:"type:text;index" json:"tenant,omitempty"`

	// Reason is the bounded assertion kind that opened the hold (run.Assertion*:
	// min, max, deltaFromBaseline, maxLag, missing). It is the label
	// caesium_dataset_holds_total is counted by, so it must stay bounded.
	Reason string `gorm:"type:text;not null;default:''" json:"reason"`

	// HeldBy* record who broke it. HeldByJobID is the producing job — the one
	// whose clean run may auto-release the hold (see the multi-producer answer
	// on run.releaseHoldsForCleanRun). The run/task refs are soft (no FK) so a
	// pruned run does not delete the breaker's state.
	HeldByJobID     uuid.UUID  `gorm:"type:uuid;index;not null" json:"held_by_job_id"`
	HeldByJobAlias  string     `gorm:"type:text;not null;default:''" json:"held_by_job_alias,omitempty"`
	HeldByRunID     *uuid.UUID `gorm:"type:uuid;index" json:"held_by_run_id,omitempty"`
	HeldByTaskID    *uuid.UUID `gorm:"type:uuid" json:"held_by_task_id,omitempty"`
	HeldByTaskRunID *uuid.UUID `gorm:"type:uuid" json:"held_by_task_run_id,omitempty"`
	HeldByStepName  string     `gorm:"type:text;not null;default:''" json:"held_by_step,omitempty"`

	// Violations is the marshalled []run.DataViolation of the MOST RECENT breach
	// folded into this hold (the opening one until an occurrence appends).
	// Keeping the latest mirrors incident.Store.appendOccurrence's `last_error`
	// and answers the question an operator actually asks — "what is wrong with
	// it now?" — while the ORIGINAL evidence is preserved unedited in the
	// dataset_held event's payload and in each contributing run's
	// TaskRun.DataViolations.
	//
	// It carries the assertion, the observed value, the breached bound AND the
	// baseline snapshot (median + sample count), because Stream F's incident
	// bundle serves this verbatim to the agent — a second read would be a second
	// point of failure and could observe a moved baseline.
	Violations datatypes.JSON `gorm:"type:json" json:"violations,omitempty"`

	// Impact is the marshalled lineage.ImpactResult blast radius as of the
	// moment the hold opened: every dataset transitively downstream of this one.
	// Empty when observed lineage records nothing for the declared name (the
	// common case without OpenLineage enabled) — the hold is still correct, it
	// just cannot show a cone.
	Impact datatypes.JSON `gorm:"type:json" json:"impact,omitempty"`

	// OccurrenceCount counts distinct breaches folded into this hold (the first
	// open is occurrence 1). Appends deliberately do NOT re-emit dataset_held.
	OccurrenceCount int `gorm:"not null;default:1" json:"occurrence_count"`

	// LastBreachAt / LastBreachRunID track the MOST RECENT breach folded into
	// this hold, as distinct from HeldBy*, which stay pinned to the first open.
	//
	// The clean-run release reads these, not OpenedAt: releasing evidence must
	// postdate the LATEST breach, or a run that started before an occurrence was
	// appended could clear a hold on the strength of data observed earlier than
	// the breach it is supposed to disprove. HeldByRunID stays the opener so
	// run.cleanSampleQuery can keep excluding the opening run's samples — those
	// were written just BEFORE OpenedAt, so a purely time-based window would
	// miss them.
	LastBreachAt    time.Time  `gorm:"not null;index" json:"last_breach_at"`
	LastBreachRunID *uuid.UUID `gorm:"type:uuid;index" json:"last_breach_run_id,omitempty"`

	// Tolerances records the per-assertion tolerance windows a human ack passed
	// (`--tolerate <assertion>=<duration>`), marshalled as
	// {"<assertion>": "<duration>"}. It is advisory evidence on the release, not
	// an enforcement mechanism in v1.
	Tolerances datatypes.JSON `gorm:"type:json" json:"tolerances,omitempty"`

	OpenedAt time.Time `gorm:"not null;index" json:"opened_at"`

	// Released* are set exactly once, by whichever release path wins.
	// ReleasedBy always records an AUTHENTICATED principal for a manual ack (the
	// endpoint is refused outright when no auth mode is active) or the literal
	// "system" for a clean-run release.
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
	ReleasedBy    string     `gorm:"type:text;not null;default:''" json:"released_by,omitempty"`
	ReleaseReason string     `gorm:"type:text;not null;default:''" json:"release_reason,omitempty"`
	ReleaseNote   string     `gorm:"type:text;not null;default:''" json:"release_note,omitempty"`
	// ReleaseRunID is the clearing run for a clean-run release. It also exempts
	// that run's own samples from run.cleanSampleQuery's non-held filter — the
	// releasing run is the evidence the dataset recovered, so its samples are
	// legitimate baseline history even though they were emitted while the hold
	// was still (momentarily) active.
	ReleaseRunID *uuid.UUID `gorm:"type:uuid;index" json:"release_run_id,omitempty"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null" json:"updated_at"`
}

// DatasetHoldKey renders the active-hold uniqueness key for a dataset. It is
// also the human-readable identity in a skipped run's SkipReason
// ("dataset_hold:<ns>/<name>"), so both sides read one function.
func DatasetHoldKey(namespace, name string) string {
	return namespace + "/" + name
}
