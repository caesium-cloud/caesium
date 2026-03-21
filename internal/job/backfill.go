package job

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	backfillstore "github.com/caesium-cloud/caesium/internal/backfill"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/robfig/cron"
	"golang.org/x/sync/semaphore"
)

// EnumerateLogicalDates returns all cron fire times in [start, end).
// loc sets the timezone used when computing schedule boundaries; pass time.UTC
// when the trigger has no timezone configured.
func EnumerateLogicalDates(schedule cron.Schedule, start, end time.Time, loc *time.Location) []time.Time {
	if loc == nil {
		loc = time.UTC
	}
	var dates []time.Time
	t := schedule.Next(start.In(loc).Add(-time.Second))
	for !t.IsZero() && t.Before(end) {
		dates = append(dates, t)
		t = schedule.Next(t)
	}
	return dates
}

// FilterDates filters logical dates based on the reprocess policy:
//
//	"none"   — skip dates that have any existing run
//	"failed" — skip dates whose latest run succeeded
//	"all"    — keep all dates
func FilterDates(store *backfillstore.Store, jobID uuid.UUID, dates []time.Time, reprocess string) ([]time.Time, error) {
	if reprocess == string(models.ReprocessAll) {
		return dates, nil
	}

	var filtered []time.Time
	for _, d := range dates {
		logicalDate := d.UTC().Format(time.RFC3339)
		status, err := store.LatestRunForLogicalDate(jobID, logicalDate)
		if err != nil {
			return nil, err
		}

		switch reprocess {
		case string(models.ReprocessNone):
			if status != "" {
				continue
			}
		case string(models.ReprocessFailed):
			if status == "succeeded" {
				continue
			}
		}
		filtered = append(filtered, d)
	}
	return filtered, nil
}

// RunBackfill executes a backfill by enumerating logical dates, filtering by
// the reprocess policy, and running each date through the standard job executor
// with a semaphore controlling max concurrency.
//
// It honours ctx cancellation: when cancelled, no new runs are started but
// any in-flight runs are allowed to finish.
func RunBackfill(
	ctx context.Context,
	b *models.Backfill,
	j *models.Job,
	schedule cron.Schedule,
	loc *time.Location,
) {
	bStore := backfillstore.Default()
	rStore := runstore.Default()

	metrics.BackfillsActive.WithLabelValues(j.Alias).Inc()
	defer metrics.BackfillsActive.WithLabelValues(j.Alias).Dec()

	dates := EnumerateLogicalDates(schedule, b.Start, b.End, loc)

	filtered, err := FilterDates(bStore, b.JobID, dates, b.Reprocess)
	if err != nil {
		log.Error("backfill: failed to filter dates", "backfill_id", b.ID, "error", err)
		if completeErr := bStore.Complete(b.ID, true); completeErr != nil {
			log.Error("backfill: failed to mark failed", "backfill_id", b.ID, "error", completeErr)
		}
		return
	}

	if err := bStore.SetTotalRuns(b.ID, len(filtered)); err != nil {
		log.Error("backfill: failed to set total_runs", "backfill_id", b.ID, "error", err)
	}

	if len(filtered) == 0 {
		log.Info("backfill: no dates to process", "backfill_id", b.ID)
		if err := bStore.Complete(b.ID, false); err != nil {
			log.Error("backfill: failed to complete", "backfill_id", b.ID, "error", err)
		}
		return
	}

	maxConcurrent := b.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	sem := semaphore.NewWeighted(int64(maxConcurrent))
	var wg sync.WaitGroup
	var failCount atomic.Int64
	cancelled := false

	for _, d := range filtered {
		select {
		case <-ctx.Done():
			cancelled = true
		default:
		}

		if cancelled {
			break
		}

		// Also poll the DB so that a cancel issued by a different process
		// (where no in-memory context exists) is honoured.
		if running, err := bStore.IsRunning(b.ID); err == nil && !running {
			cancelled = true
			break
		}

		if err := sem.Acquire(ctx, 1); err != nil {
			cancelled = true
			break
		}

		logicalDate := d.UTC().Format(time.RFC3339)
		params := map[string]string{"logical_date": logicalDate}

		r, err := rStore.StartForBackfill(j.ID, b.ID, params)
		if err != nil {
			sem.Release(1)
			log.Error("backfill: failed to create run", "backfill_id", b.ID, "logical_date", logicalDate, "error", err)
			failCount.Add(1)
			if incErr := bStore.IncrementFailed(b.ID); incErr != nil {
				log.Error("backfill: failed to increment failed_runs", "backfill_id", b.ID, "error", incErr)
			}
			continue
		}

		metrics.BackfillRunsTotal.WithLabelValues(j.Alias, "started").Inc()

		wg.Add(1)
		runID := r.ID
		go func(id uuid.UUID, ld string) {
			defer wg.Done()
			defer sem.Release(1)

			runCtx := runstore.WithContext(context.Background(), id)
			runErr := New(j, WithTriggerID(nil), WithParams(params)).Run(runCtx)
			if runErr != nil {
				log.Error("backfill: run failed", "backfill_id", b.ID, "logical_date", ld, "run_id", id, "error", runErr)
				metrics.BackfillRunsTotal.WithLabelValues(j.Alias, "failed").Inc()
				failCount.Add(1)
				if incErr := bStore.IncrementFailed(b.ID); incErr != nil {
					log.Error("backfill: failed to increment failed_runs", "backfill_id", b.ID, "error", incErr)
				}
			} else {
				metrics.BackfillRunsTotal.WithLabelValues(j.Alias, "succeeded").Inc()
				if incErr := bStore.IncrementCompleted(b.ID); incErr != nil {
					log.Error("backfill: failed to increment completed_runs", "backfill_id", b.ID, "error", incErr)
				}
			}
		}(runID, logicalDate)
	}

	wg.Wait()

	if cancelled {
		// Cancel writes the DB record; if it was already marked cancelled by
		// another process the WHERE status=running guard is a no-op.
		if cancelErr := bStore.Cancel(b.ID); cancelErr != nil {
			log.Error("backfill: failed to cancel", "backfill_id", b.ID, "error", cancelErr)
		}
		return
	}

	// Only mark complete if the record is still running (not externally cancelled).
	if running, err := bStore.IsRunning(b.ID); err != nil || !running {
		return
	}

	if err := bStore.Complete(b.ID, failCount.Load() > 0); err != nil {
		log.Error("backfill: failed to complete", "backfill_id", b.ID, "error", err)
	}
}
