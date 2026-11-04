package run

import (
	"fmt"
	"maps"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReclaimOwnerExpiredClaims returns this run's in-flight rows whose worker claim
// lease has lapsed to the dispatchable pending state and reports the rows it
// reset.
//
// It is the OWNER's half of expired-claim reaping, and it exists because the
// worker-side reaper deliberately declines to do it.  Claimer.ReclaimExpired
// guards on `NOT EXISTS (... run_leases ... lease_expires_at > now)`: a task
// belonging to a live-owned run is left alone so the reaper can never race the
// owner's dispatch loop into double-executing it.  In run-owner in-memory mode
// nothing then completed the thought — a worker that died mid-task left its row
// `running` with a dead claim, the owner counted it in flight forever, and a
// fanOut.maxParallel group wedged behind a slot that never came back.  This runs
// under the owner's own per-run lock, so it is the one implementation of the
// reset and cannot race the loop it serves.
//
// Fencing mirrors ClaimTaskForDispatch's: `owner_generation <= ownerGeneration`
// accepts rows this owner stamped and legacy generation-0 rows, and rejects rows
// a NEWER owner has already taken over — a lease this node lost must not have
// its claims reset from under the node that now holds it.  The reset columns are
// exactly Claimer.ReclaimExpired's, including leaving `attempt` alone: the
// retry-policy attempt counter is not what a lease expiry consumes (the owner's
// in-memory attempt is bumped by RunState.RequeueExpiredRows for dispatch
// bookkeeping).
func (s *Store) ReclaimOwnerExpiredClaims(runID uuid.UUID, ownerGeneration int64) ([]models.TaskRun, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: reclaim owner expired claims: nil store")
	}
	var reset []models.TaskRun
	var recovered []RecoveredRuntime
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		reset = nil
		recovered = nil
		counts.reset()
		attemptEvents := make([]event.Event, 0, 4)
		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			// One predicate shared by the Find and the Updates so the two can
			// never drift apart and reset a row the caller was never told about.
			where := "job_run_id = ? AND status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ? AND owner_generation <= ?"
			args := []any{runID, string(TaskStatusRunning), now, ownerGeneration}

			// The read locks the rows it returns (FOR UPDATE on PostgreSQL; the
			// SQLite and dqlite dialects drop the clause, their write
			// transactions being serialised). Off dqlite a worker's renewal or
			// completion could otherwise commit between this read and the
			// guarded UPDATE, which then skips the row, and the row would still
			// be reported, requeued by the owner and given a lease-expiry
			// event. Locked, the UPDATE matches exactly what was read, and the
			// renewal waits for the commit and finds the claim gone.
			var expired []models.TaskRun
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where(where, args...).Order("id").Find(&expired).Error; err != nil {
				return err
			}
			if s.afterRecoveryRowsRead != nil {
				s.afterRecoveryRowsRead(tx)
			}
			if len(expired) == 0 {
				return nil
			}
			updates := TaskResourceResetColumns()
			maps.Copy(updates, map[string]any{
				"status":           string(TaskStatusPending),
				"claimed_by":       "",
				"claim_expires_at": nil,
				"runtime_id":       "",
				"started_at":       nil,
			})
			res := tx.Model(&models.TaskRun{}).
				Where(where, args...).
				Updates(WithInvalidatedSecretLogSnapshot(updates))
			if res.Error != nil {
				return res.Error
			}
			counts.addTaskRunStatus(int(res.RowsAffected))

			// The reclaimed rows are re-EXECUTED, so the previous attempt's
			// dataset metrics go with the reset — the failover twin of the
			// retry paths' clearAttemptDatasetMetricsTx. A worker that died
			// between the post-task seam's insert and its completion report
			// would otherwise leave samples that the re-execution doubles.
			//
			// The delete follows what the UPDATE actually matched, not what the
			// Find returned. Off dqlite (Postgres: a real connection pool at
			// READ COMMITTED) a completion can land between the two, and the
			// guarded UPDATE correctly skips that row — deleting its sample
			// anyway would throw away a legitimate baseline observation.
			expiredIDs := make([]uuid.UUID, 0, len(expired))
			for i := range expired {
				expiredIDs = append(expiredIDs, expired[i].ID)
			}
			reclaimed, err := taskRunIDsWithStatusTx(tx, expiredIDs, TaskStatusPending)
			if err != nil {
				return err
			}
			if err := clearAttemptDatasetMetricsForTaskRunsTx(tx, reclaimed); err != nil {
				return err
			}
			// The previous attempts' pods, read before the reset cleared their
			// runtime ids and limited to the rows the guarded UPDATE matched.
			recovered = recoveredRuntimesFrom(expired, uuidSet(reclaimed))

			// Report, and record an event for, only those same rows.
			matched := uuidSet(reclaimed)
			landed := make([]models.TaskRun, 0, len(reclaimed))
			for i := range expired {
				if _, ok := matched[expired[i].ID]; ok {
					landed = append(landed, expired[i])
				}
			}
			if s.eventStore != nil {
				for i := range landed {
					evt, evtErr := s.recordTaskRunEventTx(tx, event.TypeTaskLeaseExpired, runID, &landed[i], &counts)
					if evtErr != nil {
						return evtErr
					}
					attemptEvents = append(attemptEvents, *evt)
				}
			}
			reset = landed
			return nil
		})
		if txErr == nil {
			pendingEvents = attemptEvents
		}
		return txErr
	})
	if err != nil {
		return nil, err
	}
	counts.commit()
	s.publishEvents(pendingEvents...)
	StopRecoveredRuntimes("owner_expired_claim", recovered)
	return reset, nil
}
