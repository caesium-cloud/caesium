package run

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// plannedExpansion reproduces the payload Store.PlanFanOutExpansionForRow hands
// the owner (internal/run/fanout.go): every instance's OutstandingPredecessors
// is the PERSISTED template row's outstanding_predecessors plus that partition's
// in-group indegree.
//
// templateOutstanding is a parameter rather than derived because that column is
// exactly what the two lanes disagree about. The SQL lane decrements it on every
// predecessor completion (batchDecrementPredecessorsTx), so by expansion time it
// names the successor's REMAINING cross-step predecessors. The owner lane never
// decrements it — CompleteTaskOwner writes terminal rows only — so it still
// names the successor's ORIGINAL predecessor count however many predecessors
// have already landed.
func plannedExpansion(t *testing.T, producerID, fannedID uuid.UUID, templateOutstanding int, parts []pkgtask.Partition) (*FanOutExpansion, []uuid.UUID) {
	t.Helper()
	graph, err := pkgtask.ValidatePartitionGraph(parts)
	require.NoError(t, err)

	insts := make([]ExpandedInstance, 0, len(parts))
	ids := make([]uuid.UUID, 0, len(parts))
	for i, p := range parts {
		id := uuid.New()
		ids = append(ids, id)
		insts = append(insts, ExpandedInstance{
			TaskRunID:               id,
			TaskID:                  fannedID,
			PartitionIndex:          i,
			Partition:               p,
			OutstandingPredecessors: templateOutstanding + graph.Indegree[p.Key],
		})
	}
	return &FanOutExpansion{
		ProducerTaskID: producerID,
		Partitions:     parts,
		Groups: []ExpandedGroup{{
			TaskID:     fannedID,
			TaskName:   "process",
			Instances:  insts,
			Dependents: graph.Dependents,
		}},
	}, ids
}

// crossStepFanOutTopo is the shape this file is about: a fanned step waiting on
// several cross-step predecessors, only ONE of which is its fanOut.from
// producer, feeding a downstream fan-in step.  preds[0] is the producer.
type crossStepFanOutTopo struct {
	preds   []uuid.UUID
	fanned  uuid.UUID
	publish uuid.UUID
	rs      *RunState
}

func newCrossStepFanOutTopo(nPreds int) *crossStepFanOutTopo {
	b := newTopoBuilder()
	preds := make([]uuid.UUID, 0, nPreds)
	for i := 0; i < nPreds; i++ {
		preds = append(preds, b.task(""))
	}
	fanned := b.task("")
	publish := b.task(jobdefschema.TriggerRuleAllSuccess)
	for _, p := range preds {
		b.edge(p, fanned)
	}
	b.edge(fanned, publish)
	return &crossStepFanOutTopo{preds: preds, fanned: fanned, publish: publish, rs: NewRunState(b.build(), 0)}
}

// TestRunState_ExpansionPreservesEarlierPredecessorProgress is the regression
// test for the reported P1: a fanned step with two cross-step predecessors whose
// NON-producer predecessor completes first.  The owner decremented the catalog
// node in memory, then expansion replaced that node with instances seeded from
// the never-decremented SQL template, so the finished predecessor was counted a
// second time and every instance stayed pending forever.
func TestRunState_ExpansionPreservesEarlierPredecessorProgress(t *testing.T) {
	topo := newCrossStepFanOutTopo(2)
	rs := topo.rs
	produce, sidecar := topo.preds[0], topo.preds[1]
	require.Equal(t, 2, rs.indegree[topo.fanned])

	// The non-producer predecessor lands FIRST.  In owner mode this advances the
	// DAG in memory only, so the persisted template row still reads 2.
	rs.ApplyCompletion(sidecar, TaskStatusSucceeded, nil)
	require.Equal(t, 1, rs.indegree[topo.fanned], "the completed predecessor is accounted for in memory")

	exp, ids := plannedExpansion(t, produce, topo.fanned, 2, strParts("a", "b"))
	rs.ApplyExpansion(exp)
	res := rs.ApplyCompletion(produce, TaskStatusSucceeded, nil)

	require.ElementsMatch(t, ids, res.Ready,
		"every instance must be ready once ALL cross-step predecessors have completed")
	for _, id := range ids {
		require.Equal(t, 0, rs.indegree[id], "instance %s still waiting on a predecessor that already completed", id)
	}

	// The expansion payload is what CompleteTaskOwner persists, so the durable
	// rows must carry the same count memory used — which is also exactly what the
	// SQL lane writes for this ordering (its template was decremented by the
	// sidecar before expansion copied it).
	for _, inst := range exp.Groups[0].Instances {
		require.Equal(t, 1, inst.OutstandingPredecessors,
			"the persisted instance rows must mirror the owner's expansion base")
	}
}

// TestRunState_ExpansionBeforeOtherPredecessor is the same DAG in the other
// order: the producer lands first, so the instances must still wait for the
// sidecar and become ready exactly once, when it lands.
func TestRunState_ExpansionBeforeOtherPredecessor(t *testing.T) {
	topo := newCrossStepFanOutTopo(2)
	rs := topo.rs
	produce, sidecar := topo.preds[0], topo.preds[1]

	exp, ids := plannedExpansion(t, produce, topo.fanned, 2, strParts("a", "b"))
	rs.ApplyExpansion(exp)
	res := rs.ApplyCompletion(produce, TaskStatusSucceeded, nil)
	require.Empty(t, res.Ready, "instances must wait for the sidecar predecessor")
	for _, id := range ids {
		require.Equal(t, 1, rs.indegree[id])
	}

	res = rs.ApplyCompletion(sidecar, TaskStatusSucceeded, nil)
	require.ElementsMatch(t, ids, res.Ready, "the last cross-step predecessor must ready the whole group")
	require.NotContains(t, res.Ready, topo.publish, "publish waits for the group itself")
}

// TestRunState_ExpansionBaseIsOrderIndependent drives every interleaving of
// three cross-step predecessors around the expansion and asserts each instance
// becomes ready exactly once and the run reaches completion.
func TestRunState_ExpansionBaseIsOrderIndependent(t *testing.T) {
	orders := [][]int{
		{0, 1, 2}, // producer first
		{1, 0, 2}, // one sidecar, producer, other sidecar
		{1, 2, 0}, // both sidecars, then the producer
		{2, 1, 0},
	}
	for _, order := range orders {
		t.Run(fmt.Sprintf("%v", order), func(t *testing.T) {
			topo := newCrossStepFanOutTopo(3)
			rs := topo.rs
			var (
				ids       []uuid.UUID
				readyOnce = map[uuid.UUID]int{}
			)
			for _, idx := range order {
				var res CompletionResult
				if idx == 0 {
					// The producer's completion carries the partitions; the owner
					// plans the expansion from the never-decremented template row.
					var exp *FanOutExpansion
					exp, ids = plannedExpansion(t, topo.preds[0], topo.fanned, 3, strParts("a", "b"))
					rs.ApplyExpansion(exp)
					res = rs.ApplyCompletion(topo.preds[0], TaskStatusSucceeded, nil)
				} else {
					res = rs.ApplyCompletion(topo.preds[idx], TaskStatusSucceeded, nil)
				}
				for _, id := range res.Ready {
					readyOnce[id]++
				}
			}
			require.Len(t, ids, 2)
			for _, id := range ids {
				require.Equal(t, 1, readyOnce[id], "instance %s must be dispatched exactly once", id)
			}

			// Drive the group to completion; the fan-in must then be released.
			var last CompletionResult
			for _, id := range ids {
				last = rs.ApplyCompletion(id, TaskStatusSucceeded, nil)
			}
			require.Equal(t, []uuid.UUID{topo.publish}, last.Ready)
			last = rs.ApplyCompletion(topo.publish, TaskStatusSucceeded, nil)
			require.True(t, last.Complete, "the run must reach completion in every predecessor order")
		})
	}
}

// TestRunState_ExpansionPreservesInGroupEdges asserts the corrected cross-step
// base is still ADDED to each partition's in-group indegree: an ordered group
// expanded after a sidecar predecessor must run its root first, not all at once.
func TestRunState_ExpansionPreservesInGroupEdges(t *testing.T) {
	topo := newCrossStepFanOutTopo(2)
	rs := topo.rs
	produce, sidecar := topo.preds[0], topo.preds[1]

	rs.ApplyCompletion(sidecar, TaskStatusSucceeded, nil)
	exp, ids := plannedExpansion(t, produce, topo.fanned, 2, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
	})
	rs.ApplyExpansion(exp)
	res := rs.ApplyCompletion(produce, TaskStatusSucceeded, nil)

	require.Equal(t, []uuid.UUID{ids[0]}, res.Ready, "only the in-group root is ready")
	require.Equal(t, 1, rs.indegree[ids[1]], "b still waits on a")
	// The durable row is written at expansion time, before the producer's own
	// decrement: one outstanding cross-step predecessor (the producer) plus b's
	// in-group edge on a.
	require.Equal(t, 1, exp.Groups[0].Instances[0].OutstandingPredecessors, "a waits only on the producer")
	require.Equal(t, 2, exp.Groups[0].Instances[1].OutstandingPredecessors,
		"the rebased row keeps the in-group edge on top of the cross-step base")

	res = rs.ApplyCompletion(ids[0], TaskStatusSucceeded, nil)
	require.Equal(t, []uuid.UUID{ids[1]}, res.Ready)
}

// TestRunState_ExpansionOfUnknownCatalogTaskKeepsPlannedCounts pins the guard:
// the owner only overrides the planner's count when it actually holds the
// catalog node the group replaces.  For a group this state never tracked, the
// planned counts stand rather than collapsing to zero (which would dispatch
// every instance immediately).
func TestRunState_ExpansionOfUnknownCatalogTaskKeepsPlannedCounts(t *testing.T) {
	b := newTopoBuilder()
	produce := b.task("")
	rs := NewRunState(b.build(), 0)

	stranger := uuid.New()
	exp, ids := plannedExpansion(t, produce, stranger, 2, strParts("a", "b"))
	rs.ApplyExpansion(exp)

	for i, id := range ids {
		require.Equal(t, 2, rs.indegree[id])
		require.Equal(t, 2, exp.Groups[0].Instances[i].OutstandingPredecessors)
		require.NotContains(t, rs.ReadyTasks(), id, "an instance with outstanding predecessors is not dispatchable")
	}
}

// --- DB-backed lanes ------------------------------------------------------

// crossStepFixture is the same DAG on real rows: N predecessor steps feeding one
// fanned step (fanOut.from = preds[0]) feeding a fan-in step.
type crossStepFixture struct {
	db      *gorm.DB
	store   *Store
	jobID   uuid.UUID
	runID   uuid.UUID
	preds   []*models.Task
	fanned  *models.Task
	publish *models.Task
}

func newCrossStepFixture(t *testing.T, nPreds int) *crossStepFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "cross-step-" + uuid.NewString()[:8]}).Error)

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)

	preds := make([]*models.Task, 0, nPreds)
	for i := 0; i < nPreds; i++ {
		p := &models.Task{
			ID: uuid.New(), JobID: jobID, AtomID: atom.ID,
			Name: fmt.Sprintf("pred-%d", i), Position: i, Type: "task",
			TriggerRule: jobdefschema.TriggerRuleAllSuccess,
		}
		require.NoError(t, db.Create(p).Error)
		preds = append(preds, p)
	}

	foCfg, err := json.Marshal(&jobdefschema.FanOut{From: preds[0].Name, MaxPartitions: 16})
	require.NoError(t, err)
	fanned := &models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atom.ID,
		Name: "process", Position: nPreds, Type: "task",
		TriggerRule: jobdefschema.TriggerRuleAllSuccess, FanOutConfig: datatypes.JSON(foCfg),
	}
	publish := &models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atom.ID,
		Name: "publish", Position: nPreds + 1, Type: "task",
		TriggerRule: jobdefschema.TriggerRuleAllSuccess,
	}
	require.NoError(t, db.Create(fanned).Error)
	require.NoError(t, db.Create(publish).Error)
	for _, p := range preds {
		require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: p.ID, ToTaskID: fanned.ID}).Error)
	}
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: fanned.ID, ToTaskID: publish.ID}).Error)

	inputs := make([]RegisterTaskInput, 0, nPreds+2)
	for _, p := range preds {
		inputs = append(inputs, RegisterTaskInput{Task: p, Atom: atom, OutstandingPredecessors: 0})
	}
	inputs = append(inputs,
		RegisterTaskInput{Task: fanned, Atom: atom, OutstandingPredecessors: nPreds},
		RegisterTaskInput{Task: publish, Atom: atom, OutstandingPredecessors: 1},
	)
	require.NoError(t, store.RegisterTasks(runRecord.ID, inputs))

	return &crossStepFixture{
		db: db, store: store, jobID: jobID, runID: runRecord.ID,
		preds: preds, fanned: fanned, publish: publish,
	}
}

func (f *crossStepFixture) instanceRows(t *testing.T) []models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.fanned.ID).
		Order("partition_index ASC").Find(&rows).Error)
	return rows
}

func (f *crossStepFixture) owner(t *testing.T) *OwnerManager {
	t.Helper()
	mgr := NewOwnerManager(f.store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(f.runID, 1))
	return mgr
}

func (f *crossStepFixture) complete(t *testing.T, mgr *OwnerManager, taskID, taskRunID uuid.UUID, parts []pkgtask.Partition) CompleteResult {
	t.Helper()
	id := taskID
	if taskRunID != uuid.Nil {
		id = taskRunID
	}
	mgr.MarkDispatched(f.runID, id, "node-1", 1, 0)
	res, err := mgr.CompleteInstance(f.runID, taskID, taskRunID, TaskStatusSucceeded, "success", "", "", nil, nil, parts)
	require.NoError(t, err)
	require.True(t, res.Owned)
	return res
}

// TestOwnerManager_ExpansionAfterOtherPredecessorCompletes drives the reported
// stall through the real owner path: the sidecar predecessor completes, then the
// producer expands the group.  Every instance must be dispatchable, and the run
// must finish.
func TestOwnerManager_ExpansionAfterOtherPredecessorCompletes(t *testing.T) {
	f := newCrossStepFixture(t, 2)
	mgr := f.owner(t)

	res := f.complete(t, mgr, f.preds[1].ID, uuid.Nil, nil)
	require.Empty(t, res.Ready, "the fanned step still waits for its producer")

	res = f.complete(t, mgr, f.preds[0].ID, uuid.Nil, strParts("a", "b"))
	require.Len(t, res.Ready, 2,
		"both instances must be ready: every cross-step predecessor has completed")

	rows := f.instanceRows(t)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, 1, row.OutstandingPredecessors,
			"the durable row must mirror the owner's expansion base (the producer's own edge, decremented in memory only)")
	}

	ready := mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, 2)
	var last CompleteResult
	for _, dt := range ready {
		last = f.complete(t, mgr, dt.TaskID, dt.TaskRunID, nil)
	}
	require.Equal(t, []uuid.UUID{f.publish.ID}, last.Ready, "the fan-in must be released by the resolved group")

	last = f.complete(t, mgr, f.publish.ID, uuid.Nil, nil)
	require.True(t, last.Complete, "the run must complete")
}

// TestOwnerManager_ExpansionBeforeOtherPredecessorCompletes is the other order
// on the same rows: the producer expands first, so the instances wait for the
// sidecar and are readied exactly once when it lands.
func TestOwnerManager_ExpansionBeforeOtherPredecessorCompletes(t *testing.T) {
	f := newCrossStepFixture(t, 2)
	mgr := f.owner(t)

	res := f.complete(t, mgr, f.preds[0].ID, uuid.Nil, strParts("a", "b"))
	require.Empty(t, res.Ready, "instances must wait for the sidecar predecessor")

	res = f.complete(t, mgr, f.preds[1].ID, uuid.Nil, nil)
	require.Len(t, res.Ready, 2, "the last predecessor readies the whole group")

	ready := mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, 2)
	var last CompleteResult
	for _, dt := range ready {
		last = f.complete(t, mgr, dt.TaskID, dt.TaskRunID, nil)
	}
	require.Equal(t, []uuid.UUID{f.publish.ID}, last.Ready)
	last = f.complete(t, mgr, f.publish.ID, uuid.Nil, nil)
	require.True(t, last.Complete)
}

// TestOwnerManager_ExpansionWithThreePredecessors covers a fanned step waiting
// on three cross-step steps, expanded in the middle of them.
func TestOwnerManager_ExpansionWithThreePredecessors(t *testing.T) {
	f := newCrossStepFixture(t, 3)
	mgr := f.owner(t)

	require.Empty(t, f.complete(t, mgr, f.preds[2].ID, uuid.Nil, nil).Ready)
	require.Empty(t, f.complete(t, mgr, f.preds[0].ID, uuid.Nil, strParts("a", "b")).Ready,
		"pred-1 has not completed yet")

	rows := f.instanceRows(t)
	for _, row := range rows {
		require.Equal(t, 2, row.OutstandingPredecessors,
			"expansion must carry forward exactly the predecessors still outstanding at expansion time")
	}

	res := f.complete(t, mgr, f.preds[1].ID, uuid.Nil, nil)
	require.Len(t, res.Ready, 2, "the final predecessor readies the group")

	var last CompleteResult
	for _, dt := range mgr.ReadyForDispatch(f.runID) {
		last = f.complete(t, mgr, dt.TaskID, dt.TaskRunID, nil)
	}
	require.Equal(t, []uuid.UUID{f.publish.ID}, last.Ready)
	require.True(t, f.complete(t, mgr, f.publish.ID, uuid.Nil, nil).Complete)
}

// TestCompleteTaskSQLLaneExpansionIsOrderIndependent is the SQL lane's half of
// the same property.  It holds there because completeTask decrements the
// template row on every predecessor completion BEFORE expansion copies it, and
// then decrements the materialized instances by task_id.  Pinned so a change to
// either half cannot silently reintroduce the owner-lane defect here.
func TestCompleteTaskSQLLaneExpansionIsOrderIndependent(t *testing.T) {
	t.Run("sidecar first", func(t *testing.T) {
		f := newCrossStepFixture(t, 2)
		require.NoError(t, f.store.CompleteTask(f.runID, f.preds[1].ID, "success", nil, nil))

		var template models.TaskRun
		require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.fanned.ID).First(&template).Error)
		require.Equal(t, 1, template.OutstandingPredecessors,
			"the SQL lane decrements the template, which is why copying it is safe there")

		_, err := f.store.CompleteTaskWithPartitions(f.runID, f.preds[0].ID, "success", nil, nil, strParts("a", "b"))
		require.NoError(t, err)

		rows := f.instanceRows(t)
		require.Len(t, rows, 2)
		for _, row := range rows {
			require.Equal(t, 0, row.OutstandingPredecessors, "partition %s must be dispatchable", row.PartitionValue)
			require.Equal(t, string(TaskStatusPending), row.Status)
		}
	})

	t.Run("producer first", func(t *testing.T) {
		f := newCrossStepFixture(t, 2)
		_, err := f.store.CompleteTaskWithPartitions(f.runID, f.preds[0].ID, "success", nil, nil, strParts("a", "b"))
		require.NoError(t, err)
		for _, row := range f.instanceRows(t) {
			require.Equal(t, 1, row.OutstandingPredecessors, "the sidecar predecessor is still outstanding")
		}

		require.NoError(t, f.store.CompleteTask(f.runID, f.preds[1].ID, "success", nil, nil))
		rows := f.instanceRows(t)
		require.Len(t, rows, 2)
		for _, row := range rows {
			require.Equal(t, 0, row.OutstandingPredecessors, "partition %s must be dispatchable", row.PartitionValue)
		}
	})

	// Three cross-step predecessors, expanded in the MIDDLE of them: the SQL
	// lane's counterpart to TestOwnerManager_ExpansionWithThreePredecessors, and
	// the case that distinguishes "the template happened to be right" from "the
	// template is decremented on every predecessor". Two decrements have to
	// survive the expansion here, not one.
	t.Run("three predecessors, expanded in the middle", func(t *testing.T) {
		f := newCrossStepFixture(t, 3)
		require.NoError(t, f.store.CompleteTask(f.runID, f.preds[2].ID, "success", nil, nil))

		_, err := f.store.CompleteTaskWithPartitions(f.runID, f.preds[0].ID, "success", nil, nil, strParts("a", "b"))
		require.NoError(t, err)
		for _, row := range f.instanceRows(t) {
			require.Equal(t, 1, row.OutstandingPredecessors,
				"partition %s must still be waiting on pred-1 alone", row.PartitionValue)
		}

		require.NoError(t, f.store.CompleteTask(f.runID, f.preds[1].ID, "success", nil, nil))
		rows := f.instanceRows(t)
		require.Len(t, rows, 2)
		for _, row := range rows {
			require.Equal(t, 0, row.OutstandingPredecessors,
				"partition %s must be dispatchable once every predecessor has landed", row.PartitionValue)
			require.Equal(t, string(TaskStatusPending), row.Status)
		}
	})
}

// TestRehydrateInGroupEdges_AgreesWithLiveExpansionBase asserts a takeover
// rebuilds the counts the live path now produces: RehydrateInGroupEdges already
// seeds instances from the catalog node's REMAINING in-memory indegree, so a
// recovered owner and a live one must reach the same readiness for the same
// completion order.
func TestRehydrateInGroupEdges_AgreesWithLiveExpansionBase(t *testing.T) {
	topo := newCrossStepFanOutTopo(2)
	rs := topo.rs
	produce, sidecar := topo.preds[0], topo.preds[1]

	// The sidecar landed and was checkpointed; the producer's expansion rows are
	// on disk but the producer's own terminal row is still in the replay tail.
	rs.ApplyCompletion(sidecar, TaskStatusSucceeded, nil)

	runID := uuid.New()
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: topo.fanned, PartitionValue: "a", PartitionIndex: 0, PartitionCount: 2, Status: string(TaskStatusPending)},
		{ID: uuid.New(), JobRunID: runID, TaskID: topo.fanned, PartitionValue: "b", PartitionIndex: 1, PartitionCount: 2, Status: string(TaskStatusPending)},
	}
	rs.RehydrateInGroupEdges(rows, []models.Task{{ID: topo.fanned, Name: "process"}})

	for _, row := range rows {
		require.Equal(t, 1, rs.indegree[row.ID],
			"a rehydrated instance inherits the catalog node's REMAINING cross-step count")
	}

	ready := rs.ApplyTerminalRow(produce, TaskStatusSucceeded, 2)
	require.ElementsMatch(t, []uuid.UUID{rows[0].ID, rows[1].ID}, ready,
		"replaying the producer's terminal row must ready every instance")
}
