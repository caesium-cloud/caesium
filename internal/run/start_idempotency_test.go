package run

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openIdempotencyStore(t *testing.T) (*gorm.DB, *Store) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return db, NewStore(db)
}

func countJobRuns(t *testing.T, db *gorm.DB, jobID uuid.UUID) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", jobID).Count(&count).Error)
	return count
}

// finishRun frees the concurrency slot a running run holds.
func finishRun(t *testing.T, db *gorm.DB, runID uuid.UUID) {
	t.Helper()
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).
		Update("status", string(StatusSucceeded)).Error)
}

// promoteQueued drains one queued start the way the dequeuer does.
func promoteQueued(t *testing.T, store *Store, jobID uuid.UUID) *JobRun {
	t.Helper()
	ctx := context.Background()
	queued, err := store.DequeueNextRun(ctx, jobID, "test-dequeuer")
	require.NoError(t, err)
	require.NotNil(t, queued, "a queued start must be waiting")
	started, err := store.StartQueuedRun(ctx, queued)
	require.NoError(t, err)
	require.NotNil(t, started)
	require.NoError(t, store.DeleteQueuedRun(ctx, queued))
	return started
}

func TestStartWithResultSameKeyReturnsOriginalRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-created", jobdef.ConcurrencyStrategyFail, 5)
	params := map[string]string{"region": "eu"}

	first, err := store.StartWithResult(context.Background(), job.ID, nil,
		WithStartParams(params), WithStartIdempotencyKey("wf-1/activity-3"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, first.Outcome)
	require.NotNil(t, first.Run)
	require.False(t, first.Replayed)

	second, err := store.StartWithResult(context.Background(), job.ID, nil,
		WithStartParams(params), WithStartIdempotencyKey("  wf-1/activity-3  "))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, second.Outcome)
	require.True(t, second.Replayed)
	require.NotNil(t, second.Run)
	require.Equal(t, first.Run.ID, second.Run.ID)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID), "a retried key must not admit a second run")

	// The key is scoped to the request, not the job: a new key is a new start.
	third, err := store.StartWithResult(context.Background(), job.ID, nil,
		WithStartParams(params), WithStartIdempotencyKey("wf-1/activity-4"))
	require.NoError(t, err)
	require.False(t, third.Replayed)
	require.NotEqual(t, first.Run.ID, third.Run.ID)
	require.Equal(t, int64(2), countJobRuns(t, db, job.ID))
}

func TestStartWithResultKeyIsScopedPerJob(t *testing.T) {
	db, store := openIdempotencyStore(t)
	jobA := createConcurrencyJob(t, db, "idem-scope-a", jobdef.ConcurrencyStrategyFail, 5)
	jobB := createConcurrencyJob(t, db, "idem-scope-b", jobdef.ConcurrencyStrategyFail, 5)

	a, err := store.StartWithResult(context.Background(), jobA.ID, nil, WithStartIdempotencyKey("shared"))
	require.NoError(t, err)
	b, err := store.StartWithResult(context.Background(), jobB.ID, nil, WithStartIdempotencyKey("shared"))
	require.NoError(t, err)
	require.False(t, b.Replayed)
	require.NotEqual(t, a.Run.ID, b.Run.ID)
}

func TestStartWithResultRejectsKeyReusedForDifferentRequest(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-reuse", jobdef.ConcurrencyStrategyFail, 5)

	_, err := store.StartWithResult(context.Background(), job.ID, nil,
		WithStartParams(map[string]string{"region": "eu"}), WithStartIdempotencyKey("k"))
	require.NoError(t, err)

	_, err = store.StartWithResult(context.Background(), job.ID, nil,
		WithStartParams(map[string]string{"region": "us"}), WithStartIdempotencyKey("k"))
	require.ErrorIs(t, err, ErrIdempotencyKeyReused)

	_, err = store.StartWithResult(context.Background(), job.ID, nil,
		WithStartParams(map[string]string{"region": "eu"}), WithStartPriority("high"), WithStartIdempotencyKey("k"))
	require.ErrorIs(t, err, ErrIdempotencyKeyReused, "priority is part of the request")

	require.Equal(t, int64(1), countJobRuns(t, db, job.ID))
}

func TestStartWithResultQueuedStartResolvesToPromotedRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-queue", jobdef.ConcurrencyStrategyQueue, 1)

	holder, err := store.StartWithResult(context.Background(), job.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, holder.Outcome)

	queued, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, queued.Outcome)
	require.NotNil(t, queued.QueueID, "a queued start must say which queue entry it is")
	require.Nil(t, queued.Run)

	again, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, again.Outcome)
	require.True(t, again.Replayed)
	require.Equal(t, *queued.QueueID, *again.QueueID)

	var depth int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Count(&depth).Error)
	require.Equal(t, int64(1), depth, "a retried queued start must not enqueue twice")

	finishRun(t, db, holder.Run.ID)
	promoted := promoteQueued(t, store, job.ID)

	resolved, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, resolved.Outcome, "the key must follow its queued start into the run it became")
	require.True(t, resolved.Replayed)
	require.NotNil(t, resolved.Run)
	require.Equal(t, promoted.ID, resolved.Run.ID)
}

func TestStartWithResultQueuedStartReportsDroppedWhenCancelled(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-dropped", jobdef.ConcurrencyStrategyQueue, 1)

	_, err := store.StartWithResult(context.Background(), job.ID, nil)
	require.NoError(t, err)
	queued, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, queued.Outcome)

	require.NoError(t, store.CancelQueuedRun(context.Background(), job.ID, *queued.QueueID))

	dropped, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeDropped, dropped.Outcome)
	require.True(t, dropped.Replayed)
	require.Equal(t, *queued.QueueID, *dropped.QueueID)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID), "a dropped start is not re-admitted")
}

func TestStartWithResultSkippedStartStaysSkipped(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-skip", jobdef.ConcurrencyStrategySkip, 1)

	holder, err := store.StartWithResult(context.Background(), job.ID, nil)
	require.NoError(t, err)

	skipped, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("s"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeSkipped, skipped.Outcome)
	require.Equal(t, StartSkipReasonMaxConcurrency, skipped.Reason)
	require.Nil(t, skipped.RunID)

	// One key is one admission decision: freeing the slot does not turn a
	// retry of the skipped start into a run.
	finishRun(t, db, holder.Run.ID)
	again, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("s"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeSkipped, again.Outcome)
	require.True(t, again.Replayed)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID))
}

func TestStartWithResultRefusedStartRecordsNothing(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-fail", jobdef.ConcurrencyStrategyFail, 1)

	holder, err := store.StartWithResult(context.Background(), job.ID, nil)
	require.NoError(t, err)

	_, err = store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("f"))
	require.ErrorIs(t, err, ErrMaxConcurrentRunsReached)
	var records int64
	require.NoError(t, db.Model(&models.RunStartIdempotency{}).Where("job_id = ?", job.ID).Count(&records).Error)
	require.Zero(t, records, "a refusal is not an outcome; the retry must re-attempt admission")

	finishRun(t, db, holder.Run.ID)
	retried, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("f"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, retried.Outcome)
	require.False(t, retried.Replayed)
}

func TestStartWithResultHeldStartReportsSkippedRun(t *testing.T) {
	setDataAssertions(t, true)
	db, store := openIdempotencyStore(t)
	dataset := "warehouse/orders"
	jobID := seedConsumerJob(t, db, "idem-held", dataset, "")
	seedActiveHold(t, db, dataset)

	held, err := store.StartWithResult(context.Background(), jobID, nil, WithStartIdempotencyKey("h"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeSkipped, held.Outcome)
	require.Equal(t, StartSkipReasonDatasetHold, held.Reason)
	require.NotNil(t, held.RunID, "a hold writes a terminal skipped run; the caller must learn its id")

	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", *held.RunID).Error)
	require.Equal(t, string(StatusSkipped), row.Status)

	again, err := store.StartWithResult(context.Background(), jobID, nil, WithStartIdempotencyKey("h"))
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Equal(t, *held.RunID, *again.RunID)
	require.Equal(t, int64(1), countJobRuns(t, db, jobID))
}

func TestStartWithResultWithoutKeyReportsQueueAndSkipOutcomes(t *testing.T) {
	db, store := openIdempotencyStore(t)
	queueJob := createConcurrencyJob(t, db, "outcome-queue", jobdef.ConcurrencyStrategyQueue, 1)
	skipJob := createConcurrencyJob(t, db, "outcome-skip", jobdef.ConcurrencyStrategySkip, 1)

	for _, job := range []*models.Job{queueJob, skipJob} {
		_, err := store.StartWithResult(context.Background(), job.ID, nil)
		require.NoError(t, err)
	}

	queued, err := store.StartWithResult(context.Background(), queueJob.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, queued.Outcome)
	require.NotNil(t, queued.QueueID)
	var row models.RunQueue
	require.NoError(t, db.First(&row, "id = ?", *queued.QueueID).Error, "queue id must name the real queue row")

	skipped, err := store.StartWithResult(context.Background(), skipJob.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeSkipped, skipped.Outcome)
	require.Equal(t, StartSkipReasonMaxConcurrency, skipped.Reason)

	var records int64
	require.NoError(t, db.Model(&models.RunStartIdempotency{}).Count(&records).Error)
	require.Zero(t, records, "starts without a key record nothing")
}

func TestStartWithResultConcurrentSameKeyAdmitsOnce(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-race", jobdef.ConcurrencyStrategyFail, 10)

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]StartResult, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("race"))
		}()
	}
	close(start)
	wg.Wait()

	var runID uuid.UUID
	fresh := 0
	for i := range callers {
		require.NoError(t, errs[i])
		require.Equal(t, StartOutcomeCreated, results[i].Outcome)
		require.NotNil(t, results[i].Run)
		if runID == uuid.Nil {
			runID = results[i].Run.ID
		}
		require.Equal(t, runID, results[i].Run.ID, "every caller must see the one admitted run")
		if !results[i].Replayed {
			fresh++
		}
	}
	require.Equal(t, 1, fresh, "exactly one caller admits; the rest replay")
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID))
}

func TestFindIdempotentStart(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-find", jobdef.ConcurrencyStrategyFail, 5)

	_, found, err := store.FindIdempotentStart(context.Background(), job.ID, WithStartIdempotencyKey("k"))
	require.NoError(t, err)
	require.False(t, found)

	_, found, err = store.FindIdempotentStart(context.Background(), job.ID)
	require.NoError(t, err)
	require.False(t, found, "no key, nothing to find")

	created, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("k"))
	require.NoError(t, err)

	got, found, err := store.FindIdempotentStart(context.Background(), job.ID, WithStartIdempotencyKey("k"))
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, got.Replayed)
	require.Equal(t, created.Run.ID, got.Run.ID)

	_, _, err = store.FindIdempotentStart(context.Background(), job.ID,
		WithStartParams(map[string]string{"x": "y"}), WithStartIdempotencyKey("k"))
	require.ErrorIs(t, err, ErrIdempotencyKeyReused)
}

func TestValidateIdempotencyKey(t *testing.T) {
	key, err := ValidateIdempotencyKey("  wf/run/activity  ")
	require.NoError(t, err)
	require.Equal(t, "wf/run/activity", key)

	key, err = ValidateIdempotencyKey("")
	require.NoError(t, err)
	require.Empty(t, key)

	_, err = ValidateIdempotencyKey(strings.Repeat("k", MaxIdempotencyKeyLength+1))
	require.ErrorIs(t, err, ErrInvalidIdempotencyKey)

	_, err = ValidateIdempotencyKey("bad\nkey")
	require.ErrorIs(t, err, ErrInvalidIdempotencyKey)
}

func TestStartRequestHashIgnoresParamOrderAndEnrichment(t *testing.T) {
	a := startRequestHash(map[string]string{"a": "1", "b": "2"}, "High")
	b := startRequestHash(map[string]string{"b": "2", "a": "1"}, " high ")
	require.Equal(t, a, b)
	require.NotEqual(t, a, startRequestHash(map[string]string{"a": "1", "b": "3"}, "high"))
	require.NotEqual(t, a, startRequestHash(map[string]string{"a": "1", "b": "2"}, ""))
	require.Equal(t, startRequestHash(nil, ""), startRequestHash(map[string]string{}, ""))
}

// TestStartWithResultUniqueConflictReadsBackWinner forces the race the
// up-front lookup cannot see: a concurrent start commits the same key after
// this start's lookup but before its transaction. The enricher seam runs in
// exactly that window, so it stands in for the concurrent winner.
func TestStartWithResultUniqueConflictReadsBackWinner(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-conflict", jobdef.ConcurrencyStrategyFail, 5)

	winner, err := store.StartWithResult(context.Background(), job.ID, nil)
	require.NoError(t, err)

	injected := false
	SetStartParamsEnricher(func(_ context.Context, conn *gorm.DB, jobID uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		if !injected {
			injected = true
			runID := winner.Run.ID
			require.NoError(t, conn.Create(&models.RunStartIdempotency{
				ID:             uuid.New(),
				JobID:          jobID,
				IdempotencyKey: "raced",
				RequestHash:    startRequestHash(nil, ""),
				Outcome:        string(StartOutcomeCreated),
				RunID:          &runID,
			}).Error)
		}
		return params, nil
	})
	t.Cleanup(func() { SetStartParamsEnricher(nil) })

	got, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("raced"))
	require.NoError(t, err)
	require.True(t, injected)
	require.True(t, got.Replayed, "the loser must answer with the winner's outcome")
	require.Equal(t, winner.Run.ID, got.Run.ID)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID), "the losing admission must roll back its run")
}

// TestReplayIdempotentStartRereadsRecordWhenQueueEntryIsGone covers a retry
// that reads a queued record just before the dequeuer promotes it: by the time
// it checks the queue the entry is deleted, but the record — rewritten in the
// promotion's own transaction — already names the run.
func TestReplayIdempotentStartRereadsRecordWhenQueueEntryIsGone(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-reread", jobdef.ConcurrencyStrategyQueue, 1)

	holder, err := store.StartWithResult(context.Background(), job.ID, nil)
	require.NoError(t, err)
	_, err = store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)

	stale, err := findStartIdempotency(db, job.ID, "q")
	require.NoError(t, err)
	require.Equal(t, string(StartOutcomeQueued), stale.Outcome)

	finishRun(t, db, holder.Run.ID)
	promoted := promoteQueued(t, store, job.ID)

	var result StartResult
	run, err := store.replayIdempotentStart(db, stale, startRequestHash(nil, ""), &result)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, result.Outcome, "a promoted start must never be reported as dropped")
	require.NotNil(t, run)
	require.Equal(t, promoted.ID, run.ID)
}

// TestStartWithResultRefusalRechecksKey covers the concurrent duplicate that
// loses the slot race under the fail strategy: the winner's run holds the only
// slot, so admission refuses before any insert could collide with the
// winner's record. The refusal must re-check the key and answer with the
// winner's run, not 409 the caller's own retry.
func TestStartWithResultRefusalRechecksKey(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-refusal", jobdef.ConcurrencyStrategyFail, 1)

	var (
		winnerID uuid.UUID
		raced    bool
	)
	SetStartParamsEnricher(func(_ context.Context, conn *gorm.DB, jobID uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		// The winner's own start runs this enricher too; only race once.
		if raced {
			return params, nil
		}
		raced = true
		// Stand in for the concurrent winner committing between this start's
		// lookup and its admission transaction.
		winner, err := store.StartWithResult(context.Background(), jobID, nil)
		require.NoError(t, err)
		winnerID = winner.Run.ID
		require.NoError(t, conn.Create(&models.RunStartIdempotency{
			ID:             uuid.New(),
			JobID:          jobID,
			IdempotencyKey: "raced",
			RequestHash:    startRequestHash(nil, ""),
			Outcome:        string(StartOutcomeCreated),
			RunID:          &winnerID,
		}).Error)
		return params, nil
	})
	t.Cleanup(func() { SetStartParamsEnricher(nil) })

	got, err := store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("raced"))
	require.NoError(t, err, "the caller's own retry must not be refused by its own run")
	require.True(t, got.Replayed)
	require.Equal(t, winnerID, got.Run.ID)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID))

	// Without a matching record the refusal stands.
	_, err = store.StartWithResult(context.Background(), job.ID, nil, WithStartIdempotencyKey("other"))
	require.ErrorIs(t, err, ErrMaxConcurrentRunsReached)
}
