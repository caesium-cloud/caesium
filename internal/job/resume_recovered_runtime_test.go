package job

import (
	"context"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

// A trigger reaches this path for a job whose run is in flight. A local attempt
// holds no claim, so nothing tells a live driver from a dead one with an orphan
// still running: re-pending the row could execute it beside its own container.
// The start does nothing, and the run is left for its driver or for recovery.
func TestLocalTriggerStartLeavesAnInFlightRunAlone(t *testing.T) {
	j, store, engine, tasks := characterizationJob(t, 1, nil)
	started, err := store.Start(j.id, nil)
	require.NoError(t, err)
	require.NoError(t, store.RegisterTasks(started.ID, []run.RegisterTaskInput{{Task: tasks[0], Atom: fakeModelAtom(tasks[0].AtomID)}}))
	require.NoError(t, store.DB().Model(&models.TaskRun{}).Where("job_run_id = ?", started.ID).Updates(map[string]any{
		"status":     string(run.TaskStatusRunning),
		"engine":     string(models.AtomEngineKubernetes),
		"runtime_id": "caesium-mapped/step-attempt1",
	}).Error)

	stops := 0
	defer run.SetRecoveryEngineForTesting(func(context.Context) (atom.Engine, error) {
		stops++
		return nil, errors.New("a trigger start must not reach the recovery engine")
	})()

	require.NoError(t, j.Run(t.Context()))

	require.Zero(t, stops, "a trigger start stopped the in-flight attempt's pod")
	require.Empty(t, engine.createRequestsForTask(tasks[0].ID), "a trigger start executed an in-flight task")
	current, err := store.Get(started.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatusRunning, current.Status)
	require.Equal(t, run.TaskStatusRunning, current.Tasks[0].Status)
	require.Equal(t, "caesium-mapped/step-attempt1", current.Tasks[0].RuntimeID)
}
