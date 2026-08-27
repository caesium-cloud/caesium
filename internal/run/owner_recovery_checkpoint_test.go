package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedChainRun creates a job with the linear DAG a -> b -> c and one pending,
// claimed task_run per step.
func seedChainRun(t *testing.T, db *gorm.DB, store *Store, claimedBy string) (uuid.UUID, [3]uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "cp-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "cp-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	a := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "a", Position: 0, CreatedAt: now, UpdatedAt: now}
	b := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "b", Position: 1, CreatedAt: now, UpdatedAt: now}
	c := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "c", Position: 2, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Task{a, b, c}).Error)
	require.NoError(t, db.Create([]*models.TaskEdge{
		{ID: uuid.New(), JobID: job.ID, FromTaskID: a.ID, ToTaskID: b.ID, CreatedAt: now},
		{ID: uuid.New(), JobID: job.ID, FromTaskID: b.ID, ToTaskID: c.ID, CreatedAt: now},
	}).Error)

	mk := func(task *models.Task, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(TaskStatusPending), ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mk(a, 0)
	mk(b, 1)
	mk(c, 1)
	return runRecord.ID, [3]uuid.UUID{a.ID, b.ID, c.ID}
}

// TestOwnerManager_RejectedCheckpointReplaysEveryTerminalRow pins the recovery
// ordering contract.
//
// The manager used to fetch TerminalTaskRunsSince(runID, checkpoint.SequenceHigh)
// BEFORE handing the checkpoint to Restore.  When Restore then rejected the blob
// (corrupt, or written by a newer snapshot version), recovery fell back to a
// from-scratch replay with a start sequence of zero — but the rows in hand were
// still only the post-checkpoint TAIL, so every terminal transition at or below
// sequence_high had vanished and the recovered owner re-dispatched work that had
// already completed.
func TestOwnerManager_RejectedCheckpointReplaysEveryTerminalRow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")
	taskA, taskB, taskC := ids[0], ids[1], ids[2]

	// Events:1 so each completion checkpoints, giving us a checkpoint at
	// sequence_high 1 (prefix) and one at 2 (tail).
	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))

	mgr.MarkDispatched(runID, taskA, "node-1", 1, 0)
	_, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskB, "node-1", 1, 0)
	_, err = mgr.Complete(runID, taskB, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	// Leave only the sequence_high=1 checkpoint, so a's completion sits at or
	// below it (prefix) and b's sits above it (tail).
	require.NoError(t, db.Where("run_id = ? AND sequence_high > ?", runID.String(), int64(1)).
		Delete(&models.RunCheckpoint{}).Error)
	// ...and make that checkpoint unreadable, exactly as a newer snapshot version
	// would look to this build.
	require.NoError(t, db.Model(&models.RunCheckpoint{}).
		Where("run_id = ? AND sequence_high = ?", runID.String(), int64(1)).
		Update("state_blob", []byte(`{"version":99}`)).Error)

	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.NotNil(t, cp)
	require.Equal(t, int64(1), cp.SequenceHigh, "precondition: the newest usable checkpoint claims sequence 1")
	require.Error(t, ValidateCheckpointBlob(cp.StateBlob), "precondition: that checkpoint is unreadable")

	// A peer takes the run over.
	takeover := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	res, err := takeover.Recover(runID, 2)
	require.NoError(t, err)

	require.Equal(t, []uuid.UUID{taskC}, res.Ready,
		"a rejected checkpoint must replay the FULL terminal history, not just the tail; "+
			"replaying only the tail loses a and re-dispatches it")
	require.Empty(t, res.ReDispatch)
	require.False(t, res.Complete)

	or, ok := takeover.get(runID)
	require.True(t, ok)
	for _, id := range []uuid.UUID{taskA, taskB} {
		st, known := or.state.TaskState(id)
		require.True(t, known)
		require.Equal(t, TaskStatusSucceeded, st.Status,
			"every terminal row, prefix and tail alike, must be replayed")
	}
	require.Equal(t, int64(2), or.state.Sequence(), "the sequence cursor must resume from the highest observed row")
}

// TestOwnerManager_UsableCheckpointStillReplaysOnlyTheTail is the control: the
// validate-first change must not turn a GOOD checkpoint into a full replay.
func TestOwnerManager_UsableCheckpointStillReplaysOnlyTheTail(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")
	taskA, taskB, taskC := ids[0], ids[1], ids[2]

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, taskA, "node-1", 1, 0)
	_, err := mgr.Complete(runID, taskA, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)
	mgr.MarkDispatched(runID, taskB, "node-1", 1, 0)
	_, err = mgr.Complete(runID, taskB, TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	takeover := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	res, err := takeover.Recover(runID, 2)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{taskC}, res.Ready)

	or, ok := takeover.get(runID)
	require.True(t, ok)
	stA, known := or.state.TaskState(taskA)
	require.True(t, known)
	require.Equal(t, TaskStatusSucceeded, stA.Status)
}

// TestValidateCheckpointBlob_AgreesWithRestore keeps the pre-flight check and
// the thing it predicts from ever drifting apart: a blob the validator accepts
// must Restore, and one it rejects must not.
func TestValidateCheckpointBlob_AgreesWithRestore(t *testing.T) {
	topo := RunTopology{Order: map[uuid.UUID]int{uuid.New(): 0}}
	good, err := NewRunState(topo, 0).Snapshot()
	require.NoError(t, err)

	for name, blob := range map[string][]byte{
		"good":            good,
		"unknown version": []byte(`{"version":99}`),
		"missing version": []byte(`{"tasks":{}}`),
		"not json":        []byte(`{`),
		"empty":           nil,
	} {
		validateErr := ValidateCheckpointBlob(blob)
		_, restoreErr := Restore(topo, blob)
		require.Equal(t, validateErr == nil, restoreErr == nil,
			"%s: validator and Restore must agree (validate=%v restore=%v)", name, validateErr, restoreErr)
	}
}
