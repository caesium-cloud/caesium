package run

import (
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WriteCheckpoint persists a run_checkpoints row for runID.  sequenceHigh is the
// highest terminal_sequence the snapshot covers; ownerGeneration fences the
// write against a stale owner; blob is the serialized state (RunState.Snapshot);
// incremental marks a delta vs a full snapshot.  Re-writing the same
// (run_id, sequence_high) overwrites — checkpoints are idempotent at a sequence.
func (s *Store) WriteCheckpoint(runID uuid.UUID, sequenceHigh, ownerGeneration int64, blob []byte, incremental bool) error {
	cp := &models.RunCheckpoint{
		RunID:           runID.String(),
		SequenceHigh:    sequenceHigh,
		OwnerGeneration: ownerGeneration,
		StateBlob:       blob,
		IsIncremental:   incremental,
		CreatedAt:       time.Now().UTC(),
	}
	return withStoreBusyRetry(func() error {
		return s.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "run_id"}, {Name: "sequence_high"}},
			UpdateAll: true,
		}).Create(cp).Error
	})
}

// LatestFullCheckpoint returns the highest-sequence full (non-incremental)
// snapshot for runID, or (nil, nil) when the run has no checkpoint yet.
func (s *Store) LatestFullCheckpoint(runID uuid.UUID) (*models.RunCheckpoint, error) {
	var cp models.RunCheckpoint
	err := s.db.
		Where("run_id = ? AND is_incremental = ?", runID.String(), false).
		Order("sequence_high DESC").
		First(&cp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cp, nil
}

// CheckpointDeltasSince returns the incremental checkpoints with sequence_high
// strictly greater than fromSeq, ascending, so recovery can apply them in order
// after the most recent full snapshot.  (v1 writes only full snapshots, so this
// normally returns empty; it exists so the recovery path is delta-ready.)
func (s *Store) CheckpointDeltasSince(runID uuid.UUID, fromSeq int64) ([]models.RunCheckpoint, error) {
	var deltas []models.RunCheckpoint
	err := s.db.
		Where("run_id = ? AND is_incremental = ? AND sequence_high > ?", runID.String(), true, fromSeq).
		Order("sequence_high ASC").
		Find(&deltas).Error
	return deltas, err
}

// PruneCheckpoints retains the most recent keepFulls full snapshots for runID
// (and every checkpoint at or after the oldest retained full's sequence),
// deleting anything older.  A no-op until more than keepFulls fulls exist.
func (s *Store) PruneCheckpoints(runID uuid.UUID, keepFulls int) error {
	if keepFulls < 1 {
		keepFulls = 1
	}
	var fullSeqs []int64
	if err := s.db.Model(&models.RunCheckpoint{}).
		Where("run_id = ? AND is_incremental = ?", runID.String(), false).
		Order("sequence_high DESC").
		Limit(keepFulls).
		Pluck("sequence_high", &fullSeqs).Error; err != nil {
		return err
	}
	if len(fullSeqs) < keepFulls {
		return nil
	}
	threshold := fullSeqs[len(fullSeqs)-1] // oldest full we are keeping
	return withStoreBusyRetry(func() error {
		return s.db.
			Where("run_id = ? AND sequence_high < ?", runID.String(), threshold).
			Delete(&models.RunCheckpoint{}).Error
	})
}

// TerminalTaskRunsSince returns the run's terminal task_runs rows with a
// terminal_sequence strictly greater than afterSeq, ordered by terminal_sequence
// ascending — the "post-checkpoint tail" a recovering owner replays on top of
// the latest checkpoint.  Uses the (job_run_id, terminal_sequence) index.
func (s *Store) TerminalTaskRunsSince(runID uuid.UUID, afterSeq int64) ([]models.TaskRun, error) {
	var rows []models.TaskRun
	err := s.db.
		Where("job_run_id = ? AND terminal_sequence > ? AND status IN ?",
			runID, afterSeq, []string{
				string(TaskStatusSucceeded),
				string(TaskStatusFailed),
				string(TaskStatusSkipped),
				string(TaskStatusCached),
				string(TaskStatusCancelled),
			}).
		Order("terminal_sequence ASC").
		Find(&rows).Error
	return rows, err
}

// DeleteCheckpoints removes all checkpoints for a run, called when a terminal
// run is archived (the durable task_runs rows remain the system of record).
func (s *Store) DeleteCheckpoints(runID uuid.UUID) error {
	return withStoreBusyRetry(func() error {
		return s.db.
			Where("run_id = ?", runID.String()).
			Delete(&models.RunCheckpoint{}).Error
	})
}
