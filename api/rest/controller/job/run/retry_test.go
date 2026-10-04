package run

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestWholeRetryAdmissionAndRegistrationOrdering(t *testing.T) {
	oldJob, oldRun, oldRetry, oldLaunch := retryGetJob, retryGetRun, retryFromFailure, retryLaunch
	t.Cleanup(func() { retryGetJob, retryGetRun, retryFromFailure, retryLaunch = oldJob, oldRun, oldRetry, oldLaunch })
	jobID, runID := uuid.New(), uuid.New()
	retryGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &models.Job{ID: jobID}, nil }
	retryGetRun = func(context.Context, uuid.UUID) (*runstorage.JobRun, error) {
		return &runstorage.JobRun{ID: runID, JobID: jobID}, nil
	}
	ownerCtx := supervisedRequestContext(t)
	owner := runlife.FromContext(ownerCtx)
	request := func(ctx context.Context) (*echo.Context, *httptest.ResponseRecorder) {
		e := echo.New()
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequestWithContext(ctx, http.MethodPost, "/retry", nil), rec)
		c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}, {Name: "run_id", Value: runID.String()}})
		return c, rec
	}
	retryFromFailure = func(id uuid.UUID) (*runstorage.JobRun, error) {
		require.Equal(t, 1, job.CancelRunContexts(id), "register before reopening")
		return &runstorage.JobRun{ID: id, JobID: jobID}, nil
	}
	var running context.Context
	var release func()
	retryLaunch = func(ctx context.Context, _ *models.Job, _ *runstorage.JobRun, r func()) { running, release = ctx, r }
	c, rec := request(ownerCtx)
	require.NoError(t, Retry(c))
	require.Equal(t, 202, rec.Code)
	require.ErrorIs(t, running.Err(), context.Canceled)
	owner.CloseAndCancel()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded)
	release()
	require.NoError(t, owner.Wait(context.Background()))
	require.Zero(t, job.CancelRunContexts(runID))
	mutated := false
	retryFromFailure = func(uuid.UUID) (*runstorage.JobRun, error) { mutated = true; return nil, nil }
	c, _ = request(ownerCtx)
	err := Retry(c)
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	require.Equal(t, 503, he.Code)
	require.False(t, mutated)
}
func TestWholeRetryFailureReleasesBothRegistrations(t *testing.T) {
	oldJob, oldRun, oldRetry, oldLaunch := retryGetJob, retryGetRun, retryFromFailure, retryLaunch
	t.Cleanup(func() { retryGetJob, retryGetRun, retryFromFailure, retryLaunch = oldJob, oldRun, oldRetry, oldLaunch })
	jobID, runID := uuid.New(), uuid.New()
	retryGetJob = func(context.Context, uuid.UUID) (*models.Job, error) { return &models.Job{ID: jobID}, nil }
	retryGetRun = func(context.Context, uuid.UUID) (*runstorage.JobRun, error) {
		return &runstorage.JobRun{JobID: jobID}, nil
	}
	retryFromFailure = func(uuid.UUID) (*runstorage.JobRun, error) { return nil, errors.New("refused") }
	retryLaunch = func(context.Context, *models.Job, *runstorage.JobRun, func()) { t.Fatal("refused retry launched") }
	ctx := supervisedRequestContext(t)
	e := echo.New()
	c := e.NewContext(httptest.NewRequestWithContext(ctx, http.MethodPost, "/retry", nil), httptest.NewRecorder())
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}, {Name: "run_id", Value: runID.String()}})
	var he *echo.HTTPError
	require.ErrorAs(t, Retry(c), &he)
	require.Equal(t, 409, he.Code)
	require.Zero(t, job.CancelRunContexts(runID))
	owner := runlife.FromContext(ctx)
	owner.CloseAndCancel()
	require.NoError(t, owner.Wait(context.Background()))
}
