package run

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestPartitionStatusCountsCoversTheWholeGroup(t *testing.T) {
	rows := []models.TaskRun{
		{Status: "succeeded"},
		{Status: "succeeded"},
		{Status: "failed"},
		{Status: "skipped"},
	}
	assert.Equal(t, map[string]int{
		"succeeded": 2,
		"failed":    1,
		"skipped":   1,
	}, partitionStatusCounts(rows))

	assert.Empty(t, partitionStatusCounts(nil))
}

func TestProjectPartitionRowsCarriesInstanceIdentity(t *testing.T) {
	start := time.Now().UTC().Add(-90 * time.Second)
	end := start.Add(30 * time.Second)
	id := uuid.New()

	rows := []models.TaskRun{{
		ID:                   id,
		PartitionValue:       "fct_orders",
		PartitionIndex:       2,
		PartitionFingerprint: "sha256:abc",
		PartitionDependsOn:   datatypes.JSON([]byte(`["dim_customer"]`)),
		Status:               "failed",
		Attempt:              2,
		Error:                "boom",
		StartedAt:            &start,
		CompletedAt:          &end,
	}}

	out := projectPartitionRows(rows)
	require.Len(t, out, 1)
	assert.Equal(t, id, out[0].TaskRunID,
		"each row must carry its own TaskRun ID so a client can address one instance")
	assert.Equal(t, "fct_orders", out[0].Value)
	assert.Equal(t, 2, out[0].Index)
	assert.Equal(t, "failed", out[0].Status)
	assert.Equal(t, "sha256:abc", out[0].Fingerprint)
	assert.Equal(t, []string{"dim_customer"}, out[0].DependsOn)
	assert.Equal(t, "30s", out[0].Duration)

	// Absolute timestamps, not only the derived duration: an operator diagnosing
	// a skewed group needs to see WHEN each instance ran, and a client that only
	// gets "30s" cannot place two partitions on a timeline or tell a slow
	// instance from a late-dispatched one. RFC3339 so the CLI's --json and the
	// UI parse the same string.
	assert.Equal(t, start.Format(time.RFC3339), out[0].StartedAt)
	assert.Equal(t, end.Format(time.RFC3339), out[0].CompletedAt)
}

func TestProjectPartitionRowsOmitsDurationWhenNotFinished(t *testing.T) {
	start := time.Now().UTC()
	out := projectPartitionRows([]models.TaskRun{{ID: uuid.New(), StartedAt: &start}})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Duration)
	assert.Equal(t, start.Format(time.RFC3339), out[0].StartedAt,
		"a running instance still has a start time; only the duration is unknown")
	assert.Empty(t, out[0].CompletedAt)
}

// TestProjectPartitionRowsOmitsTimestampsWhenNeverDispatched pins the pending
// case: both fields are omitempty, so a partition that has not started
// serializes without them rather than with a zero-value timestamp a client
// would render as 0001-01-01.
func TestProjectPartitionRowsOmitsTimestampsWhenNeverDispatched(t *testing.T) {
	out := projectPartitionRows([]models.TaskRun{{ID: uuid.New(), Status: "pending"}})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].StartedAt)
	assert.Empty(t, out[0].CompletedAt)

	encoded, err := json.Marshal(out[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "started_at")
	assert.NotContains(t, string(encoded), "completed_at")
}

// TestRetryPartitionHTTPErrorMapsNonTerminalToConflict pins the status code the
// CLI and UI branch on: a running or pending instance is a state conflict the
// caller can act on, not a server fault.
func TestRetryPartitionHTTPErrorMapsNonTerminalToConflict(t *testing.T) {
	err := retryPartitionHTTPError(runstorage.ErrTaskRunNotTerminal)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusConflict, httpErr.Code)
}

func TestRetryPartitionHTTPErrorMapsNonRunnableRunToConflict(t *testing.T) {
	err := retryPartitionHTTPError(runstorage.ErrPartitionRunNotRetryable)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusConflict, httpErr.Code)
}

func TestRetryPartitionHTTPErrorMapsMissingRowToNotFound(t *testing.T) {
	assert.Equal(t, echo.ErrNotFound, retryPartitionHTTPError(gorm.ErrRecordNotFound))
}

func TestRetryPartitionHTTPErrorMapsUnknownToInternal(t *testing.T) {
	err := retryPartitionHTTPError(errors.New("disk on fire"))
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusInternalServerError, httpErr.Code)
}

func TestRetryPartitionHTTPErrorNilIsNil(t *testing.T) {
	assert.NoError(t, retryPartitionHTTPError(nil))
}

func TestPartitionRetryRefusesOwnerBeforeMutation(t *testing.T) {
	f := newPartitionsFixture(t, 1)
	f.failPartition(t, 0)
	f.finishRun(t, string(runstorage.StatusFailed))
	called := false
	partitionRetryInstance = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskRun, bool, error) {
		called = true
		return nil, false, nil
	}
	owner := runlife.New(context.Background())
	owner.CloseAndCancel()
	for _, ctx := range []context.Context{context.Background(), runlife.WithSupervisor(context.Background(), owner)} {
		e := echo.New()
		c := e.NewContext(httptest.NewRequestWithContext(ctx, http.MethodPost, "/retry", nil), httptest.NewRecorder())
		c.SetPathValues(echo.PathValues{{Name: "id", Value: f.jobID.String()}, {Name: "run_id", Value: f.runID.String()}, {Name: "task_id", Value: f.taskID.String()}, {Name: "index", Value: "0"}})
		var he *echo.HTTPError
		require.ErrorAs(t, RetryPartition(c), &he)
		require.Equal(t, 503, he.Code)
		require.False(t, called)
	}
}
