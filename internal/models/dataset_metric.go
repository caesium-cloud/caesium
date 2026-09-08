package models

import (
	"time"

	"github.com/google/uuid"
)

// DatasetMetric is one metric sample a step self-reported for a declared
// dataset via the ##caesium::metrics marker (design-data-circuit-breaker.md
// "Data model"). It is the observation history the assertion evaluator asserts
// against and the rolling baseline is computed from — there is no materialized
// baseline table, because the window is at most CAESIUM_BASELINE_WINDOW small
// rows per metric.
//
// Rows are recorded for EVERY emitted metric, including metrics no assertion
// names yet: an assertion added later then has real history to seed its
// baseline from instead of starting cold. Replay-quarantined task runs record
// nothing at all — a what-if must never move a baseline.
//
// Like LineageDataset (the observed-lineage sibling) the row hangs off the
// TaskRun that emitted it with constraint:OnDelete:CASCADE, so metrics are
// pruned with their run. It is a catalog/observability table, deliberately NOT
// a hot per-run table: it is written once at task completion, never on the
// dispatch path, so it is absent from pkg/db hotPathModels().
//
// Identity is keyed on Name in v1. Namespace is carried from day one (empty
// string when unset, matching LineageDataset) so cross-instance namespacing can
// be added without a migration rewrite.
type DatasetMetric struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// TaskRunID is the FK into task_runs — the specific instance that emitted
	// the sample. A fanned step has N instances and therefore records N samples
	// per trigger, one per partition.
	TaskRunID uuid.UUID `gorm:"type:uuid;not null;index:idx_dataset_metric_task_run" json:"task_run_id"`
	TaskRun   TaskRun   `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// Namespace/Name are the dataset identity, matching DatasetDeclaration.
	// Namespace is unused in v1 and stored as the empty string.
	Namespace string `gorm:"type:text;not null;default:'';index:idx_dataset_metric_identity,priority:1" json:"namespace"`
	Name      string `gorm:"type:text;not null;index:idx_dataset_metric_identity,priority:2" json:"name"`

	// Metric is the emitted key (e.g. "rowCount", "null_rate_customer_id").
	Metric string `gorm:"type:text;not null;index:idx_dataset_metric_identity,priority:3" json:"metric"`

	// Value is the observed number. Timestamp-valued metrics (RFC3339
	// watermarks) are stored as epoch seconds so lag bounds and counts share
	// one numeric column — see pkg/task.DatasetMetricSample.
	Value float64 `gorm:"not null" json:"value"`

	// CreatedAt is the sample time and the ordering key for the rolling
	// baseline window, so it is the trailing column of the identity index.
	CreatedAt time.Time `gorm:"not null;index:idx_dataset_metric_identity,priority:4" json:"created_at"`
}
