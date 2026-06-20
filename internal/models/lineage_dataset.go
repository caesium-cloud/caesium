package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// LineageDataset stores a bounded reference to a dataset observed during a
// task run — a namespace+name identity plus small facet summaries.  Full
// facets are emitted out-of-process via the existing http transport; dqlite
// holds only what is needed for impact queries (references + digests).
//
// The natural key is (TaskRunID, Namespace, Name, Direction): a unique index
// enforces this at the DB level, and callers must upsert (OnConflict DoNothing
// or DoUpdates) rather than plain-insert to avoid accumulating unbounded rows
// when a task run emits the same dataset twice.  Records are deleted when the
// parent TaskRun is deleted (constraint:OnDelete:CASCADE).
type LineageDataset struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// TaskRunID is the FK into task_runs.  Records are deleted when the parent
	// task run is deleted.
	TaskRunID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_lineage_dataset_key,priority:1" json:"task_run_id"`
	TaskRun   TaskRun   `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// Namespace and Name form the OpenLineage dataset identity.
	Namespace string `gorm:"type:text;not null;uniqueIndex:idx_lineage_dataset_key,priority:2" json:"namespace"`
	Name      string `gorm:"type:text;not null;uniqueIndex:idx_lineage_dataset_key,priority:3" json:"name"`

	// Direction is "input" or "output" from the step's perspective.
	Direction string `gorm:"type:text;not null;uniqueIndex:idx_lineage_dataset_key,priority:4" json:"direction"`

	// FacetSummary is a bounded JSON object holding a digest + small facet
	// summary (step name, output keys, schema keys).  Full facets are emitted
	// via http transport — never stored here.
	FacetSummary datatypes.JSON `gorm:"type:json" json:"facet_summary,omitempty"`

	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}
