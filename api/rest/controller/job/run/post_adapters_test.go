package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestManualHTTPServiceAdaptersKeepTheAuthoritativeStoreAndOwner(t *testing.T) {
	conn := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(conn) })
	store := runstorage.NewStore(conn)
	j := models.Job{ID: uuid.New(), Alias: "manual-adapter-" + uuid.NewString()}
	require.NoError(t, conn.Create(&j).Error)
	oldStore, oldGet, oldLaunch := postRunStore, postGetJob, postLaunchRun
	postRunStore = func() *runstorage.Store { return store }
	postGetJob = func(ctx context.Context, id uuid.UUID) (*models.Job, error) {
		var row models.Job
		return &row, conn.WithContext(ctx).First(&row, "id = ?", id).Error
	}
	t.Cleanup(func() { postRunStore, postGetJob, postLaunchRun = oldStore, oldGet, oldLaunch })
	owner := runlife.New(t.Context())
	type requestKey struct{}
	requestCtx, cancelRequest := context.WithTimeout(context.WithValue(t.Context(), requestKey{}, "request-value"), time.Minute)
	defer cancelRequest()
	requestCtx = runlife.WithSupervisor(requestCtx, owner)
	observedAdmission := false
	require.NoError(t, conn.Callback().Create().Before("gorm:create").Register("test:manual_adapter_cancel_request", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "JobRun" {
			return
		}
		observedAdmission = true
		cancelRequest()
		require.Equal(t, "request-value", tx.Statement.Context.Value(requestKey{}))
		require.NoError(t, tx.Statement.Context.Err(), "request cancellation must not abort durable admission")
		_, deadline := tx.Statement.Context.Deadline()
		require.False(t, deadline)
	}))
	var executionCtx context.Context
	var release func()
	launches := 0
	postLaunchRun = func(ctx context.Context, _ *models.Job, run *runstorage.JobRun, releaseWork func()) {
		launches++
		executionCtx, release = ctx, releaseWork
		require.Equal(t, "request-value", ctx.Value(requestKey{}))
		require.NoError(t, ctx.Err())
		require.Equal(t, map[string]string{"mode": "once"}, run.Params)
	}
	t.Cleanup(func() {
		owner.CloseAndCancel()
		if release != nil {
			release()
		}
	})
	e := echo.New()
	e.POST("/v1/jobs/:id/run", Post)
	request := func(ctx context.Context) *http.Request {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/jobs/"+j.ID.String()+"/run", strings.NewReader(`{"params":{"mode":"once"}}`))
		r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		r.Header.Set(IdempotencyKeyHeader, "manual-adapter")
		return r
	}
	first := httptest.NewRecorder()
	e.ServeHTTP(first, request(requestCtx))
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	require.True(t, observedAdmission)
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.NotNil(t, release)
	var admitted StartedRunResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &admitted))
	require.NotNil(t, admitted.JobRun)
	row, err := store.Get(admitted.ID)
	require.NoError(t, err)
	require.Equal(t, admitted.ID, row.ID)
	require.NoError(t, conn.Model(&models.Job{}).Where("id = ?", j.ID).Update("paused", true).Error)
	replayed := httptest.NewRecorder()
	e.ServeHTTP(replayed, request(runlife.WithSupervisor(t.Context(), owner)))
	require.Equal(t, http.StatusAccepted, replayed.Code, replayed.Body.String())
	require.Equal(t, "true", replayed.Header().Get(IdempotentReplayedHeader))
	var replay StartedRunResponse
	require.NoError(t, json.Unmarshal(replayed.Body.Bytes(), &replay))
	require.Equal(t, admitted.ID, replay.ID)
	require.Equal(t, 1, launches)
	var count int64
	require.NoError(t, conn.Model(&models.JobRun{}).Count(&count).Error)
	require.EqualValues(t, 1, count, "paused replay must use the same store without readmitting")
	owner.CloseAndCancel()
	select {
	case <-executionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("reserved execution did not retain server cancellation")
	}
	bound, cancelBound := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelBound()
	require.ErrorIs(t, owner.Wait(bound), context.DeadlineExceeded, "owner must remain held until launch responsibility releases")
	release()
	joined, cancelJoin := context.WithTimeout(t.Context(), time.Second)
	defer cancelJoin()
	require.NoError(t, owner.Wait(joined))
}
