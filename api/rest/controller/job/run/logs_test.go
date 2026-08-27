package run

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// logs_test.go pins the fan-out selection contract of GET
// …/runs/:run_id/logs?task_id=… . Before it, the handler matched the COLLAPSED
// run-detail entry (whose RuntimeID is instance 0's container) and streamed that
// container's output for every instance of the group — silently the wrong log.

// stubInstances installs a logInstanceLoader returning the given instances and
// restores the real one afterwards. It also records whether it was called, so a
// test can prove the unfanned hot path issues no extra query.
func stubInstances(t *testing.T, instances []*runstorage.TaskRun) *bool {
	t.Helper()
	called := false
	original := logInstanceLoader
	logInstanceLoader = func(context.Context, uuid.UUID, uuid.UUID) ([]*runstorage.TaskRun, error) {
		called = true
		return instances, nil
	}
	t.Cleanup(func() { logInstanceLoader = original })
	return &called
}

func fanOutInstances(n int) []*runstorage.TaskRun {
	values := []string{"a", "b", "c", "d"}
	out := make([]*runstorage.TaskRun, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &runstorage.TaskRun{
			ID:             uuid.New(),
			Status:         runstorage.TaskStatusSucceeded,
			RuntimeID:      "container-" + values[i],
			PartitionValue: values[i],
			PartitionIndex: i,
			PartitionCount: n,
		})
	}
	return out
}

func TestResolveLogInstance_UnfannedTakesNoInstanceQuery(t *testing.T) {
	called := stubInstances(t, nil)
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 0}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, "", "")

	require.NoError(t, err)
	require.Nil(t, problem)
	require.Nil(t, selected, "an unfanned task keeps the pre-fan-out code path")
	require.False(t, *called, "the hot path must not pay for a per-instance query")
}

func TestResolveLogInstance_FannedWithoutSelectorIs400ListingInstances(t *testing.T) {
	instances := fanOutInstances(3)
	stubInstances(t, instances)
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 3}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, "", "")

	require.NoError(t, err)
	require.Nil(t, selected)
	require.NotNil(t, problem, "streaming instance 0 by default is the defect this replaced")
	require.Equal(t, http.StatusBadRequest, problem.Status)
	require.Equal(t, 3, problem.PartitionCount)
	require.Contains(t, problem.Message, "task_run_id")
	require.Contains(t, problem.Message, "partition=")
	require.Len(t, problem.Instances, 3)
	require.Equal(t, "a", problem.Instances[0].Partition)
	require.Equal(t, instances[0].ID.String(), problem.Instances[0].TaskRunID,
		"the listed task_run_id must be directly usable as the retry selector")
}

func TestResolveLogInstance_TaskRunIDSelectsThatInstance(t *testing.T) {
	instances := fanOutInstances(3)
	stubInstances(t, instances)
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 3}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, instances[2].ID.String(), "")

	require.NoError(t, err)
	require.Nil(t, problem)
	require.NotNil(t, selected)
	require.Equal(t, instances[2].ID, selected.ID)
	require.Equal(t, "container-c", selected.RuntimeID,
		"the streamed container must be the selected instance's, not instance 0's")
}

func TestResolveLogInstance_PartitionValueSelectsThatInstance(t *testing.T) {
	instances := fanOutInstances(3)
	stubInstances(t, instances)
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 3}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, "", "b")

	require.NoError(t, err)
	require.Nil(t, problem)
	require.NotNil(t, selected)
	require.Equal(t, "b", selected.PartitionValue)
	require.Equal(t, "container-b", selected.RuntimeID)
}

func TestResolveLogInstance_UnknownPartitionIs404ListingInstances(t *testing.T) {
	stubInstances(t, fanOutInstances(3))
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 3}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, "", "zz")

	require.NoError(t, err)
	require.Nil(t, selected)
	require.NotNil(t, problem)
	require.Equal(t, http.StatusNotFound, problem.Status)
	require.Contains(t, problem.Message, `no partition "zz"`)
	require.Len(t, problem.Instances, 3)
}

func TestResolveLogInstance_ForeignTaskRunIDIs404(t *testing.T) {
	stubInstances(t, fanOutInstances(3))
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 3}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, uuid.NewString(), "")

	require.NoError(t, err)
	require.Nil(t, selected)
	require.NotNil(t, problem)
	require.Equal(t, http.StatusNotFound, problem.Status,
		"a task_run_id from another run/task must not resolve through a run-scoped route")
}

func TestResolveLogInstance_MalformedTaskRunIDIs400(t *testing.T) {
	stubInstances(t, fanOutInstances(2))
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 2}

	_, _, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, "not-a-uuid", "")

	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

// TestResolveLogInstance_SelectorOnUnfannedTaskStillStreams: passing a selector
// against an unfanned task is harmless — the single row is streamed as before
// rather than 400'ing a caller that guessed wrong.
func TestResolveLogInstance_SelectorOnUnfannedTaskStillStreams(t *testing.T) {
	single := []*runstorage.TaskRun{{ID: uuid.New(), Status: runstorage.TaskStatusSucceeded, RuntimeID: "container-1"}}
	stubInstances(t, single)
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 0}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, single[0].ID.String(), "")
	require.NoError(t, err)
	require.Nil(t, problem)
	require.NotNil(t, selected)
	require.Equal(t, single[0].ID, selected.ID)
}

// TestResolveLogInstance_SinglePartitionGroupStillNeedsSelection: an N=1 group
// carries a partition value and is still fanned, so it is still explicit.
func TestResolveLogInstance_SinglePartitionGroupIsFanned(t *testing.T) {
	stubInstances(t, fanOutInstances(1))
	collapsed := &runstorage.TaskRun{ID: uuid.New(), PartitionCount: 1}

	selected, problem, err := resolveLogInstance(
		context.Background(), uuid.New(), uuid.New(), collapsed, "", "")

	require.NoError(t, err)
	require.Nil(t, selected)
	require.NotNil(t, problem)
	require.Equal(t, http.StatusBadRequest, problem.Status)
}

func TestLogInstanceRows_CapsTheListedInstances(t *testing.T) {
	instances := make([]*runstorage.TaskRun, 500)
	for i := range instances {
		instances[i] = &runstorage.TaskRun{ID: uuid.New(), PartitionValue: "p", PartitionIndex: i}
	}
	require.Len(t, logInstanceRows(instances), 200,
		"a 10k-partition group must not return a multi-megabyte error body")
}

// --- live-stream vs persisted-snapshot serving ------------------------------
//
// These pin the rule that broke the podman-runtime lane of
// TestFanOutLogsSelectInstance: every partition's log came back 200-with-an-
// empty-body. The snapshots were persisted correctly (the docker lane serves
// them, X-Caesium-Log-Source: persisted) — the handler simply could not REACH
// the fallback, because it committed "200 + live" before reading a byte and the
// podman engine only reports an unavailable stream to the first Read.

// lazyErrorReader models the podman engine's log stream. podman.go's
// ContainerLogs returns an io.Pipe immediately and runs containers.Logs in a
// goroutine, so "no such container" arrives via pw.CloseWithError — i.e. on the
// first Read, not from Logs(). The docker engine fails inside Logs() instead.
type lazyErrorReader struct {
	err    error
	closed bool
}

func (r *lazyErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (r *lazyErrorReader) Close() error             { r.closed = true; return nil }

func newLogContext(t *testing.T) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// committed returns the response AS THE CLIENT SEES IT: the status line and the
// header block captured at the moment the response was committed.
//
// Asserting on rec.Header() instead would not test this handler at all. Echo's
// Response.WriteHeader is a no-op once Committed, but Header() keeps returning
// the live, still-mutable map — so a fallback that runs AFTER the status line
// went out still appears to have set its headers, and its body still appends to
// the already-sent one. That is exactly the shape of the podman bug these tests
// exist for (200 + "live" flushed before the stream was known to be dead), so a
// recorder read through Header() reports it as a pass. httptest snapshots the
// header map inside WriteHeader, and Result() serves that snapshot; it is the
// only view of the recorder that can tell the two orderings apart.
// committedResponse is the status and headers the client actually received.
// httptest.ResponseRecorder.Header() keeps returning the live, still-mutable
// map after WriteHeader; only Result() serves the committed snapshot, which
// is what the fallback-after-commit tests must assert on.
type committedResponse struct {
	StatusCode int
	Header     http.Header
}

func committed(t *testing.T, rec *httptest.ResponseRecorder) committedResponse {
	t.Helper()
	res := rec.Result()
	_ = res.Body.Close()
	return committedResponse{StatusCode: res.StatusCode, Header: res.Header}
}

func completedInstance() *runstorage.TaskRun {
	completed := time.Now()
	return &runstorage.TaskRun{
		ID:             uuid.New(),
		Status:         runstorage.TaskStatusSucceeded,
		RuntimeID:      "container-a",
		PartitionValue: "a",
		PartitionCount: 3,
		CompletedAt:    &completed,
	}
}

// TestServeTaskLog_LazyStreamErrorFallsBackToSnapshot is the podman regression.
func TestServeTaskLog_LazyStreamErrorFallsBackToSnapshot(t *testing.T) {
	c, rec := newLogContext(t)
	reader := &lazyErrorReader{err: errors.New("no such container: container-a")}
	snapshot := &runstorage.TaskLogSnapshot{Text: "partition=a\n"}

	err := serveTaskLog(c, func(*runstorage.TaskRun, time.Time) (io.ReadCloser, error) {
		return reader, nil
	}, completedInstance(), time.Time{}, snapshot, true)

	require.NoError(t, err)
	res := committed(t, rec)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "partition=a\n", rec.Body.String(),
		"an engine that reports failure on the first Read must still reach the snapshot")
	require.Equal(t, "persisted", res.Header.Get(logHeaderSource),
		"the response must not be COMMITTED as a live stream before the stream has produced a byte: "+
			"once the status line is out, the snapshot fallback can only append to it")
	require.True(t, reader.closed, "the abandoned stream must be closed")
}

// TestServeTaskLog_SynchronousStreamErrorFallsBackToSnapshot is the docker
// shape, which already worked; it is pinned so the peek does not regress it.
func TestServeTaskLog_SynchronousStreamErrorFallsBackToSnapshot(t *testing.T) {
	c, rec := newLogContext(t)
	snapshot := &runstorage.TaskLogSnapshot{Text: "partition=b\n", Truncated: true}

	err := serveTaskLog(c, func(*runstorage.TaskRun, time.Time) (io.ReadCloser, error) {
		return nil, errors.New("Error response from daemon: No such container")
	}, completedInstance(), time.Time{}, snapshot, true)

	require.NoError(t, err)
	res := committed(t, rec)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "partition=b\n", rec.Body.String())
	require.Equal(t, "persisted", res.Header.Get(logHeaderSource))
	require.Equal(t, "true", res.Header.Get(logHeaderTruncated))
}

// TestServeTaskLog_EmptyLiveStreamPrefersSnapshot: a stream that opens and ends
// without output must not shadow a captured snapshot with an empty 200.
func TestServeTaskLog_EmptyLiveStreamPrefersSnapshot(t *testing.T) {
	c, rec := newLogContext(t)
	snapshot := &runstorage.TaskLogSnapshot{Text: "partition=c\n"}

	err := serveTaskLog(c, func(*runstorage.TaskRun, time.Time) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}, completedInstance(), time.Time{}, snapshot, true)

	require.NoError(t, err)
	res := committed(t, rec)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "partition=c\n", rec.Body.String())
	require.Equal(t, "persisted", res.Header.Get(logHeaderSource),
		"a stream that opened and ended empty must not have committed a live response")
}

// TestServeTaskLog_StreamsLiveOutput: a stream that DOES produce output is still
// streamed live, first chunk included.
func TestServeTaskLog_StreamsLiveOutput(t *testing.T) {
	c, rec := newLogContext(t)

	err := serveTaskLog(c, func(*runstorage.TaskRun, time.Time) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("partition=a\nline two\n")), nil
	}, completedInstance(), time.Time{}, &runstorage.TaskLogSnapshot{Text: "stale"}, true)

	require.NoError(t, err)
	res := committed(t, rec)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "partition=a\nline two\n", rec.Body.String(),
		"the peeked first chunk must not be dropped")
	require.Equal(t, "live", res.Header.Get(logHeaderSource),
		"a stream that does produce output is still served live")
}

// TestServeTaskLog_LazyErrorWithoutSnapshotOnFinishedTaskIsUnavailable keeps the
// pre-existing 204 answer for a finished task that captured no log.
func TestServeTaskLog_LazyErrorWithoutSnapshotOnFinishedTaskIsUnavailable(t *testing.T) {
	c, rec := newLogContext(t)

	err := serveTaskLog(c, func(*runstorage.TaskRun, time.Time) (io.ReadCloser, error) {
		return &lazyErrorReader{err: errors.New("no such container")}, nil
	}, completedInstance(), time.Time{}, nil, true)

	require.NoError(t, err)
	res := committed(t, rec)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "unavailable", res.Header.Get(logHeaderState))
}

// TestServeTaskLog_LazyErrorWithoutSnapshotOnRunningTaskIs502: a live stream
// that fails for a task still running is a transport fault, not "no log".
func TestServeTaskLog_LazyErrorWithoutSnapshotOnRunningTaskIs502(t *testing.T) {
	c, rec := newLogContext(t)
	running := &runstorage.TaskRun{ID: uuid.New(), Status: runstorage.TaskStatusRunning, RuntimeID: "container-a"}

	err := serveTaskLog(c, func(*runstorage.TaskRun, time.Time) (io.ReadCloser, error) {
		return &lazyErrorReader{err: errors.New("connection refused")}, nil
	}, running, time.Time{}, nil, false)

	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadGateway, he.Code)
	require.Empty(t, committed(t, rec).Header.Get(logHeaderSource),
		"a 502 the framework still has to render means the response must not already have been "+
			"committed as a live 200 stream")
	require.Empty(t, rec.Body.String())
}

// TestTaskLogIsFinal: a finished task's container is removed by the engine's
// Stop, so its snapshot is the authoritative log and the runtime must not be
// asked for a stream that can only fail.
func TestTaskLogIsFinal(t *testing.T) {
	completed := time.Now()
	require.True(t, taskLogIsFinal(&runstorage.TaskRun{Status: runstorage.TaskStatusSucceeded}))
	require.True(t, taskLogIsFinal(&runstorage.TaskRun{Status: runstorage.TaskStatusFailed}))
	require.True(t, taskLogIsFinal(&runstorage.TaskRun{Status: runstorage.TaskStatusRunning, CompletedAt: &completed}))
	require.False(t, taskLogIsFinal(&runstorage.TaskRun{Status: runstorage.TaskStatusRunning}))
	require.False(t, taskLogIsFinal(&runstorage.TaskRun{Status: runstorage.TaskStatusPending}))
}
