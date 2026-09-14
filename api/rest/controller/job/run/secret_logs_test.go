package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestSecretLogsStreamCumulativeSnapshotsAsDeltas(t *testing.T) {
	originalLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogPollInterval
	t.Cleanup(func() {
		scrubbedLogStateLoader = originalLoader
		scrubbedLogPollInterval = originalInterval
	})
	scrubbedLogPollInterval = time.Millisecond

	states := []*runstorage.TaskLogReadState{
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Snapshot: &runstorage.TaskLogSnapshot{Text: "first\n"}},
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Snapshot: &runstorage.TaskLogSnapshot{Text: "first\nsecond\n"}},
		{Status: runstorage.TaskStatusSucceeded, Attempt: 1, Scrubbed: true, Snapshot: &runstorage.TaskLogSnapshot{Text: "first\nsecond\n"}},
	}
	var mu sync.Mutex
	next := 0
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		mu.Lock()
		defer mu.Unlock()
		state := states[min(next, len(states)-1)]
		next++
		return state, nil
	}

	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{ID: uuid.New(), Attempt: 1})
	require.NoError(t, err)
	require.Equal(t, "first\nsecond\n", rec.Body.String())
	require.Equal(t, "live", rec.Result().Header.Get(logHeaderSource))
}

func TestSecretLogsPendingTaskReturnsOrdinaryPendingState(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), rec)
	task := &runstorage.TaskRun{ID: uuid.New(), Status: runstorage.TaskStatusPending, LogScrubbed: true}
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), task))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "pending", rec.Header().Get(logHeaderState))
}

func TestSecretLogsLiveCapAddsVisibleTruncationMarkerAfterInitialDelta(t *testing.T) {
	originalLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogPollInterval
	t.Cleanup(func() {
		scrubbedLogStateLoader = originalLoader
		scrubbedLogPollInterval = originalInterval
	})
	scrubbedLogPollInterval = time.Millisecond

	states := []*runstorage.TaskLogReadState{
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true,
			Snapshot: &runstorage.TaskLogSnapshot{Text: "first safe delta\n"}},
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true,
			Snapshot: &runstorage.TaskLogSnapshot{Text: "first safe delta\n" + incident.TruncatedLogMarker, Truncated: true}},
		{Status: runstorage.TaskStatusSucceeded, Attempt: 1, Scrubbed: true,
			Snapshot: &runstorage.TaskLogSnapshot{Text: "first safe delta\n" + incident.TruncatedLogMarker, Truncated: true}},
	}
	next := 0
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		state := states[min(next, len(states)-1)]
		next++
		return state, nil
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), rec)
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{ID: uuid.New(), Attempt: 1}))
	require.Equal(t, "live", rec.Result().Header.Get(logHeaderSource))
	require.Contains(t, rec.Body.String(), incident.TruncatedLogMarker,
		"a cap reached after headers commit must still be visible to the live client")
}

func TestSecretLogsFinalSnapshotIsPersistedSource(t *testing.T) {
	originalLoader := scrubbedLogStateLoader
	t.Cleanup(func() { scrubbedLogStateLoader = originalLoader })
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		return &runstorage.TaskLogReadState{
			Status: runstorage.TaskStatusFailed, Attempt: 1, Scrubbed: true,
			Snapshot: &runstorage.TaskLogSnapshot{Text: "safe failure\n", Truncated: true},
		}, nil
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), rec)
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{ID: uuid.New(), Attempt: 1}))
	require.Equal(t, "safe failure\n", rec.Body.String())
	require.Equal(t, "persisted", rec.Result().Header.Get(logHeaderSource))
	require.Equal(t, "true", rec.Result().Header.Get(logHeaderTruncated))
}

func TestSecretLogsEndWhenClaimIdentityChangesWithoutAttemptChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		next *runstorage.TaskLogReadState
	}{
		{
			name: "claim generation advances",
			next: &runstorage.TaskLogReadState{Status: runstorage.TaskStatusRunning, Attempt: 1,
				ClaimedBy: "worker-a", ClaimAttempt: 8, Scrubbed: true,
				Snapshot: &runstorage.TaskLogSnapshot{Text: "successor output must wait for reconnect\n"}},
		},
		{
			name: "claim is released before successor starts",
			next: &runstorage.TaskLogReadState{Status: runstorage.TaskStatusPending, Attempt: 1,
				ClaimAttempt: 7, Scrubbed: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogPollInterval
			t.Cleanup(func() {
				scrubbedLogStateLoader = originalLoader
				scrubbedLogPollInterval = originalInterval
			})
			scrubbedLogPollInterval = time.Millisecond
			states := []*runstorage.TaskLogReadState{
				{Status: runstorage.TaskStatusRunning, Attempt: 1, ClaimedBy: "worker-a", ClaimAttempt: 7,
					Scrubbed: true, Snapshot: &runstorage.TaskLogSnapshot{Text: "first claim output\n"}},
				tc.next,
			}
			next := 0
			scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
				state := states[min(next, len(states)-1)]
				next++
				return state, nil
			}

			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), rec)
			task := &runstorage.TaskRun{ID: uuid.New(), Attempt: 1, ClaimedBy: "worker-a", ClaimAttempt: 7}
			require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), task))
			require.Equal(t, "first claim output\n", rec.Body.String())
		})
	}
}

func TestLegacyRetainedLogsRemainProspectiveOnly(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), rec)
	legacy := &runstorage.TaskLogSnapshot{Text: "historic raw log\n"}
	require.NoError(t, writeLogSnapshot(c, legacy))
	require.Equal(t, legacy.Text, rec.Body.String(),
		"old rows have no historical resolved value and must not be heuristically rewritten at read time")
}
