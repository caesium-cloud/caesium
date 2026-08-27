package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestRunState_FailFastResolvesEveryPendingSibling pins fail_fast in the owner
// in-memory engine.  It is the schema DEFAULT (pkg/jobdef validateSteps sets it
// when fanOut.failurePolicy is omitted), and it was implemented only in the
// local Kahn loop: the owner engine ran the `continue` cascade for every group,
// so a sibling with no dependsOn edge to the failure kept running.
func TestRunState_FailFastResolvesEveryPendingSibling(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	publish := b.task("")
	b.edge(producer, fanned)
	b.edge(fanned, publish)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "process", 0, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "gate"},
		{Key: "x", DependsOn: []string{"gate"}},
		{Key: "y", DependsOn: []string{"gate"}},
	})
	rs.SetGroupFailurePolicy(fanned, jobdefschema.FanOutFailureFailFast)

	// "gate" is in flight when "bad" fails; x and y are still pending and have
	// no dependsOn edge to "bad" at all, so the continue cascade would leave
	// them to run.
	//
	// In flight means a container is UP, which is a stronger statement than
	// dispatched: a dispatched instance whose worker has not created a container
	// is still cancellable and fail_fast now cancels it (see
	// TestRunState_FailFastCancelsDispatchedButUnstartedSibling). MarkStarted is
	// what the owner records for a live container, so this scenario states it.
	rs.MarkDispatched(insts[1], "node-1", 1, 0)
	rs.MarkStarted(insts[1])

	res := rs.ApplyCompletion(insts[0], TaskStatusFailed, nil)

	// PENDING siblings are cancelled; the RUNNING one is deliberately not.
	// Caesium cannot kill its container, so a skipped row would claim a terminal
	// state for live work (design: "cancelling pending siblings").
	var groupSkips []SkippedTask
	for _, sk := range res.Skipped {
		require.Greater(t, sk.TerminalSequence, int64(0), "each skip needs its own sequence for replay")
		if sk.Reason == "fan-out group failed fast" {
			groupSkips = append(groupSkips, sk)
		}
	}
	require.Len(t, groupSkips, 2, "fail_fast must cancel both pending siblings: %v", res.Skipped)
	for _, sk := range groupSkips {
		require.NotEqual(t, insts[1], sk.TaskID, "a running sibling must not be marked terminal under it")
	}
	for _, id := range []uuid.UUID{insts[2], insts[3]} {
		st, ok := rs.TaskState(id)
		require.True(t, ok)
		require.Equal(t, TaskStatusSkipped, st.Status, "a pending sibling must be cancelled")
	}
	running, ok := rs.TaskState(insts[1])
	require.True(t, ok)
	require.Equal(t, TaskStatusRunning, running.Status, "the in-flight sibling must be left to finish")

	require.Empty(t, rs.ReadyTasks(), "no sibling of a failed-fast group may still be dispatchable")
	require.False(t, rs.IsComplete(), "the run cannot resolve while a sibling is still running")

	// The in-flight sibling finishing is what resolves the group. Its own
	// outcome does not matter: one sibling already failed, so the group status
	// is failed and the fan-in's all_success rule cannot be satisfied.
	res = rs.ApplyCompletion(insts[1], TaskStatusSucceeded, nil)
	require.NotContains(t, res.Ready, publish, "the fan-in must not run when the group failed")
	st, ok := rs.TaskState(publish)
	require.True(t, ok)
	require.Equal(t, TaskStatusSkipped, st.Status, "all_success over a failed group must skip the fan-in")
	require.True(t, rs.IsComplete(), "the run must resolve rather than wait out its timeout")
	require.True(t, rs.HasFailures())
}

// TestRunState_ContinuePolicyStillCascadesOnlyDependents guards the opposite
// policy: `continue` must keep resolving only the failure's transitive in-group
// dependents, leaving independent siblings to run.
func TestRunState_ContinuePolicyStillCascadesOnlyDependents(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	b.edge(producer, fanned)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "process", 0, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "dep", DependsOn: []string{"bad"}},
		{Key: "independent"},
	})
	rs.SetGroupFailurePolicy(fanned, jobdefschema.FanOutFailureContinue)

	res := rs.ApplyCompletion(insts[0], TaskStatusFailed, nil)

	require.Len(t, res.Skipped, 1, "continue must skip only the transitive dependents")
	require.Equal(t, insts[1], res.Skipped[0].TaskID)
	require.Equal(t, "fan-out dependency bad failed", res.Skipped[0].Reason)

	st, ok := rs.TaskState(insts[2])
	require.True(t, ok)
	require.Equal(t, TaskStatusPending, st.Status, "an independent sibling must keep running under `continue`")
}

// TestRunState_FailFastIsTheDefaultPolicy pins the normalization: an empty
// policy is fail_fast, matching pkg/jobdef's validateSteps default.  Getting
// this wrong makes the owner lane disagree with the local lane for every job
// that omits fanOut.failurePolicy — which is most of them.
func TestRunState_FailFastIsTheDefaultPolicy(t *testing.T) {
	b := newTopoBuilder()
	producer := b.task("")
	fanned := b.task("")
	b.edge(producer, fanned)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil)
	insts := fannedGroup(t, rs, fanned, "process", 0, []pkgtask.Partition{
		{Key: "bad"},
		{Key: "independent"},
	})
	// No SetGroupFailurePolicy call at all.

	res := rs.ApplyCompletion(insts[0], TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1, "an unset policy must behave as fail_fast")
	require.Equal(t, insts[1], res.Skipped[0].TaskID)
	require.Equal(t, "fan-out group failed fast", res.Skipped[0].Reason)
}

// TestRehydrateInGroupEdges_RestoresFailurePolicy asserts the policy survives a
// takeover — it lives only in memory, so without re-seeding it from the catalog
// a recovered owner silently reverts the group to `continue`.
func TestRehydrateInGroupEdges_RestoresFailurePolicy(t *testing.T) {
	b := newTopoBuilder()
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	runID := uuid.New()
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "bad", PartitionIndex: 0, PartitionCount: 2, Status: string(TaskStatusPending)},
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "independent", PartitionIndex: 1, PartitionCount: 2, Status: string(TaskStatusPending)},
	}
	catalog := []models.Task{{
		ID:           fanned,
		Name:         "process",
		FanOutConfig: datatypes.JSON([]byte(`{"from":"list","failurePolicy":"fail_fast"}`)),
	}}
	rs.RehydrateInGroupEdges(rows, catalog)

	res := rs.ApplyCompletion(rows[0].ID, TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1, "the rehydrated group must still fail fast")
	require.Equal(t, rows[1].ID, res.Skipped[0].TaskID)
	require.Equal(t, "fan-out group failed fast", res.Skipped[0].Reason)
}

// TestOwnerManager_FailFastSkipsPendingSiblings drives the owner path end to
// end and asserts the skips are DURABLE — resolved through the same
// CompleteTaskOwner skip list the in-group cascade uses, so a pending sibling
// is a terminal row rather than something the run waits out.
func TestOwnerManager_FailFastSkipsPendingSiblings(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "ff-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "ff-job-" + uuid.NewString()[:8], TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)
	runID := runRecord.ID

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	producer := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "list", Position: 0, CreatedAt: now, UpdatedAt: now}
	process := &models.Task{
		ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "process", Position: 1,
		FanOutConfig: datatypes.JSON([]byte(`{"from":"list","failurePolicy":"fail_fast"}`)),
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

	_, err = mgr.CompleteInstance(runID, producer.ID, uuid.Nil, TaskStatusSucceeded, "success", "", "node-1", nil, nil,
		[]pkgtask.Partition{{Key: "bad"}, {Key: "gate"}, {Key: "x"}})
	require.NoError(t, err)

	ready := mgr.ReadyForDispatch(runID)
	require.Len(t, ready, 3)

	// Fail the first instance while the other two are still pending.
	var failed uuid.UUID
	for _, dt := range ready {
		var row models.TaskRun
		require.NoError(t, db.First(&row, "id = ?", dt.TaskRunID).Error)
		if row.PartitionValue == "bad" {
			failed = dt.TaskRunID
		}
	}
	require.NotEqual(t, uuid.Nil, failed)

	res, err := mgr.CompleteInstance(runID, process.ID, failed, TaskStatusFailed, "failure", "boom", "", nil, nil, nil)
	require.NoError(t, err)
	require.Empty(t, res.Ready, "fail_fast must not release any further work")

	var rows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, process.ID).Find(&rows).Error)
	require.Len(t, rows, 3)
	for i := range rows {
		if rows[i].ID == failed {
			require.Equal(t, string(TaskStatusFailed), rows[i].Status)
			continue
		}
		require.Equal(t, string(TaskStatusSkipped), rows[i].Status,
			"pending sibling %q must be durably resolved, not left pending", rows[i].PartitionValue)
		require.Greater(t, rows[i].TerminalSequence, int64(0),
			"a skip invisible to replay would be re-dispatched after a takeover")
	}
	require.Empty(t, mgr.ReadyForDispatch(runID), "no sibling may remain dispatchable")
}

// TestRehydrateInGroupEdges_AbsentFanOutConfigFailsFast covers the branch a
// recovering owner can actually reach: the instance rows are fanned, but the
// catalog Task carries no fan_out_config at all — the step's fanOut block was
// removed by a `job apply` between the run starting and the takeover.  The
// group must still fail fast (the schema default), not silently revert to
// `continue` and keep dispatching siblings after a failure.
//
// This branch is unreachable on the live path: PlanFanOutExpansion only builds
// a group when the successor's config decodes AND its `from` names the
// producer, so a live group always has a config.  Recovery is the one place a
// fanned group can outlive its config.  The SQL lane's counterpart is
// groupFailsFastTx's `fo == nil` branch.
func TestRehydrateInGroupEdges_AbsentFanOutConfigFailsFast(t *testing.T) {
	b := newTopoBuilder()
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	runID := uuid.New()
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "bad", PartitionIndex: 0, PartitionCount: 2, Status: string(TaskStatusPending)},
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "independent", PartitionIndex: 1, PartitionCount: 2, Status: string(TaskStatusPending)},
	}
	// Catalog row with no FanOutConfig whatsoever.
	rs.RehydrateInGroupEdges(rows, []models.Task{{ID: fanned, Name: "process"}})

	res := rs.ApplyCompletion(rows[0].ID, TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1, "a fanned group with no config must still fail fast")
	require.Equal(t, rows[1].ID, res.Skipped[0].TaskID)
	require.Equal(t, "fan-out group failed fast", res.Skipped[0].Reason)
}

// TestRehydrateInGroupEdges_UndecodableFanOutConfigFailsFast is the same branch
// reached through a decode error rather than an absent column.  Failing fast is
// the fail-safe direction: stopping a group early on unreadable config is
// recoverable, while letting it keep dispatching is not.
func TestRehydrateInGroupEdges_UndecodableFanOutConfigFailsFast(t *testing.T) {
	b := newTopoBuilder()
	fanned := b.task("")
	rs := NewRunState(b.build(), 0)

	runID := uuid.New()
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "bad", PartitionIndex: 0, PartitionCount: 2, Status: string(TaskStatusPending)},
		{ID: uuid.New(), JobRunID: runID, TaskID: fanned, PartitionValue: "independent", PartitionIndex: 1, PartitionCount: 2, Status: string(TaskStatusPending)},
	}
	rs.RehydrateInGroupEdges(rows, []models.Task{{
		ID: fanned, Name: "process",
		FanOutConfig: datatypes.JSON([]byte(`{"from":`)), // truncated JSON
	}})

	res := rs.ApplyCompletion(rows[0].ID, TaskStatusFailed, nil)
	require.Len(t, res.Skipped, 1, "undecodable config must fail fast, not fall back to continue")
	require.Equal(t, "fan-out group failed fast", res.Skipped[0].Reason)
}
