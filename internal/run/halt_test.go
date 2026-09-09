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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// haltFixture is the DAG the `halt` failure policy has to get right on every
// lane. `fail` is the failure; `slow` is a step that had already STARTED when
// it happened; everything else is not yet started.
//
//	fail ──┬──▶ cleanup   (all_done)
//	       └──▶ strict    (all_success)
//	slow (running) ──▶ independent (all_success) ──┬──▶ indep-child   (all_success)
//	                                               └──▶ indep-cleanup (all_done)
//	later          (root, all_success)
//	tolerant-root  (root, always)
//
// Expected after the sweep: `cleanup` is released (the failure transaction did
// that), `strict` is rule-skipped (same), `independent` and `later` are halted,
// `indep-child` is skipped by the cascade, `indep-cleanup` is released by it,
// `slow` keeps running and `tolerant-root` stays dispatchable.
type haltFixture struct {
	db    *gorm.DB
	store *Store
	jobID uuid.UUID
	runID uuid.UUID
	atom  *models.Atom
	tasks map[string]*models.Task
}

func newHaltFixture(t *testing.T) *haltFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "halt-fixture"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)

	f := &haltFixture{db: db, store: store, jobID: jobID, runID: runRecord.ID, atom: atom, tasks: map[string]*models.Task{}}
	position := 0
	mk := func(name, rule string) *models.Task {
		task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: name, Position: position, Type: "task", TriggerRule: rule}
		position++
		require.NoError(t, db.Create(task).Error)
		f.tasks[name] = task
		return task
	}
	fail := mk("fail", jobdefschema.TriggerRuleAllSuccess)
	slow := mk("slow", jobdefschema.TriggerRuleAllSuccess)
	cleanup := mk("cleanup", jobdefschema.TriggerRuleAllDone)
	strict := mk("strict", jobdefschema.TriggerRuleAllSuccess)
	independent := mk("independent", jobdefschema.TriggerRuleAllSuccess)
	indepChild := mk("indep-child", jobdefschema.TriggerRuleAllSuccess)
	indepCleanup := mk("indep-cleanup", jobdefschema.TriggerRuleAllDone)
	later := mk("later", jobdefschema.TriggerRuleAllSuccess)
	tolerantRoot := mk("tolerant-root", jobdefschema.TriggerRuleAlways)

	f.edge(t, fail, cleanup)
	f.edge(t, fail, strict)
	f.edge(t, slow, independent)
	f.edge(t, independent, indepChild)
	f.edge(t, independent, indepCleanup)

	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: fail, Atom: atom, OutstandingPredecessors: 0},
		{Task: slow, Atom: atom, OutstandingPredecessors: 0},
		{Task: cleanup, Atom: atom, OutstandingPredecessors: 1},
		{Task: strict, Atom: atom, OutstandingPredecessors: 1},
		{Task: independent, Atom: atom, OutstandingPredecessors: 1},
		{Task: indepChild, Atom: atom, OutstandingPredecessors: 1},
		{Task: indepCleanup, Atom: atom, OutstandingPredecessors: 1},
		{Task: later, Atom: atom, OutstandingPredecessors: 0},
		{Task: tolerantRoot, Atom: atom, OutstandingPredecessors: 0},
	}))

	// `slow` has a container; `fail` ran and failed.
	require.NoError(t, store.StartTask(runRecord.ID, slow.ID, "slow-container"))
	require.NoError(t, store.StartTask(runRecord.ID, fail.ID, "fail-container"))
	require.NoError(t, store.FailTask(runRecord.ID, fail.ID, errors.New("boom")))
	return f
}

func (f *haltFixture) edge(t *testing.T, from, to *models.Task) {
	t.Helper()
	require.NoError(t, f.db.Create(&models.TaskEdge{ID: uuid.New(), JobID: f.jobID, FromTaskID: from.ID, ToTaskID: to.ID}).Error)
}

func (f *haltFixture) row(t *testing.T, name string) models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.tasks[name].ID).Find(&rows).Error)
	require.Len(t, rows, 1, "task %q must have exactly one row", name)
	return rows[0]
}

func (f *haltFixture) readyEvents(t *testing.T) []uuid.UUID {
	t.Helper()
	evts, err := f.store.EventStore().ListSince(context.Background(), 0, 1000, event.Filter{RunID: f.runID, Types: []event.Type{event.TypeTaskReady}})
	require.NoError(t, err)
	out := make([]uuid.UUID, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.TaskID)
	}
	return out
}

func (f *haltFixture) skippedEventCount(t *testing.T, name string) int {
	t.Helper()
	evts, err := f.store.EventStore().ListSince(context.Background(), 0, 1000, event.Filter{RunID: f.runID, Types: []event.Type{event.TypeTaskSkipped}})
	require.NoError(t, err)
	n := 0
	for _, e := range evts {
		if e.TaskID == f.tasks[name].ID {
			n++
		}
	}
	return n
}

func TestHaltUnstartedTasksSkipsOnlyUnstartedIntolerantSteps(t *testing.T) {
	f := newHaltFixture(t)

	// Precondition: the failure transaction alone resolved `fail`'s own
	// successors and touched nothing else.
	require.Equal(t, string(TaskStatusPending), f.row(t, "cleanup").Status)
	require.Zero(t, f.row(t, "cleanup").OutstandingPredecessors)
	require.Equal(t, string(TaskStatusSkipped), f.row(t, "strict").Status)
	require.Equal(t, string(TaskStatusPending), f.row(t, "independent").Status)
	require.Equal(t, string(TaskStatusPending), f.row(t, "later").Status)

	skipped, err := f.store.HaltUnstartedTasks(f.runID, f.tasks["fail"].ID, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{f.tasks["independent"].ID, f.tasks["indep-child"].ID, f.tasks["later"].ID}, skipped)

	want := HaltReason("fail")
	assert.Equal(t, `run halted after task "fail" failed`, want, "the reason is user-visible; pin its spelling")

	independent := f.row(t, "independent")
	assert.Equal(t, string(TaskStatusSkipped), independent.Status, "an unstarted all_success step is halted")
	assert.Equal(t, want, independent.Error)
	later := f.row(t, "later")
	assert.Equal(t, string(TaskStatusSkipped), later.Status, "an unstarted all_success ROOT is halted too")
	assert.Equal(t, want, later.Error)

	// The halt cascades like a rule-skip: the intolerant child is skipped with
	// its rule reason, the tolerant one is released and announced ready.
	indepChild := f.row(t, "indep-child")
	assert.Equal(t, string(TaskStatusSkipped), indepChild.Status)
	assert.Equal(t, `trigger rule "all_success" not satisfied`, indepChild.Error)
	indepCleanup := f.row(t, "indep-cleanup")
	assert.Equal(t, string(TaskStatusPending), indepCleanup.Status, "all_done downstream of a halted branch is released, not halted")
	assert.Zero(t, indepCleanup.OutstandingPredecessors)
	assert.Contains(t, f.readyEvents(t), f.tasks["indep-cleanup"].ID)

	// Left alone: the started step, the released tolerant successor of the
	// failure, and a tolerant root.
	assert.Equal(t, string(TaskStatusRunning), f.row(t, "slow").Status, "a started step finishes on its own")
	cleanup := f.row(t, "cleanup")
	assert.Equal(t, string(TaskStatusPending), cleanup.Status)
	assert.Zero(t, cleanup.OutstandingPredecessors)
	assert.Equal(t, string(TaskStatusPending), f.row(t, "tolerant-root").Status, "a tolerant rule is never halted")

	// Idempotent: everything halted is terminal, so a second pass — the
	// waiter's belt-and-braces sweep — writes nothing and emits nothing.
	again, err := f.store.HaltUnstartedTasks(f.runID, f.tasks["fail"].ID, nil)
	require.NoError(t, err)
	assert.Empty(t, again)
	assert.Equal(t, 1, f.skippedEventCount(t, "independent"))
	assert.Equal(t, 1, f.skippedEventCount(t, "later"))
	assert.Equal(t, 1, f.skippedEventCount(t, "indep-child"))
	assert.Zero(t, f.skippedEventCount(t, "cleanup"))
}

// A row a dispatcher has CLAIMED but not started is `running` with no
// runtime_id. It is not started work: a one-slot worker rejects that dispatch
// and the owner rolls the claim back to pending, after which the step would
// run after the failure. The sweep resolves it, revoking the claim so a worker
// still holding the dispatch cannot start it, and cascades to its successors.
func TestHaltUnstartedTasksRevokesClaimedButUnstartedRows(t *testing.T) {
	f := newHaltFixture(t)

	independent := f.tasks["independent"]
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, independent.ID).
		Updates(map[string]any{
			"status":                   string(TaskStatusRunning),
			"claimed_by":               "worker-b",
			"outstanding_predecessors": 0,
		}).Error)

	skipped, err := f.store.HaltUnstartedTasks(f.runID, f.tasks["fail"].ID, nil)
	require.NoError(t, err)
	require.Contains(t, skipped, independent.ID)

	row := f.row(t, "independent")
	assert.Equal(t, string(TaskStatusSkipped), row.Status)
	assert.Equal(t, HaltReason("fail"), row.Error)
	assert.Empty(t, row.ClaimedBy, "the claim is revoked so the worker cannot start the container")
	assert.Nil(t, row.StartedAt)

	// The cascade still runs from a claimed root.
	assert.Equal(t, string(TaskStatusSkipped), f.row(t, "indep-child").Status)
	indepCleanup := f.row(t, "indep-cleanup")
	assert.Equal(t, string(TaskStatusPending), indepCleanup.Status)
	assert.Zero(t, indepCleanup.OutstandingPredecessors)

	// A row with a container is started work and is never touched.
	assert.Equal(t, string(TaskStatusRunning), f.row(t, "slow").Status)
}

func TestHaltUnstartedTasksDerivesReasonFromEarliestFailure(t *testing.T) {
	f := newHaltFixture(t)

	// uuid.Nil is what the run-completion waiter passes: it knows a task
	// failed, not which. The reason names the earliest failed row.
	skipped, err := f.store.HaltUnstartedTasks(f.runID, uuid.Nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, skipped)
	assert.Equal(t, HaltReason("fail"), f.row(t, "later").Error)
}

func TestHaltUnstartedTasksHonoursCandidateList(t *testing.T) {
	f := newHaltFixture(t)

	// The local executor names the steps it has NOT dispatched; a dispatched
	// step whose container is still being created is `pending` in SQL and must
	// not be resolved out from under it.
	skipped, err := f.store.HaltUnstartedTasks(f.runID, f.tasks["fail"].ID, []uuid.UUID{f.tasks["later"].ID})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{f.tasks["later"].ID}, skipped)
	assert.Equal(t, string(TaskStatusSkipped), f.row(t, "later").Status)
	assert.Equal(t, string(TaskStatusPending), f.row(t, "independent").Status, "a step outside the candidate list is left alone")
}

func TestHaltUnstartedTasksLeavesStartedFanOutGroupAlone(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "halt-group"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)

	fail := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fail", Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	group := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "group", Position: 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	require.NoError(t, db.Create(fail).Error)
	require.NoError(t, db.Create(group).Error)
	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: fail, Atom: atom, OutstandingPredecessors: 0},
		{Task: group, Atom: atom, OutstandingPredecessors: 0},
	}))
	// Hand-expand `group` into two instances: p0 has a container, p1 is waiting
	// for a slot.
	var template models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, group.ID).First(&template).Error)
	require.NoError(t, db.Model(&template).Updates(map[string]any{
		"partition_value": "p0", "partition_index": 0, "partition_count": 2, "runtime_id": "p0-container", "status": string(TaskStatusRunning),
	}).Error)
	p1 := template
	p1.ID = uuid.New()
	p1.PartitionValue = "p1"
	p1.PartitionIndex = 1
	p1.RuntimeID = ""
	p1.Status = string(TaskStatusPending)
	require.NoError(t, db.Create(&p1).Error)

	require.NoError(t, store.StartTask(runRecord.ID, fail.ID, "fail-container"))
	require.NoError(t, store.FailTask(runRecord.ID, fail.ID, errors.New("boom")))

	skipped, err := store.HaltUnstartedTasks(runRecord.ID, fail.ID, nil)
	require.NoError(t, err)
	assert.Empty(t, skipped, "a fan-out group with a started instance is a started step; its pending siblings finish with it")
	var rows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, group.ID).Order("partition_index ASC").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Equal(t, string(TaskStatusRunning), rows[0].Status)
	assert.Equal(t, string(TaskStatusPending), rows[1].Status)
}

// A halted PRODUCER leaves its fanned consumer an unexpanded template. The
// cascade must skip that template rather than announce it ready — the same
// guard advanceCrossStepSuccessorsTx carries — or a tolerant fanned consumer
// would run once, unpartitioned.
func TestHaltUnstartedTasksSkipsUnexpandedTemplateOfHaltedProducer(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "halt-template"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	require.NoError(t, db.Create(atom).Error)

	fail := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fail", Position: 0, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	producer := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "producer", Position: 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess}
	fanned := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "fanned", Position: 2, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllDone}
	encoded, err := json.Marshal(&jobdefschema.FanOut{From: "producer", MaxPartitions: 8})
	require.NoError(t, err)
	fanned.FanOutConfig = datatypes.JSON(encoded)
	for _, task := range []*models.Task{fail, producer, fanned} {
		require.NoError(t, db.Create(task).Error)
	}
	require.NoError(t, db.Create(&models.TaskEdge{ID: uuid.New(), JobID: jobID, FromTaskID: producer.ID, ToTaskID: fanned.ID}).Error)
	require.NoError(t, store.RegisterTasks(runRecord.ID, []RegisterTaskInput{
		{Task: fail, Atom: atom, OutstandingPredecessors: 0},
		{Task: producer, Atom: atom, OutstandingPredecessors: 0},
		{Task: fanned, Atom: atom, OutstandingPredecessors: 1},
	}))
	require.NoError(t, store.StartTask(runRecord.ID, fail.ID, "fail-container"))
	require.NoError(t, store.FailTask(runRecord.ID, fail.ID, errors.New("boom")))

	skipped, err := store.HaltUnstartedTasks(runRecord.ID, fail.ID, nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{producer.ID, fanned.ID}, skipped)

	var fannedRow models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runRecord.ID, fanned.ID).First(&fannedRow).Error)
	assert.Equal(t, string(TaskStatusSkipped), fannedRow.Status)
	assert.Equal(t, fmt.Sprintf("fan-out producer %q did not produce a partition list", "producer"), fannedRow.Error)

	pending, err := store.PendingTasksForDispatch(context.Background(), runRecord.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, pending, "the template must never be handed to a dispatcher")
}

func TestHasReleasablePending(t *testing.T) {
	f := newHaltFixture(t)

	// `cleanup` is pending with its scalar at zero: releasable.
	releasable, err := f.store.HasReleasablePending(f.runID)
	require.NoError(t, err)
	assert.True(t, releasable)

	// Resolve everything the sweep and the dispatcher would, leaving only a
	// row that waits on the RUNNING `slow`: not releasable.
	_, err = f.store.HaltUnstartedTasks(f.runID, f.tasks["fail"].ID, []uuid.UUID{f.tasks["later"].ID})
	require.NoError(t, err)
	for _, name := range []string{"cleanup", "tolerant-root"} {
		require.NoError(t, f.store.StartTask(f.runID, f.tasks[name].ID, name+"-container"))
		require.NoError(t, f.store.CompleteTask(f.runID, f.tasks[name].ID, "success", nil, nil))
	}
	releasable, err = f.store.HasReleasablePending(f.runID)
	require.NoError(t, err)
	assert.False(t, releasable, "independent waits on a running predecessor; nothing is releasable")

	// The in-memory owner lane never decrements the scalar. A row whose
	// predecessors are all terminal is releasable regardless of what the
	// scalar says.
	require.NoError(t, f.store.CompleteTask(f.runID, f.tasks["slow"].ID, "success", nil, nil))
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", f.runID, f.tasks["independent"].ID).
		Update("outstanding_predecessors", 1).Error)
	require.Equal(t, string(TaskStatusPending), f.row(t, "independent").Status)
	releasable, err = f.store.HasReleasablePending(f.runID)
	require.NoError(t, err)
	assert.True(t, releasable, "readiness is decided from edges, not from the stale scalar")
}
