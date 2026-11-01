package run

import (
	"context"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// A run's driver is the process whose job.Run executes it (local mode) or
// awaits it (distributed mode). Nothing about a driver survives its process, so
// the row carries the one fact a peer can use: driver_heartbeat_at, stamped by
// the write that makes the run running and renewed while the driver lives.
// A heartbeat older than the TTL, or absent on a run from before the column,
// means no process is driving the run (internal/job/run_recovery.go).

// ErrRunDriverLost finalizes a run whose driver died with unfinished tasks. It
// is what recovery knows and all it knows: whether an in-flight container is
// still running or already succeeded is not recorded anywhere, so neither a
// success nor a re-execution can be justified. `caesium run retry` is how an
// operator accepts the re-execution. The text is persisted on the run and its
// tasks; CancelledRunIDs matches it exactly.
var ErrRunDriverLost = errors.New("the server driving this run stopped before it finished; " +
	"its unfinished tasks were not re-executed, and a container one of them started may still be running")

const lapsedDriverPredicate = "(driver_heartbeat_at IS NULL OR driver_heartbeat_at <= ?)"

// LapsedRun identifies a running run with no live driver.
type LapsedRun struct {
	ID    uuid.UUID
	JobID uuid.UUID
}

// RenewRunDrivers extends the heartbeat of the given runs in one statement per
// chunk, so a node's cost is per tick rather than per run on dqlite's single
// writer. updated_at is left alone: a heartbeat is not a change to the run.
func (s *Store) RenewRunDrivers(ctx context.Context, ids []uuid.UUID) error {
	now := time.Now().UTC()
	for _, chunk := range chunkTaskRunIDs(ids) {
		if err := withStoreBusyRetryContext(ctx, func() error {
			return s.db.WithContext(ctx).Model(&models.JobRun{}).
				Where("id IN ? AND status = ?", chunk, string(StatusRunning)).
				UpdateColumn("driver_heartbeat_at", now).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// LapsedRuns lists running runs whose heartbeat is at or before cutoff, oldest
// first. Quarantined replays are not listed: they never execute in local mode,
// and in distributed mode their owner finishes them.
func (s *Store) LapsedRuns(ctx context.Context, cutoff time.Time, limit int) ([]LapsedRun, error) {
	var rows []models.JobRun
	if err := s.db.WithContext(ctx).Select("id", "job_id").
		Where("status = ? AND quarantine <> true AND "+lapsedDriverPredicate, string(StatusRunning), cutoff).
		Order("started_at ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LapsedRun, 0, len(rows))
	for i := range rows {
		out = append(out, LapsedRun{ID: rows[i].ID, JobID: rows[i].JobID})
	}
	return out, nil
}

// ClaimLapsedRun takes recovery of a lapsed run. The predicate re-checks the
// lapse in the UPDATE, so one statement decides between sweeping nodes and
// between a sweep and a driver's renewal: the winner moves the row out of the
// lapsed set and every other caller updates nothing. A winner that dies leaves
// a heartbeat that lapses again, so recovery repeats instead of being lost.
func (s *Store) ClaimLapsedRun(ctx context.Context, runID uuid.UUID, cutoff time.Time) (bool, error) {
	var claimed bool
	err := withStoreBusyRetryContext(ctx, func() error {
		res := s.db.WithContext(ctx).Model(&models.JobRun{}).
			Where("id = ? AND status = ? AND "+lapsedDriverPredicate, runID, string(StatusRunning), cutoff).
			UpdateColumn("driver_heartbeat_at", time.Now().UTC())
		claimed = res.RowsAffected == 1
		return res.Error
	})
	return claimed, err
}

// RunTasksSettled reports whether the run has task rows and all of them are
// terminal: no work remains, and re-entering the run executes nothing.
func (s *Store) RunTasksSettled(ctx context.Context, runID uuid.UUID) (bool, error) {
	var statuses []string
	if err := s.db.WithContext(ctx).Model(&models.TaskRun{}).
		Where("job_run_id = ?", runID).Pluck("status", &statuses).Error; err != nil {
		return false, err
	}
	for _, status := range statuses {
		if !IsTerminal(TaskStatus(status)) {
			return false, nil
		}
	}
	return len(statuses) > 0, nil
}
