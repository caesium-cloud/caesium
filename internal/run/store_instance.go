package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Instance-keyed writes.
//
// The run store's older write surface addresses task state as
// `WHERE job_run_id = ? AND task_id = ?`, which assumes one TaskRun per
// (run, task). Fan-out breaks that assumption: N sibling rows share the pair,
// so a task-ID-keyed write is ambiguous at best and broadcasts across every
// sibling at worst. The helpers here take the TaskRun primary key and touch
// exactly one row.

// RetryTaskInstance resets one fan-out instance for another attempt.
//
// It is the instance-keyed analogue of RetryTask, which resolves its row via
// loadUniqueTaskRun and therefore cannot address a group member. Only a
// non-terminal-or-failed row is reset: the guard keeps a retry from resurrecting
// an instance that a fail_fast cancellation or an in-group skip cascade has
// already resolved (the same class of bug as the local replace-cancel
// resurrection fixed in #275).
func (s *Store) RetryTaskInstance(runID, taskRunID uuid.UUID, attempt int) error {
	if taskRunID == uuid.Nil {
		return fmt.Errorf("run: retry task instance: nil task run id")
	}
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		return s.db.Transaction(func(tx *gorm.DB) error {
			var row models.TaskRun
			if err := tx.Where("id = ? AND job_run_id = ?", taskRunID, runID).First(&row).Error; err != nil {
				return err
			}
			// ONE reset contract. The columns come from retryResetColumns (the
			// same map RetryFromFailure and RetryPartition use), so an execution
			// column added there — output, log snapshot, schema violations, exit
			// code — can never be forgotten here the way it was when this map was
			// a hand-maintained copy.
			//
			// Two deliberate overrides, and only two:
			//   attempt — this is attempt N+1 of a task still inside its run, not
			//     an operator restarting it from scratch at 1.
			//   claim   — deliberately NOT reset here, unchanged from before: the
			//     in-run retry paths address a row whose claim state is managed by
			//     their caller (see RetryTaskClaimedInstance for the variant that
			//     keeps the claim explicitly).
			updates, resetErr := retryResetColumnsForTaskRun(&row)
			if resetErr != nil {
				return resetErr
			}
			updates["attempt"] = attempt
			delete(updates, "claimed_by")
			delete(updates, "claim_expires_at")

			result := tx.Model(&models.TaskRun{}).
				Where("id = ? AND status IN ?", taskRunID, []string{
					string(TaskStatusPending),
					string(TaskStatusRunning),
					string(TaskStatusFailed),
				}).
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				// Already resolved by a cascade or a cancellation; a retry here
				// would resurrect a terminal row.
				return ErrTaskInstanceNotRetryable
			}
			counts.addTaskRunStatus(1)
			return nil
		})
	})
	if err == nil {
		counts.commit()
	}
	return err
}

// RetryTaskClaimedInstance resets one instance for its next attempt WITHOUT
// releasing it, for a worker that is retrying a task it still holds.
//
// It differs from RetryTaskClaimed in the one respect that made that method
// unusable: RetryTaskClaimed re-pends the row, and StartTaskClaimed will only
// start a row whose status is `running`, so the attempt that followed a retry
// could never start — it hit ErrTaskClaimMismatch, tore its container down and
// abandoned the task mid-budget, whatever the step's `retries` said. The worker
// never released the claim and is about to launch the next container itself, so
// `running` + the same claim is the truthful state between attempts; re-pending
// described a task waiting to be picked up that nothing was going to pick up.
//
// The guard is the claim fence: only a row still RUNNING and still claimed by
// this worker is reset. Anything else (a fail_fast cancellation, a lease that
// expired and was re-claimed elsewhere) yields ErrTaskClaimMismatch, which the
// executor already treats as "abandon quietly".
func (s *Store) RetryTaskClaimedInstance(runID, taskRunID uuid.UUID, attempt int, claimedBy string) error {
	return s.retryTaskClaimedInstanceAttempt(runID, taskRunID, attempt, claimedBy, 0)
}

// RetryTaskClaimedInstanceAttempt keeps an in-process execution retry bound to
// the exact dispatch claim. The execution-attempt counter may advance while
// claimAttempt remains constant; a same-node reclaim advances claimAttempt and
// must reject the old goroutine.
func (s *Store) RetryTaskClaimedInstanceAttempt(runID, taskRunID uuid.UUID, attempt int, claimedBy string, claimAttempt int) error {
	if claimAttempt <= 0 {
		return ErrTaskClaimMismatch
	}
	return s.retryTaskClaimedInstanceAttempt(runID, taskRunID, attempt, claimedBy, claimAttempt)
}

func (s *Store) retryTaskClaimedInstanceAttempt(runID, taskRunID uuid.UUID, attempt int, claimedBy string, claimAttempt int) error {
	if taskRunID == uuid.Nil {
		return fmt.Errorf("run: retry claimed task instance: nil task run id")
	}
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			var row models.TaskRun
			if err := tx.Where("id = ? AND job_run_id = ?", taskRunID, runID).First(&row).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrTaskClaimMismatch
				}
				return err
			}
			// The same ONE reset contract RetryTaskInstance and the operator
			// retries use, so an execution column added to retryResetColumns —
			// schema violations, exit code, a future one — is cleared here too
			// rather than leaking the previous attempt's evidence onto the next.
			//
			// Three deliberate overrides, and only three:
			//   attempt — attempt N+1, not a restart at 1.
			//   status  — NOT written: the row stays RUNNING for the whole
			//     attempt budget, because this worker is about to launch the
			//     next container itself.
			//   claim   — NOT cleared, for the same reason: the claim was never
			//     released and clearing it would invite a second claimer in.
			updates, resetErr := retryResetColumnsForTaskRun(&row)
			if resetErr != nil {
				return resetErr
			}
			updates["attempt"] = attempt
			delete(updates, "status")
			delete(updates, "claimed_by")
			delete(updates, "claim_expires_at")

			query := tx.Model(&models.TaskRun{}).
				Where("id = ? AND claimed_by = ? AND status = ?", row.ID, claimedBy, string(TaskStatusRunning))
			if claimAttempt > 0 {
				query = query.Where("claim_attempt = ?", claimAttempt)
			}
			result := query.
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			counts.addTaskRunStatus(1)
			if s.eventStore != nil {
				evt, evtErr := s.recordTaskRunEventTx(tx, event.TypeTaskRetrying, runID, &row, &counts)
				if evtErr != nil {
					return evtErr
				}
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if txErr == nil {
			pendingEvents = attemptEvents
		}
		return txErr
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

// SkipTaskInstance resolves one still-pending instance as skipped with a reason.
//
// fanOut.failurePolicy: fail_fast needs this: on the first sibling failure the
// pending siblings must be resolved, not merely left undispatched. Leaving them
// pending would hang the run until its timeout, the same failure mode the
// in-group skip cascade exists to prevent. A row that is already terminal is
// left untouched.
func (s *Store) SkipTaskInstance(runID, taskRunID uuid.UUID, reason string) error {
	if taskRunID == uuid.Nil {
		return fmt.Errorf("run: skip task instance: nil task run id")
	}
	now := time.Now().UTC()
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		return s.db.Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&models.TaskRun{}).
				Where("id = ? AND job_run_id = ? AND status NOT IN ?", taskRunID, runID, terminalTaskStatuses()).
				Updates(map[string]interface{}{
					"status":       string(TaskStatusSkipped),
					"error":        reason,
					"completed_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 {
				counts.addTaskRunStatus(1)
			}
			return nil
		})
	})
	if err == nil {
		counts.commit()
	}
	return err
}

// ErrTaskInstanceNotRetryable is returned when a retry targets an instance row
// that a cascade or cancellation has already resolved.
var ErrTaskInstanceNotRetryable = errors.New("run: task instance is not retryable")

// FailTaskInstance marks exactly one TaskRun row failed by its primary key,
// runs the transitive in-group skip cascade, and advances the group's cross-step
// successors once every sibling is terminal.
//
// It is now a thin alias for failTask: that function's taskRef parameter follows
// the TaskRun-primary-key-or-catalog-task-ID contract and is no longer
// reassigned mid-flight, so passing an instance's primary key addresses that row
// and nothing else. This wrapper survives as the name that says "by instance",
// and for its nil-id guard.
func (s *Store) FailTaskInstance(runID, taskRunID uuid.UUID, failure error) error {
	if taskRunID == uuid.Nil {
		return fmt.Errorf("run: fail task instance: nil task run id")
	}
	return s.failTask(runID, taskRunID, failure, "", false, 0, false)
}

// GroupIdentityHash folds a fan-out group's per-instance identity hashes into
// the ONE aggregate hash a downstream step folds into its own cache identity.
//
// instanceHashes must be the effective hashes of the group's terminal-SUCCESS
// instances in PARTITION-INDEX order (emission order — stable across runs and
// independent of scheduling order or which instance finished last). Empty
// entries are skipped; an all-empty list yields "".
//
// The definition is fixed by the design and is shared with the SQL read path
// (predecessorGroupHash, internal/run/store.go): it is
//
//	sha256( "fanout-group:" || h(0) || "\n" || h(1) || "\n" || … )
//
// Both sites reuse fanOutGroupHashPrefix so the two can never disagree on the
// namespace. One aggregate entry per predecessor is load-bearing: N entries
// would change the SHAPE of the downstream key's pred_hash: lines, so adding or
// removing a single partition would re-key the whole downstream subtree.
func GroupIdentityHash(instanceHashes []string) string {
	sum := sha256.New()
	sum.Write([]byte(fanOutGroupHashPrefix))
	wrote := false
	for _, h := range instanceHashes {
		if h == "" {
			continue
		}
		sum.Write([]byte(h))
		sum.Write([]byte("\n"))
		wrote = true
	}
	if !wrote {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Instance-keyed reads that the fan-out schedulers need.
//
// These live beside the instance-keyed writes above for the same reason: the
// historic surface addresses a task as `(job_run_id, task_id)`, which names N
// rows once a step fans out.

// InstanceIdentity is one fan-out instance's persisted outcome: what it emitted
// and the identity it presents to downstream consumers.
//
// It exists because neither of the two read surfaces the local scheduler already
// has can answer "what did the siblings I did NOT execute produce?".
// TaskRunInstances returns the run.TaskRun projection, which deliberately omits
// the hash columns; the in-memory maps runFannedGroup builds only ever describe
// the instances THIS invocation dispatched. After a manual partition retry that
// is a single instance, and the group's fan-in aggregate and aggregate identity
// hash must still cover all N.
type InstanceIdentity struct {
	TaskRunID      uuid.UUID
	PartitionValue string
	PartitionIndex int
	Status         TaskStatus
	// Output is the decoded output map the instance recorded (nil when it
	// emitted none, or when the column could not be decoded).
	Output map[string]string
	// IdentityHash is the EFFECTIVE identity — effective_hash when a
	// value-verified short-circuit was proven for this instance, else hash. It
	// is the same value the SQL read path folds into a downstream key
	// (predecessorGroupHash), so a hash rebuilt from these rows and one computed
	// by the distributed lane cannot disagree.
	IdentityHash string
}

// FanOutInstanceIdentities returns every instance row of (runID, taskID) in
// partition-index order with its persisted output and effective identity hash.
//
// Ordering is load-bearing: GroupIdentityHash folds its input in partition-index
// order, so a caller may pass the successful entries straight through.
func (s *Store) FanOutInstanceIdentities(ctx context.Context, runID, taskID uuid.UUID) ([]InstanceIdentity, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: fan-out instance identities: nil store")
	}

	var rows []models.TaskRun
	if err := s.db.WithContext(ctx).
		Select("id", "status", "output", "hash", "effective_hash", "partition_value", "partition_index").
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Order("partition_index ASC, created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]InstanceIdentity, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		identity := InstanceIdentity{
			TaskRunID:      row.ID,
			PartitionValue: row.PartitionValue,
			PartitionIndex: row.PartitionIndex,
			Status:         TaskStatus(row.Status),
			IdentityHash:   effectiveTaskHash(row.Hash, row.EffectiveHash),
		}
		if len(row.Output) > 0 {
			var decoded map[string]string
			if err := json.Unmarshal(row.Output, &decoded); err != nil {
				// A row whose output cannot be decoded contributes nothing
				// rather than failing the whole rebuild: the aggregate is
				// best-effort reporting, and losing one partition's outputs is
				// strictly better than losing the run.
				log.Warn("failed to decode persisted instance output",
					"run_id", runID, "task_id", taskID, "task_run_id", row.ID, "error", err)
			} else if len(decoded) > 0 {
				identity.Output = decoded
			}
		}
		out = append(out, identity)
	}
	return out, nil
}

// ValidateTaskOutputSchemaInstance is the instance-keyed form of
// ValidateTaskOutputSchema (internal/run/schema_validation.go).
//
// It differs in exactly one respect: the violations are recorded on the TaskRun
// named by taskRunID rather than on whatever row `(runID, taskID)` resolves to.
// That matters because SaveSchemaViolations now refuses an ambiguous catalog
// task id, and the refusal is only logged — so under fan-out the step-keyed form
// silently recorded NOTHING. In fail mode that loses the evidence for the very
// failure being reported; in warn mode it opens a schema_violation incident with
// no row behind it. taskID is still carried for the log lines, the error text
// and the emitted event, all of which identify the STEP.
func ValidateTaskOutputSchemaInstance(
	store *Store,
	runID, taskID, taskRunID uuid.UUID,
	output map[string]string,
	outputSchema []byte,
	schemaValidation string,
) error {
	return validateTaskOutputSchemaInstance(store, runID, taskID, taskRunID, output,
		outputSchema, schemaValidation, "", 0, false)
}

// ValidateTaskOutputSchemaInstanceClaimedAttempt is the live worker form. If
// the row has been reclaimed, it returns ErrTaskClaimMismatch and emits neither
// stale evidence nor a schema incident for the newer execution.
func ValidateTaskOutputSchemaInstanceClaimedAttempt(
	store *Store,
	runID, taskID, taskRunID uuid.UUID,
	output map[string]string,
	outputSchema []byte,
	schemaValidation string,
	claimedBy string,
	claimAttempt int,
) error {
	if claimAttempt <= 0 {
		return ErrTaskClaimMismatch
	}
	return validateTaskOutputSchemaInstance(store, runID, taskID, taskRunID, output,
		outputSchema, schemaValidation, claimedBy, claimAttempt, true)
}

func validateTaskOutputSchemaInstance(
	store *Store,
	runID, taskID, taskRunID uuid.UUID,
	output map[string]string,
	outputSchema []byte,
	schemaValidation string,
	claimedBy string,
	claimAttempt int,
	enforceClaim bool,
) error {
	if len(outputSchema) == 0 || schemaValidation == "" {
		return nil
	}

	violations, err := pkgtask.ValidateOutputSchemaBytes(output, outputSchema)
	if err != nil {
		log.Warn("schema validation error", "task_id", taskID, "error", err)
		return nil
	}
	if len(violations) == 0 {
		return nil
	}

	log.Warn("task output schema violations", "task_id", taskID, "task_run_id", taskRunID, "violations", len(violations))
	violationRef := taskRunID
	if violationRef == uuid.Nil {
		violationRef = taskID
	}
	var saveErr error
	if enforceClaim {
		saveErr = store.SaveSchemaViolationsClaimedAttempt(runID, violationRef, violations, claimedBy, claimAttempt)
	} else {
		saveErr = store.SaveSchemaViolations(runID, violationRef, violations)
	}
	if saveErr != nil {
		if enforceClaim {
			return saveErr
		}
		log.Warn("failed to persist schema violations", "task_id", taskID, "task_run_id", taskRunID, "error", saveErr)
	}

	if schemaValidation == jobdef.SchemaValidationFail {
		// In fail mode the task fails and its task_failed event already carries
		// the violations, so no separate event is emitted.
		return fmt.Errorf("task %s output violates declared schema: %d violation(s)", taskID, len(violations))
	}

	if !enforceClaim {
		// The legacy/unclaimed lane has no later exact-claim acceptance point.
		publishSchemaViolationEvent(store, runID, taskID, len(violations))
	}
	// The claimed lane publishes only after the exact terminal completion
	// commits. Saving evidence and publishing here are not atomic: the claim can
	// expire between them, allowing attempt N to emit an incident after attempt
	// N+1 owns the row.

	return nil
}
