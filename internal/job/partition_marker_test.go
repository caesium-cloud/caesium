package job

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type partitionMarkerEngine struct {
	atom.Engine
	stops   int
	stopErr error
}

func (e *partitionMarkerEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("##caesium::partitions not-json\n")), nil
}
func (e *partitionMarkerEngine) Stop(req *atom.EngineStopRequest) error {
	e.stops++
	if !req.Force {
		return errors.New("partition failure must force stop")
	}
	_ = e.Engine.Stop(req)
	return e.stopErr
}

func TestLocalPartitionMarkerErrorPreservesStopCause(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
	taskSvc := &fakeTaskService{tasks: models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID}}}
	persistGraph(t, db, taskSvc.tasks, nil)
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	stopErr := errors.New("stop failed")
	engine := &partitionMarkerEngine{Engine: newFakeEngine(), stopErr: stopErr}
	opts := withTestDeps(store, env.Environment{MaxParallelTasks: 1, TaskFailurePolicy: taskFailurePolicyHalt, ExecutionMode: executionModeLocal}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)
	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	var partitionErr *pkgtask.PartitionError
	require.ErrorAs(t, err, &partitionErr)
	require.ErrorIs(t, err, stopErr)
	require.Equal(t, 1, engine.stops)
	snapshot := latestRunSnapshot(t, store, jobID)
	require.Equal(t, run.TaskStatusFailed, taskRunByID(snapshot, taskID).Status)
	require.Empty(t, taskRunByID(snapshot, taskID).Output)
}
