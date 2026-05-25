package models

import "time"

// RunCheckpoint persists a point-in-time snapshot of an owner's in-memory DAG
// state for a run (run-owner mode, CAESIUM_RUN_OWNER_ENABLED=true).  Combined
// with the terminal task_runs rows written after the checkpoint
// (terminal_sequence > sequence_high), it lets a new owner reconstruct exact
// run state after the previous owner crashes — recovery bounded by the
// checkpoint interval.
//
// Like task_runs, a checkpoint is per-run and transactionally local to its
// run's other rows: it lives in the catalog DB when unsharded, and in the run's
// hot shard when sharding is enabled (so it appears in both models.All and
// hotPathModels).
type RunCheckpoint struct {
	// RunID identifies the job run.  Stored as text per existing GORM
	// convention for run-owner tables.
	RunID string `gorm:"type:text;primaryKey;index:idx_run_checkpoint_run_seq,priority:1" json:"run_id"`

	// SequenceHigh is the highest terminal_sequence covered by this checkpoint.
	// Recovery applies the checkpoint, then layers task_runs rows with a strictly
	// greater terminal_sequence.  Part of the primary key so each run keeps a
	// history of checkpoints (pruned on archival).
	SequenceHigh int64 `gorm:"primaryKey;autoIncrement:false;index:idx_run_checkpoint_run_seq,priority:2" json:"sequence_high"`

	// OwnerGeneration is the RunLease.Generation of the writing owner — a fence
	// column so a stale owner's checkpoint can be distinguished from the current
	// owner's during a takeover race.
	OwnerGeneration int64 `gorm:"not null" json:"owner_generation"`

	// StateBlob is the serialized RunState snapshot (or delta).  Encoding is an
	// internal detail owned by the checkpoint writer/reader (JSON in v1); the
	// column is opaque bytes.
	StateBlob []byte `gorm:"type:blob;not null" json:"-"`

	// IsIncremental is false for a full snapshot and true for a delta containing
	// only the task states that changed since the prior checkpoint.  Recovery
	// walks back to the most recent full snapshot and applies deltas forward.
	IsIncremental bool `gorm:"not null;default:false" json:"is_incremental"`

	// CreatedAt is the wall-clock write time (diagnostics only; ordering uses
	// SequenceHigh, never timestamps).
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}
