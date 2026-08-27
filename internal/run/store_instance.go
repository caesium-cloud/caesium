package run

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
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
			result := tx.Model(&models.TaskRun{}).
				Where("id = ? AND status IN ?", taskRunID, []string{
					string(TaskStatusPending),
					string(TaskStatusRunning),
					string(TaskStatusFailed),
				}).
				Updates(map[string]interface{}{
					"status":                 string(TaskStatusPending),
					"attempt":                attempt,
					"runtime_id":             "",
					"started_at":             nil,
					"completed_at":           nil,
					"result":                 "",
					"output":                 nil,
					"branch_selections":      nil,
					"log_text":               "",
					"log_truncated":          false,
					"error":                  "",
					"rate_limit_retry_after": nil,
					"cache_hit":              false,
					"cache_origin_run_id":    nil,
					"cache_created_at":       nil,
					"cache_expires_at":       nil,
				})
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
	return s.failTask(runID, taskRunID, failure, "", false)
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
