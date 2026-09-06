package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// A FAILED PLAIN TASK must release its successors exactly the way a succeeded
// one does: decrement each successor's outstanding_predecessors, then let the
// TRIGGER RULE decide — `all_done` runs, `all_success` is skipped with the rule
// reason.
//
// It did not. resolveInstanceFailureTx returned early for any row that was not
// a fan-out instance, on the claim that "an unfanned task's successors are
// advanced by the ordinary trigger-rule path" — and no such path exists, so the
// row scalar every lane gates on (runFannedGroup's readiness read, the
// distributed claimer's `outstanding_predecessors = 0` predicate) was never
// advanced. The fanned consumer of a failed plain step swept as "never
// dispatched"; the all_success consumer sat `pending` on a terminal run.
//
// The fixture is the smallest shape that pins both halves at once:
//
//	produce ──▶ fan ( `all_done`, fanOut.from = produce, 2 partitions )
//	           ▲
//	gate ──────┘
//	  └──────▶ tail ( `all_success` ) ──▶ tailChild
//
// `produce` succeeds first so `fan` is materialized, then `gate` FAILS. Both
// routes into a failed row (FailTask — attempts exhausted / infrastructure
// error; CompleteTask with result "failure" — the container ran and exited
// non-zero, which is the route the distributed worker actually takes) share
// resolveInstanceFailureTx, so each is driven here rather than trusting that
// they still do.
type plainFailureFixture struct {
	db    *gorm.DB
	store *Store
	runID uuid.UUID

	produce   *models.Task
	gate      *models.Task
	fan       *models.Task
	tail      *models.Task
	tailChild *models.Task
}

func newPlainFailureFixture(t *testing.T) *plainFailureFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "plain-failure-fixture"}).Error)

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.23",
		Command: `["echo","hi"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	mkTask := func(name string, position int, rule string) *models.Task {
		return &models.Task{
			ID: uuid.New(), JobID: jobID, AtomID: atom.ID,
			Name: name, Position: position, Type: "task", TriggerRule: rule,
		}
	}

	f := &plainFailureFixture{
		db:        db,
		store:     store,
		runID:     runRecord.ID,
		produce:   mkTask("produce", 0, jobdefschema.TriggerRuleAllSuccess),
		gate:      mkTask("gate", 1, jobdefschema.TriggerRuleAllSuccess),
		fan:       mkTask("fan", 2, jobdefschema.TriggerRuleAllDone),
		tail:      mkTask("tail", 3, jobdefschema.TriggerRuleAllSuccess),
		tailChild: mkTask("tail-child", 4, jobdefschema.TriggerRuleAllSuccess),
	}

	encoded, err := json.Marshal(&jobdefschema.FanOut{
		From: "produce", MaxPartitions: 8, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	require.NoError(t, err)
	f.fan.FanOutConfig = datatypes.JSON(encoded)

	for _, task := range []*models.Task{f.produce, f.gate, f.fan, f.tail, f.tailChild} {
		require.NoError(t, db.Create(task).Error)
	}
	for _, edge := range [][2]uuid.UUID{
		{f.produce.ID, f.fan.ID},
		{f.gate.ID, f.fan.ID},
		{f.gate.ID, f.tail.ID},
		{f.tail.ID, f.tailChild.ID},
	} {
		require.NoError(t, db.Create(&models.TaskEdge{
			ID: uuid.New(), JobID: jobID, FromTaskID: edge[0], ToTaskID: edge[1],
		}).Error)
	}

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: f.produce, Atom: atom, OutstandingPredecessors: 0},
		{Task: f.gate, Atom: atom, OutstandingPredecessors: 0},
		{Task: f.fan, Atom: atom, OutstandingPredecessors: 2},
		{Task: f.tail, Atom: atom, OutstandingPredecessors: 1},
		{Task: f.tailChild, Atom: atom, OutstandingPredecessors: 1},
	}))

	// `produce` succeeds and emits the partition list, materializing `fan`.
	// After this, every `fan` instance still waits on `gate`.
	_, err = store.CompleteTaskWithPartitions(
		runRecord.ID, f.produce.ID, "success", nil, nil,
		[]pkgtask.Partition{{Key: "p0"}, {Key: "p1"}},
	)
	require.NoError(t, err)

	for _, row := range f.rows(t, f.fan.ID) {
		require.Equal(t, 1, row.OutstandingPredecessors,
			"fixture invariant: partition %q must still be waiting on `gate` before it fails", row.PartitionValue)
		require.Equal(t, string(TaskStatusPending), row.Status)
	}
	return f
}

func (f *plainFailureFixture) rows(t *testing.T, taskID uuid.UUID) []models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, taskID).
		Order("partition_index ASC").Find(&rows).Error)
	return rows
}

func (f *plainFailureFixture) row(t *testing.T, taskID uuid.UUID) models.TaskRun {
	t.Helper()
	rows := f.rows(t, taskID)
	require.Len(t, rows, 1)
	return rows[0]
}

// eventTaskIDs returns the task ids carried by every event of the given type on
// this run, in sequence order.
func (f *plainFailureFixture) eventTaskIDs(t *testing.T, typ event.Type) []uuid.UUID {
	t.Helper()
	evts, err := f.store.EventStore().ListSince(context.Background(), 0, 1000, event.Filter{
		RunID: f.runID,
		Types: []event.Type{typ},
	})
	require.NoError(t, err)
	out := make([]uuid.UUID, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.TaskID)
	}
	return out
}

func countTaskID(ids []uuid.UUID, want uuid.UUID) int {
	n := 0
	for _, id := range ids {
		if id == want {
			n++
		}
	}
	return n
}

// assertPlainFailureReleasedSuccessors is the shared contract both failure
// routes must satisfy.
func assertPlainFailureReleasedSuccessors(t *testing.T, f *plainFailureFixture) {
	t.Helper()

	require.Equal(t, string(TaskStatusFailed), f.row(t, f.gate.ID).Status)

	// The tolerant consumer is RELEASED: every instance's row scalar reaches
	// zero — the scalar runFannedGroup and the distributed claimer both gate on
	// — and each is announced ready.
	fanRows := f.rows(t, f.fan.ID)
	require.Len(t, fanRows, 2)
	for _, row := range fanRows {
		assert.Equal(t, 0, row.OutstandingPredecessors,
			"an all_done consumer of a FAILED plain task must be released; partition %q", row.PartitionValue)
		assert.Equal(t, string(TaskStatusPending), row.Status,
			"a released consumer is dispatchable, not resolved; partition %q", row.PartitionValue)
	}
	readyIDs := f.eventTaskIDs(t, event.TypeTaskReady)
	assert.Positive(t, countTaskID(readyIDs, f.fan.ID),
		"the released all_done consumer must emit task_ready (%v)", readyIDs)

	// The intolerant consumer is SKIPPED, with the byte-identical reason the
	// other three shouldRunTaskTx copies emit (cacheHitTask, completeTask's
	// success branch, skipTaskAndDescendantsTx). Four sites spell this string;
	// pinning it here is what stops them drifting apart.
	tail := f.row(t, f.tail.ID)
	assert.Equal(t, string(TaskStatusSkipped), tail.Status,
		"an all_success consumer of a failed predecessor must be skipped, not left pending")
	assert.Equal(t, fmt.Sprintf("trigger rule %q not satisfied", jobdefschema.TriggerRuleAllSuccess), tail.Error)

	// …and the skip cascades transitively, as it does on every other route.
	tailChild := f.row(t, f.tailChild.ID)
	assert.Equal(t, string(TaskStatusSkipped), tailChild.Status,
		"the skipped consumer's own descendant must be resolved too")

	// Exactly one task_skipped per resolved task: markTaskSkippedTx is
	// pending-only, so the local executor's own store.SkipTask cascade — which
	// runs after this transaction commits — must not produce a second event.
	skipped := f.eventTaskIDs(t, event.TypeTaskSkipped)
	assert.Equal(t, 1, countTaskID(skipped, f.tail.ID), "task_skipped must be emitted once for `tail` (%v)", skipped)
	assert.Equal(t, 1, countTaskID(skipped, f.tailChild.ID), "task_skipped must be emitted once for `tail-child` (%v)", skipped)
	assert.Zero(t, countTaskID(skipped, f.fan.ID), "the all_done consumer must not be skipped")
}

// TestFailTaskAdvancesPlainSuccessors drives the attempts-exhausted route.
func TestFailTaskAdvancesPlainSuccessors(t *testing.T) {
	f := newPlainFailureFixture(t)
	require.NoError(t, f.store.FailTask(f.runID, f.gate.ID, errors.New("boom")))
	assertPlainFailureReleasedSuccessors(t, f)
}

// TestCompleteTaskFailureResultAdvancesPlainSuccessors drives the non-zero-exit
// route — the one the distributed worker takes (sink.Succeeded with result
// "failure" → CompleteTaskClaimed → completeTask's TaskStatusFailed branch).
func TestCompleteTaskFailureResultAdvancesPlainSuccessors(t *testing.T) {
	f := newPlainFailureFixture(t)
	require.NoError(t, f.store.CompleteTask(f.runID, f.gate.ID, "failure", nil, nil))
	assertPlainFailureReleasedSuccessors(t, f)
}

// TestPlainFailureSkipIsIdempotentWithExecutorCascade pins Ledger L2 directly:
// the local executor re-issues store.SkipTask for the same successors after the
// failure transaction commits (internal/job/job.go skipDescendantsFiltered), and
// that second pass must be a total no-op — no status change, no second
// task_skipped event, no further predecessor decrement.
func TestPlainFailureSkipIsIdempotentWithExecutorCascade(t *testing.T) {
	f := newPlainFailureFixture(t)
	require.NoError(t, f.store.FailTask(f.runID, f.gate.ID, errors.New("boom")))

	before := f.rows(t, f.fan.ID)
	require.NoError(t, f.store.SkipTask(f.runID, f.tail.ID, "skipped due to failed dependency task gate"))
	require.NoError(t, f.store.SkipTask(f.runID, f.tailChild.ID, "skipped due to failed dependency task gate"))

	tail := f.row(t, f.tail.ID)
	assert.Equal(t, fmt.Sprintf("trigger rule %q not satisfied", jobdefschema.TriggerRuleAllSuccess), tail.Error,
		"the executor's later cascade must not overwrite the store's truer reason")

	skipped := f.eventTaskIDs(t, event.TypeTaskSkipped)
	assert.Equal(t, 1, countTaskID(skipped, f.tail.ID), "the second skip must emit no event (%v)", skipped)
	assert.Equal(t, 1, countTaskID(skipped, f.tailChild.ID), "the second skip must emit no event (%v)", skipped)

	after := f.rows(t, f.fan.ID)
	require.Len(t, after, len(before))
	for i := range after {
		assert.Equal(t, before[i].OutstandingPredecessors, after[i].OutstandingPredecessors,
			"a no-op skip must not decrement anything; partition %q", after[i].PartitionValue)
	}
}

// TestFanOutInstanceFailureStillGatesOnGroupTerminal is the negative control: a
// fanned instance keeps the group-terminal gate, so the first of two siblings
// failing must NOT advance the group's cross-step successors.
func TestFanOutInstanceFailureStillGatesOnGroupTerminal(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{
		From: "discover", MaxPartitions: 8, FailurePolicy: jobdefschema.FanOutFailureContinue,
	})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)

	rows := f.instances(t)
	require.Len(t, rows, 2)
	require.NoError(t, f.store.FailTaskInstance(f.runID, rows[0].ID, errors.New("boom")))

	require.NoError(t, f.db.Transaction(func(tx *gorm.DB) error {
		allTerm, gErr := f.store.groupAllTerminalTx(tx, f.runID, f.consumer.ID)
		require.NoError(t, gErr)
		assert.False(t, allTerm,
			"one failed sibling must not make the group terminal — the gate is what keeps the fan-in advanced once")
		return nil
	}))
}

// TestFailedFanOutProducerSkipsUnexpandedConsumerTemplate is the negative
// control for the release A1 introduced.
//
// Expansion happens inside the PRODUCER's completion transaction, so a fanned
// consumer whose producer FAILED never gets a partition list. Releasing that
// consumer the way a plain one is released left its template row pending with
// outstanding_predecessors = 0 — and a template is indistinguishable in SQL
// from an ordinary unfanned task (partition_count = 0, partition_value = ''),
// so PendingTasksForDispatch, ClaimTaskForDispatch and the local dispatch would
// all have run the fanned step ONCE, unpartitioned, with no CAESIUM_PARTITION.
//
// The rule is tolerant here (all_done) precisely so the trigger rule cannot be
// what saves it: a group that can never materialize must resolve `skipped`,
// which is what design-dynamic-fanout.md prescribes for the same situation
// under `onEmpty: skip`.
func TestFailedFanOutProducerSkipsUnexpandedConsumerTemplate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "failed-producer-template"}).Error)

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","hi"]`}
	require.NoError(t, db.Create(atom).Error)

	produce := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "produce", Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	fan := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fan", Position: 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllDone}
	tail := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "tail", Position: 2, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}

	encoded, err := json.Marshal(&jobdefschema.FanOut{From: "produce", MaxPartitions: 8})
	require.NoError(t, err)
	fan.FanOutConfig = datatypes.JSON(encoded)

	for _, task := range []*models.Task{produce, fan, tail} {
		require.NoError(t, db.Create(task).Error)
	}
	for _, edge := range [][2]uuid.UUID{{produce.ID, fan.ID}, {fan.ID, tail.ID}} {
		require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: edge[0], ToTaskID: edge[1]}).Error)
	}

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: produce, Atom: atom, OutstandingPredecessors: 0},
		{Task: fan, Atom: atom, OutstandingPredecessors: 1},
		{Task: tail, Atom: atom, OutstandingPredecessors: 1},
	}))

	require.NoError(t, store.FailTask(runRecord.ID, produce.ID, errors.New("producer blew up")))

	var fanRows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, fan.ID).Find(&fanRows).Error)
	require.Len(t, fanRows, 1, "a failed producer expands nothing, so the template is still the only row")
	assert.Equal(t, string(TaskStatusSkipped), fanRows[0].Status,
		"an unexpandable fan-out template must be skipped, never handed to a dispatcher as an unpartitioned task")
	assert.Equal(t, `fan-out producer "produce" did not produce a partition list`, fanRows[0].Error)

	// And the skip cascades by the ordinary rules, so nothing downstream is
	// stranded waiting on a group that will never exist.
	var tailRow models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, tail.ID).First(&tailRow).Error)
	assert.Equal(t, string(TaskStatusSkipped), tailRow.Status)

	// The dispatcher must see nothing left to pick up.
	pending, err := store.PendingTasksForDispatch(context.Background(), runRecord.ID, 16)
	require.NoError(t, err)
	assert.Empty(t, pending, "no row of this run may remain dispatchable: %+v", pending)
}
