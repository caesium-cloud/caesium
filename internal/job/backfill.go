package job

import (
	"context"
	"errors"
	"fmt"
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

const backfillAcquirePollInterval = 250 * time.Millisecond

// Outcomes of trying to create one backfill date's run. They double as the
// caesium_backfill_runs_total result labels.
const (
	backfillDateStarted   = "started"
	backfillDateSkipped   = "skipped"
	backfillDateFailed    = "failed"
	backfillDateSucceeded = "succeeded"
)

// backfillDateOutcome classifies what a StartForBackfill error means for one
// date in the window.
//
// A REFUSED admission is not a failed date. The data circuit breaker's
// upstream-hold gate sits AHEAD of admit's backfill early-return, so backfilling
// a consumer of a held dataset returns ErrRunSkipped (which ErrRunHeldUpstream
// wraps) for every date in the window. Counting those as failures would mark the
// whole backfill failed and emit one `failed` sample per date, for runs the
// operator's own safety policy deliberately stopped — and each one already left
// a terminal `skipped` JobRun row carrying its reason, so nothing is lost by not
// counting it here.
//
// ErrRunQueued cannot reach this path (a backfill run is never enqueued) but is
// classified alongside it for the same reason: it did not fail.
func backfillDateOutcome(err error) string {
	switch {
	case err == nil:
		return backfillDateStarted
	case errors.Is(err, runstore.ErrRunSkipped), errors.Is(err, runstore.ErrRunQueued):
		return backfillDateSkipped
	default:
		return backfillDateFailed
	}
}

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
func FilterDates(store *backfillstore.Store, jobID uuid.UUID, dates []time.Time, reprocess models.ReprocessPolicy) ([]time.Time, error) {
	if reprocess == models.ReprocessAll {
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
		case models.ReprocessNone:
			if status != "" {
				continue
			}
		case models.ReprocessFailed:
			if status == "succeeded" {
				continue
			}
		}
		filtered = append(filtered, d)
	}
	return filtered, nil
}

type backfillStateReader interface {
	IsRunning(id uuid.UUID) (bool, error)
	IsCancelRequested(id uuid.UUID) (bool, error)
}

func shouldStopBackfill(ctx context.Context, store backfillStateReader, backfillID uuid.UUID) bool {
	select {
	case <-ctx.Done():
		return true
	default:
	}

	cancelRequested, err := store.IsCancelRequested(backfillID)
	if err != nil {
		log.Warn("backfill: failed to check cancel request", "backfill_id", backfillID, "error", err)
	} else if cancelRequested {
		return true
	}

	running, err := store.IsRunning(backfillID)
	if err != nil {
		log.Warn("backfill: failed to check running state", "backfill_id", backfillID, "error", err)
		return false
	}

	return !running
}

func waitForBackfillSlot(
	ctx context.Context,
	sem *semaphore.Weighted,
	shouldStop func() bool,
) bool {
	for {
		if shouldStop() {
			return false
		}

		acquireCtx, cancel := context.WithTimeout(ctx, backfillAcquirePollInterval)
		err := sem.Acquire(acquireCtx, 1)
		cancel()

		switch {
		case err == nil:
			return true
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			continue
		default:
			log.Error("backfill: failed to acquire concurrency slot", "error", err)
			return false
		}
	}
}

// RunBackfill executes a backfill by enumerating logical dates, filtering by
// the reprocess policy, and running each date through the standard job executor
// with a semaphore controlling max concurrency.
//
// It honours ctx cancellation: when cancelled, no new runs are started but
// any in-flight runs are allowed to finish. Server lifetime cancellation still
// reaches those independently reserved child runs.
func RunBackfill(
	ctx context.Context,
	b *models.Backfill,
	j *models.Job,
	schedule cron.Schedule,
	loc *time.Location,
) {
	runBackfill(ctx, b, j, schedule, loc, backfillstore.Default(), runstore.Default(), func(ctx context.Context, j *models.Job, params map[string]string) error {
		return New(j, WithTriggerID(nil), WithParams(params)).Run(ctx)
	})
}

// runBackfill keeps driver/store ownership in one lexical scope. Execution is
// injected privately so lifecycle tests exercise the real enumeration and joins.
func runBackfill(ctx context.Context, b *models.Backfill, j *models.Job, schedule cron.Schedule, loc *time.Location,
	bStore *backfillstore.Store, rStore *runstore.Store, execute func(context.Context, *models.Job, map[string]string) error) {

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
	var completed atomic.Int64
	// skipCount tracks dates admission refused on purpose (today: a consumed
	// dataset is held). It is reported in the summary log but deliberately kept
	// out of both the progress counters and the failed/succeeded verdict: a
	// skipped date neither ran nor failed.
	var skipCount atomic.Int64
	cancelled := false

	// Coalesce per-run progress into periodic batched writes. One UPDATE per run
	// on the same backfills row serializes through dqlite's single writer and can
	// starve concurrent control-plane writes (e.g. cancellation) past the
	// busy-retry budget. The flusher persists accumulated deltas ~once a second,
	// with a final flush below, so the counters lag at most one interval during
	// the run and are exact once it drains.
	flushStop := make(chan struct{})
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		const flushInterval = time.Second
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		var lastCompleted, lastFailed int64
		flush := func() {
			c, f := completed.Load(), failCount.Load()
			dc, df := c-lastCompleted, f-lastFailed
			if dc == 0 && df == 0 {
				return
			}
			if err := bStore.AddProgress(b.ID, dc, df); err != nil {
				log.Error("backfill: failed to flush progress", "backfill_id", b.ID, "error", err)
				return // keep last* so the delta retries on the next flush
			}
			lastCompleted, lastFailed = c, f
		}
		for {
			select {
			case <-flushStop:
				flush()
				return
			case <-ticker.C:
				flush()
			}
		}
	}()

	for _, d := range filtered {
		if shouldStopBackfill(ctx, bStore, b.ID) {
			cancelled = true
			break
		}

		if !waitForBackfillSlot(ctx, sem, func() bool {
			return shouldStopBackfill(ctx, bStore, b.ID)
		}) {
			cancelled = true
			break
		}

		if shouldStopBackfill(ctx, bStore, b.ID) {
			sem.Release(1)
			cancelled = true
			break
		}

		logicalDate := d.UTC().Format(time.RFC3339)
		params := map[string]string{"logical_date": logicalDate}

		workCtx, releaseWork, err := reserveLocalChild(ctx)
		if err != nil {
			sem.Release(1)
			cancelled = true
			log.Info("backfill: child submission refused", "backfill_id", b.ID, "logical_date", logicalDate, "error", err)
			break
		}
		r, err := rStore.StartForBackfill(j.ID, b.ID, params)
		if err != nil {
			// A failed readback can follow a committed insertion. Responsibility
			// for that exact row stays reserved until conditional finalization.
			if committedID, ok := runstore.CommittedRunID(err); ok {
				cause := fmt.Errorf("backfill: committed child admission failed: %w", err)
				// CompleteIfActive owns bounded retries for transient store contention.
				if _, completeErr := rStore.CompleteIfActive(committedID, cause); completeErr != nil {
					log.Error("backfill: committed child could not be finalized; leaving it for an operator",
						"backfill_id", b.ID, "run_id", committedID, "error", completeErr)
				}
			}
			releaseWork()
			sem.Release(1)
			if backfillDateOutcome(err) == backfillDateSkipped {
				log.Warn("backfill: date skipped by admission policy",
					"backfill_id", b.ID, "logical_date", logicalDate,
					"held_upstream", errors.Is(err, runstore.ErrRunHeldUpstream), "reason", err)
				metrics.BackfillRunsTotal.WithLabelValues(j.Alias, backfillDateSkipped).Inc()
				skipCount.Add(1)
				continue
			}
			log.Error("backfill: failed to create run", "backfill_id", b.ID, "logical_date", logicalDate, "error", err)
			failCount.Add(1)
			continue
		}

		if r == nil {
			releaseWork()
			sem.Release(1)
			skipCount.Add(1)
			metrics.BackfillRunsTotal.WithLabelValues(j.Alias, backfillDateSkipped).Inc()
			continue
		}
		cancelCtx, releaseCancel := RegisterRunCancel(workCtx, r.ID)
		metrics.BackfillRunsTotal.WithLabelValues(j.Alias, backfillDateStarted).Inc()

		wg.Add(1)
		runID := r.ID
		go func(id uuid.UUID, ld string) {
			defer wg.Done()
			defer sem.Release(1)
			defer releaseWork()
			defer releaseCancel()

			// Each backfill run is independently cancellable: cancelling one of
			// them must stop that run's container, not the whole backfill.
			runCtx := runstore.WithContext(cancelCtx, id)
			runErr := execute(runCtx, j, params)
			if runErr != nil {
				log.Error("backfill: run failed", "backfill_id", b.ID, "logical_date", ld, "run_id", id, "error", runErr)
				metrics.BackfillRunsTotal.WithLabelValues(j.Alias, backfillDateFailed).Inc()
				failCount.Add(1)
			} else {
				metrics.BackfillRunsTotal.WithLabelValues(j.Alias, backfillDateSucceeded).Inc()
				completed.Add(1)
			}
		}(runID, logicalDate)
	}

	wg.Wait()

	// Stop the flusher and persist any remaining counts before the terminal
	// status write, so completed_runs/failed_runs are exact once the backfill
	// finishes.
	close(flushStop)
	<-flusherDone

	if cancelled {
		// Mark terminal cancellation only after the backfill has stopped
		// launching new runs and all in-flight work has drained.
		if cancelErr := bStore.MarkCancelled(b.ID); cancelErr != nil {
			log.Error("backfill: failed to cancel", "backfill_id", b.ID, "error", cancelErr)
		}
		return
	}

	// Only mark complete if the record is still running (not externally cancelled).
	if running, err := bStore.IsRunning(b.ID); err != nil || !running {
		return
	}

	if skipped := skipCount.Load(); skipped > 0 {
		log.Warn("backfill: some dates were refused by admission policy and did not run",
			"backfill_id", b.ID, "skipped", skipped, "completed", completed.Load(), "failed", failCount.Load())
	}

	if err := bStore.Complete(b.ID, failCount.Load() > 0); err != nil {
		log.Error("backfill: failed to complete", "backfill_id", b.ID, "error", err)
	}
}
