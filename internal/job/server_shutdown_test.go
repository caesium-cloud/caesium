package job

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type shutdownStartedEngine struct {
	*fakeEngine
	started chan<- string
	stopped chan<- atom.EngineStopRequest
}

func (e *shutdownStartedEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	e.started <- req.ID // StartTask has committed before the executor calls Wait.
	return e.fakeEngine.Wait(req)
}

func (e *shutdownStartedEngine) Stop(req *atom.EngineStopRequest) error {
	err := e.fakeEngine.Stop(req)
	e.stopped <- *req
	return err
}

func TestLocalServerShutdownStopsRuntimeWithoutFailingDurableWork(t *testing.T) {
	j, store, engine, tasks := characterizationJob(t, 2, [][2]int{{0, 1}})
	engine.runDurationByName[tasks[0].ID.String()] = time.Hour
	started := make(chan string, 1)
	stopped := make(chan atom.EngineStopRequest, 1)
	j.newDockerEngine = func(context.Context) atom.Engine {
		return &shutdownStartedEngine{fakeEngine: engine, started: started, stopped: stopped}
	}
	owner := runlife.New(t.Context())
	ctx, release, err := owner.Reserve(runlife.WithSupervisor(t.Context(), owner))
	require.NoError(t, err)
	done := make(chan error, 1)
	callbacks := 0
	j.dispatchRunCallbacks = func(context.Context, uuid.UUID, uuid.UUID, error) error { callbacks++; return nil }
	go func() { defer release(); done <- j.Run(ctx) }()
	t.Cleanup(func() {
		owner.CloseAndCancelCause(runlife.ErrServerShutdown)
		wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, owner.Wait(wait))
	})
	var runtimeID string
	select {
	case runtimeID = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("local atom did not start")
	}
	before := latestRunSnapshot(t, store, j.id)
	require.Equal(t, run.TaskStatusRunning, taskRunByID(before, tasks[0].ID).Status)
	owner.CloseAndCancelCause(runlife.ErrServerShutdown)
	select {
	case err := <-done:
		require.ErrorIs(t, err, runlife.ErrServerShutdown)
	case <-time.After(5 * time.Second):
		t.Fatal("local owner did not join shutdown")
	}
	select {
	case request := <-stopped:
		require.Equal(t, runtimeID, request.ID, "force-stop must target the exact started runtime")
		require.True(t, request.Force, "owned runtime still needs bounded shutdown cleanup")
		require.Zero(t, request.Timeout)
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown returned without stopping the exact runtime")
	}
	require.Zero(t, callbacks)
	after, err := store.Get(before.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatusRunning, after.Status)
	require.Nil(t, after.CompletedAt)
	require.Equal(t, run.TaskStatusRunning, taskRunByID(after, tasks[0].ID).Status)
	require.Equal(t, run.TaskStatusPending, taskRunByID(after, tasks[1].ID).Status)
	require.Empty(t, engine.createRequestsForTask(tasks[1].ID))
	var count int64
	require.NoError(t, store.DB().Model(&models.ExecutionEvent{}).Where("run_id = ? AND type IN ?", before.ID,
		[]string{string(event.TypeTaskFailed), string(event.TypeTaskSkipped), string(event.TypeRunTerminal)}).Count(&count).Error)
	require.Zero(t, count)
}

func TestAbortedResumeShutdownPreservesPendingReplacementWork(t *testing.T) {
	j, store, _, tasks := characterizationJob(t, 1, nil)
	snapshot, err := store.Start(j.id, nil)
	require.NoError(t, err)
	require.NoError(t, store.RegisterTasks(snapshot.ID, []run.RegisterTaskInput{{Task: tasks[0], Atom: fakeModelAtom(tasks[0].AtomID)}}))
	require.NoError(t, store.DB().Model(&models.TaskRun{}).Where("job_run_id = ?", snapshot.ID).Update("partition_retry_pending", true).Error)
	var before models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ?", snapshot.ID).First(&before).Error)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(runlife.ErrServerShutdown)
	callbacks := 0
	j.dispatchRunCallbacks = func(context.Context, uuid.UUID, uuid.UUID, error) error { callbacks++; return nil }
	j.finalizeAbortedResume(ctx, store, snapshot.ID, context.Canceled)
	var after models.TaskRun
	require.NoError(t, store.DB().First(&after, "id = ?", before.ID).Error)
	require.Equal(t, before, after)
	got, err := store.Get(snapshot.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatusRunning, got.Status)
	require.Zero(t, callbacks)
}
