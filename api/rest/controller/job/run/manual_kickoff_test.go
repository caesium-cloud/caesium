package run

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	atomsvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	tasksvc "github.com/caesium-cloud/caesium/api/rest/service/task"
	edgesvc "github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/job"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

type manualObservedEngine struct {
	atom.Engine
	calls   atomic.Int32
	created chan struct{}
}

func (e *manualObservedEngine) Create(*atom.EngineCreateRequest) (atom.Atom, error) {
	e.calls.Add(1)
	e.created <- struct{}{}
	return nil, errors.New("observed hermetic runtime creation")
}

func TestManualHTTPFencesCancellationCommittedBeforeKickoffRegistration(t *testing.T) {
	for _, cancelBeforeRegistration := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancelled", false: "active-control"}[cancelBeforeRegistration], func(t *testing.T) {
			conn := jobdeftestutil.OpenTestDB(t)
			t.Cleanup(func() { jobdeftestutil.CloseDB(conn) })
			store := runstorage.NewStore(conn)
			j := models.Job{ID: uuid.New(), Alias: "manual-fence-" + uuid.NewString()}
			a := models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "hermetic-image", Command: `["true"]`, Spec: datatypes.JSON(`{}`)}
			task := models.Task{ID: uuid.New(), JobID: j.ID, AtomID: a.ID}
			require.NoError(t, conn.Create(&j).Error)
			require.NoError(t, conn.Create(&a).Error)
			require.NoError(t, conn.Create(&task).Error)
			oldStore, oldJob, oldStart, oldExecution, oldLaunch := postRunStore, postGetJob, postStartRun, runExecution, postLaunchRun
			postRunStore = func() *runstorage.Store { return store }
			postGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &j, nil }
			engine := &manualObservedEngine{created: make(chan struct{}, 1)}
			kickoffDone := make(chan struct{}, 1)
			postLaunchRun = func(ctx context.Context, m *models.Job, r *runstorage.JobRun, release func()) {
				oldLaunch(ctx, m, r, func() { release(); kickoffDone <- struct{}{} })
			}
			var executionCalls atomic.Int32
			runExecution = func(ctx context.Context, m *models.Job, params map[string]string) error {
				executionCalls.Add(1)
				return job.New(m,
					job.WithParams(params), job.WithRunStoreFactory(func() *runstorage.Store { return store }),
					job.WithEnvVariables(func() env.Environment { return env.Environment{ExecutionMode: "local", MaxParallelTasks: 1} }),
					job.WithTaskServiceFactory(func(ctx context.Context) tasksvc.Task { return tasksvc.ServiceWithDB(ctx, conn) }),
					job.WithAtomServiceFactory(func(ctx context.Context) atomsvc.Atom { return atomsvc.ServiceWithDB(ctx, conn) }),
					job.WithTaskEdgeServiceFactory(func(ctx context.Context) edgesvc.TaskEdge { return edgesvc.ServiceWithDB(ctx, conn) }),
					job.WithDockerEngineFactory(func(context.Context) atom.Engine { return engine }),
					job.WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error { return nil }),
				).Run(ctx)
			}
			t.Cleanup(func() {
				postRunStore, postGetJob, postStartRun, runExecution, postLaunchRun = oldStore, oldJob, oldStart, oldExecution, oldLaunch
			})
			var admitted uuid.UUID
			var originalCause string
			postStartRun = func(ctx context.Context, id uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error) {
				result, err := oldStart(ctx, id, opts...)
				if err == nil && result.Run != nil {
					admitted = result.Run.ID
					if cancelBeforeRegistration {
						require.NoError(t, store.CancelRun(context.Background(), admitted))
						require.Zero(t, job.CancelRunContexts(admitted), "cancellation must precede launcher registration")
						current, getErr := store.Get(admitted)
						require.NoError(t, getErr)
						originalCause = current.Error
					}
				}
				return result, err
			}
			owner := runlife.New(context.Background())
			t.Cleanup(func() {
				owner.CloseAndCancel()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, owner.Wait(ctx))
			})
			e := echo.New()
			e.POST("/v1/jobs/:id/run", Post)
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(runlife.WithSupervisor(t.Context(), owner), http.MethodPost, "/v1/jobs/"+j.ID.String()+"/run", strings.NewReader(`{}`))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			if !cancelBeforeRegistration {
				select {
				case <-engine.created:
				case <-time.After(5 * time.Second):
					t.Fatal("active control never reached real local runtime Create")
				}
			}
			// Keep the owner alive until the real launcher has finished fencing or
			// executing. Server cancellation must not hide a missing durable fence.
			select {
			case <-kickoffDone:
			case <-time.After(5 * time.Second):
				t.Fatal("manual kickoff did not finish while owner remained live")
			}
			owner.CloseAndCancel()
			waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer waitCancel()
			require.NoError(t, owner.Wait(waitCtx))
			row, err := store.Get(admitted)
			require.NoError(t, err)
			if cancelBeforeRegistration {
				require.Zero(t, executionCalls.Load())
				require.Zero(t, engine.calls.Load())
				require.Equal(t, runstorage.StatusCancelled, row.Status)
				require.Equal(t, originalCause, row.Error)
				var count int64
				require.NoError(t, conn.Model(&models.TaskRun{}).Where("job_run_id = ?", admitted).Count(&count).Error)
				require.Zero(t, count)
			} else {
				require.EqualValues(t, 1, engine.calls.Load())
			}
			require.Zero(t, job.CancelRunContexts(admitted))
		})
	}
}

func TestManualFenceFailsClosedAndStopsPermanentReadErrorsImmediately(t *testing.T) {
	oldGet, oldFinalize, oldExecution := postGetRun, postFinalizeCommittedRun, runExecution
	t.Cleanup(func() { postGetRun, postFinalizeCommittedRun, runExecution = oldGet, oldFinalize, oldExecution })
	for _, mode := range []string{"unreadable", "unknown", "missing", "cancelled-context", "cancel-during-read", "contention"} {
		t.Run(mode, func(t *testing.T) {
			id := uuid.New()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads, finalizes := 0, 0
			failure := errors.New("permanent read failure")
			postGetRun = func(got uuid.UUID) (*runstorage.JobRun, error) {
				require.Equal(t, id, got)
				reads++
				switch mode {
				case "unreadable":
					return nil, failure
				case "missing":
					return nil, nil
				case "unknown":
					return &runstorage.JobRun{ID: id, Status: "unknown"}, nil
				case "cancel-during-read":
					cancel()
				case "contention":
					if reads == 1 {
						return nil, errors.New("database is locked")
					}
				}
				return &runstorage.JobRun{ID: id, Status: runstorage.StatusRunning}, nil
			}
			postFinalizeCommittedRun = func(got uuid.UUID, cause error) (bool, error) {
				require.Equal(t, id, got)
				require.Error(t, cause)
				finalizes++
				return true, nil
			}
			executed := false
			runExecution = func(context.Context, *models.Job, map[string]string) error { executed = true; return nil }
			if mode == "cancelled-context" {
				cancel()
			}
			executeManualRun(ctx, &models.Job{ID: uuid.New()}, &runstorage.JobRun{ID: id})
			if mode == "contention" {
				require.True(t, executed)
				require.Equal(t, 2, reads)
				require.Zero(t, finalizes)
			} else {
				require.False(t, executed)
				require.Equal(t, 1, finalizes)
				if mode == "cancelled-context" {
					require.Zero(t, reads)
				} else {
					require.Equal(t, 1, reads)
				}
			}
		})
	}
}
