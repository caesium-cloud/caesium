package run

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
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
