package run

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// fanOutRunFixture is a producer -> fanned successor run with both template rows
// already seeded, ready for the owner to expand.
type fanOutRunFixture struct {
	runID    uuid.UUID
	producer *models.Task
	shard    *models.Task
}

func seedFanOutProducerRun(t *testing.T, db *gorm.DB, store *Store) fanOutRunFixture {
	t.Helper()
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "atom-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "atom-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	producer := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "producer", Position: 0, CreatedAt: now, UpdatedAt: now}
	shard := &models.Task{
		ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "shard", Position: 1,
		FanOutConfig: datatypes.JSON([]byte(`{"from":"producer"}`)),
		CreatedAt:    now, UpdatedAt: now,
	}
	require.NoError(t, db.Create([]*models.Task{producer, shard}).Error)
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: job.ID, FromTaskID: producer.ID, ToTaskID: shard.ID, CreatedAt: now}).Error)

	mk := func(task *models.Task, claimedBy string, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(TaskStatusPending), ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mk(producer, "node-1", 0)
	mk(shard, "", 1)

	return fanOutRunFixture{runID: runRecord.ID, producer: producer, shard: shard}
}

// failNextUpdate arms a one-shot failure on the next UPDATE statement this DB
// issues.  It is a real transaction abort — gorm sees the error before the
// statement runs, so the enclosing CompleteTaskOwner transaction rolls back
// exactly as a genuine dqlite write failure would.
func failNextUpdate(t *testing.T, db *gorm.DB) *atomic.Bool {
	t.Helper()
	var armed atomic.Bool
	err := db.Callback().Update().Before("gorm:update").Register("test:fail_next_update", func(tx *gorm.DB) {
		if armed.CompareAndSwap(true, false) {
			_ = tx.AddError(errors.New("injected durable write failure"))
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Callback().Update().Remove("test:fail_next_update")
	})
	return &armed
}

// TestOwnerManager_FailedCommitPublishesNothingThenConverges pins the
// completion/durable-write atomicity contract.
//
// CompleteInstance used to apply ApplyExpansion + ApplyCompletion to the
// AUTHORITATIVE in-memory state and only then call CompleteTaskOwner.  When that
// transaction failed, the producer was already terminal in memory, so the
// worker's redelivery took ApplyCompletion's already-terminal branch, skipped
// expansion re-planning, and re-persisted the completion with expansion = nil —
// while the owner's ready queue held instance ids that exist in no row and
// dispatched them forever against a missing row.
func TestOwnerManager_FailedCommitPublishesNothingThenConverges(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	fx := seedFanOutProducerRun(t, db, store)

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(fx.runID, 1))
	mgr.MarkDispatched(fx.runID, fx.producer.ID, "node-1", 1, 0)

	partitions := []pkgtask.Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}}

	armed := failNextUpdate(t, db)
	armed.Store(true)

	_, err := mgr.CompleteInstance(fx.runID, fx.producer.ID, uuid.Nil, TaskStatusSucceeded,
		"success", "", "node-1", nil, nil, partitions)
	require.Error(t, err, "the injected write failure must surface to the worker as an error")
	require.False(t, armed.Load(), "the injected failure must actually have fired")

	// Nothing was written...
	var shardRows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", fx.runID, fx.shard.ID).Find(&shardRows).Error)
	require.Len(t, shardRows, 1, "a rolled-back completion must not persist any instance rows")

	var producerRow models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", fx.runID, fx.producer.ID).First(&producerRow).Error)
	require.NotEqual(t, string(TaskStatusSucceeded), producerRow.Status,
		"a rolled-back completion must not leave a terminal row behind")

	// ...and nothing was published: the producer is still in flight in memory and
	// the ready queue holds no instance id.
	or, ok := mgr.get(fx.runID)
	require.True(t, ok)
	producerState, known := or.state.TaskState(fx.producer.ID)
	require.True(t, known)
	require.False(t, IsTerminal(producerState.Status),
		"the authoritative state must not show a completion whose write was rolled back")
	require.Empty(t, mgr.ReadyForDispatch(fx.runID),
		"no instance may be dispatchable before its rows exist")

	// The worker re-POSTs the identical envelope; this time the write lands.
	res, err := mgr.CompleteInstance(fx.runID, fx.producer.ID, uuid.Nil, TaskStatusSucceeded,
		"success", "", "node-1", nil, nil, partitions)
	require.NoError(t, err)
	require.True(t, res.Owned)
	require.Len(t, res.Ready, 3, "the redelivery must re-plan the expansion the failed commit dropped")

	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", fx.runID, fx.shard.ID).Find(&shardRows).Error)
	require.Len(t, shardRows, 3, "the DB must carry all three instance rows")

	rowIDs := make(map[uuid.UUID]bool, len(shardRows))
	for _, row := range shardRows {
		rowIDs[row.ID] = true
	}
	ready := mgr.ReadyForDispatch(fx.runID)
	require.Len(t, ready, 3)
	for _, dt := range ready {
		require.Equal(t, fx.shard.ID, dt.TaskID)
		require.True(t, rowIDs[dt.TaskRunID],
			"every dispatchable instance id must name a row that exists: %s", dt.TaskRunID)
	}

	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", fx.runID, fx.producer.ID).First(&producerRow).Error)
	require.Equal(t, string(TaskStatusSucceeded), producerRow.Status)
	require.Greater(t, producerRow.TerminalSequence, int64(0))
}

// TestOwnerManager_CompletionAfterDurableExpansionAdoptsPersistedRows covers the
// other side of the same rollback: the commit LANDED but the owner never saw the
// ack, so it discarded its staged state.  The redelivery re-plans, and the
// planner cannot find a unique successor template any more (the group is already
// N rows).  Reading that as "the producer failed" would kill a group that is
// already materialized, so the owner adopts the durable rows instead.
func TestOwnerManager_CompletionAfterDurableExpansionAdoptsPersistedRows(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	fx := seedFanOutProducerRun(t, db, store)

	partitions := []pkgtask.Partition{{Key: "a"}, {Key: "b"}}

	// Persist the expansion out-of-band: this is the durable state a commit that
	// landed without an ack leaves behind.
	exp, err := store.PlanFanOutExpansion(fx.runID, fx.producer.ID, partitions)
	require.NoError(t, err)
	require.NoError(t, store.CompleteTaskOwner(fx.runID, fx.producer.ID, TaskStatusSucceeded,
		"success", "", "node-1", nil, nil, 1, 1, nil, exp))

	var shardRows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", fx.runID, fx.shard.ID).Find(&shardRows).Error)
	require.Len(t, shardRows, 2, "precondition: the group is already expanded durably")

	// A fresh owner that never saw that write now receives the redelivery.
	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(fx.runID, 1))
	mgr.MarkDispatched(fx.runID, fx.producer.ID, "node-1", 1, 0)

	res, err := mgr.CompleteInstance(fx.runID, fx.producer.ID, uuid.Nil, TaskStatusSucceeded,
		"success", "", "node-1", nil, nil, partitions)
	require.NoError(t, err)
	require.True(t, res.Owned)

	or, ok := mgr.get(fx.runID)
	require.True(t, ok)
	producerState, known := or.state.TaskState(fx.producer.ID)
	require.True(t, known)
	require.Equal(t, TaskStatusSucceeded, producerState.Status,
		"an already-expanded group must not be read as a producer failure")

	rowIDs := make(map[uuid.UUID]bool, len(shardRows))
	for _, row := range shardRows {
		rowIDs[row.ID] = true
	}
	ready := mgr.ReadyForDispatch(fx.runID)
	require.Len(t, ready, 2, "the owner must adopt the two persisted instances")
	for _, dt := range ready {
		require.True(t, rowIDs[dt.TaskRunID],
			"adopted instance ids must be the DURABLE ones: %s", dt.TaskRunID)
	}
}

// TestRunState_CloneIsolatesEveryMutation is the unit-level guarantee the staged
// completion path rests on: a mutation applied to the copy must be invisible to
// the original, including through the slice-valued fan-out maps that
// ApplyExpansion appends to.
func TestRunState_CloneIsolatesEveryMutation(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	topo := RunTopology{
		Adjacency:    map[uuid.UUID][]uuid.UUID{a: {b}},
		Predecessors: map[uuid.UUID][]uuid.UUID{b: {a}},
		Order:        map[uuid.UUID]int{a: 0, b: 1},
	}
	rs := NewRunState(topo, 0)

	clone := rs.Clone()
	clone.ApplyExpansion(&FanOutExpansion{
		ProducerTaskID: a,
		Groups: []ExpandedGroup{{
			TaskID: b,
			Instances: []ExpandedInstance{
				{TaskRunID: c, TaskID: b, PartitionIndex: 0, Partition: pkgtask.Partition{Key: "p0"}, OutstandingPredecessors: 1},
			},
		}},
	})
	res := clone.ApplyCompletion(a, TaskStatusSucceeded, nil)
	require.True(t, res.Applied)

	// The original is untouched: sequence, terminal count, ready queue, and the
	// instance maps.
	require.Equal(t, int64(0), rs.Sequence(), "clone mutations must not advance the original's cursor")
	require.Equal(t, []uuid.UUID{a}, rs.ReadyTasks(), "the original's ready queue must be unchanged")
	_, isInstance := rs.CatalogTaskID(c)
	require.False(t, isInstance, "the original must not learn about the clone's instances")
	origA, ok := rs.TaskState(a)
	require.True(t, ok)
	require.Equal(t, TaskStatusPending, origA.Status, "the original's task states must be independent pointers")

	// And the clone really did advance.
	require.Equal(t, int64(1), clone.Sequence())
	require.Equal(t, []uuid.UUID{c}, clone.ReadyTasks())
}
