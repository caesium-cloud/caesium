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
// Per-run mutation is serialized by the manager mutex, so RunState (which is not
// itself concurrency-safe) is only ever touched under the lock.
type OwnerManager struct {
	store *Store
	cfg   CheckpointConfig

	mu   sync.Mutex
	runs map[uuid.UUID]*ownedRun
}

type ownedRun struct {
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

// Adopt seeds a fresh in-memory state for a run this node created and owns at
// the given generation.  Topology is loaded from the catalog.  Idempotent: a
// second Adopt for an already-tracked run is a no-op.
func (m *OwnerManager) Adopt(runID uuid.UUID, generation int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[runID]; ok {
		return nil
	}
	topo, err := m.store.LoadRunTopology(runID)
	if err != nil {
		return err
	}
	m.runs[runID] = &ownedRun{
		state:  NewRunState(topo, 0),
		writer: NewCheckpointWriter(m.store, runID, m.cfg),
		gen:    generation,
	}
	return nil
}

// Recover rebuilds a run's in-memory state after a lease takeover: it loads the
// topology, the latest checkpoint, and the post-checkpoint terminal rows, then
// reconstructs RunState.  The RecoveryResult tells the caller which tasks are
// ready and which running tasks must be re-dispatched.  A forced checkpoint is
// written immediately so the new generation's view is durable.
func (m *OwnerManager) Recover(runID uuid.UUID, generation int64) (RecoveryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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
	// Reset the DB rows of tasks left in-flight by the dead owner to pending so
	// the re-dispatch (RecoveryResult.ReDispatch, already re-queued in RunState)
	// can re-claim them.  Best-effort: a failure just delays the re-claim.
	if len(res.ReDispatch) > 0 {
		if rErr := m.store.ResetInFlightTasks(runID); rErr != nil {
			log.Warn("owner manager: reset in-flight tasks failed", "run_id", runID, "error", rErr)
		}
	}
	or := &ownedRun{
		state:  rs,
		writer: NewCheckpointWriter(m.store, runID, m.cfg),
		gen:    generation,
	}
	m.runs[runID] = or
	// Persist a checkpoint stamped with the new generation immediately.
	_ = or.writer.Force(rs, generation)
	return res, nil
}

// Owns reports whether this node is tracking in-memory state for the run.
func (m *OwnerManager) Owns(runID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.runs[runID]
	return ok
}

// Ready returns the run's current ready queue in dispatch order, or nil if the
// run is not owned by this node.
func (m *OwnerManager) Ready(runID uuid.UUID) []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	or, ok := m.runs[runID]
	if !ok {
		return nil
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	or, ok := m.runs[runID]
	if !ok {
		return nil
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if or, ok := m.runs[runID]; ok {
		or.state.MarkDispatched(taskID, worker, attempt, leaseExpiresAtMs)
	}
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
func (m *OwnerManager) Complete(runID, taskID uuid.UUID, status TaskStatus, result, errMsg, claimedBy string, output map[string]string, branchSelections []string) (CompleteResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	or, ok := m.runs[runID]
	if !ok {
		return CompleteResult{Owned: false}, nil
	}

	var branchSkips []uuid.UUID
	if len(branchSelections) > 0 {
		skips, err := m.store.ResolveBranchSkips(taskID, branchSelections)
		if err != nil {
			return CompleteResult{Owned: true}, err
		}
		branchSkips = skips
	}

	res := or.state.ApplyCompletion(taskID, status, branchSkips)

	if err := m.store.CompleteTaskOwner(runID, taskID, status, result, errMsg, claimedBy, output, branchSelections, res.TerminalSequence, or.gen, res.Skipped); err != nil {
		return CompleteResult{Owned: true}, err
	}

	// Checkpoint on cadence (best-effort: a failed checkpoint is recoverable from
	// the durable terminal rows, so it must not fail the completion).
	_ = or.writer.Maybe(or.state, or.gen)

	// When the DAG is complete, finalize the run here.  This makes the owner the
	// authoritative finalizer, which is essential after a takeover (the original
	// node's waitForRunCompletion is gone); in the normal case the triggering
	// node's waitForRunCompletion also calls store.Complete, which is an
	// idempotent no-op once we have set the terminal status.
	if res.Complete {
		var runErr error
		if or.state.HasFailures() {
			runErr = fmt.Errorf("run %s completed with failed task(s)", runID)
		}
		if cErr := m.store.Complete(runID, runErr); cErr != nil {
			// Don't fail the completion on a finalize error — the terminal rows
			// are durable and waitForRunCompletion / a later sweep will retry.
			log.Error("owner manager: run finalize failed", "run_id", runID, "error", cErr)
		}
		m.dropLocked(runID)
	}

	return CompleteResult{Ready: res.Ready, Complete: res.Complete, Owned: true}, nil
}

// Drop releases the run's in-memory state (on completion or lease loss).  A
// final checkpoint is forced so a subsequent takeover replays the least tail.
func (m *OwnerManager) Drop(runID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropLocked(runID)
}

// dropLocked releases a run; the caller must hold m.mu.
func (m *OwnerManager) dropLocked(runID uuid.UUID) {
	if or, ok := m.runs[runID]; ok {
		_ = or.writer.Force(or.state, or.gen)
		delete(m.runs, runID)
	}
}
