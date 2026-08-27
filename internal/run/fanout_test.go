package run

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// fanOutFixture builds a minimal job + run whose DAG is producer -> consumer,
// with the consumer carrying a fanOut config pointed at the producer.
type fanOutFixture struct {
	db       *gorm.DB
	store    *Store
	jobID    uuid.UUID
	runID    uuid.UUID
	producer *models.Task
	consumer *models.Task
}

func newFanOutFixture(t *testing.T, fo *jobdefschema.FanOut) *fanOutFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:    jobID,
		Alias: "fanout-fixture",
	}).Error)

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hi"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	producer := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "discover", Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	consumer := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "process", Position: 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	if fo != nil {
		encoded, err := json.Marshal(fo)
		require.NoError(t, err)
		consumer.FanOutConfig = datatypes.JSON(encoded)
	}
	require.NoError(t, db.Create(producer).Error)
	require.NoError(t, db.Create(consumer).Error)
	require.NoError(t, db.Create(&models.TaskEdge{
		ID:         uuid.New(),
		JobID:      jobID,
		FromTaskID: producer.ID,
		ToTaskID:   consumer.ID,
	}).Error)

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: producer, Atom: atom, OutstandingPredecessors: 0},
		{Task: consumer, Atom: atom, OutstandingPredecessors: 1},
	}))

	return &fanOutFixture{
		db:       db,
		store:    store,
		jobID:    jobID,
		runID:    runRecord.ID,
		producer: producer,
		consumer: consumer,
	}
}

func (f *fanOutFixture) instances(t *testing.T) []models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Order("partition_index ASC").Find(&rows).Error)
	return rows
}

func (f *fanOutFixture) producerRow(t *testing.T) *models.TaskRun {
	t.Helper()
	row, err := loadUniqueTaskRun(f.db, f.runID, f.producer.ID)
	require.NoError(t, err)
	return row
}

func (f *fanOutFixture) expand(t *testing.T, parts []pkgtask.Partition) (*FanOutExpansion, error) {
	t.Helper()
	var (
		expansion *FanOutExpansion
		events    []event.Event
		counts    dbWriteCounts
	)
	err := f.db.Transaction(func(tx *gorm.DB) error {
		producer, err := loadUniqueTaskRun(tx, f.runID, f.producer.ID)
		if err != nil {
			return err
		}
		expansion, err = f.store.expandFanOutSuccessorsTx(
			tx, f.runID, f.producer.ID, producer, f.producer.Name,
			[]uuid.UUID{f.consumer.ID}, parts, &events, &counts,
		)
		return err
	})
	return expansion, err
}

func strParts(keys ...string) []pkgtask.Partition {
	out := make([]pkgtask.Partition, 0, len(keys))
	for _, k := range keys {
		out = append(out, pkgtask.Partition{Key: k})
	}
	return out
}

// --- expandFanOutSuccessorsTx --------------------------------------------

func TestExpandFanOutSuccessorsStringForm(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	expansion, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	require.NotNil(t, expansion)
	require.Len(t, expansion.Groups, 1)
	require.Len(t, expansion.Groups[0].Instances, 3)

	rows := f.instances(t)
	require.Len(t, rows, 3, "three partitions must materialize three TaskRun rows")
	for i, row := range rows {
		assert.Equal(t, i, row.PartitionIndex)
		assert.Equal(t, 3, row.PartitionCount)
		assert.Equal(t, string(TaskStatusPending), row.Status)
		// Every instance inherits the template's cross-step indegree.
		assert.Equal(t, 1, row.OutstandingPredecessors)
	}
	assert.Equal(t, []string{"a", "b", "c"},
		[]string{rows[0].PartitionValue, rows[1].PartitionValue, rows[2].PartitionValue})

	// Instance 0 is the rewritten template row, not a new row.
	assert.Equal(t, expansion.Groups[0].Instances[0].TaskRunID, rows[0].ID)

	// The normalized list is persisted on the PRODUCER's row.
	producer := f.producerRow(t)
	require.NotEmpty(t, producer.Partitions)
	var persisted []pkgtask.Partition
	require.NoError(t, json.Unmarshal(producer.Partitions, &persisted))
	assert.Len(t, persisted, 3)
}

func TestExpandFanOutSuccessorsObjectFormDiamond(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	// root -> {left, right} -> join
	parts := []pkgtask.Partition{
		{Key: "root", Fingerprint: "sha256:0000000000000000000000000000000000000000000000000000000000000001"},
		{Key: "left", DependsOn: []string{"root"}},
		{Key: "right", DependsOn: []string{"root"}},
		{Key: "join", DependsOn: []string{"left", "right"}},
	}
	_, err := f.expand(t, parts)
	require.NoError(t, err)

	rows := f.instances(t)
	require.Len(t, rows, 4)
	byKey := map[string]models.TaskRun{}
	for _, r := range rows {
		byKey[r.PartitionValue] = r
	}
	// template indegree (1) + in-group indegree
	assert.Equal(t, 1, byKey["root"].OutstandingPredecessors, "root has no in-group dependency")
	assert.Equal(t, 2, byKey["left"].OutstandingPredecessors)
	assert.Equal(t, 2, byKey["right"].OutstandingPredecessors)
	assert.Equal(t, 3, byKey["join"].OutstandingPredecessors, "join waits on two siblings")

	var joinDeps []string
	require.NoError(t, json.Unmarshal(byKey["join"].PartitionDependsOn, &joinDeps))
	assert.ElementsMatch(t, []string{"left", "right"}, joinDeps)
	assert.NotEmpty(t, byKey["root"].PartitionFingerprint)
}

func TestExpandFanOutSuccessorsOnEmptySkip(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16, OnEmpty: jobdefschema.FanOutOnEmptySkip})

	expansion, err := f.expand(t, nil)
	require.NoError(t, err)
	require.Len(t, expansion.Groups, 1)
	assert.True(t, expansion.Groups[0].Skipped)

	rows := f.instances(t)
	require.Len(t, rows, 1)
	assert.Equal(t, string(TaskStatusSkipped), rows[0].Status)
	assert.Contains(t, rows[0].Error, "fan-out produced no partitions")
}

func TestExpandFanOutSuccessorsOnEmptyFail(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16, OnEmpty: jobdefschema.FanOutOnEmptyFail})

	_, err := f.expand(t, nil)
	require.Error(t, err)
	var perr *pkgtask.PartitionError
	require.ErrorAs(t, err, &perr)
	assert.Contains(t, err.Error(), "fan-out produced no partitions")

	// The transaction rolled back: the template is untouched.
	rows := f.instances(t)
	require.Len(t, rows, 1)
	assert.Equal(t, string(TaskStatusPending), rows[0].Status)
}

func TestExpandFanOutSuccessorsCapOverflowNamesOffendingKey(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 2})

	_, err := f.expand(t, strParts("a", "b", "c"))
	require.Error(t, err)
	var perr *pkgtask.PartitionError
	require.ErrorAs(t, err, &perr)
	assert.Contains(t, err.Error(), "exceeds count cap 2")
	assert.Contains(t, err.Error(), `"c"`, "the error must name the first offending key")

	// Nothing was inserted — the producer fails loudly, it never truncates.
	rows := f.instances(t)
	require.Len(t, rows, 1)
}

func TestExpandFanOutSuccessorsCycleFailsProducer(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	parts := []pkgtask.Partition{
		{Key: "a", DependsOn: []string{"b"}},
		{Key: "b", DependsOn: []string{"a"}},
	}
	_, err := f.expand(t, parts)
	require.Error(t, err)
	var perr *pkgtask.PartitionError
	require.ErrorAs(t, err, &perr, "an in-group cycle must fail the PRODUCING task, not the run")

	rows := f.instances(t)
	require.Len(t, rows, 1, "no instance rows may be inserted before the graph validates")
}

func TestExpandFanOutSuccessorsIgnoresUnrelatedSuccessor(t *testing.T) {
	// The successor's fanOut.from names a different step, so it must not expand.
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "some-other-step", MaxPartitions: 16})

	expansion, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	assert.Empty(t, expansion.Groups)
	assert.Len(t, f.instances(t), 1)
}

// --- PlanFanOutExpansion --------------------------------------------------

func TestPlanFanOutExpansionAssignsIDsWithoutWriting(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	plan, err := f.store.PlanFanOutExpansion(f.runID, f.producer.ID, strParts("x", "y"))
	require.NoError(t, err)
	require.Len(t, plan.Groups, 1)
	require.Len(t, plan.Groups[0].Instances, 2)

	assert.Len(t, f.instances(t), 1, "planning must not insert rows")

	// The planned IDs are what CompleteTaskOwner then persists, so the owner's
	// in-memory state and the durable rows agree on instance identity.
	var events []event.Event
	var counts dbWriteCounts
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		return f.store.persistExpansionTx(tx, f.runID, plan, &events, &counts)
	}))

	rows := f.instances(t)
	require.Len(t, rows, 2)
	assert.Equal(t, plan.Groups[0].Instances[0].TaskRunID, rows[0].ID)
	assert.Equal(t, plan.Groups[0].Instances[1].TaskRunID, rows[1].ID)
}

func TestPlanFanOutExpansionRejectsCycle(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.store.PlanFanOutExpansion(f.runID, f.producer.ID, []pkgtask.Partition{
		{Key: "a", DependsOn: []string{"b"}},
		{Key: "b", DependsOn: []string{"a"}},
	})
	require.Error(t, err)
}

// --- decrementInGroupDependentsTx ----------------------------------------

func TestDecrementInGroupDependentsOnlyTouchesDependents(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c"},
	})
	require.NoError(t, err)

	rows := f.instances(t)
	byKey := map[string]models.TaskRun{}
	for _, r := range rows {
		byKey[r.PartitionValue] = r
	}
	completed := byKey["a"]

	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		return f.store.decrementInGroupDependentsTx(tx, f.runID, &completed)
	}))

	after := f.instances(t)
	afterByKey := map[string]models.TaskRun{}
	for _, r := range after {
		afterByKey[r.PartitionValue] = r
	}
	assert.Equal(t, 1, afterByKey["a"].OutstandingPredecessors, "the completing instance is untouched")
	assert.Equal(t, 1, afterByKey["b"].OutstandingPredecessors, "b depended on a: 2 -> 1")
	assert.Equal(t, 1, afterByKey["c"].OutstandingPredecessors, "c is independent and must not be decremented")
}

// --- skipInGroupDependentsTx (D3c) ---------------------------------------

// TestSkipInGroupDependentsIsReplayVisible pins D3c: a cascade-skipped instance
// must be a first-class terminal row — stamped with its own terminal_sequence so
// TerminalTaskRunsSince replays it, and carrying its own task_skipped event.
// The original implementation was a raw UPDATE with neither, so a recovering
// owner believed the skipped instances were still pending and the run hung.
func TestSkipInGroupDependentsIsReplayVisibleAndEmitsEvents(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"b"}},
		{Key: "independent"},
	})
	require.NoError(t, err)

	rows := f.instances(t)
	byKey := map[string]models.TaskRun{}
	for _, r := range rows {
		byKey[r.PartitionValue] = r
	}
	failed := byKey["a"]
	failed.Status = string(TaskStatusFailed)

	var events []event.Event
	var counts dbWriteCounts
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		return f.store.skipInGroupDependentsTx(tx, f.runID, &failed, &events, &counts)
	}))

	after := f.instances(t)
	afterByKey := map[string]models.TaskRun{}
	for _, r := range after {
		afterByKey[r.PartitionValue] = r
	}
	assert.Equal(t, string(TaskStatusSkipped), afterByKey["b"].Status)
	assert.Equal(t, string(TaskStatusSkipped), afterByKey["c"].Status, "the cascade must be transitive")
	assert.Equal(t, string(TaskStatusPending), afterByKey["independent"].Status)
	assert.Contains(t, afterByKey["b"].Error, "fan-out dependency a failed")

	// Each skipped instance carries its OWN non-zero terminal_sequence.
	require.Greater(t, afterByKey["b"].TerminalSequence, int64(0))
	require.Greater(t, afterByKey["c"].TerminalSequence, int64(0))
	assert.NotEqual(t, afterByKey["b"].TerminalSequence, afterByKey["c"].TerminalSequence,
		"siblings must not share a sequence: the replay tail is a strictly-ordered space")

	// …so the replay tail sees them.
	tail, err := f.store.TerminalTaskRunsSince(f.runID, 0)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, row := range tail {
		seen[row.PartitionValue] = true
	}
	assert.True(t, seen["b"], "skipped instance b must appear in TerminalTaskRunsSince")
	assert.True(t, seen["c"], "skipped instance c must appear in TerminalTaskRunsSince")

	// …and each emitted its own event naming its own partition.
	require.Len(t, events, 2)
	partitions := map[string]bool{}
	for _, evt := range events {
		assert.Equal(t, event.TypeTaskSkipped, evt.Type)
		var payload TaskRun
		require.NoError(t, json.Unmarshal(evt.Payload, &payload))
		partitions[payload.PartitionValue] = true
	}
	assert.True(t, partitions["b"])
	assert.True(t, partitions["c"], "each instance's event must carry its own partition, not a sibling's")
}

// --- groupAllTerminalTx ---------------------------------------------------

func TestGroupAllTerminalTx(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)

	var allTerm bool
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		allTerm, err = f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	assert.False(t, allTerm, "pending instances mean the group is not resolved")

	rows := f.instances(t)
	now := time.Now().UTC()
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).
		Updates(map[string]any{"status": string(TaskStatusSucceeded), "started_at": now.Add(-time.Minute), "completed_at": now}).Error)

	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		allTerm, err = f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	assert.False(t, allTerm, "one terminal sibling does not resolve the group")

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[1].ID).
		Updates(map[string]any{"status": string(TaskStatusFailed), "started_at": now.Add(-time.Minute), "completed_at": now}).Error)

	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		allTerm, err = f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	assert.True(t, allTerm)
}

// --- fanOut.maxParallel ---------------------------------------------------

// TestFanOutMaxParallelCapsInFlightInstances drives the owner-dispatch claim
// path directly: with maxParallel=2, a third concurrent claim must be refused
// while two instances are running, and must succeed once one finishes.
func TestFanOutMaxParallelCapsInFlightInstances(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 2})
	_, err := f.expand(t, strParts("p0", "p1", "p2"))
	require.NoError(t, err)

	// Clear the cross-step indegree so the instances are claimable.
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Update("outstanding_predecessors", 0).Error)

	rows := f.instances(t)
	require.NoError(t, f.store.ClaimTaskForDispatch(f.runID, rows[0].ID, "worker-a", 0, time.Minute, false))
	require.NoError(t, f.store.ClaimTaskForDispatch(f.runID, rows[1].ID, "worker-b", 0, time.Minute, false))

	err = f.store.ClaimTaskForDispatch(f.runID, rows[2].ID, "worker-c", 0, time.Minute, false)
	require.ErrorIs(t, err, ErrTaskClaimMismatch, "maxParallel=2 must refuse a third in-flight instance")

	assertRunningCount(t, f, 2)

	// Finish one; the third is now claimable.
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).
		Update("status", string(TaskStatusSucceeded)).Error)
	require.NoError(t, f.store.ClaimTaskForDispatch(f.runID, rows[2].ID, "worker-c", 0, time.Minute, false))
	assertRunningCount(t, f, 2)
}

// TestFanOutOrderedChainCompletesUnderMaxParallelOne is the deadlock proof the
// design demands: readiness comes from TERMINAL siblings, never from a free
// slot, so an ordered chain deeper than maxParallel cannot deadlock.
func TestFanOutOrderedChainCompletesUnderMaxParallelOne(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 1})
	_, err := f.expand(t, []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"b"}},
	})
	require.NoError(t, err)

	// The producer is done, so drop the cross-step predecessor from every
	// instance; the in-group edges remain.
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.consumer.ID).
		Update("outstanding_predecessors", gorm.Expr("outstanding_predecessors - 1")).Error)

	for step := 0; step < 3; step++ {
		var ready []models.TaskRun
		require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ? AND status = ? AND outstanding_predecessors = 0",
			f.runID, f.consumer.ID, string(TaskStatusPending)).
			Order("partition_index ASC").Find(&ready).Error)
		require.Len(t, ready, 1, "exactly one instance is ready at each step of an ordered chain")

		require.NoError(t, f.store.ClaimTaskForDispatch(f.runID, ready[0].ID, "worker", 0, time.Minute, false))
		assertRunningCount(t, f, 1)

		// Complete it, which decrements the next instance in the chain.
		require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
			row, err := loadTaskRunByIDOrUnique(tx, f.runID, ready[0].ID)
			if err != nil {
				return err
			}
			if err := tx.Model(&models.TaskRun{}).Where("id = ?", row.ID).
				Update("status", string(TaskStatusSucceeded)).Error; err != nil {
				return err
			}
			return f.store.decrementInGroupDependentsTx(tx, f.runID, row)
		}))
	}

	rows := f.instances(t)
	for _, row := range rows {
		assert.Equal(t, string(TaskStatusSucceeded), row.Status,
			"partition %s must have run: an ordered chain deeper than maxParallel must not deadlock", row.PartitionValue)
	}
}

func assertRunningCount(t *testing.T, f *fanOutFixture, want int64) {
	t.Helper()
	var n int64
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND status = ?", f.runID, f.consumer.ID, string(TaskStatusRunning)).
		Count(&n).Error)
	assert.LessOrEqual(t, n, want)
	assert.Equal(t, want, n)
}

// --- collapseFanOutGroups -------------------------------------------------

func TestCollapseFanOutGroupsEmitsStatusHistogram(t *testing.T) {
	taskID := uuid.New()
	otherID := uuid.New()
	start := time.Now().UTC().Add(-time.Hour)
	mid := start.Add(10 * time.Minute)
	end := start.Add(30 * time.Minute)

	rows := []*TaskRun{
		{ID: uuid.New(), TaskID: taskID, Status: TaskStatusSucceeded, PartitionValue: "a", PartitionIndex: 0, PartitionCount: 3, StartedAt: &mid, CompletedAt: &end},
		{ID: uuid.New(), TaskID: taskID, Status: TaskStatusSucceeded, PartitionValue: "b", PartitionIndex: 1, PartitionCount: 3, StartedAt: &start},
		{ID: uuid.New(), TaskID: taskID, Status: TaskStatusFailed, PartitionValue: "c", PartitionIndex: 2, PartitionCount: 3},
		{ID: uuid.New(), TaskID: otherID, Status: TaskStatusSucceeded},
	}

	out := collapseFanOutGroups(rows)
	require.Len(t, out, 2, "N siblings collapse to one entry")

	group := out[0]
	assert.Equal(t, taskID, group.ID)
	assert.Equal(t, 3, group.PartitionCount)
	assert.Equal(t, TaskStatusFailed, group.Status, "any failed instance fails the group")
	assert.Equal(t, map[string]int{
		string(TaskStatusSucceeded): 2,
		string(TaskStatusFailed):    1,
	}, group.PartitionStatusCounts)
	require.NotNil(t, group.StartedAt)
	assert.Equal(t, start, *group.StartedAt, "the group starts when its earliest instance did")
	require.NotNil(t, group.CompletedAt)
	assert.Equal(t, end, *group.CompletedAt, "the group ends when its latest instance did")

	unfanned := out[1]
	assert.Equal(t, 0, unfanned.PartitionCount)
	assert.Nil(t, unfanned.PartitionStatusCounts,
		"partition_status_counts must be omitted for an unfanned task")
}

func TestCollapseFanOutGroupsSingleInstanceGroup(t *testing.T) {
	taskID := uuid.New()
	rows := []*TaskRun{
		{ID: uuid.New(), TaskID: taskID, Status: TaskStatusSucceeded, PartitionValue: "only", PartitionIndex: 0, PartitionCount: 1},
	}
	out := collapseFanOutGroups(rows)
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].PartitionCount)
	assert.Nil(t, out[0].PartitionStatusCounts,
		"a one-instance group needs no histogram; partition_count already says everything")
}

// --- metrics --------------------------------------------------------------

// TestFanOutMetricsUseJobAliasAndTaskName pins the label contract
// ({job alias, task name}). The expansion call site passed the PRODUCER's task
// name into the job-alias slot, so every caesium_fanout_partitions_total series
// was mislabelled and could not be joined against any other job-scoped metric.
func TestFanOutMetricsUseJobAliasAndTaskName(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})

	before := metricstestutil.CounterValue(t, metrics.FanOutPartitionsTotal, "fanout-fixture", "process")
	_, err := f.expand(t, strParts("a", "b", "c"))
	require.NoError(t, err)
	after := metricstestutil.CounterValue(t, metrics.FanOutPartitionsTotal, "fanout-fixture", "process")

	assert.Equal(t, float64(3), after-before,
		"labels are the JOB ALIAS and the fanned step's name")
	assert.Equal(t, float64(0),
		metricstestutil.CounterValue(t, metrics.FanOutPartitionsTotal, "discover", "process"),
		"the producer's task name must never appear in the job-alias slot")
}

// TestFanOutGroupDurationObservedWhenSQLLaneResolvesGroup covers the SQL half of
// caesium_fanout_group_duration_seconds. The owner in-memory lane already
// observed it from owner_manager.go, so without this the series only ever had
// data under CAESIUM_RUN_OWNER_IN_MEMORY — a mode-dependent metric, which is the
// hardest kind of gap to notice.
func TestFanOutGroupDurationObservedWhenSQLLaneResolvesGroup(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)

	before := metricstestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, "fanout-fixture", "process")

	rows := f.instances(t)
	now := time.Now().UTC()
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).Updates(map[string]any{
		"status":       string(TaskStatusSucceeded),
		"started_at":   now.Add(-2 * time.Minute),
		"completed_at": now.Add(-time.Minute),
	}).Error)

	// Not resolved yet: one sibling is still pending.
	var allTerm bool
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		allTerm, err = f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	require.False(t, allTerm)
	assert.Equal(t, before,
		metricstestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, "fanout-fixture", "process"),
		"a partially-complete group must not be observed")

	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", rows[1].ID).Updates(map[string]any{
		"status":       string(TaskStatusSucceeded),
		"started_at":   now.Add(-90 * time.Second),
		"completed_at": now,
	}).Error)
	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		var err error
		allTerm, err = f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	require.True(t, allTerm)
	assert.Equal(t, before+1,
		metricstestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, "fanout-fixture", "process"),
		"resolving the group observes its span exactly once")
}

// TestFanOutGroupDurationIgnoresUnfannedTasks keeps the series fan-out-only.
func TestFanOutGroupDurationIgnoresUnfannedTasks(t *testing.T) {
	f := newFanOutFixture(t, nil)
	before := metricstestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, "fanout-fixture", "process")

	row, err := loadUniqueTaskRun(f.db, f.runID, f.consumer.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", row.ID).Updates(map[string]any{
		"status":       string(TaskStatusSucceeded),
		"started_at":   now.Add(-time.Minute),
		"completed_at": now,
	}).Error)

	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		_, err := f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		return err
	}))
	assert.Equal(t, before,
		metricstestutil.HistogramSampleCount(t, metrics.FanOutGroupDurationSeconds, "fanout-fixture", "process"))
}
