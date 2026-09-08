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
// attempt that is about to be retried. It belongs to the SAME reset contract as
// retryResetColumns and is called wherever that map is applied — the columns
// live on task_runs, these rows live in another table, and both are the
// previous attempt's evidence.
//
// Without it a retried instance double-counts: the retry paths reuse one
// TaskRun row, so attempt 1's samples and attempt 2's samples both hang off it,
// and when the row finally lands `succeeded` the baseline reads BOTH as clean
// history of one logical run. That matters most for the case this feature
// creates: an onViolation: fail verdict is itself an attempt failure, so the
// value the breaker just rejected would come back as baseline history through
// the very retry it triggered.
//
// Best-effort by design in one respect only: it runs inside the caller's
// transaction, so a failure rolls the retry back rather than silently leaking
// rows.
func clearAttemptDatasetMetricsTx(tx *gorm.DB, taskRunID uuid.UUID) error {
	if tx == nil || taskRunID == uuid.Nil {
		return nil
	}
	return tx.Where("task_run_id = ?", taskRunID).Delete(&models.DatasetMetric{}).Error
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
