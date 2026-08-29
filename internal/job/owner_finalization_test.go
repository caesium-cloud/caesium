package job

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/task"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func jobsActiveValue(t *testing.T, jobID uuid.UUID) float64 {
	t.Helper()
	var metric dto.Metric
	gauge, err := metrics.JobsActive.GetMetricWithLabelValues(jobID.String())
	require.NoError(t, err)
	require.NoError(t, gauge.(prometheus.Metric).Write(&metric))
	return metric.GetGauge().GetValue()
}

func TestFinishRunGenerationTakeoverFinalizesAndDispatchesCallback(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	leases := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(leases)
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	lease, err := leases.GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	observed := run.LeaseVersion{Generation: lease.Generation, StateRevision: lease.StateRevision}

	require.NoError(t, db.Model(&models.RunLease{}).
		Where("run_id = ?", runRecord.ID.String()).
		Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error)
	taken, err := leases.AcquireExpiredLeases(context.Background(), "node-b", time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), taken)

	var callbacks atomic.Int32
	j := &job{
		id: jobID,
		dispatchRunCallbacks: func(context.Context, uuid.UUID, uuid.UUID, error) error {
			callbacks.Add(1)
			return nil
		},
	}
	j.finishRun(context.Background(), store, runRecord.ID, false, nil, observed)

	require.Equal(t, int32(1), callbacks.Load())
	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", runRecord.ID).Error)
	require.Equal(t, string(run.StatusSucceeded), row.Status)
}

func TestFinishRunStaleRetryWaiterDoesNotDuplicateCallback(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	leases := run.NewLeaseStore(db)
	waiterStore := run.NewStore(db).WithLeaseStore(leases)
	retryStore := run.NewStore(db).WithLeaseStore(leases)
	jobID := uuid.New()
	runRecord, err := waiterStore.Start(jobID, nil)
	require.NoError(t, err)
	lease, err := leases.GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	firstRevision := run.LeaseVersion{Generation: lease.Generation, StateRevision: lease.StateRevision}

	var callbacks atomic.Int32
	j := &job{
		id: jobID,
		dispatchRunCallbacks: func(context.Context, uuid.UUID, uuid.UUID, error) error {
			callbacks.Add(1)
			return nil
		},
	}

	staleReady := make(chan struct{})
	releaseStale := make(chan struct{})
	staleDone := make(chan struct{})
	go func() {
		defer close(staleDone)
		close(staleReady)
		<-releaseStale
		j.finishRun(context.Background(), waiterStore, runRecord.ID, false, nil, firstRevision)
	}()
	<-staleReady

	// The first execution is terminalized by the owner while its SQL waiter is
	// delayed. A real retry then reopens the same run and bumps StateRevision.
	disposition, err := retryStore.CompleteAtStateRevision(
		runRecord.ID, fmt.Errorf("first execution failed"), firstRevision.StateRevision,
	)
	require.NoError(t, err)
	require.Equal(t, run.CompletionFinalized, disposition)
	_, err = retryStore.RetryFromFailure(runRecord.ID)
	require.NoError(t, err)
	lease, err = leases.GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	secondRevision := run.LeaseVersion{Generation: lease.Generation, StateRevision: lease.StateRevision}
	require.Greater(t, secondRevision.StateRevision, firstRevision.StateRevision)

	// The current waiter owns terminalization and callback delivery. Releasing
	// the stale waiter afterward must not deliver the retried epoch again.
	j.finishRun(context.Background(), retryStore, runRecord.ID, false, nil, secondRevision)
	close(releaseStale)
	select {
	case <-staleDone:
	case <-time.After(time.Second):
		t.Fatal("stale waiter did not finish")
	}

	require.Equal(t, int32(1), callbacks.Load())
	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", runRecord.ID).Error)
	require.Equal(t, string(run.StatusSucceeded), row.Status)
}

func TestFinishRunAlreadyTerminalUsesDurableWinningOutcome(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	leases := run.NewLeaseStore(db)
	waiterStore := run.NewStore(db).WithLeaseStore(leases)
	winnerStore := run.NewStore(db).WithLeaseStore(run.NewLeaseStore(db))
	jobID := uuid.New()
	runRecord, err := waiterStore.Start(jobID, nil)
	require.NoError(t, err)
	lease, err := leases.GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	observed := run.LeaseVersion{Generation: lease.Generation, StateRevision: lease.StateRevision}

	durableFailure := errors.New("durable owner failure")
	disposition, err := winnerStore.CompleteAtStateRevision(
		runRecord.ID, durableFailure, observed.StateRevision,
	)
	require.NoError(t, err)
	require.Equal(t, run.CompletionFinalized, disposition)

	callbackErrors := make(chan error, 1)
	j := &job{
		id: jobID,
		dispatchRunCallbacks: func(_ context.Context, _, _ uuid.UUID, callbackErr error) error {
			callbackErrors <- callbackErr
			return nil
		},
	}
	// The losing waiter has a stale local success result. Because the durable
	// winner already finalized the exact revision as failed, its callback must
	// carry the durable failure rather than the waiter's nil result.
	j.finishRun(context.Background(), waiterStore, runRecord.ID, false, nil, observed)

	select {
	case callbackErr := <-callbackErrors:
		require.EqualError(t, callbackErr, durableFailure.Error())
	default:
		t.Fatal("expected one durable callback")
	}
	select {
	case extra := <-callbackErrors:
		t.Fatalf("unexpected duplicate callback: %v", extra)
	default:
	}
}

func TestFinishRunCancellationFencesRetryEpochCallbacks(t *testing.T) {
	metrics.JobsActive.Reset()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	leases := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(leases)
	remote := run.NewStore(db).WithLeaseStore(run.NewLeaseStore(db))
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	firstLease, err := leases.GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	first := run.LeaseVersion{Generation: firstLease.Generation, StateRevision: firstLease.StateRevision}

	// A remote owner closes revision 1, leaving this Store's original local
	// gauge entry alive. The retry transaction registers a distinct revision 2.
	disposition, err := remote.CompleteAtStateRevision(runRecord.ID, errors.New("first failed"), first.StateRevision)
	require.NoError(t, err)
	require.Equal(t, run.CompletionFinalized, disposition)
	_, secondRevision, err := store.RetryFromFailureVersion(runRecord.ID)
	require.NoError(t, err)
	require.Greater(t, secondRevision, first.StateRevision)
	require.Equal(t, float64(2), jobsActiveValue(t, jobID))

	require.NoError(t, store.CancelRun(context.Background(), runRecord.ID))
	require.Equal(t, float64(0), jobsActiveValue(t, jobID),
		"cancellation must retire every local retry epoch without waiting for stale runners")
	retiredLease, err := leases.GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	require.Equal(t, secondRevision, retiredLease.StateRevision)
	require.NotEqual(t, firstLease.OwnerNode, retiredLease.OwnerNode)
	taken, err := leases.AcquireExpiredLeases(context.Background(), "node-after-cancel", time.Minute)
	require.NoError(t, err)
	require.Zero(t, taken, "a terminal lease tombstone must never be taken over")

	callbackErrors := make(chan error, 2)
	jobModel := &models.Job{ID: jobID, Alias: "cancelled-retry-epoch"}
	newRunner := func(expected int64) Job {
		return New(jobModel,
			WithRunStoreFactory(func() *run.Store { return store }),
			WithTaskServiceFactory(func(context.Context) task.Task { return &fakeTaskService{} }),
			WithExpectedStateRevision(expected),
			WithDispatchRunCallbacks(func(_ context.Context, _, _ uuid.UUID, callbackErr error) error {
				callbackErrors <- callbackErr
				return nil
			}),
		)
	}

	// Both retry runners are delayed until after cancellation. The stale
	// revision-1 runner rejects at startup; the exact revision-2 runner sees the
	// retired terminal lease and delivers only the durable cancellation callback
	// without entering task registration or execution.
	err = newRunner(first.StateRevision).Run(run.WithContext(context.Background(), runRecord.ID))
	require.ErrorIs(t, err, run.ErrOwnerStateChanged)
	require.Equal(t, float64(0), jobsActiveValue(t, jobID))

	err = newRunner(secondRevision).Run(run.WithContext(context.Background(), runRecord.ID))
	require.NoError(t, err)
	select {
	case callbackErr := <-callbackErrors:
		require.Error(t, callbackErr)
		require.Contains(t, callbackErr.Error(), "cancelled")
	default:
		t.Fatal("current cancelled revision did not deliver its callback")
	}
	select {
	case extra := <-callbackErrors:
		t.Fatalf("stale revision delivered a duplicate callback: %v", extra)
	default:
	}
}

func TestRetryRunnerBindsTransactionRevisionAndRetiresSupersededEpoch(t *testing.T) {
	metrics.JobsActive.Reset()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	leases := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(leases)
	remote := run.NewStore(db).WithLeaseStore(run.NewLeaseStore(db))
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	// Advance through revisions 1 and 2 before the revision-2 resumed goroutine
	// starts. The delayed runner must reject revision 3 and retire only its own
	// process-local epoch instead of silently blessing the newer retry.
	disposition, err := store.CompleteAtStateRevision(runRecord.ID, errors.New("revision 1 failed"), 1)
	require.NoError(t, err)
	require.Equal(t, run.CompletionFinalized, disposition)
	_, revision2, err := store.RetryFromFailureVersion(runRecord.ID)
	require.NoError(t, err)
	disposition, err = remote.CompleteAtStateRevision(runRecord.ID, errors.New("revision 2 failed"), revision2)
	require.NoError(t, err)
	require.Equal(t, run.CompletionFinalized, disposition)
	_, revision3, err := store.RetryFromFailureVersion(runRecord.ID)
	require.NoError(t, err)
	require.Greater(t, revision3, revision2)
	require.Equal(t, float64(2), jobsActiveValue(t, jobID))

	var callbacks atomic.Int32
	jobModel := &models.Job{ID: jobID, Alias: "revision-binding"}
	newRunner := func(expected int64) Job {
		return New(jobModel,
			WithRunStoreFactory(func() *run.Store { return store }),
			WithTaskServiceFactory(func(context.Context) task.Task { return &fakeTaskService{} }),
			WithExpectedStateRevision(expected),
			WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error {
				callbacks.Add(1)
				return nil
			}),
		)
	}

	err = newRunner(revision2).Run(run.WithContext(context.Background(), runRecord.ID))
	require.ErrorIs(t, err, run.ErrOwnerStateChanged)
	require.Equal(t, int32(0), callbacks.Load())
	require.Equal(t, float64(1), jobsActiveValue(t, jobID))

	err = newRunner(revision3).Run(run.WithContext(context.Background(), runRecord.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no tasks")
	require.Equal(t, int32(1), callbacks.Load())
	require.Equal(t, float64(0), jobsActiveValue(t, jobID))
}
