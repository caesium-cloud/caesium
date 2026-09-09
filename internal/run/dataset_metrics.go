package run

import (
	"context"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Retention-pruner cadence, mirroring the shipped event pruners
// (internal/event.StartIngestRetentionPruner): tick hourly, but never less
// often than the retention itself and never more often than once a minute.
const (
	defaultDatasetMetricPruneInterval = time.Hour
	minDatasetMetricPruneInterval     = time.Minute
)

// InsertDatasetMetrics persists a batch of self-reported dataset metrics. The
// caller has already resolved every row's dataset identity and task-run
// reference; this is a plain append — there is no upsert key, because each task
// run's samples ARE the history the rolling baseline reads.
//
// This is the UNFENCED form, and it is the LOCAL executor's (internal/job,
// enforceClaim=false): there is exactly one process executing the task, no
// claim on the row to check, and nothing that can supersede it. Every
// distributed caller goes through the fenced form — see TaskClaim.
func InsertDatasetMetrics(ctx context.Context, conn *gorm.DB, rows []models.DatasetMetric) error {
	if conn == nil || len(rows) == 0 {
		return nil
	}
	return conn.WithContext(ctx).Create(&rows).Error
}

// TaskClaim is the identity of ONE claim a distributed worker holds on ONE
// TaskRun row: the worker that claimed it, and WHICH of that worker's claims
// this is.
//
// ClaimAttempt is what makes it a token rather than a name. Both reset paths
// (ResetInFlightTasks, ReclaimOwnerExpiredClaims) clear claimed_by to the empty
// string, and both claim statements set `claim_attempt = claim_attempt + 1`, so
// a worker whose lease expired and that then re-claims the SAME row presents
// the same claimed_by with a higher attempt. Fencing on the name alone would let
// that worker's superseded attempt write onto its own new attempt's row — the
// ABA case, and the only one a single-worker lane can produce at all.
//
// A nil *TaskClaim means "unfenced": the local executor, which holds no claim.
type TaskClaim struct {
	ClaimedBy    string
	ClaimAttempt int
}

// String renders the claim for a log line: `worker-a#3`. The nil receiver is
// the unfenced local path and says so rather than panicking in a log call.
func (c *TaskClaim) String() string {
	if c == nil {
		return "unfenced"
	}
	return fmt.Sprintf("%s#%d", c.ClaimedBy, c.ClaimAttempt)
}

// insertDatasetMetricsFencedTx persists a batch of samples only while the claim
// that produced them still holds the TaskRun row, and reports whether it did.
//
// THE RACE it closes (issue #438). The reset paths clear a row's samples inside
// the transaction that hands the row to another worker, so the only remaining
// window is a SUPERSEDED worker finishing its post-task seam afterwards:
//
//	worker A executes, its lease expires
//	  -> ReclaimOwnerExpiredClaims / ResetInFlightTasks re-pends the row and
//	     deletes A's samples in the same transaction
//	worker B claims the row, executes, inserts ITS samples
//	worker A finally reaches the seam and inserts too
//
// and the baseline for that (dataset, metric) now carries two sample sets for
// one logical run — one of them from an attempt whose completion was itself
// claim-rejected and which therefore never became history in any other table.
// Every other terminal write on the row is claim-fenced (completeTask, failTask,
// cacheHitTask, RetryTaskClaimedInstance); this one was not.
//
// The check is a read of the row's live claim inside the caller's WRITE
// transaction, preceded by lockTaskRunForMetricFenceTx — the same
// dialect-conditional row lock lockJobRunForPartitionRetryTx and
// lockGroupForTerminalDecisionTx take, and for the same reason: on dqlite and
// SQLite writers already serialize, but on Postgres at READ COMMITTED a reclaim
// committing between this read and the INSERT would otherwise slip a sample set
// past a fence that had just passed.
//
// A missing row is a mismatch, not an error: the run was deleted or pruned out
// from under this attempt, and its samples have nowhere to hang.
func insertDatasetMetricsFencedTx(tx *gorm.DB, taskRunID uuid.UUID, claim *TaskClaim, rows []models.DatasetMetric) (bool, error) {
	if len(rows) == 0 {
		return true, nil
	}
	if claim != nil {
		held, err := taskRunClaimHeldTx(tx, taskRunID, *claim)
		if err != nil {
			return false, err
		}
		if !held {
			return false, nil
		}
	}
	return true, tx.Create(&rows).Error
}

// taskRunClaimHeldTx reports whether taskRunID is still held by exactly `claim`.
// A row that no longer exists reads as not held: it was deleted or pruned out
// from under this attempt, and the samples have nowhere to hang.
//
// The mismatch is logged HERE, where the claim that actually holds the row is
// in hand — the caller's drop line carries the run, task and sample count but
// cannot say who took the row over.
func taskRunClaimHeldTx(tx *gorm.DB, taskRunID uuid.UUID, claim TaskClaim) (bool, error) {
	if taskRunID == uuid.Nil {
		return false, nil
	}
	if err := lockTaskRunForMetricFenceTx(tx, taskRunID); err != nil {
		return false, err
	}
	var found []struct {
		ClaimedBy    string `gorm:"column:claimed_by"`
		ClaimAttempt int    `gorm:"column:claim_attempt"`
	}
	if err := tx.Model(&models.TaskRun{}).
		Select("claimed_by", "claim_attempt").
		Where("id = ?", taskRunID).
		Limit(1).
		Find(&found).Error; err != nil {
		return false, err
	}
	if len(found) == 0 {
		log.Warn("dataset metric claim fence: the task run is gone",
			"task_run_id", taskRunID, "stale_claim", claim.String())
		return false, nil
	}
	current := TaskClaim{ClaimedBy: found[0].ClaimedBy, ClaimAttempt: found[0].ClaimAttempt}
	if current != claim {
		log.Warn("dataset metric claim fence: the task run was taken over",
			"task_run_id", taskRunID, "stale_claim", claim.String(), "current_claim", current.String())
		return false, nil
	}
	return true, nil
}

// lockTaskRunForMetricFenceTx locks the fenced row for the duration of the
// caller's transaction on a dialect with real write concurrency, so the
// check-then-insert above cannot be split by a concurrent reclaim. dqlite and
// SQLite serialize writers already and cannot parse FOR UPDATE, so they take
// the empty statement — the same split lockGroupForTerminalDecisionTx makes.
func lockTaskRunForMetricFenceTx(tx *gorm.DB, taskRunID uuid.UUID) error {
	if tx == nil || tx.Dialector == nil {
		return fmt.Errorf("run: dataset metric claim fence requires a database dialect")
	}
	stmt, err := metricFenceLockSQL(tx.Name())
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	var locked []struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	return tx.Raw(stmt, taskRunID).Scan(&locked).Error
}

// metricFenceLockSQL returns the dialect's lock statement, or "" for a dialect
// whose writers already serialize. An unknown dialect is an error rather than a
// silent "" so a future backend surfaces the missing guard at run time instead
// of shipping the race.
func metricFenceLockSQL(dialect string) (string, error) {
	switch dialect {
	case "postgres":
		return "SELECT id FROM task_runs WHERE id = ? FOR UPDATE", nil
	case "dqlite", "sqlite", "sqlite3":
		return "", nil
	default:
		return "", fmt.Errorf("run: unsupported dialect %q for the dataset metric claim fence", dialect)
	}
}

// clearAttemptDatasetMetricsTx deletes the samples one TaskRun recorded for the
// attempt that is about to run again. It belongs to the SAME reset contract as
// retryResetColumns and is called wherever a row is reset for re-execution —
// the columns live on task_runs, these rows live in another table, and both are
// the previous attempt's evidence.
//
// Without it a re-executed instance double-counts: every one of these paths
// reuses ONE TaskRun row, so attempt 1's samples and attempt 2's samples both
// hang off it, and when the row finally lands `succeeded` the baseline reads
// BOTH as clean history of one logical run. Two families of caller:
//
//   - the retries (retryResetColumns' five sites). This is the case the feature
//     itself creates: an onViolation: fail verdict IS an attempt failure, so the
//     value the breaker just rejected would come back as baseline history
//     through the very retry it triggered.
//   - the failover resets — ResetInFlightTasks (owner takeover, run resumption)
//     and ReclaimOwnerExpiredClaims (worker lease expiry). A worker that dies
//     between this seam's insert and reportCompletion leaves its samples on a
//     row another worker then re-runs.
//
// Not covered, deliberately: the pre-execution re-pends (ReleaseTaskClaim, the
// rate-limit deferral). Those re-pend a row whose attempt never reached the
// post-task seam, so there is nothing to clear.
//
// THE OTHER HALF OF THE CONTRACT (issue #438, closed): clearing is only sound
// if the superseded worker cannot re-insert afterwards. It reaches its own
// post-task seam whenever it survives the reclaim, so the distributed insert is
// fenced on the claim — see insertDatasetMetricsFencedTx.
//
// It runs inside the caller's transaction, so a failure rolls the reset back
// rather than silently leaking rows.
func clearAttemptDatasetMetricsTx(tx *gorm.DB, taskRunID uuid.UUID) error {
	if taskRunID == uuid.Nil {
		return nil
	}
	return clearAttemptDatasetMetricsForTaskRunsTx(tx, []uuid.UUID{taskRunID})
}

// clearAttemptDatasetMetricsForTaskRunsTx is the batch form, for the failover
// resets that re-pend every in-flight row of a run at once. The caller must
// pass the rows its guarded UPDATE actually matched, not the rows it selected:
// deleting a just-succeeded row's sample would discard legitimate baseline
// history.
func clearAttemptDatasetMetricsForTaskRunsTx(tx *gorm.DB, taskRunIDs []uuid.UUID) error {
	if tx == nil || len(taskRunIDs) == 0 {
		return nil
	}
	for _, chunk := range chunkTaskRunIDs(taskRunIDs) {
		if err := tx.Where("task_run_id IN ?", chunk).Delete(&models.DatasetMetric{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// maxTaskRunIDsPerStatement bounds how many ids one `IN (...)` list binds. A
// single run can hold CAESIUM_FANOUT_MAX_PARTITIONS rows per fanned step
// (default 1024), so a leader restart with several fanned steps in flight would
// otherwise bind several thousand parameters in one statement — under SQLite's
// ceiling, but on the restart path rather than a bounded one.
const maxTaskRunIDsPerStatement = 500

// chunkTaskRunIDs splits an id list into statement-sized batches. It returns
// the input as a single chunk when it already fits, so the common case pays
// nothing.
func chunkTaskRunIDs(ids []uuid.UUID) [][]uuid.UUID {
	if len(ids) <= maxTaskRunIDsPerStatement {
		return [][]uuid.UUID{ids}
	}
	chunks := make([][]uuid.UUID, 0, (len(ids)+maxTaskRunIDsPerStatement-1)/maxTaskRunIDsPerStatement)
	for start := 0; start < len(ids); start += maxTaskRunIDsPerStatement {
		end := min(start+maxTaskRunIDsPerStatement, len(ids))
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

// PruneDatasetMetrics deletes samples older than retention and returns how many
// rows went. A non-positive retention means "keep forever" and prunes nothing.
func PruneDatasetMetrics(ctx context.Context, conn *gorm.DB, retention time.Duration) (int64, error) {
	if conn == nil || retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-retention)
	res := conn.WithContext(ctx).
		Where("created_at <= ?", cutoff).
		Delete(&models.DatasetMetric{})
	return res.RowsAffected, res.Error
}

// StartDatasetMetricRetentionPruner runs PruneDatasetMetrics on a ticker until
// ctx is done. It is started from cmd/start only when
// CAESIUM_DATA_ASSERTIONS_ENABLED is set — with the feature off, no metrics are
// ever written, so there is nothing to prune and no goroutine to run.
func StartDatasetMetricRetentionPruner(ctx context.Context, conn *gorm.DB, retention time.Duration) {
	if conn == nil || retention <= 0 {
		return
	}

	interval := max(min(retention, defaultDatasetMetricPruneInterval), minDatasetMetricPruneInterval)
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				count, err := PruneDatasetMetrics(ctx, conn, retention)
				if err != nil {
					log.Error("dataset metric retention pruner failed", "error", err)
					continue
				}
				if count > 0 {
					log.Info("dataset metric retention pruner removed old rows", "count", count)
				}
			}
		}
	}()
}
