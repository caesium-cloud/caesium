package run

import (
	"context"
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

// fanOutFailoverFixture is the DAG this file's failover scenarios run on:
//
//	seed -> list -> process (fanned) -> publish
//
// `seed` exists so a checkpoint can be taken BEFORE the group is expanded —
// without a completion ahead of the producer, every checkpoint in the run
// already knows about the instances and D5d's "the last checkpoint predates the
// expansion" case is unreachable.  `publish` is the cross-step fan-in successor,
// so a takeover that rebuilt the group wrongly shows up as a run that either
// stalls or releases `publish` twice.
type fanOutFailoverFixture struct {
	db     *gorm.DB
	store  *Store
	leases *LeaseStore

	runID   uuid.UUID
	jobID   uuid.UUID
	seed    uuid.UUID
	list    uuid.UUID
	process uuid.UUID
	publish uuid.UUID
}

// newFanOutFailoverFixture seeds the DAG above with one pending task_run per
// step.  fanOutJSON is stamped on `process` verbatim so a test can pick its own
// maxParallel / failurePolicy; the catalog row is the only place the owner reads
// it from, on the live path and on recovery alike.
func newFanOutFailoverFixture(t *testing.T, fanOutJSON string) *fanOutFailoverFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	leases := NewLeaseStore(db)
	store := NewStore(db).WithLeaseStore(leases)

	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID: uuid.New(), Alias: "ff-trig-" + uuid.NewString()[:8],
		Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{
		ID: uuid.New(), Alias: "ff-job-" + uuid.NewString()[:8],
		TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID: uuid.New(), Engine: models.AtomEngineDocker,
		Image: "alpine:3.23", Command: `["true"]`, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	mkTask := func(name string, position int, fanOut string) *models.Task {
		task := &models.Task{
			ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: name, Position: position,
			Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
			CreatedAt: now, UpdatedAt: now,
		}
		if fanOut != "" {
			task.FanOutConfig = datatypes.JSON([]byte(fanOut))
		}
		require.NoError(t, db.Create(task).Error)
		return task
	}
	seed := mkTask("seed", 0, "")
	list := mkTask("list", 1, "")
	process := mkTask("process", 2, fanOutJSON)
	publish := mkTask("publish", 3, "")

	for _, e := range [][2]uuid.UUID{
		{seed.ID, list.ID}, {list.ID, process.ID}, {process.ID, publish.ID},
	} {
		require.NoError(t, db.Create(&models.TaskEdge{
			ID: uuid.New(), JobID: job.ID, FromTaskID: e[0], ToTaskID: e[1], CreatedAt: now,
		}).Error)
	}

	mkRun := func(task *models.Task, outstanding int) {
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atom.ID,
			Engine: atom.Engine, Image: atom.Image, Command: atom.Command,
			Status: string(TaskStatusPending), Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: outstanding, CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	mkRun(seed, 0)
	mkRun(list, 1)
	mkRun(process, 1)
	mkRun(publish, 1)

	return &fanOutFailoverFixture{
		db: db, store: store, leases: leases,
		runID: runRecord.ID, jobID: job.ID,
		seed: seed.ID, list: list.ID, process: process.ID, publish: publish.ID,
	}
}

// instancesByKey indexes the group's durable rows by partition key.
func (f *fanOutFailoverFixture) instancesByKey(t *testing.T) map[string]models.TaskRun {
	t.Helper()
	var rows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.process).
		Order("partition_index ASC").Find(&rows).Error)
	out := make(map[string]models.TaskRun, len(rows))
	for _, row := range rows {
		out[row.PartitionValue] = row
	}
	return out
}

func (f *fanOutFailoverFixture) rowByID(t *testing.T, id uuid.UUID) models.TaskRun {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", id).Error)
	return row
}

// orderedParts is the group shape both scenarios use: two independent chains
// (a->b, c->d) plus two free partitions.  A chain is what makes the in-group
// edge rebuild observable — if a takeover loses it, `b` never becomes ready and
// the run stalls rather than failing an assertion about state.
func orderedParts() []pkgtask.Partition {
	return []pkgtask.Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c"},
		{Key: "d", DependsOn: []string{"c"}},
		{Key: "e"},
		{Key: "f"},
	}
}

// claimAndDispatch performs the two halves of a real dispatch for one ready
// task: the durable claim the worker's HandleDispatch takes, then the owner's
// in-memory record of it.  Using the store rather than a raw UPDATE is what
// makes the claim carry this owner's generation, which is the fence a takeover
// has to clear.
func claimAndDispatch(t *testing.T, f *fanOutFailoverFixture, mgr *OwnerManager, node string, gen int64, dt DispatchableTask) {
	t.Helper()
	_, err := f.store.ClaimTaskForDispatch(f.runID, dt.ExecutionRef(), node, gen, 1, time.Minute, true)
	require.NoError(t, err)
	mgr.MarkDispatched(f.runID, dt.ExecutionRef(), node, dt.Attempt, 0)
}

// drainRun dispatches and completes everything the owner offers until the run
// reports complete, and returns how many times each identity was announced
// ready.  Bounded by maxRounds so a stall fails the test instead of hanging it,
// and it fails immediately on a round that offers nothing while the run is
// unfinished — which is exactly how a lost in-group edge presents.
func drainRun(t *testing.T, f *fanOutFailoverFixture, mgr *OwnerManager, node string, gen int64) map[uuid.UUID]int {
	t.Helper()
	readyCounts := make(map[uuid.UUID]int)
	const maxRounds = 30
	for round := 0; round < maxRounds; round++ {
		ready := mgr.ReadyForDispatch(f.runID)
		require.NotEmpty(t, ready, "round %d: the run stalled — nothing is dispatchable and the run is not complete", round)
		for _, dt := range ready {
			claimAndDispatch(t, f, mgr, node, gen, dt)
		}
		for _, dt := range ready {
			res, err := mgr.CompleteInstance(f.runID, dt.TaskID, dt.TaskRunID,
				TaskStatusSucceeded, "success", "", node, nil, nil, nil)
			require.NoError(t, err)
			for _, id := range res.Ready {
				readyCounts[id]++
			}
			if res.Complete {
				return readyCounts
			}
		}
	}
	t.Fatalf("run did not complete within %d dispatch rounds", maxRounds)
	return readyCounts
}

// takeover expires the current lease, acquires it for `node`, and returns the
// bumped generation — the real failover sweep, not a hand-written generation.
func takeover(t *testing.T, f *fanOutFailoverFixture, node string) int64 {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.db.Model(&models.RunLease{}).
		Where("run_id = ?", f.runID.String()).
		Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error)

	n, err := f.leases.AcquireExpiredLeases(ctx, node, 10*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "the expired lease must be taken over")

	owned, err := f.leases.OwnedRunsWithGenerations(ctx, node)
	require.NoError(t, err)
	gen, ok := owned[f.runID]
	require.True(t, ok, "%s now owns the run", node)
	require.Greater(t, gen, int64(1), "takeover must bump the generation")
	return gen
}

// TestFailoverMidGroup_ReDispatchesOnlyUnfinishedInstances is the end-to-end
// owner-failover-mid-group scenario acceptance criterion 4 asks for, and that
// G3 / D4 / D5 each list as their missing coverage.
//
// An owner expands a fanned group, finishes one partition, has two more in
// flight, and dies.  The takeover must re-dispatch ONLY the unfinished
// instances: the finished sibling stays terminal and is never re-run, the
// partition whose in-group prerequisite has not landed stays blocked,
// fanOut.maxParallel still caps the rebuilt group, and the group fans in
// exactly once.
func TestFailoverMidGroup_ReDispatchesOnlyUnfinishedInstances(t *testing.T) {
	ctx := context.Background()
	f := newFanOutFailoverFixture(t, `{"from":"list","maxParallel":2}`)
	// Events:1 so every durable completion checkpoints — the state a takeover
	// reads is then always the state the previous owner last committed.
	cfg := CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 5}

	_, err := f.leases.AcquireLease(ctx, f.runID, "node-A", 10*time.Second)
	require.NoError(t, err)
	mgrA := NewOwnerManager(f.store, cfg)
	require.NoError(t, mgrA.Adopt(f.runID, 1))

	// --- Owner A: run the DAG up to the middle of the group ---
	for _, id := range []uuid.UUID{f.seed, f.list} {
		ready := mgrA.ReadyForDispatch(f.runID)
		require.Len(t, ready, 1)
		require.Equal(t, id, ready[0].TaskID)
		claimAndDispatch(t, f, mgrA, "node-A", 1, ready[0])
		if id == f.seed {
			_, err = mgrA.Complete(f.runID, id, TaskStatusSucceeded, "success", "", "node-A", nil, nil)
			require.NoError(t, err)
		}
	}
	// The producer's completion carries the partition list, which is what
	// expands the group inside the same critical section (D4).
	res, err := mgrA.CompleteInstance(f.runID, f.list, uuid.Nil, TaskStatusSucceeded,
		"success", "", "node-A", nil, nil, orderedParts())
	require.NoError(t, err)
	require.ElementsMatch(t,
		[]string{"a", "c", "e", "f"}, partitionKeysOf(t, f, res.Ready),
		"only the dependency-free partitions are ready; b and d wait on their in-group prerequisite")

	inst := f.instancesByKey(t)
	require.Len(t, inst, 6)

	// maxParallel holds on the live path: four partitions are ready, two may run.
	ready := mgrA.ReadyForDispatch(f.runID)
	require.Len(t, ready, 2, "fanOut.maxParallel must cap the owner's ready queue")
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[0])
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[1])

	// Partition a finishes, releasing its in-group dependent b.
	res, err = mgrA.CompleteInstance(f.runID, f.process, inst["a"].ID, TaskStatusSucceeded,
		"success", "", "node-A", nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{inst["b"].ID}, res.Ready, "a's completion must release b")
	aBefore := f.rowByID(t, inst["a"].ID)
	require.Equal(t, string(TaskStatusSucceeded), aBefore.Status)
	require.Greater(t, aBefore.TerminalSequence, int64(0))

	// b is dispatched after that last checkpoint — the realistic checkpoint lag.
	ready = mgrA.ReadyForDispatch(f.runID)
	require.NotEmpty(t, ready)
	require.Equal(t, inst["b"].ID, ready[0].TaskRunID, "b is next in partition order")
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[0])

	// --- Owner A dies mid-group; owner B takes the lease over ---
	genB := takeover(t, f, "node-B")
	mgrB := NewOwnerManager(f.store, cfg)
	rec, err := mgrB.Recover(f.runID, genB)
	require.NoError(t, err)
	require.False(t, rec.Complete, "half the group is unfinished")

	// b and c were both durably running under the dead owner's generation. The
	// takeover requeues every prior-generation claim, including b which was
	// dispatched after the last checkpoint.
	require.ElementsMatch(t, []uuid.UUID{inst["b"].ID, inst["c"].ID}, rec.ReDispatch,
		"every instance claimed by the dead owner is explicit re-dispatch work")

	// The full dispatchable set after the takeover: every unfinished instance
	// whose prerequisites are satisfied, and nothing else.
	or, ok := mgrB.get(f.runID)
	require.True(t, ok)
	require.ElementsMatch(t, []string{"b", "c", "e", "f"}, partitionKeysOf(t, f, or.state.ready),
		"the takeover must queue exactly the unfinished, unblocked instances")
	require.NotContains(t, partitionKeysOf(t, f, or.state.ready), "a",
		"a finished sibling must never be re-dispatched")
	require.NotContains(t, partitionKeysOf(t, f, or.state.ready), "d",
		"d's in-group prerequisite c has not completed, so it stays blocked")

	// a survives the takeover terminal, with the sequence it was stamped with.
	stA, known := or.state.TaskState(inst["a"].ID)
	require.True(t, known)
	require.Equal(t, TaskStatusSucceeded, stA.Status)
	require.Equal(t, aBefore.Status, f.rowByID(t, inst["a"].ID).Status)
	require.Equal(t, aBefore.TerminalSequence, f.rowByID(t, inst["a"].ID).TerminalSequence,
		"the takeover must not restamp a finished instance")

	// maxParallel is re-seeded from the catalog, so the rebuilt group is capped.
	require.Len(t, rec.Ready, 2, "fanOut.maxParallel must survive the takeover")
	require.Equal(t, 2, or.state.maxParallel[f.process])

	// b's durable row was left claimed by the dead owner; the takeover must have
	// returned it to the claimable pool or the re-dispatch cannot take a claim.
	bRow := f.rowByID(t, inst["b"].ID)
	require.Equal(t, string(TaskStatusPending), bRow.Status)
	require.Equal(t, "", bRow.ClaimedBy)

	// --- Owner B finishes the run ---
	readyCounts := drainRun(t, f, mgrB, "node-B", genB)
	require.Equal(t, 1, readyCounts[f.publish],
		"the group must fan in exactly once — publish is announced ready one time")
	for id, n := range readyCounts {
		require.Equalf(t, 1, n, "identity %s was announced ready %d times", id, n)
	}

	assertGroupFinishedCleanly(t, f, len(inst), aBefore)
}

// TestFailoverMidGroup_CheckpointPredatesExpansion is D5d stated as an
// end-to-end assertion rather than a state-rebuild one.
//
// The last usable checkpoint was written BEFORE the producer completed, so it
// still holds the unexpanded catalog node for `process` and knows nothing about
// the group.  Recovery must rebuild the group — and its in-group edges — from
// the durable partition_key / partition_depends_on columns BEFORE replaying the
// terminal tail; a rebuild that happened after the replay would leave b holding
// the indegree its already-completed prerequisite a was supposed to decrement,
// and the run would stall after takeover with no failed assertion to point at.
func TestFailoverMidGroup_CheckpointPredatesExpansion(t *testing.T) {
	ctx := context.Background()
	f := newFanOutFailoverFixture(t, `{"from":"list","maxParallel":2}`)
	cfg := CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 5}

	_, err := f.leases.AcquireLease(ctx, f.runID, "node-A", 10*time.Second)
	require.NoError(t, err)
	mgrA := NewOwnerManager(f.store, cfg)
	require.NoError(t, mgrA.Adopt(f.runID, 1))

	// seed completes at sequence 1 and checkpoints — the last checkpoint that
	// will survive below, and the one that predates the group.
	ready := mgrA.ReadyForDispatch(f.runID)
	require.Len(t, ready, 1)
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[0])
	_, err = mgrA.Complete(f.runID, f.seed, TaskStatusSucceeded, "success", "", "node-A", nil, nil)
	require.NoError(t, err)

	ready = mgrA.ReadyForDispatch(f.runID)
	require.Len(t, ready, 1)
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[0])
	_, err = mgrA.CompleteInstance(f.runID, f.list, uuid.Nil, TaskStatusSucceeded,
		"success", "", "node-A", nil, nil, orderedParts())
	require.NoError(t, err)

	inst := f.instancesByKey(t)
	require.Len(t, inst, 6)

	// a and c are dispatched; a completes, c is still in flight.
	ready = mgrA.ReadyForDispatch(f.runID)
	require.Len(t, ready, 2)
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[0])
	claimAndDispatch(t, f, mgrA, "node-A", 1, ready[1])
	_, err = mgrA.CompleteInstance(f.runID, f.process, inst["a"].ID, TaskStatusSucceeded,
		"success", "", "node-A", nil, nil, nil)
	require.NoError(t, err)
	aBefore := f.rowByID(t, inst["a"].ID)
	require.Equal(t, string(TaskStatusSucceeded), aBefore.Status)
	require.Greater(t, aBefore.TerminalSequence, int64(0))

	// Roll the checkpoint history back to before the expansion.  Everything the
	// owner did from the producer's completion onward now lives only in the
	// durable task_runs rows, which is the state this item says is authoritative.
	require.NoError(t, f.db.Where("run_id = ? AND sequence_high >= ?", f.runID.String(), int64(2)).
		Delete(&models.RunCheckpoint{}).Error)
	cp, err := f.store.LatestFullCheckpoint(f.runID)
	require.NoError(t, err)
	require.NotNil(t, cp)
	require.Equal(t, int64(1), cp.SequenceHigh, "precondition: the surviving checkpoint is the pre-expansion one")

	topo, err := f.store.LoadRunTopology(f.runID)
	require.NoError(t, err)
	pre, err := Restore(topo, cp.StateBlob)
	require.NoError(t, err)
	_, knowsCatalogNode := pre.TaskState(f.process)
	require.True(t, knowsCatalogNode,
		"precondition: the surviving checkpoint still holds the UNEXPANDED catalog node")
	for key, row := range inst {
		_, knowsInstance := pre.TaskState(row.ID)
		require.Falsef(t, knowsInstance, "precondition: the checkpoint cannot know instance %s", key)
	}

	// --- Takeover ---
	genB := takeover(t, f, "node-B")
	mgrB := NewOwnerManager(f.store, cfg)
	rec, err := mgrB.Recover(f.runID, genB)
	require.NoError(t, err)
	require.False(t, rec.Complete)

	or, ok := mgrB.get(f.runID)
	require.True(t, ok)

	// The group was rebuilt from the durable partition columns, and a's terminal
	// row was replayed by INSTANCE identity onto that rebuilt group.
	stA, known := or.state.TaskState(inst["a"].ID)
	require.True(t, known, "the rebuilt state must know the instance rows")
	require.Equal(t, TaskStatusSucceeded, stA.Status,
		"a's terminal row must be replayed onto the instance, not onto the catalog task")

	// The D5d assertion: a completed BEFORE the takeover and its in-group
	// dependent b must still be dispatchable afterwards.
	require.Contains(t, partitionKeysOf(t, f, or.state.ready), "b",
		"the in-group edge a->b must be rebuilt BEFORE the tail replay, or b never becomes ready")
	require.ElementsMatch(t, []string{"b", "c", "e", "f"}, partitionKeysOf(t, f, or.state.ready),
		"only the unfinished, unblocked instances are queued")
	require.NotContains(t, partitionKeysOf(t, f, or.state.ready), "d",
		"d still waits on c, whose completion never landed")

	// The cap comes back from the catalog, not the checkpoint.
	require.Equal(t, 2, or.state.maxParallel[f.process])
	require.Len(t, rec.Ready, 2, "four instances are dispatchable; maxParallel offers two")

	// c was running in the DB when the owner died. Even though the snapshot
	// predates the group, recovery rebuilds the instance from durable rows and
	// reports the prior-generation claim as explicit re-dispatch work.
	require.Equal(t, []uuid.UUID{inst["c"].ID}, rec.ReDispatch)
	cRow := f.rowByID(t, inst["c"].ID)
	require.Equal(t, string(TaskStatusPending), cRow.Status)
	require.Equal(t, "", cRow.ClaimedBy, "the dead owner's claim must be cleared for the re-dispatch")

	// --- Owner B finishes the run ---
	readyCounts := drainRun(t, f, mgrB, "node-B", genB)
	require.Equal(t, 1, readyCounts[f.publish], "the group must fan in exactly once")

	assertGroupFinishedCleanly(t, f, len(inst), aBefore)
}

// partitionKeysOf maps a set of owner-state identities back to partition keys,
// so an assertion failure names the units of work rather than TaskRun UUIDs.
func partitionKeysOf(t *testing.T, f *fanOutFailoverFixture, ids []uuid.UUID) []string {
	t.Helper()
	byID := make(map[uuid.UUID]string)
	for key, row := range f.instancesByKey(t) {
		byID[row.ID] = key
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if key, ok := byID[id]; ok {
			out = append(out, key)
			continue
		}
		out = append(out, id.String())
	}
	return out
}

// assertGroupFinishedCleanly is the shared post-failover contract: the run is
// finalized succeeded, the group still has exactly the instances it was expanded
// with (no takeover re-expansion), every one of them is terminal with its own
// sequence, and preCrash — the row of the instance that completed BEFORE the
// crash, read at that moment — comes out of the failover byte-for-byte the same.
func assertGroupFinishedCleanly(t *testing.T, f *fanOutFailoverFixture, wantInstances int, preCrash models.TaskRun) {
	t.Helper()

	var jr models.JobRun
	require.NoError(t, f.db.First(&jr, "id = ?", f.runID).Error)
	require.Equal(t, string(StatusSucceeded), jr.Status, "the run must finalize succeeded after failover")

	after := f.instancesByKey(t)
	require.Len(t, after, wantInstances, "a takeover must not re-expand the group")
	seqs := make(map[int64]bool, len(after))
	for key, row := range after {
		require.Equalf(t, string(TaskStatusSucceeded), row.Status, "partition %s", key)
		require.Greaterf(t, row.TerminalSequence, int64(0),
			"partition %s must carry a terminal_sequence or replay cannot see it", key)
		require.Falsef(t, seqs[row.TerminalSequence], "partition %s shares a terminal_sequence", key)
		seqs[row.TerminalSequence] = true
	}

	// The instance that completed before the crash: same outcome, same sequence,
	// same attempt, and claimed exactly once — a re-dispatch would have bumped
	// claim_attempt, and a re-stamp would have moved terminal_sequence.
	finished := f.rowByID(t, preCrash.ID)
	require.Equal(t, string(TaskStatusSucceeded), finished.Status)
	require.Equal(t, preCrash.TerminalSequence, finished.TerminalSequence,
		"the takeover must not restamp a finished instance")
	require.Equal(t, preCrash.ClaimAttempt, finished.ClaimAttempt,
		"a finished instance must not be re-claimed after the takeover")
	require.Equal(t, preCrash.Attempt, finished.Attempt,
		"a finished instance must not be retried after the takeover")

	var publishRows []models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ?", f.runID, f.publish).
		Find(&publishRows).Error)
	require.Len(t, publishRows, 1)
	require.Equal(t, string(TaskStatusSucceeded), publishRows[0].Status)
}
