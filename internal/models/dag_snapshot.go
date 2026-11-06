package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// DagSnapshot is an append-only record of a job's full topology (tasks + edges)
// at a point in time. One row is written per apply when the topology or the
// job's namespace changes; otherwise the existing row is reused (dedup by
// ContentHash and Namespace against the latest row).
//
// This is the persistence layer for Component 3 (Version DAG topology) from
// docs/design/data-plane-memory.md. The live graph in task_edges still drives
// execution; dag_snapshots preserves history so "the pipeline as of commit X"
// is reconstructable from dqlite without a git checkout.
type DagSnapshot struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`

	// JobID links this snapshot to its job. Rows are cascade-deleted when the
	// job is hard-deleted; soft-deleted jobs retain their snapshots.
	// Part of the composite dedup index (job_id, content_hash), the
	// query-by-recency index (job_id, created_at) and the namespace index
	// (job_id, namespace).
	JobID uuid.UUID `gorm:"type:uuid;not null;index:idx_dag_snapshot_job_hash;index:idx_dag_snapshot_job_created;index:idx_dag_snapshot_job_namespace,priority:1;index:idx_dag_snapshot_job_sequence,priority:1" json:"job_id"`
	Job   Job       `gorm:"constraint:OnDelete:CASCADE" json:"-"`

	// Namespace is the namespace the job was in when this snapshot was written
	// (identity-and-access B7). A move writes a fresh snapshot in the new
	// namespace, so each owner's topology history stays with it. NULL marks a
	// legacy row written before the column existed: its owner cannot be
	// recovered, so it is shown only to unrestricted callers and never
	// back-filled. Not serialised, so unrestricted responses keep their shape.
	Namespace *string `gorm:"type:text;index:idx_dag_snapshot_job_namespace,priority:2" json:"-"`

	// ContentHash is a SHA-256 hex digest of the canonical topology (sorted
	// task names each with image+command, plus sorted from→to edge pairs).
	// It is the dedup key: if the most-recent snapshot for this job already
	// carries this hash, no new row is written.
	ContentHash string `gorm:"type:text;not null;index:idx_dag_snapshot_job_hash" json:"content_hash"`

	// GitCommit is the provenance commit SHA at apply time (empty when the
	// apply carries no provenance). Together with ContentHash it lets callers
	// identify "when did this topology first appear and from which commit."
	GitCommit string `gorm:"type:text;not null;default:''" json:"git_commit"`

	// Sequence is a per-job monotonic insertion ordinal (1, 2, ...) assigned
	// inside the apply transaction as max(sequence)+1, so it totals the order of
	// snapshots even when two share a CreatedAt (or the clock steps backwards).
	// Dqlite serialises writes through one Raft leader and the apply runs in a
	// transaction, so two concurrent applies cannot read the same maximum. Rows
	// written before the column existed are backfilled in (created_at, id) order
	// by db.MigrateDagSnapshotSequence; 0 means "not yet assigned" and sorts
	// before every assigned value. Nullable with no default tag on purpose
	// (dqlite re-tightens default-tagged columns on every AutoMigrate).
	Sequence int64 `gorm:"type:integer;index:idx_dag_snapshot_job_sequence,priority:2" json:"sequence"`

	// Author is the authenticated principal that applied this topology (API
	// applies) or the git source that synced it ("git-source:<id>"). NULL on
	// rows written before the column existed and on applies with no known actor:
	// historical authorship is unrecoverable and is never back-filled.
	Author *string `gorm:"type:text" json:"author,omitempty"`

	// Ref is the git ref (branch or tag) from the apply's provenance. NULL when
	// the apply carried none and on legacy rows (forward-only).
	Ref *string `gorm:"type:text" json:"ref,omitempty"`

	// Tasks is a JSON array of task descriptors (name, image, command, behavior) captured
	// at snapshot time. It is informational — the live task_runs rows remain
	// authoritative for execution.
	Tasks datatypes.JSON `gorm:"type:json;not null" json:"tasks"`

	// Edges is a JSON array of edge descriptors ({from, to, provenance_commit})
	// captured at snapshot time.
	Edges datatypes.JSON `gorm:"type:json;not null" json:"edges"`

	// CreatedAt is the wall-clock time the snapshot was written (append-only;
	// rows are never updated). Part of the idx_dag_snapshot_job_created index
	// so the dedup query (ORDER BY created_at DESC LIMIT 1) uses the index.
	CreatedAt time.Time `gorm:"not null;index:idx_dag_snapshot_job_created" json:"created_at"`
}

// DagSnapshotTask is the per-task descriptor stored inside DagSnapshot.Tasks.
// Command is stored as a slice (not space-joined) to preserve argument boundaries.
type DagSnapshotTask struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	// Behavior is the canonical JSON of every other behavior-affecting step
	// field (env, nodeSelector, retries, triggerRule, cache, inputSchema,
	// outputSchema, volumeMounts, kueue, rateLimit, fanOut, ...). It is nil on
	// rows written before the descriptor was broadened; every newly written row
	// carries it (at least "{}"), which is how blame tells "legacy row, fields
	// unknown" from "field unset". Env values are never stored in the clear: a
	// secret:// reference is kept verbatim and a literal is replaced by a
	// "sha256:<prefix>" digest.
	Behavior json.RawMessage `json:"behavior,omitempty"`
}

// DagSnapshotEdge is the per-edge descriptor stored inside DagSnapshot.Edges.
type DagSnapshotEdge struct {
	From             string `json:"from"`
	To               string `json:"to"`
	ProvenanceCommit string `json:"provenance_commit,omitempty"`
}
