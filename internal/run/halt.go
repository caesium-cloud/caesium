package run

import (
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// HaltReason is the skip reason every executor stamps on a step the `halt`
// failure policy resolved without running it. It is spelled here, once, so the
// local Kahn loop, the distributed worker and the run-completion waiter cannot
// drift the way the four `trigger rule %q not satisfied` sites once could.
func HaltReason(failedTaskName string) string {
	return fmt.Sprintf("run halted after task %q failed", failedTaskName)
}

// HaltUnstartedTasks is the store half of CAESIUM_TASK_FAILURE_POLICY=halt.
//
// Under `halt` a failure stops the run from ADMITTING new work; it does not
// stop work that is already running (Caesium cannot kill a container it did
// not decide to start), and it does not stop the steps whose trigger rule says
// they run regardless of upstream failure. Concretely, it resolves as `skipped`
// every step of runID that
//
//   - has not started — none of its rows has a container (taskRunStarted) or
//     has left `pending`; a fan-out group with even one started instance is
//     left alone and finishes on its own —
//   - and whose trigger rule is NOT failure-tolerant (IsTolerantTriggerRule:
//     all_done, always, one_success, all_failed are tolerant; all_success, the
//     default, is not).
//
// Each skip cascades exactly as a rule-skip does (skipTaskAndDescendantsTx):
// the skipped step's successors are decremented, a tolerant successor whose
// rule is now satisfied is announced ready (task_ready) so the dispatcher can
// pick it up, and an intolerant one is skipped with its rule reason. That is
// what lets a cleanup step downstream of a halted branch still run — `all_done`
// is satisfied by a skipped predecessor — while the halted branch itself never
// starts.
//
// The reason stamped on every directly halted row is HaltReason(name of
// failedTaskID). Pass uuid.Nil to name the run's earliest failed task instead,
// which is what the run-completion waiter does when it only knows THAT a task
// failed. `only`, when non-nil, restricts the sweep to those catalog task ids:
// the local executor passes the steps it has not dispatched, because a row it
// has dispatched but not yet started (its container is being created) is still
// `pending` in SQL and must not be resolved out from under it.
//
// Idempotent: every write is pending-only, so a second call — the waiter's
// belt-and-braces sweep after the worker's — is a no-op. Returns the catalog
// task ids of every step skipped, directly or by cascade, so the local executor
// can bring its in-memory DAG into line.
func (s *Store) HaltUnstartedTasks(runID, failedTaskID uuid.UUID, only []uuid.UUID) ([]uuid.UUID, error) {
	var (
		pendingEvents []event.Event
		counts        dbWriteCounts
		skipped       []uuid.UUID
	)
	var onlySet map[uuid.UUID]struct{}
	if only != nil {
		onlySet = make(map[uuid.UUID]struct{}, len(only))
		for _, id := range only {
			onlySet[id] = struct{}{}
		}
	}
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8)
		var attemptSkipped []uuid.UUID
		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			var rows []models.TaskRun
			if err := tx.Where("job_run_id = ?", runID).
				Order("created_at ASC, partition_index ASC").
				Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}

			// A step is "not started" when none of its rows has a container
			// (taskRunStarted) or has already reached a terminal status. A row
			// that is `running` only because a dispatcher CLAIMED it — no
			// runtime_id yet — is not started: both claim paths flip a row to
			// running before any container exists, and a one-slot worker can
			// reject that dispatch and roll the claim back to pending seconds
			// later. Reading it as started would let the very work halt exists
			// to stop start after the failure (the fail_fast sibling cancel
			// learned the same lesson; see RunState.ApplyCompletion).
			type stepState struct {
				unstarted int
				started   bool
			}
			steps := make(map[uuid.UUID]*stepState, len(rows))
			order := make([]uuid.UUID, 0, len(rows))
			for i := range rows {
				row := &rows[i]
				st, ok := steps[row.TaskID]
				if !ok {
					st = &stepState{}
					steps[row.TaskID] = st
					order = append(order, row.TaskID)
				}
				switch {
				case taskRunStarted(row):
					st.started = true
				case row.Status == string(TaskStatusPending), row.Status == string(TaskStatusRunning):
					st.unstarted++
				default:
					st.started = true
				}
			}

			reason, err := s.haltReasonTx(tx, runID, failedTaskID)
			if err != nil {
				return err
			}

			for _, taskID := range order {
				st := steps[taskID]
				if st.unstarted == 0 || st.started {
					continue
				}
				if onlySet != nil {
					if _, ok := onlySet[taskID]; !ok {
						continue
					}
				}
				_, rule, err := s.resolveTriggerRuleTx(tx, runID, taskID)
				if err != nil {
					return err
				}
				if IsTolerantTriggerRule(rule) {
					continue
				}
				ids, err := s.skipTaskAndDescendantsUsingTx(tx, runID, taskID, reason, s.markTaskCancelledBeforeStartTx, &attemptEvents, &counts)
				if err != nil {
					return err
				}
				attemptSkipped = append(attemptSkipped, ids...)
			}
			return nil
		})
		if txErr == nil {
			pendingEvents = attemptEvents
			skipped = attemptSkipped
		}
		return txErr
	})
	if err != nil {
		return nil, err
	}
	counts.commit()
	s.publishEvents(pendingEvents...)

	// The in-memory run owner (CAESIUM_RUN_OWNER_IN_MEMORY=true) dispatches
	// from its own RunState, not from these rows, and it consumes neither
	// task_skipped events nor the row scalar. Left alone it keeps a halted
	// step on its ready queue — every dispatch of it is refused, the row is
	// already skipped — and never learns that the cleanup this sweep released
	// is ready, so the waiter (which reads the rows) waits forever for a
	// step nobody dispatches. Fold the rows in, in sequence order, through
	// the same transition recovery replays terminal rows with.
	if len(skipped) > 0 {
		var rows []models.TaskRun
		if err := s.db.Where("job_run_id = ? AND task_id IN ? AND status = ?", runID, skipped, string(TaskStatusSkipped)).
			Order("terminal_sequence ASC").
			Find(&rows).Error; err != nil {
			log.Warn("halt: could not load swept rows for the in-memory run owner", "run_id", runID, "error", err)
		} else {
			s.syncRunStateTerminalRows(runID, rows)
		}
	}
	return skipped, nil
}

// markTaskCancelledBeforeStartTx is the halt sweep's taskSkipMarker: every row
// of the step that has nothing running — pending, or claimed with no container
// yet — is resolved skipped through markInstanceCancelledBeforeStartTx, which
// also revokes the claim so a worker holding that dispatch cannot start it
// (StartTaskClaimed then fails with ErrTaskClaimMismatch and no container is
// created). A row with a container is left alone, in the guarded UPDATE
// itself, so a start racing this sweep wins and the row runs to completion.
func (s *Store) markTaskCancelledBeforeStartTx(tx *gorm.DB, runID, taskID uuid.UUID, reason string, pendingEvents *[]event.Event, counts *dbWriteCounts) (bool, error) {
	var rows []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC").
		Find(&rows).Error; err != nil {
		return false, err
	}
	any := false
	for i := range rows {
		marked, err := s.markInstanceCancelledBeforeStartTx(tx, runID, &rows[i], reason, pendingEvents, counts)
		if err != nil {
			return any, err
		}
		any = any || marked
	}
	return any, nil
}

// haltReasonTx builds the HaltReason for the sweep: the catalog name of
// failedTaskID, or — when the caller passed uuid.Nil — of the run's earliest
// failed row (lowest terminal_sequence). Falls back to the id when the catalog
// row is gone, and to a generic reason when the run has no failed row at all.
func (s *Store) haltReasonTx(tx *gorm.DB, runID, failedTaskID uuid.UUID) (string, error) {
	if failedTaskID == uuid.Nil {
		var failed models.TaskRun
		err := tx.Select("task_id").
			Where("job_run_id = ? AND status = ?", runID, string(TaskStatusFailed)).
			Order("terminal_sequence ASC, completed_at ASC").
			First(&failed).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "run halted after a task failed", nil
			}
			return "", err
		}
		failedTaskID = failed.TaskID
	}
	var task models.Task
	if err := tx.Select("name").First(&task, "id = ?", failedTaskID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return HaltReason(failedTaskID.String()), nil
		}
		return "", err
	}
	if task.Name == "" {
		return HaltReason(failedTaskID.String()), nil
	}
	return HaltReason(task.Name), nil
}

// HasReleasablePending reports whether runID still has a pending step that
// SOMETHING will act on: one whose predecessors have all reached a terminal
// status. Such a step is either about to be claimed by a dispatcher (its rule
// is satisfied) or about to be skipped by the advancement cascade (it is not)
// — either way the run is not stalled, and a run-completion waiter must keep
// waiting rather than finalize.
//
// Readiness is decided from the run's EDGES, not from the outstanding_predecessors
// scalar, so the answer is the same on every lane: the in-memory run owner
// (CAESIUM_RUN_OWNER_IN_MEMORY=true) deliberately advances its DAG without
// decrementing that scalar, which made a scalar-only read declare its
// released successors stalled. A row whose scalar is already zero is counted
// without the edge walk.
func (s *Store) HasReleasablePending(runID uuid.UUID) (bool, error) {
	var releasable bool
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var rows []models.TaskRun
		if err := tx.Select("id", "task_id", "outstanding_predecessors").
			Where("job_run_id = ? AND status = ?", runID, string(TaskStatusPending)).
			Find(&rows).Error; err != nil {
			return err
		}
		checked := make(map[uuid.UUID]bool, len(rows))
		for i := range rows {
			row := &rows[i]
			if row.OutstandingPredecessors == 0 {
				releasable = true
				return nil
			}
			if done, ok := checked[row.TaskID]; ok {
				if done {
					releasable = true
					return nil
				}
				continue
			}
			statuses, err := s.predecessorStatusesTx(tx, runID, row.TaskID)
			if err != nil {
				return err
			}
			allTerminal := true
			for _, st := range statuses {
				if !IsTerminal(st) {
					allTerminal = false
					break
				}
			}
			checked[row.TaskID] = allTerminal
			if allTerminal {
				releasable = true
				return nil
			}
		}
		return nil
	})
	return releasable, err
}
