package run

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// store_readsurface_test.go covers the instance-addressed read surfaces the
// observability endpoints use. TaskLogSnapshotForInstance in particular is what
// `GET …/runs/:run_id/logs` serves whenever no live container stream is
// available — which, since every engine's Stop is stop-AND-remove, is every
// request for a task that has finished.

// readSurfaceFixture materializes one catalog task expanded into N fan-out
// instance rows, as the expansion transaction does.
type readSurfaceFixture struct {
	db          *gorm.DB
	store       *Store
	runID       uuid.UUID
	taskID      uuid.UUID
	instanceIDs []uuid.UUID
	keys        []string
}

func newReadSurfaceFixture(t *testing.T, keys ...string) *readSurfaceFixture {
	t.Helper()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atomModel := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hello"]`,
	}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomModel.ID, Name: "process"}
	require.NoError(t, db.Create(task).Error)

	f := &readSurfaceFixture{db: db, store: store, runID: runRecord.ID, taskID: task.ID, keys: keys}
	for i, key := range keys {
		id := uuid.New()
		f.instanceIDs = append(f.instanceIDs, id)
		row := &models.TaskRun{
			ID:          id,
			JobRunID:    runRecord.ID,
			TaskID:      task.ID,
			AtomID:      atomModel.ID,
			Engine:      models.AtomEngineDocker,
			Image:       atomModel.Image,
			Command:     atomModel.Command,
			Status:      string(TaskStatusSucceeded),
			Attempt:     1,
			MaxAttempts: 1,
		}
		if len(keys) > 1 || key != "" {
			row.PartitionValue = key
			row.PartitionIndex = i
			row.PartitionCount = len(keys)
		}
		require.NoError(t, db.Create(row).Error)
	}
	return f
}

// TestTaskLogSnapshotForInstanceReturnsThatInstancesLog: the (run, task)
// predicate GetTaskLogSnapshot uses matches N rows for a fanned group and
// returns an arbitrary sibling's log. This one is keyed on the TaskRun PK.
func TestTaskLogSnapshotForInstanceReturnsThatInstancesLog(t *testing.T) {
	f := newReadSurfaceFixture(t, "a", "b", "c")
	for i, key := range f.keys {
		require.NoError(t, f.store.SaveTaskLogSnapshot(f.runID, f.instanceIDs[i],
			&TaskLogSnapshot{Text: "partition=" + key + "\n"}))
	}

	for i, key := range f.keys {
		snapshot, err := f.store.TaskLogSnapshotForInstance(context.Background(), f.runID, f.instanceIDs[i])
		require.NoError(t, err)
		require.NotNil(t, snapshot, "partition %s has a persisted log", key)
		require.Equal(t, "partition="+key+"\n", snapshot.Text,
			"partition %s must get its own log, not a sibling's", key)
		require.False(t, snapshot.Truncated)
	}
}

// TestTaskLogSnapshotForInstanceCarriesTruncation: the truncation flag drives
// the X-Caesium-Log-Truncated response header, so it must survive the read even
// though a truncated capture can still be non-empty.
func TestTaskLogSnapshotForInstanceCarriesTruncation(t *testing.T) {
	f := newReadSurfaceFixture(t, "a", "b")
	require.NoError(t, f.store.SaveTaskLogSnapshot(f.runID, f.instanceIDs[1],
		&TaskLogSnapshot{Text: "head of a very long log", Truncated: true}))

	snapshot, err := f.store.TaskLogSnapshotForInstance(context.Background(), f.runID, f.instanceIDs[1])
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.True(t, snapshot.Truncated)
}

// TestTaskLogSnapshotForInstanceNilWhenNothingCaptured: a silent container is
// (nil, nil), which the handler renders as a 204 log state rather than an error.
func TestTaskLogSnapshotForInstanceNilWhenNothingCaptured(t *testing.T) {
	f := newReadSurfaceFixture(t, "a", "b")

	snapshot, err := f.store.TaskLogSnapshotForInstance(context.Background(), f.runID, f.instanceIDs[0])
	require.NoError(t, err)
	require.Nil(t, snapshot)
}

// TestTaskLogSnapshotForInstanceIsRunScoped: a TaskRun id from another run must
// not resolve through a run-scoped route, even though the id alone is unique.
func TestTaskLogSnapshotForInstanceIsRunScoped(t *testing.T) {
	f := newReadSurfaceFixture(t, "a")
	require.NoError(t, f.store.SaveTaskLogSnapshot(f.runID, f.instanceIDs[0],
		&TaskLogSnapshot{Text: "partition=a\n"}))

	_, err := f.store.TaskLogSnapshotForInstance(context.Background(), uuid.New(), f.instanceIDs[0])
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// TestTaskRunInstancesOrderedByPartitionIndex pins the stable order the logs
// endpoint's 400 body and the partitions endpoint both depend on.
func TestTaskRunInstancesOrderedByPartitionIndex(t *testing.T) {
	f := newReadSurfaceFixture(t, "a", "b", "c")

	instances, err := f.store.TaskRunInstances(context.Background(), f.runID, f.taskID)
	require.NoError(t, err)
	require.Len(t, instances, 3)
	for i, inst := range instances {
		require.Equal(t, f.keys[i], inst.PartitionValue)
		require.Equal(t, i, inst.PartitionIndex)
		require.Equal(t, f.instanceIDs[i], inst.ID,
			"the returned rows must keep their own TaskRun primary key so a caller can address one")
	}
}
