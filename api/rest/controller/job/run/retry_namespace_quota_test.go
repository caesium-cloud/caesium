package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/api/rest/namespaceadmission"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestRetryAtNamespaceQuotaIs409WithRetryAfter: a retry the namespace run
// quota refuses is 409 namespace_quota with Retry-After, naming the namespace
// and its cap, and nothing is launched.
func TestRetryAtNamespaceQuotaIs409WithRetryAfter(t *testing.T) {
	oldJob, oldRun, oldRetry, oldLaunch := retryGetJob, retryGetRun, retryFromFailure, retryLaunch
	t.Cleanup(func() { retryGetJob, retryGetRun, retryFromFailure, retryLaunch = oldJob, oldRun, oldRetry, oldLaunch })
	jobID, runID := uuid.New(), uuid.New()
	retryGetJob = func(context.Context, uuid.UUID) (*models.Job, error) {
		return &models.Job{ID: jobID, Alias: "quota-job", Namespace: "marketing"}, nil
	}
	retryGetRun = func(context.Context, uuid.UUID) (*runstorage.JobRun, error) {
		return &runstorage.JobRun{ID: runID, JobID: jobID, Namespace: "marketing"}, nil
	}
	retryFromFailure = func(uuid.UUID) (*runstorage.JobRun, error) {
		return nil, fmt.Errorf("retry: %w", &runstorage.NamespaceQuotaError{Namespace: "marketing", Limit: 1})
	}
	launched := false
	retryLaunch = func(context.Context, *models.Job, *runstorage.JobRun, func()) { launched = true }

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequestWithContext(supervisedRequestContext(t), http.MethodPost, "/retry", nil), rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}, {Name: "run_id", Value: runID.String()}})
	require.NoError(t, Retry(c))

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "5", rec.Header().Get("Retry-After"))
	var body namespaceadmission.QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "namespace_quota", body.Error)
	require.Equal(t, "marketing", body.Namespace)
	require.Equal(t, 1, body.MaxConcurrentRuns)
	require.Contains(t, body.Message, `namespace "marketing"`)
	require.False(t, launched)
}
