package run

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// fannedGroup expands catalog task `taskID` into instances for the supplied
// partitions, wiring in-group edges from each partition's DependsOn.
func fannedGroup(t *testing.T, rs *RunState, taskID uuid.UUID, name string, maxParallel int, parts []pkgtask.Partition) []uuid.UUID {
	t.Helper()
	graph, err := pkgtask.ValidatePartitionGraph(parts)
	require.NoError(t, err)

	base := rs.indegree[taskID]
	insts := make([]ExpandedInstance, 0, len(parts))
	ids := make([]uuid.UUID, 0, len(parts))
	for i, p := range parts {
		id := uuid.New()
		ids = append(ids, id)
		indegree := 0
		if graph != nil {
			indegree = graph.Indegree[p.Key]
		}
		insts = append(insts, ExpandedInstance{
			TaskRunID:               id,
			TaskID:                  taskID,
			PartitionIndex:          i,
			Partition:               p,
			OutstandingPredecessors: base + indegree,
		})
	}
	group := ExpandedGroup{TaskID: taskID, TaskName: name, MaxParallel: maxParallel, Instances: insts}
	if graph != nil {
		group.Dependents = graph.Dependents
	}
	rs.ApplyExpansion(&FanOutExpansion{ProducerTaskID: uuid.New(), Partitions: parts, Groups: []ExpandedGroup{group}})
	return ids
}

// TestRunState_GroupResolvesCrossStepSuccessorExactlyOnce pins the over-decrement
// defect: when an in-group failure cascades skips across a group, every member is
// seeded into the successor traversal, so a cross-step successor was decremented
// once per seed instead of once per group.
func TestRunState_GroupResolvesCrossStepSuccessorExactlyOnce(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	other := b.task("")
	downstream := b.task(jobdefschema.TriggerRuleAllDone)
	b.edge(producer, fanned)
	b.edge(fanned, downstream)
	b.edge(other, downstream)
	rs := NewRunState(b.build(), 0)

	// downstream has two predecessors: the fanned group and an unrelated step.
	require.Equal(t, 2, rs.indegree[downstream])

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "shard", 0, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"b"}},
	})
	// This case is about the `continue` cascade seeding several group members
	// into one traversal. fail_fast (the default) seeds the same way, so the
	// single-decrement property is asserted for it too by
	// TestRunState_FailFastResolvesEveryPendingSibling.
	rs.SetGroupFailurePolicy(fanned, jobdefschema.FanOutFailureContinue)
	// The producer already decremented the template; instances inherit it.
	require.Equal(t, 0, rs.indegree[insts[0]])

	res := rs.ApplyCompletion(insts[0], TaskStatusFailed, nil)

	// b and c are both skipped (transitive in-group cascade).
	require.Len(t, res.Skipped, 2, "a failed root must skip its transitive in-group dependents")
	for _, sk := range res.Skipped {
		require.Equal(t, "fan-out dependency a failed", sk.Reason,
			"owner-mode skip reason must name the partition key, matching the SQL path")
	}

	// The group is now fully resolved. downstream must have been decremented
	// exactly once for the whole group, leaving `other` outstanding.
	require.Equal(t, 1, rs.indegree[downstream],
		"a resolved fan-out group must decrement a cross-step successor exactly once")
	require.NotContains(t, res.Ready, downstream, "downstream is not ready until `other` completes")

	res = rs.ApplyCompletion(other, TaskStatusSucceeded, nil)
	require.Equal(t, 0, rs.indegree[downstream])
	require.Equal(t, []uuid.UUID{downstream}, res.Ready, "downstream must become ready exactly once")
}

// TestRunState_ReadyHasNoDuplicatesOnGroupResolve asserts the returned ready
// slice never repeats a successor when several group members seed the traversal.
func TestRunState_ReadyHasNoDuplicatesOnGroupResolve(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	downstream := b.task(jobdefschema.TriggerRuleAllDone)
	b.edge(producer, fanned)
	b.edge(fanned, downstream)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "shard", 0, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"a"}},
	})

	res := rs.ApplyCompletion(insts[0], TaskStatusFailed, nil)
	seen := map[uuid.UUID]int{}
	for _, id := range res.Ready {
		seen[id]++
	}
	for id, n := range seen {
		require.Equalf(t, 1, n, "task %s dispatched %d times from one completion", id, n)
	}
	require.Equal(t, []uuid.UUID{downstream}, res.Ready)
}

// TestRunState_InGroupSkipReasonUsesPartitionKey pins the partitionKey stub:
// the owner-mode skip reason must be the same text the SQL cascade writes.
func TestRunState_InGroupSkipReasonUsesPartitionKey(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	b.edge(producer, fanned)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "shard", 0, []pkgtask.Partition{
		{Key: "region=us-east-1"},
		{Key: "region=eu-west-1", DependsOn: []string{"region=us-east-1"}},
	})
	// The per-key reason belongs to the dependency cascade; fail_fast resolves
	// the whole group with a single group-level reason instead.
	rs.SetGroupFailurePolicy(fanned, jobdefschema.FanOutFailureContinue)

	res := rs.ApplyCompletion(insts[0], TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1)
	require.Equal(t, "fan-out dependency region=us-east-1 failed", res.Skipped[0].Reason)
}

// TestRehydrateInGroupEdges_RestoresMaxParallel pins D5d: after a takeover the
// rehydrated state must re-apply fanOut.maxParallel, or D6's cap is silently
// unenforced for the rest of the run.
func TestRehydrateInGroupEdges_RestoresMaxParallel(t *testing.T) {
	b := newTopoBuilder()
	fanned := b.task("")
	topo := b.build()
	rs := NewRunState(topo, 0)

	runID := uuid.New()
	rows := make([]models.TaskRun, 0, 4)
	for i, key := range []string{"a", "b", "c", "d"} {
		rows = append(rows, models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: fanned,
			PartitionValue: key, PartitionIndex: i, PartitionCount: 4,
			Status: string(TaskStatusPending),
		})
	}
	catalog := []models.Task{{
		ID:           fanned,
		Name:         "shard",
		FanOutConfig: datatypes.JSON([]byte(`{"from":"producer","maxParallel":2}`)),
	}}

	rs.RehydrateInGroupEdges(rows, catalog)

	ready := rs.ReadyTasks()
	require.Len(t, ready, 2, "rehydrated group must respect fanOut.maxParallel after takeover")
}

// TestRehydrateInGroupEdges_RestoresPartitionKeys asserts the skip reason
// survives a takeover (the rehydrated state knows each instance's key).
func TestRehydrateInGroupEdges_RestoresPartitionKeys(t *testing.T) {
	b := newTopoBuilder()
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	runID := uuid.New()
	depsB, err := json.Marshal([]string{"a"})
	require.NoError(t, err)
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "a", PartitionIndex: 0, PartitionCount: 2, Status: string(TaskStatusPending)},
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "b", PartitionIndex: 1, PartitionCount: 2, Status: string(TaskStatusPending), PartitionDependsOn: datatypes.JSON(depsB)},
	}
	rs.RehydrateInGroupEdges(rows, []models.Task{{
		ID: fanned, Name: "shard",
		FanOutConfig: datatypes.JSON([]byte(`{"from":"producer","failurePolicy":"continue"}`)),
	}})

	res := rs.ApplyCompletion(rows[0].ID, TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1)
	require.Equal(t, "fan-out dependency a failed", res.Skipped[0].Reason)
}

// TestRunState_GroupDurationObservedOnce asserts the owner engine reports a
// resolved group's wall-clock duration exactly once, so
// caesium_fanout_group_duration_seconds has an observation point.
func TestRunState_GroupDurationObservedOnce(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	b.edge(producer, fanned)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "shard", 0, []pkgtask.Partition{{Key: "a"}, {Key: "b"}})

	rs.MarkDispatched(insts[0], "node-1", 1, 0)
	rs.MarkDispatched(insts[1], "node-1", 1, 0)
	time.Sleep(2 * time.Millisecond)

	rs.ApplyCompletion(insts[0], TaskStatusSucceeded, nil)
	require.Empty(t, rs.TakeResolvedGroups(), "a half-finished group has not resolved")

	rs.ApplyCompletion(insts[1], TaskStatusCached, nil)
	resolved := rs.TakeResolvedGroups()
	require.Len(t, resolved, 1)
	require.Equal(t, fanned, resolved[0].TaskID)
	require.Equal(t, "shard", resolved[0].TaskName)
	require.Greater(t, resolved[0].Duration, time.Duration(0))
	require.Empty(t, rs.TakeResolvedGroups(), "a group's duration is reported exactly once")
}

// TestOwnerManager_ObservesFanOutGroupDuration drives the owner path end to end
// — producer completion expands the group, both instances are dispatched and
// complete — and asserts caesium_fanout_group_duration_seconds actually receives
// an observation.  The collector was registered but never observed anywhere.
func TestOwnerManager_ObservesFanOutGroupDuration(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "fo-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	alias := "fo-metric-job-" + uuid.NewString()[:8]
	job := &models.Job{ID: uuid.New(), Alias: alias, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	runID := runRecord.ID

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
			ID: uuid.New(), JobRunID: runID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(TaskStatusPending), ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mk(producer, "node-1", 0)
	// Expansion clears claimed_by on every instance it inserts, so the instances
	// complete under an empty claim fence.
	mk(shard, "", 1)

	before := metrictestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, alias, "shard")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, producer.ID, "node-1", 1, 0)

	res, err := mgr.CompleteInstance(runID, producer.ID, uuid.Nil, TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "a"}, {Key: "b"}})
	require.NoError(t, err)
	require.True(t, res.Owned)
	require.Len(t, res.Ready, 2, "the producer's completion must ready both instances")

	ready := mgr.ReadyForDispatch(runID)
	require.Len(t, ready, 2)
	for _, dt := range ready {
		require.Equal(t, shard.ID, dt.TaskID, "the dispatch envelope must carry the catalog task id")
		require.NotEqual(t, uuid.Nil, dt.TaskRunID, "and the instance TaskRun id")
		mgr.MarkDispatched(runID, dt.TaskRunID, "node-1", 1, 0)
	}
	for i, dt := range ready {
		status := TaskStatusSucceeded
		if i == 1 {
			// A cache hit is a completion, and with per-unit fingerprints it is
			// the common path through a group.
			status = TaskStatusCached
		}
		_, err := mgr.CompleteInstance(runID, dt.TaskID, dt.TaskRunID, status, "success", "", "", nil, nil, nil)
		require.NoError(t, err)
	}

	after := metrictestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, alias, "shard")
	require.Equal(t, before+1, after,
		"a resolved fan-out group must record exactly one group-duration observation")
}

// TestOwnerManager_CachedProducerExpandsGroup pins the owner half of the
// cached-producer contract: a producer satisfied from cache is still a
// completion that carries a partition list, and it must expand its group.  A
// group that only expands on `succeeded` collapses to the template row on every
// warm run — the failure mode is invisible on a cold first run.
func TestOwnerManager_CachedProducerExpandsGroup(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "cp-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "cp-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	runID := runRecord.ID

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	producer := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "list", Position: 0, CreatedAt: now, UpdatedAt: now}
	process := &models.Task{
		ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "process", Position: 1,
		FanOutConfig: datatypes.JSON([]byte(`{"from":"list"}`)),
		CreatedAt:    now, UpdatedAt: now,
	}
	require.NoError(t, db.Create([]*models.Task{producer, process}).Error)
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: job.ID, FromTaskID: producer.ID, ToTaskID: process.ID, CreatedAt: now}).Error)

	mk := func(task *models.Task, claimedBy string, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(TaskStatusPending), ClaimedBy: claimedBy, Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mk(producer, "node-1", 0)
	mk(process, "", 1)

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, producer.ID, "node-1", 1, 0)

	res, err := mgr.CompleteInstance(runID, producer.ID, uuid.Nil, TaskStatusCached, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "a"}, {Key: "b"}, {Key: "c"}})
	require.NoError(t, err)
	require.True(t, res.Owned)
	require.Len(t, res.Ready, 3, "a CACHED producer must expand its group, not collapse it to one row")

	var rows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, process.ID).Find(&rows).Error)
	require.Len(t, rows, 3, "three instance rows must be persisted")

	var producerRow models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, producer.ID).First(&producerRow).Error)
	require.True(t, producerRow.CacheHit, "the producer's row must record the cache hit")
}
