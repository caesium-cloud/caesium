package run

import (
	"context"
	"net/http"
	"testing"

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
