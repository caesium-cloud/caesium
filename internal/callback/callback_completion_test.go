package callback

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// More than one process can finish the same run, so these tests dispatch from
// two independent handles standing in for two servers.

type countingTarget struct {
	*httptest.Server
	hits   atomic.Int32
	status atomic.Int32
}

func newCountingTarget(t *testing.T) *countingTarget {
	t.Helper()
	target := &countingTarget{}
	target.status.Store(http.StatusOK)
	target.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		target.hits.Add(1)
		w.WriteHeader(int(target.status.Load()))
	}))
	t.Cleanup(target.Close)
	return target
}

func dispatcherFor(db *gorm.DB, target *countingTarget) *Dispatcher {
	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(target.Client())
	return dispatcher
}

func callbackRows(t *testing.T, db *gorm.DB, runID uuid.UUID) []models.CallbackRun {
	t.Helper()
	var rows []models.CallbackRun
	require.NoError(t, db.Where("job_run_id = ?", runID).Order("retry_count asc").Find(&rows).Error)
	return rows
}

// A driver presumed dead thaws, or two processes wait on one run: whichever
// dispatches second delivers nothing.
func TestDispatchDeliversOncePerCompletion(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)

	require.NoError(t, dispatcherFor(dbA, target).Dispatch(context.Background(), jobID, runID, nil))
	require.NoError(t, dispatcherFor(dbB, target).Dispatch(context.Background(), jobID, runID, nil))

	require.Equal(t, int32(1), target.hits.Load())
	require.Len(t, callbackRows(t, dbA, runID), 1)
}

func TestConcurrentDispatchersHaveOneWinner(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)

	var failures atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		conn := dbA
		if i%2 == 1 {
			conn = dbB
		}
		wg.Go(func() {
			if err := dispatcherFor(conn, target).Dispatch(context.Background(), jobID, runID, nil); err != nil {
				failures.Add(1)
			}
		})
	}
	wg.Wait()

	require.Zero(t, failures.Load())
	require.Equal(t, int32(1), target.hits.Load())
}

// The claim is the row, so a failed delivery holds it too: a second dispatcher
// must not turn a failure into a silent retry. An operator still can, and more
// than once, because the claim must not get in the way of a deliberate re-send.
func TestAFailedDeliveryIsLeftToTheOperatorsRetry(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	target.status.Store(http.StatusInternalServerError)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)

	require.Error(t, dispatcherFor(dbA, target).Dispatch(context.Background(), jobID, runID, nil))
	require.NoError(t, dispatcherFor(dbB, target).Dispatch(context.Background(), jobID, runID, nil))
	require.Equal(t, int32(1), target.hits.Load())

	require.Error(t, dispatcherFor(dbA, target).RetryFailed(context.Background(), runID))
	target.status.Store(http.StatusOK)
	require.NoError(t, dispatcherFor(dbA, target).RetryFailed(context.Background(), runID))

	require.Equal(t, int32(3), target.hits.Load())
	rows := callbackRows(t, dbA, runID)
	require.Len(t, rows, 3)
	require.Equal(t, models.CallbackRunStatusSucceeded, rows[2].Status)
}

// A retried run is a new completion, and a whole-run retry has always notified.
func TestARetriedRunDeliversAgain(t *testing.T) {
	dbA, _ := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)
	store := run.NewStore(dbA)
	require.NoError(t, dbA.Model(&models.JobRun{}).Where("id = ?", runID).Update("status", string(run.StatusFailed)).Error)
	dispatcher := dispatcherFor(dbA, target)

	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))
	_, err := store.RetryFromFailure(runID)
	require.NoError(t, err)
	require.NoError(t, store.Complete(runID, nil))
	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))
	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))

	require.Equal(t, int32(2), target.hits.Load(), "one delivery per completion")
}

// A retry reopens and finishes the run just after a slower dispatcher read it.
// That dispatcher holds the old result, so it must claim the old completion: if
// its key came from a later read it would spend the retry's claim on the old
// payload, and the retry's real result would never go out.
func TestAStaleDispatcherCannotSpendARetrysClaim(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)
	var mu sync.Mutex
	var delivered []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var meta Metadata
		if err := json.NewDecoder(r.Body).Decode(&meta); err == nil {
			mu.Lock()
			delivered = append(delivered, meta.Status)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	jobID, runID := seedCallbackRun(t, dbA, server.URL)
	require.NoError(t, dbA.Model(&models.JobRun{}).Where("id = ?", runID).Update("status", string(run.StatusFailed)).Error)
	stale, retry := NewDispatcher(dbA), NewDispatcher(dbB)
	stale.WithHTTPClient(server.Client())
	retry.WithHTTPClient(server.Client())
	store := run.NewStore(dbB)

	fired := false
	require.NoError(t, dbA.Callback().Query().After("gorm:query").Register("test:retry-after-run-read", func(tx *gorm.DB) {
		if fired || tx.Statement.Table != "job_runs" {
			return
		}
		fired = true
		_, err := store.RetryFromFailure(runID)
		require.NoError(t, err)
		require.NoError(t, store.Complete(runID, nil))
	}))

	require.NoError(t, stale.Dispatch(context.Background(), jobID, runID, nil))
	require.True(t, fired)
	require.NoError(t, retry.Dispatch(context.Background(), jobID, runID, nil))

	require.Equal(t, []string{string(run.StatusFailed), string(run.StatusSucceeded)}, delivered,
		"each completion must report its own result")
}

// A claimer can die partway through a job's callbacks, and handlers can take a
// full timeout each. What it never attempted must still go out: a per-run claim
// would lose those, and a lost callback is worse than a duplicate.
func TestADeadClaimersUnattemptedCallbacksStillGoOut(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)

	var delivered models.Callback
	require.NoError(t, dbA.First(&delivered, "job_id = ?", jobID).Error)
	second := &models.Callback{ID: uuid.New(), JobID: jobID, Type: models.CallbackTypeNotification, Configuration: delivered.Configuration}
	require.NoError(t, dbA.Create(second).Error)

	state, err := run.NewStore(dbA).Get(runID)
	require.NoError(t, err)
	claimed, err := dispatcherFor(dbA, target).createAttempt(context.Background(), &models.CallbackRun{
		ID: uuid.New(), CallbackID: delivered.ID, JobID: jobID, JobRunID: runID,
		Status: models.CallbackRunStatusRunning, CompletionKey: completionKey(state),
	})
	require.NoError(t, err)
	require.True(t, claimed)

	require.NoError(t, dispatcherFor(dbB, target).Dispatch(context.Background(), jobID, runID, nil))

	require.Equal(t, int32(1), target.hits.Load(), "only the unattempted callback goes out")
	require.Len(t, callbackRows(t, dbA, runID), 2)
}

// Rows from before the column carry no key, and neither does an operator's
// re-send: neither may stand in for this completion's delivery.
func TestAnAttemptWithoutACompletionKeyClaimsNothing(t *testing.T) {
	dbA, _ := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)
	var cb models.Callback
	require.NoError(t, dbA.First(&cb, "job_id = ?", jobID).Error)
	dispatcher := dispatcherFor(dbA, target)

	for range 2 {
		claimed, err := dispatcher.createAttempt(context.Background(), &models.CallbackRun{
			ID: uuid.New(), CallbackID: cb.ID, JobID: jobID, JobRunID: runID, Status: models.CallbackRunStatusFailed,
		})
		require.NoError(t, err)
		require.True(t, claimed, "the index must not treat NULL keys as equal")
	}

	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))
	require.Equal(t, int32(1), target.hits.Load())
}

// A run from before timeout_started_at still has a stable completion.
func TestARunWithoutAnExecutionWindowStillDeliversOnce(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)
	require.NoError(t, dbA.Model(&models.JobRun{}).Where("id = ?", runID).UpdateColumn("timeout_started_at", nil).Error)

	require.NoError(t, dispatcherFor(dbA, target).Dispatch(context.Background(), jobID, runID, nil))
	require.NoError(t, dispatcherFor(dbB, target).Dispatch(context.Background(), jobID, runID, nil))

	require.Equal(t, int32(1), target.hits.Load())
}

// An engine whose completion write failed dispatches anyway. That must not use up
// the claim, or whoever really finalizes the run could not report it.
func TestADispatchBeforeTheRunIsFinalClaimsNothing(t *testing.T) {
	dbA, _ := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, runID := seedCallbackRun(t, dbA, target.URL)
	require.NoError(t, dbA.Model(&models.JobRun{}).Where("id = ?", runID).
		Updates(map[string]any{"status": string(run.StatusRunning), "completed_at": nil}).Error)
	dispatcher := dispatcherFor(dbA, target)

	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))
	require.NoError(t, run.NewStore(dbA).Complete(runID, errors.New("recovered")))
	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))

	require.Equal(t, int32(2), target.hits.Load())
}

// Without a readable completion there is nothing to claim. Dispatch reports it
// rather than deliver unfenced, the same as for a run it cannot load at all.
func TestDispatchReportsARunItCannotRead(t *testing.T) {
	dbA, _ := openConcurrentTestDBs(t)
	target := newCountingTarget(t)
	jobID, _ := seedCallbackRun(t, dbA, target.URL)

	err := dispatcherFor(dbA, target).Dispatch(context.Background(), jobID, uuid.New(), errors.New("run failed"))

	require.Error(t, err)
	require.Zero(t, target.hits.Load())
}
