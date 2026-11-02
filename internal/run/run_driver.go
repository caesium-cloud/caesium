package run

import (
	"context"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// A run's driver is the process whose job.Run executes it (local mode) or
// awaits it (distributed mode). Nothing about a driver survives its process, so
// the row carries the one fact a peer can use: driver_heartbeat_at, stamped by
// the write that makes the run running and rewritten while the driver lives.
//
// The value is an opaque token. Liveness is that it CHANGES, never how old it
// reads: a renewal is stamped before it queues on dqlite's single writer, so a
// committed value can be arbitrarily stale, and clocks differ between nodes.
// Recovery therefore only ever compares a value for equality with one it
// observed (internal/job/run_recovery.go), NULL included, which is what a run
// from before the column holds.

// ErrRunDriverLost finalizes a run whose driver died with unfinished tasks. It
// is what recovery knows and all it knows: whether an in-flight container is
// still running or already succeeded is not recorded anywhere, so neither a
// success nor a re-execution can be justified. `caesium run retry` is how an
// operator accepts the re-execution. The text is persisted on the run and its
// tasks; CancelledRunIDs matches it exactly.
var ErrRunDriverLost = errors.New("the server driving this run stopped before it finished; " +
	"its unfinished tasks were not re-executed, and a container one of them started may still be running")

// RunHeartbeat is a running run's identity and its current heartbeat.
type RunHeartbeat struct {
	ID        uuid.UUID
	JobID     uuid.UUID
	Heartbeat *time.Time
}

// heartbeatStamp is microsecond UTC, the precision every supported database
// stores, so a value reads back equal to what was written.
func heartbeatStamp() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// heartbeatIs restricts q to a row still holding the observed value.
func heartbeatIs(q *gorm.DB, observed *time.Time) *gorm.DB {
	if observed == nil {
		return q.Where("driver_heartbeat_at IS NULL")
	}
	return q.Where("driver_heartbeat_at = ?", observed.UTC())
}

// RenewRunDrivers rewrites the heartbeat of the given runs in one statement per
// chunk, so a node's cost is per tick rather than per run on dqlite's single
// writer. updated_at is left alone: a heartbeat is not a change to the run.
func (s *Store) RenewRunDrivers(ctx context.Context, ids []uuid.UUID) error {
	for _, chunk := range chunkTaskRunIDs(ids) {
		if err := withStoreBusyRetryContext(ctx, func() error {
			return s.db.WithContext(ctx).Model(&models.JobRun{}).
				Where("id IN ? AND status = ?", chunk, string(StatusRunning)).
				UpdateColumn("driver_heartbeat_at", heartbeatStamp()).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// RunHeartbeats reads every running run's heartbeat. Quarantined replays are not
// listed: they never execute in local mode, and in distributed mode their owner
// finishes them. `status` is indexed and the set is what is in flight now.
func (s *Store) RunHeartbeats(ctx context.Context) ([]RunHeartbeat, error) {
	var rows []models.JobRun
	if err := s.db.WithContext(ctx).Select("id", "job_id", "driver_heartbeat_at").
		Where("status = ? AND quarantine <> true", string(StatusRunning)).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RunHeartbeat, 0, len(rows))
	for i := range rows {
		out = append(out, RunHeartbeat{ID: rows[i].ID, JobID: rows[i].JobID, Heartbeat: rows[i].DriverHeartbeatAt})
	}
	return out, nil
}

// ReplaceRunHeartbeat swaps the heartbeat from the observed value to a fresh one
// and returns it, or nil if the row no longer holds that value. One statement
// decides between sweeping nodes and between a sweep and a renewal: anything
// that committed since the observation wins, however old its stamp reads.
func (s *Store) ReplaceRunHeartbeat(ctx context.Context, runID uuid.UUID, observed *time.Time) (*time.Time, error) {
	var replaced *time.Time
	err := withStoreBusyRetryContext(ctx, func() error {
		replaced = nil
		stamp := heartbeatStamp()
		res := heartbeatIs(s.db.WithContext(ctx).Model(&models.JobRun{}).
			Where("id = ? AND status = ?", runID, string(StatusRunning)), observed).
			UpdateColumn("driver_heartbeat_at", stamp)
		if res.Error == nil && res.RowsAffected == 1 {
			replaced = &stamp
		}
		return res.Error
	})
	return replaced, err
}

// FinalizeLostRun is CompleteIfActive with ErrRunDriverLost, in the same
// statement that proves the row still holds the observed heartbeat: a renewal
// that commits first keeps the run alive, whichever instant it was stamped.
func (s *Store) FinalizeLostRun(runID uuid.UUID, observed *time.Time) (bool, error) {
	return s.completeIfActive(runID, ErrRunDriverLost, func(q *gorm.DB) *gorm.DB {
		return heartbeatIs(q, observed)
	})
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
