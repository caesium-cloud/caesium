package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestRetryRefusesRunLeftInPreviousNamespace: a retry of a run whose job has
// since moved namespaces is 409 namespace_moved for every caller, naming both
// namespaces and pointing at replay, before anything is reserved or reset; the
// store's in-transaction refusal maps to the same answer (B4).
func TestRetryRefusesRunLeftInPreviousNamespace(t *testing.T) {
	oldJob, oldRun, oldRetry, oldLaunch := retryGetJob, retryGetRun, retryFromFailure, retryLaunch
	t.Cleanup(func() { retryGetJob, retryGetRun, retryFromFailure, retryLaunch = oldJob, oldRun, oldRetry, oldLaunch })
	jobID, runID := uuid.New(), uuid.New()
	jobNamespace := "finance"
	retryGetJob = func(context.Context, uuid.UUID) (*models.Job, error) {
		return &models.Job{ID: jobID, Alias: "moved-job", Namespace: jobNamespace}, nil
	}
	retryGetRun = func(context.Context, uuid.UUID) (*runstorage.JobRun, error) {
		return &runstorage.JobRun{ID: runID, JobID: jobID, Namespace: "marketing"}, nil
	}
	reset := 0
	retryFromFailure = func(uuid.UUID) (*runstorage.JobRun, error) { reset++; return nil, nil }
	retryLaunch = func(context.Context, *models.Job, *runstorage.JobRun, func()) {}
	request := func() (*echo.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequestWithContext(supervisedRequestContext(t), http.MethodPost, "/retry", nil), rec)
		c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}, {Name: "run_id", Value: runID.String()}})
		return c, rec
	}
	requireMoved := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusConflict, rec.Code)
		var body NamespaceMovedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, "namespace_moved", body.Error)
		require.Equal(t, "marketing", body.RunNamespace)
		require.Equal(t, "finance", body.JobNamespace)
		require.Equal(t, runID.String(), body.RunID)
		require.Contains(t, body.Message, `namespace "marketing"`)
		require.Contains(t, body.Message, `namespace "finance"`)
		require.Contains(t, body.Message, "/replay")
	}

	c, rec := request()
	require.NoError(t, Retry(c))
	requireMoved(rec)
	require.Zero(t, reset, "nothing is reset for a refused retry")

	// The job moved between the handler's check and the store's transaction.
	jobNamespace = "marketing"
	retryFromFailure = func(uuid.UUID) (*runstorage.JobRun, error) {
		return nil, runstorage.CheckRunNamespace(runID, "marketing", "finance")
	}
	retryGetJob = func(context.Context, uuid.UUID) (*models.Job, error) {
		return &models.Job{ID: jobID, Alias: "moved-job", Namespace: "marketing"}, nil
	}
	c, rec = request()
	require.NoError(t, Retry(c))
	requireMoved(rec)
}

// TestPartitionRetryRefusesRunLeftInPreviousNamespace: a partition retry
// relaunches the job's current model inside the historical run exactly like a
// whole-run retry, and is refused the same way (B4).
func TestPartitionRetryRefusesRunLeftInPreviousNamespace(t *testing.T) {
	oldJob, oldRun, oldRetry := partitionGetJob, partitionGetRun, partitionRetryInstance
	t.Cleanup(func() { partitionGetJob, partitionGetRun, partitionRetryInstance = oldJob, oldRun, oldRetry })
	jobID, runID := uuid.New(), uuid.New()
	partitionGetJob = func(context.Context, uuid.UUID) (*models.Job, error) {
		return &models.Job{ID: jobID, Alias: "moved-job", Namespace: "finance"}, nil
	}
	partitionGetRun = func(context.Context, uuid.UUID) (*runstorage.JobRun, error) {
		return &runstorage.JobRun{ID: runID, JobID: jobID, Namespace: "marketing"}, nil
	}
	retried := false
	partitionRetryInstance = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskRun, bool, error) {
		retried = true
		return nil, false, nil
	}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequestWithContext(supervisedRequestContext(t), http.MethodPost, "/retry", nil), rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}, {Name: "run_id", Value: runID.String()},
		{Name: "task_id", Value: uuid.NewString()}, {Name: "index", Value: "0"}})
	require.NoError(t, RetryPartition(c))
	require.Equal(t, http.StatusConflict, rec.Code)
	var body NamespaceMovedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "namespace_moved", body.Error)
	require.False(t, retried)
}
