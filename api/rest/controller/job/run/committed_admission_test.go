package run

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"gorm.io/gorm/clause"
)

func TestManualHTTPFinalizesCommittedAdmissionBeforeReleasingOwnership(t *testing.T) {
	conn := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(conn) })
	store := runstorage.NewStore(conn)
	j := models.Job{ID: uuid.New(), Alias: "manual-committed-" + uuid.NewString()}
	require.NoError(t, conn.Create(&j).Error)
	other := models.JobRun{ID: uuid.New(), JobID: j.ID, Status: string(runstorage.StatusRunning)}
	require.NoError(t, conn.Create(&other).Error)
	oldStore, oldGet, oldExecution := postRunStore, postGetJob, runExecution
	postRunStore = func() *runstorage.Store { return store }
	postGetJob = func(ctx context.Context, id uuid.UUID) (*models.Job, error) {
		var row models.Job
		return &row, conn.WithContext(ctx).First(&row, "id = ?", id).Error
	}
	var engineCalls atomic.Int32
	var finalizationCalls atomic.Int32
	runExecution = func(context.Context, *models.Job, map[string]string) error { engineCalls.Add(1); return nil }
	t.Cleanup(func() { postRunStore, postGetJob, runExecution = oldStore, oldGet, oldExecution })
	injected := errors.New("manual post-insert readback failed")
	var committedID uuid.UUID
	injectedRead := false
	finalizing := make(chan struct{}, 1)
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	const createCallback = "test:manual_observe_commit"
	const queryCallback = "test:manual_fail_readback"
	const updateCallback = "test:manual_block_finalization"
	require.NoError(t, conn.Callback().Create().After("gorm:create").Register(createCallback, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*models.JobRun); ok {
			committedID = row.ID
		}
	}))
	require.NoError(t, conn.Callback().Query().Before("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
		// loadRun uses a joined Table("job_runs").First into an anonymous
		// projection; its parsed schema is not named JobRun. Match that exact
		// qualified-ID read, and assert it is outside the insertion transaction.
		if tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || committedID == uuid.Nil || injectedRead {
			return
		}
		where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
		if !ok {
			return
		}
		for _, condition := range where.Exprs {
			expr, ok := condition.(clause.Expr)
			if !ok || expr.SQL != "job_runs.id = ?" || len(expr.Vars) != 1 {
				continue
			}
			id, ok := expr.Vars[0].(uuid.UUID)
			if !ok || id != committedID {
				continue
			}
			_, insideTransaction := tx.Statement.ConnPool.(gorm.TxCommitter)
			require.False(t, insideTransaction, "fault must follow the committed insertion")
			injectedRead = true
			_ = tx.AddError(injected)
			return
		}
	}))
	require.NoError(t, conn.Callback().Update().Before("gorm:update").Register(updateCallback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "JobRun" {
			finalizationCalls.Add(1)
			var active models.JobRun
			require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).First(&active, "id = ?", committedID).Error)
			require.Equal(t, string(runstorage.StatusRunning), active.Status)
			finalizing <- struct{}{}
			<-finish
		}
	}))
	owner := runlife.New(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		owner.CloseAndCancel()
		unblock()
		<-done
		_ = conn.Callback().Create().Remove(createCallback)
		_ = conn.Callback().Query().Remove(queryCallback)
		_ = conn.Callback().Update().Remove(updateCallback)
	})
	e := echo.New()
	e.POST("/v1/jobs/:id/run", Post)
	request := func(ctx context.Context) *http.Request {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/jobs/"+j.ID.String()+"/run", strings.NewReader(`{"params":{"mode":"once"}}`))
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		r.Header.Set(IdempotencyKeyHeader, "committed-manual-start")
		return r
	}
	rec := httptest.NewRecorder()
	go func() { defer close(done); e.ServeHTTP(rec, request(runlife.WithSupervisor(t.Context(), owner))) }()
	select {
	case <-finalizing:
	case <-time.After(5 * time.Second):
		t.Fatal("manual committed error did not reach finalization")
	}
	owner.CloseAndCancel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded)
	unblock()
	<-done
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	require.NoError(t, owner.Wait(waitCtx))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "internal server error", body["message"])
	require.NotContains(t, rec.Body.String(), injected.Error())
	var actual, untouched models.JobRun
	require.NoError(t, conn.First(&actual, "id = ?", committedID).Error)
	require.NoError(t, conn.First(&untouched, "id = ?", other.ID).Error)
	require.Equal(t, string(runstorage.StatusFailed), actual.Status)
	require.Contains(t, actual.Error, injected.Error())
	require.NotNil(t, actual.CompletedAt)
	require.Equal(t, string(runstorage.StatusRunning), untouched.Status)
	require.Nil(t, untouched.CompletedAt)
	var taskCount int64
	require.NoError(t, conn.Model(&models.TaskRun{}).Where("job_run_id = ?", actual.ID).Count(&taskCount).Error)
	require.Zero(t, taskCount)
	replayOwner := runlife.New(context.Background())
	replayed := httptest.NewRecorder()
	e.ServeHTTP(replayed, request(runlife.WithSupervisor(t.Context(), replayOwner)))
	replayOwner.CloseAndCancel()
	replayWait, replayCancel := context.WithTimeout(context.Background(), time.Second)
	defer replayCancel()
	require.NoError(t, replayOwner.Wait(replayWait))
	require.Equal(t, http.StatusAccepted, replayed.Code)
	require.Equal(t, "true", replayed.Header().Get(IdempotentReplayedHeader))
	var result StartedRunResponse
	require.NoError(t, json.Unmarshal(replayed.Body.Bytes(), &result))
	require.NotNil(t, result.JobRun)
	require.Equal(t, actual.ID, result.ID)
	require.Equal(t, runstorage.StatusFailed, result.Status)
	require.Equal(t, actual.Error, result.Error)
	require.Zero(t, engineCalls.Load())
	require.EqualValues(t, 1, finalizationCalls.Load(), "same-key replay causes no duplicate completion")
	var runCount int64
	require.NoError(t, conn.Model(&models.JobRun{}).Where("job_id = ?", j.ID).Count(&runCount).Error)
	require.EqualValues(t, 2, runCount)
}

func TestManualHTTPLeavesDeclinedAndUncommittedAdmissionUnchanged(t *testing.T) {
	oldGet, oldStart, oldFinalize, oldLaunch := postGetJob, postStartRun, postFinalizeCommittedRun, postLaunchRun
	t.Cleanup(func() {
		postGetJob, postStartRun, postFinalizeCommittedRun, postLaunchRun = oldGet, oldStart, oldFinalize, oldLaunch
	})
	id := uuid.New()
	postGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &models.Job{ID: id}, nil }
	postFinalizeCommittedRun = func(uuid.UUID, error) (bool, error) {
		t.Fatal("non-committed admission reached finalization")
		return false, nil
	}
	postLaunchRun = func(context.Context, *models.Job, *runstorage.JobRun, func()) {
		t.Fatal("non-created admission launched")
	}
	for _, mode := range []string{"declined", "error"} {
		t.Run(mode, func(t *testing.T) {
			postStartRun = func(context.Context, uuid.UUID, ...runstorage.StartOption) (runstorage.StartResult, error) {
				if mode == "error" {
					return runstorage.StartResult{}, errors.New("no commit")
				}
				return runstorage.StartResult{}, nil
			}
			e := echo.New()
			e.POST("/v1/jobs/:id/run", Post)
			owner := runlife.New(context.Background())
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequestWithContext(runlife.WithSupervisor(t.Context(), owner), http.MethodPost, "/v1/jobs/"+id.String()+"/run", nil))
			owner.CloseAndCancel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, owner.Wait(ctx))
			if mode == "error" {
				require.Equal(t, http.StatusInternalServerError, rec.Code)
			} else {
				require.Equal(t, http.StatusAccepted, rec.Code)
				require.Empty(t, rec.Body.String())
			}
		})
	}
}
