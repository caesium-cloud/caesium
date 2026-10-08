package job

import (
	"context"
	"errors"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOwnerCancellationAtCompletionBoundary(t *testing.T) {
	for _, phase := range []string{"normal", "early resume"} {
		for _, cancelAtHook := range []bool{false, true} {
			name := phase + "/live owner"
			if cancelAtHook {
				name = phase + "/canceled after recovery"
			}
			t.Run(name, func(t *testing.T) {
				db := jobdeftestutil.OpenTestDB(t)
				t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
				store := run.NewStore(db)
				jobID := uuid.New()
				model := &models.Job{ID: jobID, Alias: "owner-finalization-boundary", RunTimeout: time.Hour}
				require.NoError(t, db.Create(model).Error)
				jr, err := store.Start(jobID, nil)
				require.NoError(t, err)
				finished := time.Now().UTC().Add(-time.Second)
				running := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(run.TaskStatusRunning), ClaimedBy: "original-worker"}
				terminal := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(run.TaskStatusFailed), Error: "original task failure", CompletedAt: &finished, TerminalSequence: 4}
				require.NoError(t, db.Create(&running).Error)
				require.NoError(t, db.Create(&terminal).Error)
				var originalRunning, originalTerminal models.TaskRun
				require.NoError(t, db.First(&originalRunning, "id = ?", running.ID).Error)
				require.NoError(t, db.First(&originalTerminal, "id = ?", terminal.ID).Error)
				ownerCtx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				shutdownCause := errors.New("owner canceled immediately before completion")
				injected := false
				if phase == "early resume" {
					callback := "test:boundary_early_backend_read"
					require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
						_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
						if !injected && !transaction && tx.Statement.Table == "job_runs" && len(tx.Statement.Joins) > 0 {
							injected = true
							tx.AddError(context.Canceled) // Backend error, owner still live.
						}
					}))
					t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
				}
				var callbackCause error
				opts := withTestDeps(store, env.Environment{ExecutionMode: executionModeDistributed},
					&fakeTaskService{}, &fakeAtomService{}, &fakeTaskEdgeService{}, newFakeEngine())
				opts = append(opts, WithDispatchRunCallbacks(func(_ context.Context, _, _ uuid.UUID, cause error) error {
					callbackCause = cause
					return nil
				}))
				runner := New(model, opts...).(*job)
				hooked := false
				pending := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(run.TaskStatusPending), PartitionRetryPending: true}
				runner.beforeComplete = func(id uuid.UUID) {
					require.False(t, hooked, "this fixture reaches a single completion boundary")
					hooked = true
					require.Equal(t, jr.ID, id)
					require.NoError(t, ownerCtx.Err(), "cancellation must first land inside the hook")
					if cancelAtHook {
						// Model fresh retry acceptance after the recovery scan, then
						// real owner cancellation just before the terminal write.
						require.NoError(t, db.Create(&pending).Error)
						cancel(shutdownCause)
					}
				}
				returned := runner.Run(run.WithContext(ownerCtx, jr.ID))
				require.Error(t, returned)
				require.True(t, hooked)
				if phase == "early resume" {
					require.True(t, injected)
				}
				var gotRunning, gotTerminal models.TaskRun
				require.NoError(t, db.First(&gotRunning, "id = ?", running.ID).Error)
				require.NoError(t, db.First(&gotTerminal, "id = ?", terminal.ID).Error)
				require.Equal(t, originalTerminal, gotTerminal)
				if cancelAtHook {
					require.True(t, run.IsRunCancellationError(callbackCause))
					require.ErrorIs(t, callbackCause, shutdownCause)
					var gotPending models.TaskRun
					require.NoError(t, db.First(&gotPending, "id = ?", pending.ID).Error)
					for _, row := range []models.TaskRun{gotRunning, gotPending} {
						require.Equal(t, string(run.TaskStatusFailed), row.Status)
						require.Equal(t, shutdownCause.Error(), row.Error)
						require.NotNil(t, row.CompletedAt)
						require.Empty(t, row.ClaimedBy)
						require.False(t, row.PartitionRetryPending)
					}
				} else {
					require.NoError(t, ownerCtx.Err())
					require.False(t, run.IsRunCancellationError(callbackCause))
					require.Equal(t, originalRunning, gotRunning)
				}
			})
		}
	}
}
