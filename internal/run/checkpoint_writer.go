package run

import (
	"time"

	"github.com/google/uuid"
)

// checkpointPersister is the slice of *Store the writer depends on, named as an
// interface so tests can substitute a fake.
type checkpointPersister interface {
	WriteCheckpoint(runID uuid.UUID, sequenceHigh, ownerGeneration int64, blob []byte, incremental bool) error
	PruneCheckpoints(runID uuid.UUID, keepFulls int) error
}

// CheckpointConfig controls checkpoint cadence and retention.
type CheckpointConfig struct {
	Events    int           // checkpoint after this many new terminal transitions
	Interval  time.Duration // ...or this much elapsed since the last checkpoint, whichever first
	KeepFulls int           // full snapshots to retain when pruning
}

// CheckpointWriter persists a RunState snapshot on a cadence — whichever comes
// first of Events new terminal transitions or Interval elapsed — and prunes old
// checkpoints afterward.  One per owned run; the owner calls Maybe after
// applying completions and Force on graceful handoff/shutdown.  v1 always writes
// full snapshots (is_incremental=false); delta checkpoints are a later
// optimization.  Not safe for concurrent use.
type CheckpointWriter struct {
	store   checkpointPersister
	runID   uuid.UUID
	cfg     CheckpointConfig
	lastSeq int64
	lastAt  time.Time
	now     func() time.Time // injectable for tests
}

// NewCheckpointWriter builds a writer for runID; zero/negative config values
// fall back to the design defaults (100 events, 2s, keep 3 fulls).
func NewCheckpointWriter(store checkpointPersister, runID uuid.UUID, cfg CheckpointConfig) *CheckpointWriter {
	if cfg.Events <= 0 {
		cfg.Events = 100
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.KeepFulls <= 0 {
		cfg.KeepFulls = 3
	}
	w := &CheckpointWriter{store: store, runID: runID, cfg: cfg, now: time.Now}
	w.lastAt = w.now()
	return w
}

// due reports whether a checkpoint should be written at the current sequence.
func (w *CheckpointWriter) due(seq int64) bool {
	if seq <= w.lastSeq {
		return false // nothing new since the last checkpoint
	}
	if seq-w.lastSeq >= int64(w.cfg.Events) {
		return true
	}
	return w.now().Sub(w.lastAt) >= w.cfg.Interval
}

// Maybe writes a checkpoint when the cadence threshold is met, else no-ops.
func (w *CheckpointWriter) Maybe(rs *RunState, ownerGeneration int64) error {
	if !w.due(rs.Sequence()) {
		return nil
	}
	return w.Force(rs, ownerGeneration)
}

// Force writes a full checkpoint and prunes, regardless of cadence.
func (w *CheckpointWriter) Force(rs *RunState, ownerGeneration int64) error {
	blob, err := rs.Snapshot()
	if err != nil {
		return err
	}
	seq := rs.Sequence()
	if err := w.store.WriteCheckpoint(w.runID, seq, ownerGeneration, blob, false); err != nil {
		return err
	}
	w.lastSeq = seq
	w.lastAt = w.now()
	return w.store.PruneCheckpoints(w.runID, w.cfg.KeepFulls)
}
