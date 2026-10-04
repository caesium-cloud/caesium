package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	asvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	tsvc "github.com/caesium-cloud/caesium/api/rest/service/task"
	esvc "github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type retryReadFixture struct {
	db     *gorm.DB
	store  *runstorage.Store
	j      *models.Job
	entry  *runstorage.JobRun
	taskID uuid.UUID
}

func newRetryReadFixture(t *testing.T) *retryReadFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	tr := &models.Trigger{ID: uuid.New(), Type: models.TriggerTypeCron}
	require.NoError(t, db.Create(tr).Error)
	j := &models.Job{ID: uuid.New(), Alias: "retry-read-" + uuid.NewString(), TriggerID: tr.ID}
	require.NoError(t, db.Create(j).Error)
	a := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "frozen:1", Command: `["false"]`}
	require.NoError(t, db.Create(a).Error)
	task := &models.Task{ID: uuid.New(), JobID: j.ID, AtomID: a.ID, Name: "work"}
	require.NoError(t, db.Create(task).Error)
	store := runstorage.NewStore(db)
	r, err := store.Start(j.ID, nil, runstorage.WithStartParams(map[string]string{"input": "durable", "zero": "0"}))
	require.NoError(t, err)
	require.NoError(t, store.RegisterTask(r.ID, task, a, 0))
	require.NoError(t, store.FailTask(r.ID, task.ID, errors.New("first attempt failed")))
	require.NoError(t, store.Complete(r.ID, errors.New("first run failed")))
	entry, err := store.Get(r.ID)
	require.NoError(t, err)
	return &retryReadFixture{db: db, store: store, j: j, entry: entry, taskID: task.ID}
}

func (f *retryReadFixture) install(t *testing.T) {
	t.Helper()
	oldJob, oldRun, oldRetry, oldLaunch, oldExecute := retryGetJob, retryGetRun, retryFromFailure, retryLaunch, runExecution
	t.Cleanup(func() {
		retryGetJob, retryGetRun, retryFromFailure, retryLaunch, runExecution = oldJob, oldRun, oldRetry, oldLaunch, oldExecute
	})
	retryGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return f.j, nil }
	retryGetRun = func(_ context.Context, id uuid.UUID) (*runstorage.JobRun, error) { return f.store.Get(id) }
	retryLaunch = launchWholeRunRetry
	retryFromFailure = f.store.RetryFromFailure
}

func retryHTTP(t *testing.T, ctx context.Context, jobID, runID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.POST("/v1/jobs/:id/runs/:run_id/retry", Retry)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("/v1/jobs/%s/runs/%s/retry", jobID, runID), nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

type heldRetryEngine struct {
	atom.Engine // Create fails; no other engine operation is reached.
	created     chan<- atom.EngineCreateRequest
	finish      <-chan struct{}
}

func (e *heldRetryEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.created <- *req
	<-e.finish
	return nil, errors.New("test retry engine creation failed")
}

func TestWholeRetryHTTPPostCommitFailureLaunchesOwnedEngineWithDurableParams(t *testing.T) {
	f := newRetryReadFixture(t)
	f.install(t)
	fault := errors.New("post-commit retry refresh failed")
	reads := 0
	retryFromFailure = func(id uuid.UUID) (*runstorage.JobRun, error) {
		const callback = "test:http_retry_postcommit_refresh"
		require.NoError(t, f.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
			_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
			if tx.Statement.Table == "job_runs" && len(tx.Statement.Joins) > 0 && !transaction {
				reads++
				_ = tx.AddError(fault)
			}
		}))
		defer func() { require.NoError(t, f.db.Callback().Query().Remove(callback)) }()
		return f.store.RetryFromFailure(id)
	}
	created := make(chan atom.EngineCreateRequest, 1)
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	owner := runlife.New(context.Background())
	t.Cleanup(func() {
		unblock()
		owner.CloseAndCancel()
		wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, owner.Wait(wait))
	})
	runExecution = func(ctx context.Context, j *models.Job, params map[string]string) error {
		return job.New(j,
			job.WithParams(params), job.WithRunStoreFactory(func() *runstorage.Store { return f.store }),
			job.WithEnvVariables(func() env.Environment { return env.Environment{ExecutionMode: "local", MaxParallelTasks: 1} }),
			job.WithTaskServiceFactory(func(ctx context.Context) tsvc.Task { return tsvc.ServiceWithDB(ctx, f.db) }),
			job.WithAtomServiceFactory(func(ctx context.Context) asvc.Atom { return asvc.ServiceWithDB(ctx, f.db) }),
			job.WithTaskEdgeServiceFactory(func(ctx context.Context) esvc.TaskEdge { return esvc.ServiceWithDB(ctx, f.db) }),
			job.WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error { return nil }),
			job.WithDockerEngineFactory(func(context.Context) atom.Engine { return &heldRetryEngine{created: created, finish: finish} }),
		).Run(ctx)
	}
	rec := retryHTTP(t, runlife.WithSupervisor(t.Context(), owner), f.j.ID, f.entry.ID)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), fault.Error(), "read failure remains a diagnostic response")
	require.Equal(t, 1, reads)
	var req atom.EngineCreateRequest
	select {
	case req = <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("committed retry did not reach its engine")
	}
	require.Equal(t, "frozen:1", req.Image)
	require.Equal(t, "durable", req.Spec.Env["CAESIUM_PARAM_INPUT"])
	require.Equal(t, "0", req.Spec.Env["CAESIUM_PARAM_ZERO"])
	require.Equal(t, f.entry.ID.String(), req.Spec.Env["CAESIUM_RUN_ID"])
	owner.CloseAndCancel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded, "engine and persistence are still owned")
	unblock()
	wait, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	require.NoError(t, owner.Wait(wait))
	require.Zero(t, job.CancelRunContexts(f.entry.ID))
	row, err := f.store.Get(f.entry.ID)
	require.NoError(t, err)
	require.Equal(t, runstorage.StatusFailed, row.Status)
	require.Equal(t, f.entry.Params, row.Params)
}

func TestWholeRetryHTTPUnrelatedCommittedIdentityDoesNotLaunch(t *testing.T) {
	f := newRetryReadFixture(t)
	f.install(t)
	retryFromFailure = func(uuid.UUID) (*runstorage.JobRun, error) {
		return nil, fmt.Errorf("wrapped: %w", &runstorage.RunCommittedError{RunID: uuid.New(), JobID: f.j.ID, Err: errors.New("unrelated committed run")})
	}
	retryLaunch = func(context.Context, *models.Job, *runstorage.JobRun, func()) {
		t.Fatal("unrelated committed identity launched")
	}
	ctx := supervisedRequestContext(t)
	rec := retryHTTP(t, ctx, f.j.ID, f.entry.ID)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Zero(t, job.CancelRunContexts(f.entry.ID))
	owner := runlife.FromContext(ctx)
	owner.CloseAndCancel()
	require.NoError(t, owner.Wait(t.Context()))
}

func TestWholeRetryHTTPPreCommitFaultDoesNotLaunchOrReopen(t *testing.T) {
	f := newRetryReadFixture(t)
	f.install(t)
	fault := errors.New("retry transaction update refused")
	const callback = "test:http_retry_update_fault"
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" {
			_ = tx.AddError(fault)
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Update().Remove(callback) })
	retryLaunch = func(context.Context, *models.Job, *runstorage.JobRun, func()) { t.Fatal("rolled-back retry launched") }
	ctx := supervisedRequestContext(t)
	rec := retryHTTP(t, ctx, f.j.ID, f.entry.ID)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), fault.Error())
	require.Zero(t, job.CancelRunContexts(f.entry.ID))
	require.NoError(t, f.db.Callback().Update().Remove(callback))
	row, err := f.store.Get(f.entry.ID)
	require.NoError(t, err)
	require.Equal(t, runstorage.StatusFailed, row.Status)
	require.Equal(t, runstorage.TaskStatusFailed, row.Tasks[0].Status)
	owner := runlife.FromContext(ctx)
	owner.CloseAndCancel()
	require.NoError(t, owner.Wait(t.Context()))
}
