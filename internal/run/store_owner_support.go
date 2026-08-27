package run

import (
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
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
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		reset = nil
		counts.reset()
		attemptEvents := make([]event.Event, 0, 4)
		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			// One predicate shared by the Find and the Updates so the two can
			// never drift apart and reset a row the caller was never told about.
			where := "job_run_id = ? AND status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ? AND owner_generation <= ?"
			args := []interface{}{runID, string(TaskStatusRunning), now, ownerGeneration}

			var expired []models.TaskRun
			if err := tx.Where(where, args...).Find(&expired).Error; err != nil {
				return err
			}
			if len(expired) == 0 {
				return nil
			}
			res := tx.Model(&models.TaskRun{}).
				Where(where, args...).
				Updates(map[string]interface{}{
					"status":           string(TaskStatusPending),
					"claimed_by":       "",
					"claim_expires_at": nil,
					"runtime_id":       "",
					"started_at":       nil,
				})
			if res.Error != nil {
				return res.Error
			}
			counts.addTaskRunStatus(int(res.RowsAffected))

			if s.eventStore != nil {
				for i := range expired {
					evt, evtErr := s.recordTaskRunEventTx(tx, event.TypeTaskLeaseExpired, runID, &expired[i], &counts)
					if evtErr != nil {
						return evtErr
					}
					attemptEvents = append(attemptEvents, *evt)
				}
			}
			reset = expired
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
	return reset, nil
}
