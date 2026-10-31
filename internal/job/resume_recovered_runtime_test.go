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

// A local attempt holds no claim or lease, so a resume cannot tell a dead
// executor from another node still driving the run: a webhook served by that
// other node reaches this same path. The resume re-executes the row and must
// leave the previous attempt's pod alone, whichever node it runs on.
func TestLocalResumeLeavesThePreviousAttemptsPodAlone(t *testing.T) {
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
		return nil, errors.New("a local resume must not reach the recovery engine")
	})()

	require.NoError(t, j.Run(t.Context()))

	require.Zero(t, stops, "a local resume stopped the previous attempt's pod")
	require.Len(t, engine.createRequestsForTask(tasks[0].ID), 1, "the resume re-executes the row")
	resumed, err := store.Get(started.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, resumed.Status)
}
