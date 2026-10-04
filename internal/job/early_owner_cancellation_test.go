package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEarlyResumedOwnerCancellationSettlesRealRows(t *testing.T) {
	for _, mode := range []string{"active", "quarantined", "durably cancelled"} {
		t.Run(mode, func(t *testing.T) {
			db := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
			store := run.NewStore(db)
			jobID, taskID := uuid.New(), uuid.New()
			model := &models.Job{ID: jobID, Alias: "early-resume-owner-cancel"}
			require.NoError(t, db.Create(model).Error)
			jr, err := store.Start(jobID, nil)
			require.NoError(t, err)
			if mode == "quarantined" {
				require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", jr.ID).Update("quarantine", true).Error)
			}
			finished := time.Now().UTC().Add(-time.Second)
			lease := time.Now().UTC().Add(time.Minute)
			rows := []models.TaskRun{
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(run.TaskStatusRunning), ClaimedBy: "worker", ClaimExpiresAt: &lease, RuntimeID: "owned-runtime", PartitionCount: 3, PartitionIndex: 0},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(run.TaskStatusPending), PartitionRetryPending: true, PartitionCount: 3, PartitionIndex: 1},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(run.TaskStatusFailed), Error: "prior task cause", CompletedAt: &finished, TerminalSequence: 7, PartitionCount: 3, PartitionIndex: 2},
			}
			require.NoError(t, db.Create(&rows).Error)
			if mode == "durably cancelled" {
				require.NoError(t, store.CancelRun(t.Context(), jr.ID))
			}
			var prior []models.TaskRun
			require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("id ASC").Find(&prior).Error)
			shutdownCause := errors.New("server stopped before resume snapshot")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			injected := false
			callback := "test:early_resolve_run_contention"
			require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
				if injected || transaction || tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 {
					return
				}
				injected = true
				// Actual Store.Get returned its joined read, then contention
				// makes resolveRun enter its retry wait. Cancellation wins there,
				// before the normal completion defer or task registration exists.
				tx.AddError(errors.New("database is locked"))
				cancel(shutdownCause)
			}))
			t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
			callbacks := 0
			var callbackCause error
			engine := newFakeEngine()
			opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeDistributed},
				&fakeTaskService{}, &fakeAtomService{}, &fakeTaskEdgeService{}, engine)
			opts = append(opts, WithDispatchRunCallbacks(func(_ context.Context, _, callbackRunID uuid.UUID, cause error) error {
				callbacks++
				callbackCause = cause
				require.Equal(t, jr.ID, callbackRunID)
				return nil
			}))
			returned := New(model, opts...).Run(run.WithContext(ctx, jr.ID))
			require.True(t, injected, "must cancel the actual resolveRun contention path")
			require.ErrorIs(t, returned, context.Canceled)
			var after []models.TaskRun
			require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("id ASC").Find(&after).Error)
			snapshot, err := store.Get(jr.ID)
			require.NoError(t, err)
			if mode == "durably cancelled" {
				require.Equal(t, run.StatusCancelled, snapshot.Status)
				require.Equal(t, "cancelled by concurrency replacement", snapshot.Error)
				require.Equal(t, prior, after)
				require.Zero(t, callbacks)
				return
			}
			require.Equal(t, run.StatusFailed, snapshot.Status)
			require.Equal(t, shutdownCause.Error(), snapshot.Error)
			var terminalOriginal models.TaskRun
			require.NoError(t, db.First(&terminalOriginal, "id = ?", rows[2].ID).Error)
			for _, original := range prior {
				if original.ID == rows[2].ID {
					require.Equal(t, original, terminalOriginal)
				}
			}
			for _, id := range []uuid.UUID{rows[0].ID, rows[1].ID} {
				var got models.TaskRun
				require.NoError(t, db.First(&got, "id = ?", id).Error)
				require.Equal(t, string(run.TaskStatusFailed), got.Status)
				require.Equal(t, shutdownCause.Error(), got.Error)
				require.NotNil(t, got.CompletedAt)
				require.Empty(t, got.ClaimedBy)
				require.Nil(t, got.ClaimExpiresAt)
				require.False(t, got.PartitionRetryPending)
			}
			require.Empty(t, engine.createRequestsForTask(taskID), "closed owner must not hand pending retry work to another engine")
			var failed []models.ExecutionEvent
			require.NoError(t, db.Where("run_id = ? AND type = ?", jr.ID, string(event.TypeTaskFailed)).Find(&failed).Error)
			require.Len(t, failed, 2)
			if mode == "quarantined" {
				require.Zero(t, callbacks)
				for _, evt := range failed {
					require.True(t, evt.Quarantine)
				}
			} else {
				require.Equal(t, 1, callbacks)
				require.True(t, run.IsRunCancellationError(callbackCause))
				require.ErrorIs(t, callbackCause, shutdownCause)
			}
		})
	}
}

func TestEarlyBackendCancelledReadRetainsLiveOwnerSemantics(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	jobID := uuid.New()
	model := &models.Job{ID: jobID, Alias: "early-live-owner-read-canceled"}
	require.NoError(t, db.Create(model).Error)
	jr, err := store.Start(jobID, nil)
	require.NoError(t, err)
	row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(run.TaskStatusRunning), ClaimedBy: "original-worker"}
	require.NoError(t, db.Create(&row).Error)
	var before models.TaskRun
	require.NoError(t, db.First(&before, "id = ?", row.ID).Error)
	injected := false
	callback := "test:early_backend_canceled_read"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if !injected && !transaction && tx.Statement.Table == "job_runs" && len(tx.Statement.Joins) > 0 {
			injected = true
			tx.AddError(context.Canceled)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	var completionCause error
	opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeDistributed},
		&fakeTaskService{}, &fakeAtomService{}, &fakeTaskEdgeService{}, newFakeEngine())
	opts = append(opts, WithDispatchRunCallbacks(func(_ context.Context, _, _ uuid.UUID, cause error) error {
		completionCause = cause
		return nil
	}))
	ownerCtx := t.Context()
	returned := New(model, opts...).Run(run.WithContext(ownerCtx, jr.ID))
	require.True(t, injected)
	require.ErrorIs(t, returned, context.Canceled)
	require.NoError(t, ownerCtx.Err())
	require.ErrorIs(t, completionCause, context.Canceled)
	require.False(t, run.IsRunCancellationError(completionCause))
	var after models.TaskRun
	require.NoError(t, db.First(&after, "id = ?", row.ID).Error)
	require.Equal(t, before, after, "a canceled backend read cannot sweep a live owner's task")
}
