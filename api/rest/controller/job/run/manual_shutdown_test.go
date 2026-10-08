package run

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestManualHTTPServerShutdownPreservesPreDispatchClaims(t *testing.T) {
	for _, mode := range []string{"before-read", "during-read", "read-error", "ordinary-cancel", "ordinary-read-error"} {
		t.Run(mode, func(t *testing.T) {
			conn := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(conn) })
			store := runstorage.NewStore(conn)
			j := models.Job{ID: uuid.New(), Alias: "manual-shutdown-" + uuid.NewString()}
			require.NoError(t, conn.Create(&j).Error)
			owner := runlife.New(t.Context())
			oldStore, oldJob, oldStart, oldGet, oldLaunch, oldFinalize, oldExecution := postRunStore, postGetJob, postStartRun, postGetRun, postLaunchRun, postFinalizeCommittedRun, runExecution
			postRunStore = func() *runstorage.Store { return store }
			postGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &j, nil }
			t.Cleanup(func() {
				postRunStore, postGetJob, postStartRun, postGetRun, postLaunchRun, postFinalizeCommittedRun, runExecution = oldStore, oldJob, oldStart, oldGet, oldLaunch, oldFinalize, oldExecution
			})
			var beforeRun models.JobRun
			var beforeTask models.TaskRun
			var beforeEvents []models.ExecutionEvent
			postStartRun = func(ctx context.Context, id uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error) {
				result, err := oldStart(ctx, id, opts...)
				if err != nil {
					return result, err
				}
				require.NotNil(t, result.Run)
				lease := time.Now().UTC().Add(time.Minute)
				beforeTask = models.TaskRun{ID: uuid.New(), JobRunID: result.Run.ID, TaskID: uuid.New(), Status: string(runstorage.TaskStatusRunning), ClaimedBy: "remote-worker", ClaimExpiresAt: &lease, RuntimeID: "remote-runtime"}
				require.NoError(t, conn.Create(&beforeTask).Error)
				require.NoError(t, conn.First(&beforeRun, "id = ?", result.Run.ID).Error)
				require.NoError(t, conn.First(&beforeTask, "id = ?", beforeTask.ID).Error)
				require.NoError(t, conn.Where("run_id = ?", result.Run.ID).Order("sequence").Find(&beforeEvents).Error)
				return result, nil
			}
			var reads, finalizes, executions atomic.Int32
			failure := errors.New("manual readiness read failed")
			postGetRun = func(id uuid.UUID) (*runstorage.JobRun, error) {
				reads.Add(1)
				current, err := store.Get(id)
				switch mode {
				case "during-read", "read-error":
					owner.CloseAndCancelCause(runlife.ErrServerShutdown)
				case "ordinary-cancel":
					owner.CloseAndCancel()
					// Observe real cancellation, rather than assuming AfterFunc ran.
				}
				if mode == "read-error" || mode == "ordinary-read-error" {
					return nil, failure
				}
				return current, err
			}
			postFinalizeCommittedRun = func(id uuid.UUID, cause error) (bool, error) {
				finalizes.Add(1)
				return store.CompleteIfActive(id, cause)
			}
			runExecution = func(context.Context, *models.Job, map[string]string) error { executions.Add(1); return nil }
			done := make(chan struct{}, 1)
			postLaunchRun = func(ctx context.Context, model *models.Job, row *runstorage.JobRun, release func()) {
				if mode == "before-read" {
					owner.CloseAndCancelCause(runlife.ErrServerShutdown)
				}
				// Use the production registration, goroutine, fence and release.
				oldLaunch(ctx, model, row, func() { release(); done <- struct{}{} })
			}
			t.Cleanup(func() {
				owner.CloseAndCancelCause(runlife.ErrServerShutdown)
				wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, owner.Wait(wait))
			})
			// Synchronize ordinary cancellation inside the actual read seam. The
			// shutdown cases deliberately need no callback scheduling assumption.
			if mode == "ordinary-cancel" {
				postLaunchRun = func(ctx context.Context, model *models.Job, row *runstorage.JobRun, release func()) {
					get := postGetRun
					postGetRun = func(id uuid.UUID) (*runstorage.JobRun, error) {
						current, err := get(id)
						select {
						case <-ctx.Done():
						case <-time.After(5 * time.Second):
							return nil, errors.New("ordinary cancellation did not reach reserved work")
						}
						return current, err
					}
					oldLaunch(ctx, model, row, func() { release(); done <- struct{}{} })
				}
			}
			e := echo.New()
			e.POST("/v1/jobs/:id/run", Post)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequestWithContext(runlife.WithSupervisor(t.Context(), owner), http.MethodPost, "/v1/jobs/"+j.ID.String()+"/run", nil))
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("manual launcher did not join")
			}
			require.Zero(t, executions.Load(), "pre-dispatch termination must not call the engine")
			var afterRun models.JobRun
			var afterTask models.TaskRun
			var afterEvents []models.ExecutionEvent
			require.NoError(t, conn.First(&afterRun, "id = ?", beforeRun.ID).Error)
			require.NoError(t, conn.First(&afterTask, "id = ?", beforeTask.ID).Error)
			require.NoError(t, conn.Where("run_id = ?", beforeRun.ID).Order("sequence").Find(&afterEvents).Error)
			if mode == "ordinary-cancel" || mode == "ordinary-read-error" {
				require.EqualValues(t, 1, finalizes.Load())
				require.Equal(t, string(runstorage.StatusFailed), afterRun.Status)
				require.NotNil(t, afterRun.CompletedAt)
				if mode == "ordinary-cancel" {
					require.Contains(t, afterRun.Error, context.Canceled.Error())
				} else {
					require.Contains(t, afterRun.Error, failure.Error())
				}
			} else {
				require.Zero(t, finalizes.Load())
				require.Equal(t, beforeRun, afterRun)
				require.Equal(t, beforeTask, afterTask)
				require.Equal(t, beforeEvents, afterEvents)
			}
			if mode == "before-read" {
				require.Zero(t, reads.Load())
			} else {
				require.EqualValues(t, 1, reads.Load())
			}
		})
	}
}

func TestManualHTTPServerShutdownPreservesCommittedReadbackFailure(t *testing.T) {
	for _, mode := range []string{"shutdown", "request-cancel-before-shutdown", "ordinary-cancel"} {
		t.Run(mode, func(t *testing.T) {
			conn := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(conn) })
			store := runstorage.NewStore(conn)
			j := models.Job{ID: uuid.New(), Alias: "manual-readback-shutdown-" + uuid.NewString()}
			require.NoError(t, conn.Create(&j).Error)
			owner := runlife.New(t.Context())
			requestCtx, cancelRequest := context.WithCancel(runlife.WithSupervisor(t.Context(), owner))
			defer cancelRequest()
			oldStore, oldJob, oldFinalize, oldLaunch := postRunStore, postGetJob, postFinalizeCommittedRun, postLaunchRun
			postRunStore = func() *runstorage.Store { return store }
			postGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &j, nil }
			finalizes, launches := 0, 0
			postFinalizeCommittedRun = func(id uuid.UUID, cause error) (bool, error) { finalizes++; return store.CompleteIfActive(id, cause) }
			postLaunchRun = func(context.Context, *models.Job, *runstorage.JobRun, func()) { launches++ }
			t.Cleanup(func() {
				postRunStore, postGetJob, postFinalizeCommittedRun, postLaunchRun = oldStore, oldJob, oldFinalize, oldLaunch
				owner.CloseAndCancel()
				wait, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				require.NoError(t, owner.Wait(wait))
			})
			var committedID uuid.UUID
			var beforeRun models.JobRun
			var beforeEvents []models.ExecutionEvent
			failure := errors.New("committed admission readback failed during shutdown")
			const createCallback = "test:manual_shutdown_observe_commit"
			const queryCallback = "test:manual_shutdown_readback"
			require.NoError(t, conn.Callback().Create().After("gorm:create").Register(createCallback, func(tx *gorm.DB) {
				if row, ok := tx.Statement.Dest.(*models.JobRun); ok {
					committedID = row.ID
				}
			}))
			injected := false
			require.NoError(t, conn.Callback().Query().Before("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
				if tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || committedID == uuid.Nil || injected {
					return
				}
				_, insideTransaction := tx.Statement.ConnPool.(gorm.TxCommitter)
				require.False(t, insideTransaction, "failure must follow the committed insertion")
				injected = true
				require.NoError(t, conn.First(&beforeRun, "id = ?", committedID).Error)
				require.NoError(t, conn.Where("run_id = ?", committedID).Order("sequence").Find(&beforeEvents).Error)
				if mode == "request-cancel-before-shutdown" {
					cancelRequest()
				}
				if mode == "ordinary-cancel" {
					owner.CloseAndCancel()
				} else {
					owner.CloseAndCancelCause(runlife.ErrServerShutdown)
				}
				_ = tx.AddError(failure)
			}))
			t.Cleanup(func() {
				_ = conn.Callback().Create().Remove(createCallback)
				_ = conn.Callback().Query().Remove(queryCallback)
			})
			e := echo.New()
			e.POST("/v1/jobs/:id/run", Post)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/v1/jobs/"+j.ID.String()+"/run", nil))
			require.True(t, injected)
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			require.NotContains(t, rec.Body.String(), failure.Error())
			require.Zero(t, launches)
			var afterRun models.JobRun
			var afterEvents []models.ExecutionEvent
			require.NoError(t, conn.First(&afterRun, "id = ?", committedID).Error)
			require.NoError(t, conn.Where("run_id = ?", committedID).Order("sequence").Find(&afterEvents).Error)
			if mode == "ordinary-cancel" {
				require.Equal(t, 1, finalizes)
				require.Equal(t, string(runstorage.StatusFailed), afterRun.Status)
				require.Contains(t, afterRun.Error, failure.Error())
				require.NotNil(t, afterRun.CompletedAt)
			} else {
				require.Zero(t, finalizes)
				require.Equal(t, beforeRun, afterRun)
				require.Equal(t, beforeEvents, afterEvents)
			}
		})
	}
}
