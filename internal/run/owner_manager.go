package run

import (
	"fmt"
	"sync"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// OwnerManager holds the authoritative in-memory RunState for the runs this node
// owns (run-owner in-memory mode, CAESIUM_RUN_OWNER_IN_MEMORY=true).  It is the
// integration seam the dispatch loop and the /internal/complete handler call:
//
//   - Adopt(runID)   — seed a fresh RunState for a run this node just created.
//   - Recover(runID) — rebuild a run's state from checkpoint + terminal tail on
//                      lease takeover.
//   - Ready(runID)   — the ready queue the dispatch loop pulls from.
//   - MarkDispatched — record a task pushed to a worker.
//   - Complete(...)  — apply a worker completion: advance the DAG in memory,
//                      durably write only terminal rows, and checkpoint.
//   - Drop(runID)    — release a run on completion or lease loss.
//
// Concurrency: the global mu guards only the runs map (brief lookups / inserts /
// deletes).  All per-run work — RunState mutation and the run's DB operations —
// is serialized by that run's own ownedRun.mu, so different runs proceed
// concurrently and a slow DB call for one run never blocks the others.  DB work
// done while building a run (Adopt/Recover) runs before the run is published
// into the map, so it holds no manager lock at all.
type OwnerManager struct {
	store *Store
	cfg   CheckpointConfig

	mu   sync.Mutex
	runs map[uuid.UUID]*ownedRun
}

type ownedRun struct {
	mu     sync.Mutex
	state  *RunState
	writer *CheckpointWriter
	gen    int64
}

// NewOwnerManager builds a manager backed by store, using cfg for checkpoint
// cadence and retention.
func NewOwnerManager(store *Store, cfg CheckpointConfig) *OwnerManager {
	return &OwnerManager{
		store: store,
		cfg:   cfg,
		runs:  make(map[uuid.UUID]*ownedRun),
	}
}

// get returns the ownedRun for a run, holding the map lock only briefly.
func (m *OwnerManager) get(runID uuid.UUID) (*ownedRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	or, ok := m.runs[runID]
	return or, ok
}

// put publishes a freshly-built ownedRun into the map.  Idempotent: if the run
// is already tracked it keeps the existing entry and reports false.
func (m *OwnerManager) put(runID uuid.UUID, or *ownedRun) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runID]; ok {
		return false
	}
	m.runs[runID] = or
	return true
}

// Adopt seeds a fresh in-memory state for a run this node created and owns at
// the given generation.  Topology is loaded from the catalog (outside any lock).
// Idempotent: a second Adopt for an already-tracked run is a no-op.
func (m *OwnerManager) Adopt(runID uuid.UUID, generation int64) error {
	if _, ok := m.get(runID); ok {
		return nil
	}
	topo, err := m.store.LoadRunTopology(runID)
	if err != nil {
		return err
	}
	m.put(runID, &ownedRun{
		state:  NewRunState(topo, 0),
		writer: NewCheckpointWriter(m.store, runID, m.cfg),
		gen:    generation,
	})
	return nil
}

// Recover rebuilds a run's in-memory state after a lease takeover: it loads the
// topology, the latest checkpoint, and the post-checkpoint terminal rows, then
// reconstructs RunState.  All of this runs outside any manager lock (the run is
// not yet published).  The RecoveryResult tells the caller which tasks are ready
// and which running tasks were re-queued for dispatch.
func (m *OwnerManager) Recover(runID uuid.UUID, generation int64) (RecoveryResult, error) {
	topo, err := m.store.LoadRunTopology(runID)
	if err != nil {
		return RecoveryResult{}, err
	}
	checkpoint, err := m.store.LatestFullCheckpoint(runID)
	if err != nil {
		return RecoveryResult{}, err
	}
	var afterSeq int64
	if checkpoint != nil {
		afterSeq = checkpoint.SequenceHigh
	}
	rows, err := m.store.TerminalTaskRunsSince(runID, afterSeq)
	if err != nil {
		return RecoveryResult{}, err
	}
	rs, res, err := RecoverRunState(topo, checkpoint, rows)
	if err != nil {
		return RecoveryResult{}, err
	}
	// Reset every DB row the dead owner left running back to pending (clearing
	// the stale claim) so the new owner can re-dispatch+claim them.  Always run
	// this, not just when RunState re-queued tasks: the checkpoint can lag the
	// DB (the dead owner dispatched a task after its last checkpoint), so a row
	// may be "running" in the DB while the recovered in-memory state shows it
	// "ready".  Best-effort: a failure just delays the re-claim.
	if rErr := m.store.ResetInFlightTasks(runID); rErr != nil {
		log.Warn("owner manager: reset in-flight tasks failed", "run_id", runID, "error", rErr)
	}
	or := &ownedRun{
		state:  rs,
		writer: NewCheckpointWriter(m.store, runID, m.cfg),
		gen:    generation,
	}
	// Persist a checkpoint stamped with the new generation immediately.
	_ = or.writer.Force(rs, generation)
	m.put(runID, or)
	log.Info("run owner: recovered run on takeover", "run_id", runID, "generation", generation,
		"ready", len(res.Ready), "redispatch", len(res.ReDispatch), "complete", res.Complete)
	return res, nil
}

// Owns reports whether this node is tracking in-memory state for the run.
func (m *OwnerManager) Owns(runID uuid.UUID) bool {
	_, ok := m.get(runID)
	return ok
}

// Ready returns the run's current ready queue in dispatch order, or nil if the
// run is not owned by this node.
func (m *OwnerManager) Ready(runID uuid.UUID) []uuid.UUID {
	or, ok := m.get(runID)
	if !ok {
		return nil
	}
	or.mu.Lock()
	defer or.mu.Unlock()
	return or.state.ReadyTasks()
}

// DispatchableTask is a ready task plus the attempt number to stamp on its
// dispatch (1 for a first run, incremented for a re-dispatch after recovery).
type DispatchableTask struct {
	TaskID  uuid.UUID
	Attempt int
}

// ReadyForDispatch returns the run's ready tasks paired with their current
// attempt, for the dispatch loop to push.  Nil if the run is not owned here.
func (m *OwnerManager) ReadyForDispatch(runID uuid.UUID) []DispatchableTask {
	or, ok := m.get(runID)
	if !ok {
		return nil
	}
	or.mu.Lock()
	defer or.mu.Unlock()
	ready := or.state.ReadyTasks()
	out := make([]DispatchableTask, 0, len(ready))
	for _, id := range ready {
		attempt := 1
		if st, ok := or.state.TaskState(id); ok && st.Attempt > 0 {
			attempt = st.Attempt
		}
		out = append(out, DispatchableTask{TaskID: id, Attempt: attempt})
	}
	return out
}

// MarkDispatched records that a ready task was pushed to a worker.
func (m *OwnerManager) MarkDispatched(runID, taskID uuid.UUID, worker string, attempt int, leaseExpiresAtMs int64) {
	or, ok := m.get(runID)
	if !ok {
		return
	}
	or.mu.Lock()
	defer or.mu.Unlock()
	or.state.MarkDispatched(taskID, worker, attempt, leaseExpiresAtMs)
}

// CompleteResult reports the outcome of applying a worker completion.
type CompleteResult struct {
	Ready    []uuid.UUID // tasks that newly became ready to dispatch
	Complete bool        // the run reached a terminal state
	Owned    bool        // false if this node does not own the run (caller should fall back)
}

// Complete applies a worker-reported terminal outcome to the owned run: it
// resolves any branch skips, advances the in-memory DAG, durably writes the
// terminal rows (completed task + skips) via CompleteTaskOwner, and checkpoints
// on cadence.  Returns the newly-ready tasks and whether the run is complete.
// Owned is false when this node does not own the run, signalling the caller to
// fall back to the SQL path.
//
// All run work is serialized by the run's own lock; finalize/drop run after that
// lock is released, so the brief map lock is never held during a DB call.
func (m *OwnerManager) Complete(runID, taskID uuid.UUID, status TaskStatus, result, errMsg, claimedBy string, output map[string]string, branchSelections []string) (CompleteResult, error) {
	or, ok := m.get(runID)
	if !ok {
		return CompleteResult{Owned: false}, nil
	}

	or.mu.Lock()

	var branchSkips []uuid.UUID
	if len(branchSelections) > 0 {
		skips, err := m.store.ResolveBranchSkips(taskID, branchSelections)
		if err != nil {
			or.mu.Unlock()
			return CompleteResult{Owned: true}, err
		}
		branchSkips = skips
	}

	res := or.state.ApplyCompletion(taskID, status, branchSkips)

	if err := m.store.CompleteTaskOwner(runID, taskID, status, result, errMsg, claimedBy, output, branchSelections, res.TerminalSequence, or.gen, res.Skipped); err != nil {
		or.mu.Unlock()
		return CompleteResult{Owned: true}, err
	}

	// Checkpoint on cadence (best-effort: a failed checkpoint is recoverable from
	// the durable terminal rows, so it must not fail the completion).
	_ = or.writer.Maybe(or.state, or.gen)

	complete := res.Complete
	hasFailures := or.state.HasFailures()
	or.mu.Unlock()

	// When the DAG is complete, finalize the run.  This makes the owner the
	// authoritative finalizer, which is essential after a takeover (the original
	// node's waitForRunCompletion is gone); in the normal case the triggering
	// node's waitForRunCompletion also calls store.Complete, which is an
	// idempotent no-op once we have set the terminal status.  Done after the run
	// lock is released so the finalize DB call doesn't extend the critical
	// section.
	if complete {
		var runErr error
		if hasFailures {
			runErr = fmt.Errorf("run %s completed with failed task(s)", runID)
		}
		if cErr := m.store.Complete(runID, runErr); cErr != nil {
			log.Error("owner manager: run finalize failed", "run_id", runID, "error", cErr)
		}
		m.Drop(runID)
	}

	return CompleteResult{Ready: res.Ready, Complete: complete, Owned: true}, nil
}

// Drop releases the run's in-memory state (on completion or lease loss).  A
// final checkpoint is forced so a subsequent takeover replays the least tail.
func (m *OwnerManager) Drop(runID uuid.UUID) {
	m.mu.Lock()
	or, ok := m.runs[runID]
	if ok {
		delete(m.runs, runID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	or.mu.Lock()
	_ = or.writer.Force(or.state, or.gen)
	or.mu.Unlock()
}
