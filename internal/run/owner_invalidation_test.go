package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests cover the two ways the owner in-memory lane could keep a run's
// PRE-RETRY state after RetryPartition reopened it — the failure the
// integration suite sees as `TestFanOutHTTPRetryPartition` "a reset instance
// never ran again ... 60s after a 200 retry" (master run 34062579647, job
// 101565958611). Both are invisible to the DB-backed lanes, which read the rows
// every tick and cache nothing.

// TestInvalidateRunStateLeavesNoSnapshotTheRetryCannotSee pins the seam
// Store.invalidateRunState uses.
//
// It used to call OwnerManager.Drop, and Drop's contract is to force a final
// checkpoint of the state it is discarding — on this path, exactly the "run is
// complete" snapshot the retry has just invalidated. That snapshot is durable
// between Drop and the DeleteCheckpoints that follows, and the pointer Drop
// dropped is not marked stale, so a completion still holding it can write the
// same snapshot again afterwards. Either copy is enough to make the next
// recovery reconstruct a complete run: the reset row stops being terminal, and
// the terminal tail a recovery replays never reports rows that stopped being
// terminal.
//
// Release is the primitive that does not have that window, and this test fails
// against Drop.
func TestInvalidateRunStateLeavesNoSnapshotTheRetryCannotSee(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, ids[0], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids[0], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	// A completion in flight captured the run pointer before the retry landed.
	mgr.mu.Lock()
	captured := mgr.runs[runID]
	mgr.mu.Unlock()
	require.NotNil(t, captured, "precondition: the owner is tracking the run")

	// The retry commits and the store invalidates the cached state.
	store.invalidateRunState(runID)

	require.False(t, mgr.Owns(runID),
		"the reopened run must be forgotten so the next tick rebuilds it from the rows")

	// The in-flight completion now reaches its checkpoint writes.
	captured.mu.Lock()
	captured.checkpointMaybe()
	captured.checkpointForce()
	captured.mu.Unlock()

	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp,
		"no snapshot taken before the retry may survive it: recovery would restore a complete run "+
			"and never dispatch the reset instance")
}

// TestOwnerManagerRefusesAStateBuiltAcrossAnInvalidation pins the other half:
// the rebuild that was already running when the retry landed.
//
// Adopt and Recover do their DB work outside the manager lock by design, so a
// dispatch tick can be halfway through rebuilding a run at the moment
// RetryPartition reopens it. Without the epoch guard, that rebuild's put is the
// last word — the manager ends up owning a state that predates the retry,
// ReadyForDispatch returns nothing, and nothing ever invalidates it again
// because the store already did.
func TestOwnerManagerRefusesAStateBuiltAcrossAnInvalidation(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, _ := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))

	topo, err := store.LoadRunTopology(runID)
	require.NoError(t, err)
	build := func() *ownedRun {
		return &ownedRun{
			state:  NewRunState(topo, 0),
			writer: NewCheckpointWriter(store, runID, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}),
			gen:    2,
		}
	}

	// A dispatch tick starts rebuilding: it takes the epoch first.
	epoch := mgr.invalidationEpoch(runID)

	// The retry commits mid-rebuild.
	store.invalidateRunState(runID)
	require.False(t, mgr.Owns(runID))

	require.Equal(t, putStale, mgr.put(runID, build(), epoch),
		"a state built before the invalidation must not be published over it")
	require.False(t, mgr.Owns(runID),
		"the refused rebuild must leave the run unowned so the next tick rebuilds it")

	// A rebuild started after the invalidation publishes normally.
	require.Equal(t, putPublished, mgr.put(runID, build(), mgr.invalidationEpoch(runID)))
	require.True(t, mgr.Owns(runID))
}

// TestInvalidateRunStateFencesARebuildOfAnAlreadyDroppedRun is the shape the
// integration flake actually takes.
//
// The run FINISHED first: the owner completed it and dropped it, so by the time
// the operator retries a partition the manager is not tracking it at all — the
// dispatch loop is instead re-recovering it every tick, because the lease
// outlives completion and `!Owns` is the loop's only rebuild trigger. An
// invalidation that treats "not tracked" as "nothing to do" lets the in-flight
// rebuild republish the completed run, and the reset instance is never
// dispatched.
func TestInvalidateRunStateFencesARebuildOfAnAlreadyDroppedRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, _ := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	topo, err := store.LoadRunTopology(runID)
	require.NoError(t, err)

	// The run completed and the owner let it go.
	mgr.Drop(runID)
	require.False(t, mgr.Owns(runID))

	// A dispatch tick begins re-recovering the finished run.
	epoch := mgr.invalidationEpoch(runID)

	// The partition retry reopens it while that rebuild is in flight.
	store.invalidateRunState(runID)

	stale := &ownedRun{
		state:  NewRunState(topo, 0),
		writer: NewCheckpointWriter(store, runID, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}),
		gen:    2,
	}
	require.Equal(t, putStale, mgr.put(runID, stale, epoch),
		"an untracked run still has to fence a rebuild that predates the reopen")
}

// TestRecoverDoesNotRewriteACheckpointForARunReleasedMidPublish drives the
// window between Recover's publish and its generation checkpoint.
//
// The retry is injected INSIDE that window through the recoverAfterPublish test
// seam, so this exercises the real Recover, not a hand-typed imitation of it.
// Recover publishes, the store then reopens the run — marking the state it just
// published stale and deleting the run's checkpoints — and Recover's write must
// therefore be suppressed. Reverting that write to a raw writer.Force (which
// ignores `stale`) puts a pre-retry "run is complete" snapshot back on disk
// after the invalidation removed it, and this test goes red.
func TestRecoverDoesNotRewriteACheckpointForARunReleasedMidPublish(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, _ := seedChainRun(t, db, store, "node-1")

	cfg := CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}
	mgr := NewOwnerManager(store, cfg)
	// The retry commits after Recover publishes and before it checkpoints.
	var fired int
	mgr.recoverAfterPublish = func(id uuid.UUID) {
		fired++
		store.invalidateRunState(id)
	}

	_, err := mgr.Recover(runID, 2)
	require.NoError(t, err)
	require.Equal(t, 1, fired, "the seam must have run inside Recover's publish window")

	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp,
		"Recover's own checkpoint must not resurrect a snapshot the retry invalidated mid-publish")
}

// TestDropCheckpointsBeforeForgettingSoAReleaseCanStillReachIt drives the
// window between Drop's two steps.
//
// The retry is injected INSIDE that window through the dropMidpoint seam. With
// the checkpoint written first, the retry still finds the run tracked, marks it
// stale and deletes what was written. With the old forget-first order the retry
// finds nothing to mark stale, its DeleteCheckpoints runs against an empty
// table, and Drop's forced "run is complete" snapshot lands afterwards and
// survives — so reverting the order makes this test red.
func TestDropCheckpointsBeforeForgettingSoAReleaseCanStillReachIt(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, ids := seedChainRun(t, db, store, "node-1")

	mgr := NewOwnerManager(store, CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	require.NoError(t, mgr.Adopt(runID, 1))
	mgr.MarkDispatched(runID, ids[0], "node-1", 1, 0)
	_, err := mgr.Complete(runID, ids[0], TaskStatusSucceeded, "success", "", "node-1", nil, nil)
	require.NoError(t, err)

	var fired int
	mgr.dropMidpoint = func(id uuid.UUID) {
		fired++
		store.invalidateRunState(id)
	}

	mgr.Drop(runID)
	require.Equal(t, 1, fired, "the seam must have run inside Drop's window")
	require.False(t, mgr.Owns(runID))

	cp, err := store.LatestFullCheckpoint(runID)
	require.NoError(t, err)
	require.Nil(t, cp,
		"a retry landing inside Drop must be able to remove the snapshot Drop writes")
}

// TestInvalidationStampsAreMonotonicAcrossAPublish is the ABA guard.
//
// A stamp must never go backwards for a run, or a rebuild holding an old
// snapshot becomes acceptable again. Two ways that could happen, both closed
// here: a per-run counter restarting at 1 after the entry was cleared, and put
// clearing the entry at all (which resets the run to the "never invalidated"
// value of 0 that every pre-first-invalidation rebuild holds). Stamps come from
// a manager-wide monotonic sequence and survive a publish.
func TestInvalidationStampsAreMonotonicAcrossAPublish(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, _ := seedChainRun(t, db, store, "node-1")

	cfg := CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3}
	mgr := NewOwnerManager(store, cfg)
	require.NoError(t, mgr.Adopt(runID, 1))
	topo, err := store.LoadRunTopology(runID)
	require.NoError(t, err)
	build := func() *ownedRun {
		return &ownedRun{state: NewRunState(topo, 0), writer: NewCheckpointWriter(store, runID, cfg), gen: 2}
	}

	// G1 starts rebuilding, then a retry invalidates the run.
	g1Epoch := mgr.invalidationEpoch(runID)
	store.invalidateRunState(runID)
	first := mgr.invalidationEpoch(runID)
	require.Greater(t, first, g1Epoch)

	// G2 rebuilds after the invalidation and publishes. The stamp SURVIVES the
	// publish: clearing it would reset the run to 0, which is exactly the value
	// G1 is still holding.
	require.Equal(t, putPublished, mgr.put(runID, build(), first))
	require.Equal(t, first, mgr.invalidationEpoch(runID),
		"publishing must not clear the stamp back to the never-invalidated value")

	// The run completes and is dropped — which must NOT stamp it, or ordinary
	// completion churn would force spurious rebuild retries.
	mgr.Drop(runID)
	require.Equal(t, first, mgr.invalidationEpoch(runID), "Drop is not an invalidation")

	// A second retry invalidates it again. The new stamp must be strictly
	// greater than the first, so G1's snapshot can never match it.
	store.invalidateRunState(runID)
	second := mgr.invalidationEpoch(runID)
	require.Greater(t, second, first, "stamps must be monotonic, not a per-run count")

	require.Equal(t, putStale, mgr.put(runID, build(), g1Epoch),
		"G1's pre-invalidation rebuild must still be refused after a publish/drop cycle")
}
