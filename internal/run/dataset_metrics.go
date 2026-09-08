package run

import (
	"context"
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
func InsertDatasetMetrics(ctx context.Context, conn *gorm.DB, rows []models.DatasetMetric) error {
	if conn == nil || len(rows) == 0 {
		return nil
	}
	return conn.WithContext(ctx).Create(&rows).Error
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
// KNOWN GAP (tracked as an issue, not closed here): InsertDatasetMetrics has no
// claim fence, so a superseded worker can still write samples onto a row it no
// longer owns AFTER a reclaim has cleared them. Every other terminal write on
// the row is claim-fenced; this one is not.
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
// resets that re-pend every in-flight row of a run in one statement.
func clearAttemptDatasetMetricsForTaskRunsTx(tx *gorm.DB, taskRunIDs []uuid.UUID) error {
	if tx == nil || len(taskRunIDs) == 0 {
		return nil
	}
	return tx.Where("task_run_id IN ?", taskRunIDs).Delete(&models.DatasetMetric{}).Error
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
