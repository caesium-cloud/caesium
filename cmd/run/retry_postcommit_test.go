package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestLocalWholeRetryPostCommitReadFailureTransfersRegistrationAndParams(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	tr := &models.Trigger{ID: uuid.New(), Type: models.TriggerTypeCron}
	require.NoError(t, db.Create(tr).Error)
	j := &models.Job{ID: uuid.New(), Alias: "local-retry-read-" + uuid.NewString(), TriggerID: tr.ID}
	require.NoError(t, db.Create(j).Error)
	store := runstorage.NewStore(db)
	r, err := store.Start(j.ID, nil, runstorage.WithStartParams(map[string]string{"input": "durable"}))
	require.NoError(t, err)
	require.NoError(t, store.Complete(r.ID, errors.New("first attempt failed")))
	entry, err := store.Get(r.ID)
	require.NoError(t, err)
	fault := errors.New("local retry refresh failed")
	const callback = "test:local_retry_postcommit_refresh"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if tx.Statement.Table == "job_runs" && len(tx.Statement.Joins) > 0 && !transaction {
			_ = tx.AddError(fault)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	var launched *runstorage.JobRun
	var registered context.Context
	var release func()
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	returned, err := startLocalWholeRunRetry(t.Context(), j, r.ID, entry, store.RetryFromFailure,
		func(ctx context.Context, _ *models.Job, run *runstorage.JobRun, done func()) {
			registered, launched, release = ctx, run, done
		})
	require.Nil(t, returned, "the CLI must still report its read error")
	require.ErrorIs(t, err, fault)
	committedID, committed := runstorage.CommittedRunID(err)
	require.True(t, committed)
	require.Equal(t, r.ID, committedID)
	require.NotNil(t, launched)
	require.Equal(t, r.ID, launched.ID)
	require.Equal(t, j.ID, launched.JobID)
	require.Equal(t, runstorage.StatusRunning, launched.Status)
	require.Equal(t, entry.Params, launched.Params)
	require.Empty(t, launched.Tasks, "the engine must reload reset tasks rather than reuse pre-retry state")
	require.NoError(t, registered.Err(), "helper return must not release transferred registration")
	require.Equal(t, 1, job.CancelRunContexts(r.ID))
	require.ErrorIs(t, registered.Err(), context.Canceled)
	release()
	require.Zero(t, job.CancelRunContexts(r.ID))
	require.NoError(t, db.Callback().Query().Remove(callback))
	row, loadErr := store.Get(r.ID)
	require.NoError(t, loadErr)
	require.Equal(t, runstorage.StatusRunning, row.Status)
	require.Equal(t, entry.Params, row.Params)
}

func TestLocalWholeRetryRejectsUnrelatedCommittedOrPreloadedIdentity(t *testing.T) {
	for _, kind := range []string{"unrelated marker", "unrelated preloaded run", "unrelated preloaded job", "ordinary failure"} {
		t.Run(kind, func(t *testing.T) {
			j := &models.Job{ID: uuid.New()}
			runID := uuid.New()
			entry := &runstorage.JobRun{ID: runID, JobID: j.ID, Params: map[string]string{"input": "durable"}}
			markerID := runID
			fault := errors.New("retry failed")
			admissionErr := fmt.Errorf("wrapped: %w", &runstorage.RunCommittedError{RunID: markerID, JobID: j.ID, Err: fault})
			switch kind {
			case "unrelated marker":
				admissionErr = &runstorage.RunCommittedError{RunID: uuid.New(), JobID: j.ID, Err: fault}
			case "unrelated preloaded run":
				entry.ID = uuid.New()
			case "unrelated preloaded job":
				entry.JobID = uuid.New()
			default:
				admissionErr = fault
			}
			_, err := startLocalWholeRunRetry(context.Background(), j, runID, entry,
				func(uuid.UUID) (*runstorage.JobRun, error) { return nil, admissionErr },
				func(context.Context, *models.Job, *runstorage.JobRun, func()) {
					t.Fatal("unvalidated committed retry launched")
				})
			require.ErrorIs(t, err, fault)
			require.Zero(t, job.CancelRunContexts(runID))
		})
	}
}

func TestLocalWholeRetryWrappedMatchingMarkerRetainsRegistration(t *testing.T) {
	j := &models.Job{ID: uuid.New()}
	entry := &runstorage.JobRun{ID: uuid.New(), JobID: j.ID, Params: map[string]string{"zero": "0"}}
	fault := errors.New("refresh failed")
	diagnostic := fmt.Errorf("wrapped: %w", &runstorage.RunCommittedError{RunID: entry.ID, JobID: j.ID, Err: fault})
	started := make(chan context.Context, 1)
	finish := make(chan struct{})
	done := make(chan struct{})
	_, err := startLocalWholeRunRetry(t.Context(), j, entry.ID, entry,
		func(uuid.UUID) (*runstorage.JobRun, error) { return nil, diagnostic },
		func(ctx context.Context, _ *models.Job, got *runstorage.JobRun, release func()) {
			require.Equal(t, entry.Params, got.Params)
			go func() { defer close(done); defer release(); started <- ctx; <-finish }()
		})
	require.Same(t, diagnostic, err, "CLI preserves the original returned error")
	ctx := <-started
	require.NoError(t, ctx.Err())
	require.Equal(t, 1, job.CancelRunContexts(entry.ID))
	close(finish)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry did not release")
	}
	require.Zero(t, job.CancelRunContexts(entry.ID))
}
