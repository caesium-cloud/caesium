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
	originalLoader, originalSnapshotLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogSnapshotLoader, scrubbedLogPollInterval
	t.Cleanup(func() {
		scrubbedLogStateLoader = originalLoader
		scrubbedLogSnapshotLoader = originalSnapshotLoader
		scrubbedLogPollInterval = originalInterval
	})
	scrubbedLogPollInterval = time.Millisecond

	states := []*runstorage.TaskLogReadState{
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: 6},
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: 13},
		{Status: runstorage.TaskStatusSucceeded, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: 13},
	}
	snapshots := []*runstorage.TaskLogSnapshot{{Text: "first\n"}, {Text: "first\nsecond\n"}}
	snapshotReads := 0
	scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
		snapshot := snapshots[min(snapshotReads, len(snapshots)-1)]
		snapshotReads++
		return snapshot, nil
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
	err := serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{ID: uuid.New(), Attempt: 1, LogGeneration: "gen-a"})
	require.NoError(t, err)
	require.Equal(t, "first\nsecond\n", rec.Body.String())
	require.Equal(t, "live", rec.Result().Header.Get(logHeaderSource))
	require.Equal(t, 2, snapshotReads, "unchanged metadata polls must not reread the full snapshot")
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

func TestSecretLogsTerminalBeforePrepareReturnsEmptyWithoutLoading(t *testing.T) {
	originalState, originalSnapshot := scrubbedLogStateLoader, scrubbedLogSnapshotLoader
	t.Cleanup(func() { scrubbedLogStateLoader, scrubbedLogSnapshotLoader = originalState, originalSnapshot })
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		t.Fatal("terminal task without a producer generation must not start polling")
		return nil, nil
	}
	scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
		t.Fatal("terminal task without a producer generation must not load a snapshot")
		return nil, nil
	}
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), rec)
	task := &runstorage.TaskRun{ID: uuid.New(), Status: runstorage.TaskStatusFailed, LogScrubbed: true}
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), task))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "empty", rec.Header().Get(logHeaderState))
}

func TestSecretLogsLiveCapAddsVisibleTruncationMarkerAfterInitialDelta(t *testing.T) {
	originalLoader, originalSnapshotLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogSnapshotLoader, scrubbedLogPollInterval
	t.Cleanup(func() {
		scrubbedLogStateLoader = originalLoader
		scrubbedLogSnapshotLoader = originalSnapshotLoader
		scrubbedLogPollInterval = originalInterval
	})
	scrubbedLogPollInterval = time.Millisecond

	states := []*runstorage.TaskLogReadState{
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: len("first safe delta\n")},
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: len("first safe delta\n" + incident.TruncatedLogMarker), Truncated: true},
		{Status: runstorage.TaskStatusSucceeded, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: len("first safe delta\n" + incident.TruncatedLogMarker), Truncated: true},
	}
	snapshots := []*runstorage.TaskLogSnapshot{
		{Text: "first safe delta\n"},
		{Text: "first safe delta\n" + incident.TruncatedLogMarker, Truncated: true},
	}
	snapshotRead := 0
	scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
		snapshot := snapshots[min(snapshotRead, len(snapshots)-1)]
		snapshotRead++
		return snapshot, nil
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
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{ID: uuid.New(), Attempt: 1, LogGeneration: "gen-a"}))
	require.Equal(t, "live", rec.Result().Header.Get(logHeaderSource))
	require.Contains(t, rec.Body.String(), incident.TruncatedLogMarker,
		"a cap reached after headers commit must still be visible to the live client")
}

func TestSecretLogsFinalSnapshotIsPersistedSource(t *testing.T) {
	originalLoader, originalSnapshotLoader := scrubbedLogStateLoader, scrubbedLogSnapshotLoader
	t.Cleanup(func() { scrubbedLogStateLoader, scrubbedLogSnapshotLoader = originalLoader, originalSnapshotLoader })
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		return &runstorage.TaskLogReadState{
			Status: runstorage.TaskStatusFailed, Attempt: 1, Scrubbed: true, Generation: "gen-a",
			LogBytes: len("safe failure\n"), Truncated: true,
		}, nil
	}
	scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
		return &runstorage.TaskLogSnapshot{Text: "safe failure\n", Truncated: true}, nil
	}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil), rec)
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{ID: uuid.New(), Attempt: 1, LogGeneration: "gen-a"}))
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
				ClaimedBy: "worker-a", ClaimAttempt: 8, Scrubbed: true, Generation: "gen-b"},
		},
		{
			name: "claim is released before successor starts",
			next: &runstorage.TaskLogReadState{Status: runstorage.TaskStatusPending, Attempt: 1,
				ClaimAttempt: 7, Scrubbed: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalLoader, originalSnapshotLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogSnapshotLoader, scrubbedLogPollInterval
			t.Cleanup(func() {
				scrubbedLogStateLoader = originalLoader
				scrubbedLogSnapshotLoader = originalSnapshotLoader
				scrubbedLogPollInterval = originalInterval
			})
			scrubbedLogPollInterval = time.Millisecond
			states := []*runstorage.TaskLogReadState{
				{Status: runstorage.TaskStatusRunning, Attempt: 1, ClaimedBy: "worker-a", ClaimAttempt: 7,
					Scrubbed: true, Generation: "gen-a", LogBytes: len("first claim output\n")},
				tc.next,
			}
			scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
				return &runstorage.TaskLogSnapshot{Text: "first claim output\n"}, nil
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
			task := &runstorage.TaskRun{ID: uuid.New(), Attempt: 1, ClaimedBy: "worker-a", ClaimAttempt: 7, LogGeneration: "gen-a"}
			require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), task))
			require.Equal(t, "first claim output\n", rec.Body.String())
		})
	}
}

func TestSecretLogsGenerationChangeBeforeHeadersReturnsPending(t *testing.T) {
	originalLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogPollInterval
	t.Cleanup(func() {
		scrubbedLogStateLoader = originalLoader
		scrubbedLogPollInterval = originalInterval
	})
	scrubbedLogPollInterval = time.Millisecond
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		return &runstorage.TaskLogReadState{
			Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-b",
		}, nil
	}
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), rec)
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{
		ID: uuid.New(), Status: runstorage.TaskStatusRunning, Attempt: 1, LogGeneration: "gen-a",
	}))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "pending", rec.Header().Get(logHeaderState))
}

func TestSecretLogsNonMonotonicChangeAfterHeadersEndsStream(t *testing.T) {
	originalLoader, originalSnapshotLoader, originalInterval := scrubbedLogStateLoader, scrubbedLogSnapshotLoader, scrubbedLogPollInterval
	t.Cleanup(func() {
		scrubbedLogStateLoader, scrubbedLogSnapshotLoader = originalLoader, originalSnapshotLoader
		scrubbedLogPollInterval = originalInterval
	})
	scrubbedLogPollInterval = time.Millisecond
	states := []*runstorage.TaskLogReadState{
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: 6},
		{Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true, Generation: "gen-a", LogBytes: 5},
	}
	stateRead := 0
	scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
		state := states[min(stateRead, len(states)-1)]
		stateRead++
		return state, nil
	}
	snapshots := []*runstorage.TaskLogSnapshot{{Text: "first\n"}, {Text: "other"}}
	snapshotRead := 0
	scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
		snapshot := snapshots[min(snapshotRead, len(snapshots)-1)]
		snapshotRead++
		return snapshot, nil
	}
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), rec)
	require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{
		ID: uuid.New(), Status: runstorage.TaskStatusRunning, Attempt: 1, LogGeneration: "gen-a",
	}))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "first\n", rec.Body.String(), "an HTTP status error cannot be written after live bytes")
}

func TestSecretLogsGenerationFenceClosesMetadataSnapshotRace(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before headers", true: "after headers"}[afterCommit], func(t *testing.T) {
			originalState, originalSnapshot, originalInterval := scrubbedLogStateLoader, scrubbedLogSnapshotLoader, scrubbedLogPollInterval
			t.Cleanup(func() {
				scrubbedLogStateLoader, scrubbedLogSnapshotLoader = originalState, originalSnapshot
				scrubbedLogPollInterval = originalInterval
			})
			scrubbedLogPollInterval = time.Millisecond
			stateRead := 0
			scrubbedLogStateLoader = func(context.Context, uuid.UUID, uuid.UUID) (*runstorage.TaskLogReadState, error) {
				stateRead++
				bytes := 6
				if afterCommit && stateRead > 1 {
					bytes = 13
				}
				return &runstorage.TaskLogReadState{
					Status: runstorage.TaskStatusRunning, Attempt: 1, Scrubbed: true,
					Generation: "gen-a", LogBytes: bytes,
				}, nil
			}
			snapshotRead := 0
			scrubbedLogSnapshotLoader = func(context.Context, uuid.UUID, uuid.UUID, string) (*runstorage.TaskLogSnapshot, error) {
				snapshotRead++
				if afterCommit && snapshotRead == 1 {
					return &runstorage.TaskLogSnapshot{Text: "first\n"}, nil
				}
				// Simulates successor Prepare after the metadata read. A fenced
				// SELECT cannot return the successor's snapshot under gen-a.
				return nil, runstorage.ErrTaskClaimMismatch
			}
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), rec)
			require.NoError(t, serveScrubbedTaskLog(c, uuid.New(), &runstorage.TaskRun{
				ID: uuid.New(), Status: runstorage.TaskStatusRunning, Attempt: 1, LogGeneration: "gen-a",
			}))
			if afterCommit {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "first\n", rec.Body.String())
			} else {
				require.Equal(t, http.StatusNoContent, rec.Code)
				require.Equal(t, "pending", rec.Header().Get(logHeaderState))
			}
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
