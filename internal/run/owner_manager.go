package run

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

// OwnerManager holds the authoritative in-memory RunState for the runs this node
// owns (run-owner in-memory mode, CAESIUM_RUN_OWNER_IN_MEMORY=true).  It is the
// integration seam the dispatch loop and the /internal/complete handler call:
//
//   - Adopt(runID)   — seed a fresh RunState for a run this node just created.
//   - Recover(runID) — rebuild a run's state from checkpoint + terminal tail on
//     lease takeover.
//   - Ready(runID)   — the ready queue the dispatch loop pulls from.
//   - MarkDispatched — record a task pushed to a worker.
//   - Complete(...)  — apply a worker completion: advance the DAG in memory,
//     durably write only terminal rows, and checkpoint.
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
	// reclaimInterval bounds how often a run re-queries the durable rows for
	// expired worker claims when its own bookkeeping shows nothing overdue.
	reclaimInterval time.Duration

	mu   sync.Mutex
	runs map[uuid.UUID]*ownedRun
}

// defaultOwnerReclaimInterval is the floor between owner-side expired-claim
// queries for one run.  The owner's in-memory lease bookkeeping triggers a reap
// immediately when it already knows a lease has lapsed, so this interval only
// governs the safety-net sweep that catches leases the owner cannot see expire
// (a worker renews claim_expires_at without telling its owner).
const defaultOwnerReclaimInterval = 15 * time.Second

type ownedRun struct {
	mu     sync.Mutex
	state  *RunState
	writer *CheckpointWriter
	gen    int64
	// lastReap is when this run last ran the owner-side expired-claim query.
	// It bounds that query to one per reclaimInterval per run in the common case
	// where nothing has gone wrong (see OwnerManager.ReclaimExpiredClaims).
	lastReap time.Time
	// stale is set (under mu) by Release: the state is known to predate a
	// per-partition retry the store refused to let this owner complete over,
	// so no writer may checkpoint it again — a completion that captured this
	// pointer before the release included.
	stale bool
}

// checkpointMaybe is the cadence checkpoint; mu must be held.
func (or *ownedRun) checkpointMaybe() {
	if or.stale {
		return
	}
	_ = or.writer.Maybe(or.state, or.gen)
}

// checkpointForce is the unconditional checkpoint taken when the run is
// dropped; mu must be held.
func (or *ownedRun) checkpointForce() {
	if or.stale {
		return
	}
	_ = or.writer.Force(or.state, or.gen)
}

// NewOwnerManager builds a manager backed by store, using cfg for checkpoint
// cadence and retention.
func NewOwnerManager(store *Store, cfg CheckpointConfig) *OwnerManager {
	m := &OwnerManager{
		store:           store,
		cfg:             cfg,
		reclaimInterval: defaultOwnerReclaimInterval,
		runs:            make(map[uuid.UUID]*ownedRun),
	}
	// This manager is a CACHE of the store's task_runs rows, so the store has to
	// be able to invalidate it. Registering here rather than in the server
	// bootstrap keeps the two halves of the contract in one place: a retry that
	// re-opens a run must not leave a snapshot behind that still calls it
	// complete (Store.invalidateRunState).
	store.SetRunStateCache(m)
	return m
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
	// Decide whether the checkpoint is usable BEFORE the tail query, not after.
	// The tail is filtered by the checkpoint's sequence_high; RecoverRunState
	// then falls back to a from-scratch replay (replay start zero) if Restore
	// rejects the blob — but the rows in hand would still be only the
	// post-checkpoint tail, so every terminal transition at or below
	// sequence_high vanished and the recovered owner re-dispatched work that had
	// already completed.  Validating here lets the fallback drop the sequence
	// filter in the same breath it drops the checkpoint.  ValidateCheckpointBlob
	// is the same acceptance test Restore applies, so the two cannot disagree.
	if checkpoint != nil {
		if vErr := ValidateCheckpointBlob(checkpoint.StateBlob); vErr != nil {
			log.Warn("owner manager: unusable checkpoint; replaying every terminal row",
				"run_id", runID, "sequence_high", checkpoint.SequenceHigh, "error", vErr)
			checkpoint = nil
		}
	}
	var afterSeq int64
	if checkpoint != nil {
		afterSeq = checkpoint.SequenceHigh
	}
	rows, err := m.store.TerminalTaskRunsSince(runID, afterSeq)
	if err != nil {
		return RecoveryResult{}, err
	}
	var allRows []models.TaskRun
	var catalog []models.Task
	if m.store != nil && m.store.DB() != nil {
		if err := m.store.DB().Where("job_run_id = ?", runID).Find(&allRows).Error; err != nil {
			return RecoveryResult{}, err
		}
		// The catalog rows carry fanOut (maxParallel, step name), which the
		// checkpoint deliberately does not snapshot: two copies of one graph can
		// disagree after a partial write, so the catalog stays authoritative.
		// Without them a takeover drops the group's in-flight cap for the rest
		// of the run.
		var jobRun models.JobRun
		if err := m.store.DB().Select("job_id").First(&jobRun, "id = ?", runID).Error; err == nil {
			if err := m.store.DB().Where("job_id = ?", jobRun.JobID).Find(&catalog).Error; err != nil {
				return RecoveryResult{}, err
			}
		}
	}
	rs, res, err := RecoverRunStateWithFanOut(topo, checkpoint, rows, allRows, catalog)
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
//
// The two identities are deliberately separate and both load-bearing.  TaskID is
// always the *catalog* task, which is what rate-limit rules, trigger rules, and
// every other catalog lookup are keyed by.  TaskRunID names the specific
// instance row to execute and to fence its completion against; it is uuid.Nil
// for an unfanned task, where the (run, task) pair still names exactly one row.
// Collapsing the two — sending an instance id as the task id — makes every
// catalog lookup silently miss.
type DispatchableTask struct {
	TaskID    uuid.UUID
	TaskRunID uuid.UUID
	Attempt   int
}

// ExecutionRef is the identity a dispatched task's row-level operations address:
// the instance when the task is fanned, the catalog task otherwise (which
// loadTaskRunByIDOrUnique resolves to the run's single row).
func (d DispatchableTask) ExecutionRef() uuid.UUID {
	if d.TaskRunID != uuid.Nil {
		return d.TaskRunID
	}
	return d.TaskID
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
		dt := DispatchableTask{TaskID: id, Attempt: attempt}
		if catalogID, isInstance := or.state.CatalogTaskID(id); isInstance {
			dt.TaskID = catalogID
			dt.TaskRunID = id
		}
		out = append(out, dt)
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
// Completions can be delivered more than once — a worker re-POSTs the identical
// envelope when this handler answers 503 for transient dqlite contention, and by
// then the DAG has already advanced in memory even though the durable write did
// not land.  ApplyCompletion replays what that first delivery decided, so the
// retry re-persists the same terminal rows at the same sequences.  The write is
// gated on CompletionResult.Durable: a result with nothing this owner stamped
// must never be persisted, because CompleteTaskOwner would write
// terminal_sequence = 0 and recovery reads the terminal tail with
// `terminal_sequence > ?`, making the row invisible after a takeover.
//
// All run work is serialized by the run's own lock; finalize/drop run after that
// lock is released, so the brief map lock is never held during a DB call.
func (m *OwnerManager) Complete(runID, taskID uuid.UUID, status TaskStatus, result, errMsg, claimedBy string, output map[string]string, branchSelections []string) (CompleteResult, error) {
	return m.CompleteInstance(runID, taskID, uuid.Nil, status, result, errMsg, claimedBy, output, branchSelections, nil)
}

func (m *OwnerManager) CompleteInstance(runID, taskID, taskRunID uuid.UUID, status TaskStatus, result, errMsg, claimedBy string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) (CompleteResult, error) {
	or, ok := m.get(runID)
	if !ok {
		return CompleteResult{Owned: false}, nil
	}

	// A container that exits non-zero is NOT reported as a failure: the worker
	// treats "the container ran and told us its result" as a completion and calls
	// sink.Succeeded, so this arrives as status=succeeded carrying result
	// "failure" (internal/worker/completion_sink.go). The SQL lane re-derives the
	// real status from the result string inside completeTask; taking the reported
	// status at face value here recorded the task SUCCEEDED in memory, so the
	// fail_fast branch never ran and every pending sibling of a failed partition
	// kept going. The durable row still went failed, but only on the LATER
	// sink.Failed delivery — after the DAG had advanced — so the row and the
	// owner's state disagreed. One reported outcome, one status, both lanes.
	status = effectiveTerminalStatus(status, result)

	or.mu.Lock()

	// Every transition below is staged on a CLONE and published only after the
	// durable write commits.  Applying to the authoritative state first and
	// writing second meant a failed transaction left the two disagreeing in the
	// one direction that cannot heal: the producer was already terminal in
	// memory, so the worker's redelivery took ApplyCompletion's already-terminal
	// branch, skipped expansion re-planning, and re-persisted the completion with
	// expansion = nil — while this owner's ready queue held instance ids that
	// exist in no row and dispatched them, forever, against a missing row.
	// Staging makes the pair atomic in the only sense that matters: either the
	// rows and the state both advanced, or neither did and the retry replans from
	// scratch.
	staged := or.state.Clone()

	// The worker stamps a TaskRunID on EVERY completion, but this state keys an
	// unfanned task by its catalog id — so ask the state which of the two it
	// stored rather than assuming. See RunState.CompletionIdentity.
	identity := staged.CompletionIdentity(taskID, taskRunID)

	var branchSkips []uuid.UUID
	if len(branchSelections) > 0 {
		skips, err := m.store.ResolveBranchSkips(runID, taskID, branchSelections)
		if err != nil {
			or.mu.Unlock()
			return CompleteResult{Owned: true}, err
		}
		branchSkips = skips
	}

	var expansion *FanOutExpansion
	var expansionSkipped []SkippedTask
	if status == TaskStatusSucceeded || status == TaskStatusCached {
		if ts, ok := staged.TaskState(identity); !ok || !IsTerminal(ts.Status) {
			// Both identities: the catalog task carries the fanOut wiring, while
			// `identity` names the row that ran. A fanned instance's catalog id
			// resolves to N rows, and the resulting ErrAmbiguousTaskRun would be
			// read below as the task having failed.
			exp, planErr := m.store.PlanFanOutExpansionForRow(runID, taskID, identity, partitions)
			switch {
			case planErr == nil:
				if exp != nil && len(exp.Groups) > 0 {
					expansion = exp
					expansionSkipped = staged.ApplyExpansion(exp)
					m.seedFanOutPolicies(staged, exp)
				}
			case errors.Is(planErr, ErrAmbiguousTaskRun):
				// The successor group already has N rows, so the planner cannot
				// find a unique template.  That means a previous delivery's write
				// DID land even though this owner saw it fail (a commit the client
				// never got the ack for) and then rolled its staged state back.
				// Failing the producer here would kill a group that is already
				// materialized; adopt the durable rows instead, which is the same
				// reconstruction recovery performs on takeover.
				log.Warn("owner manager: fan-out group already expanded durably; adopting persisted rows",
					"run_id", runID, "task_id", taskID, "error", planErr)
				m.adoptPersistedExpansion(staged, runID)
			default:
				status = TaskStatusFailed
				if errMsg == "" {
					errMsg = planErr.Error()
				}
			}
		}
	}

	if status == TaskStatusFailed {
		// fail_fast is about to decide which siblings it can still cancel, and
		// that turns on which of them have actually STARTED — a distinction this
		// state cannot make on its own, because a dispatch is recorded when a
		// peer accepts the push, not when its worker creates the container.
		// Refresh it from the durable rows first; runtime_id is the marker both
		// lanes use (see taskRunStarted). Best effort: a read failure leaves the
		// flags as they were, which can only make the cancel more conservative.
		if catalogID, isInstance := staged.CatalogTaskID(identity); isInstance {
			if rows, rErr := m.store.TaskRunsForTask(runID, catalogID); rErr == nil {
				staged.SyncStartedFromRows(rows)
			} else {
				log.Warn("owner manager: could not refresh fan-out group start state",
					"run_id", runID, "task_id", catalogID, "error", rErr)
			}
		}
	}

	res := staged.ApplyCompletion(identity, status, branchSkips)
	if len(expansionSkipped) > 0 {
		res.Skipped = append(expansionSkipped, res.Skipped...)
		staged.prependCompletionSkips(identity, expansionSkipped)
	}
	if !res.Applied {
		// The DAG had already advanced for this task when this completion arrived
		// — most often a worker re-delivering the identical envelope after this
		// handler answered 503 for transient contention.  Logged rather than
		// swallowed: a re-delivery means the first delivery's durable write may
		// never have landed.
		log.Warn("owner manager: completion re-delivered for an already-terminal task",
			"run_id", runID, "task_id", taskID, "status", status,
			"terminal_sequence", res.TerminalSequence, "repersisting", res.Durable())
	}

	// Persist only what this owner actually stamped.  A re-delivery replays the
	// first delivery's sequence and skips, so the retry re-writes the same
	// terminal rows and repairs a write that never landed.  A result with nothing
	// stamped must never be written: CompleteTaskOwner would set
	// terminal_sequence = 0, and recovery reads the terminal tail with
	// `terminal_sequence > ?`, so the row would be invisible to a future takeover.
	if res.Durable() {
		if err := m.store.CompleteTaskOwner(runID, identity, status, result, errMsg, claimedBy, output, branchSelections, res.TerminalSequence, or.gen, res.Skipped, expansion); err != nil {
			// The staged state is dropped on the floor: nothing was published, so
			// the authoritative state still shows this task in flight and the
			// worker's retry re-plans the expansion and re-stamps the same
			// sequence from an unchanged cursor.  Publishing here is what created
			// instance ids with no rows behind them.
			or.mu.Unlock()
			return CompleteResult{Owned: true}, err
		}
		// The write landed — publish the staged transition.
		or.state = staged

		// Checkpoint on cadence (best-effort: a failed checkpoint is recoverable
		// from the durable terminal rows, so it must not fail the completion).
		or.checkpointMaybe()
	}
	// A result with nothing durable is a no-op replay (or a completion for a task
	// this state never tracked); its staged copy is discarded for the same reason
	// a failed commit's is — nothing was written, so nothing may be published.

	complete := or.state.IsComplete()
	hasFailures := or.state.HasFailures()
	// A fan-out group's duration is measured from its first instance's dispatch
	// to the completion that made its last instance terminal — the transition
	// this call just applied. Taken inside the run lock (it mutates the
	// already-reported set), observed outside it.
	resolvedGroups := or.state.TakeResolvedGroups()
	or.mu.Unlock()

	if len(resolvedGroups) > 0 {
		alias := m.jobAliasForRun(runID)
		for _, g := range resolvedGroups {
			if g.Duration <= 0 {
				// Start unknown (the group's first dispatch predates this
				// owner's takeover); reporting a bogus duration is worse than
				// reporting none.
				continue
			}
			name := g.TaskName
			if name == "" {
				name = g.TaskID.String()
			}
			metrics.FanOutGroupDurationSeconds.WithLabelValues(alias, name).Observe(g.Duration.Seconds())
		}
	}

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
		if cErr := m.store.Complete(runID, runErr); errors.Is(cErr, ErrRunHasPendingWork) {
			// A per-partition retry was accepted into this run after the owner
			// saw it drain. RetryPartition dropped the run's checkpoints so a
			// recovering owner replays the rows from scratch and finds the
			// reset instance; every snapshot THIS owner holds predates the
			// retry and says the run is complete. Dropping with a forced
			// checkpoint (or keeping the one written on cadence moments ago)
			// would hand recovery exactly that stale state: it would replay
			// only the terminal rows after it, never see the pending retry,
			// and leave the run running forever. Release without a checkpoint
			// and discard any written since the retry, so recovery rebuilds
			// from the post-retry truth.
			log.Error("owner manager: run left running for a pending partition retry the owner cannot execute; releasing it for recovery", "run_id", runID, "error", cErr)
			if rErr := m.Release(runID); rErr != nil {
				log.Error("owner manager: failed to discard checkpoints after a refused completion", "run_id", runID, "error", rErr)
			}
			return CompleteResult{Ready: res.Ready, Complete: complete, Owned: true}, nil
		} else if cErr != nil {
			log.Error("owner manager: run finalize failed", "run_id", runID, "error", cErr)
		}
		m.Drop(runID)
	}

	return CompleteResult{Ready: res.Ready, Complete: complete, Owned: true}, nil
}

// WithReclaimInterval overrides the floor between owner-side expired-claim
// queries for one run.  Zero or negative restores the default.
func (m *OwnerManager) WithReclaimInterval(d time.Duration) *OwnerManager {
	if d <= 0 {
		d = defaultOwnerReclaimInterval
	}
	m.reclaimInterval = d
	return m
}

// ReclaimExpiredClaims returns this run's in-flight tasks whose worker claim
// lease has lapsed to the ready queue, and reports which it re-queued.
//
// It closes the one hole the two existing reapers leave between them.  A worker
// that dies mid-task leaves its row `running` with a dead claim_expires_at.
// Claimer.ReclaimExpired will not touch it — its live-lease guard deliberately
// skips rows belonging to a run whose owner is alive, so the reaper can never
// race the owner's dispatch loop.  The owner, meanwhile, only ever re-queued
// in-flight work on TAKEOVER (Recover → requeueRunning); for a run whose owner
// is perfectly healthy, the instance stayed `running` in memory forever,
// consuming a fanOut.maxParallel slot and blocking the run from ever completing.
//
// The durable claim_expires_at is authoritative, not the owner's own
// LeaseExpiresAtMs: a worker renews its claim lease directly and never tells the
// owner, so the in-memory copy is only a filter (see RunState.AnyLeaseOverdue) —
// it can over-report, never under-report, and the query settles it.
//
// The reset and the re-queue happen under the run's own lock, so this cannot
// race the dispatch it feeds.  Returns nil for a run this node does not own.
func (m *OwnerManager) ReclaimExpiredClaims(runID uuid.UUID) []uuid.UUID {
	or, ok := m.get(runID)
	if !ok {
		return nil
	}
	or.mu.Lock()
	defer or.mu.Unlock()

	now := time.Now()
	// Two cheap gates before the query, in order of cost: nothing in flight whose
	// lease looks overdue, or a reap already run recently for this run.  Together
	// they keep the steady state at zero queries and the worst case at one per
	// run per interval, even for a task that legitimately outlives the owner's
	// (never-renewed) copy of its lease.
	if !or.state.AnyLeaseOverdue(now.UnixMilli()) {
		return nil
	}
	if now.Sub(or.lastReap) < m.reclaimInterval {
		return nil
	}
	or.lastReap = now

	rows, err := m.store.ReclaimOwnerExpiredClaims(runID, or.gen)
	if err != nil {
		log.Warn("owner manager: expired-claim reap failed", "run_id", runID, "error", err)
		return nil
	}
	requeued := or.state.RequeueExpiredRows(rows)
	if len(requeued) > 0 {
		log.Warn("run owner: re-queued tasks whose worker claim lease expired",
			"run_id", runID, "generation", or.gen, "count", len(requeued))
	}
	return requeued
}

// adoptPersistedExpansion rebuilds a fan-out group's in-memory nodes from the
// run's durable instance rows.  It is the "reload from rows" arm of the staged
// completion path: when the planner reports the successor template is ambiguous,
// the group is already materialized in the DB and the owner must adopt it rather
// than fail the producer.  Best-effort — a read failure leaves the state as it
// was, and the completion proceeds against the unexpanded catalog node.
func (m *OwnerManager) adoptPersistedExpansion(state *RunState, runID uuid.UUID) {
	if state == nil || m.store == nil || m.store.DB() == nil {
		return
	}
	var rows []models.TaskRun
	if err := m.store.DB().Where("job_run_id = ?", runID).Find(&rows).Error; err != nil {
		log.Warn("owner manager: could not load rows to adopt persisted expansion",
			"run_id", runID, "error", err)
		return
	}
	var catalog []models.Task
	var jobRun models.JobRun
	if err := m.store.DB().Select("job_id").First(&jobRun, "id = ?", runID).Error; err == nil {
		if err := m.store.DB().Where("job_id = ?", jobRun.JobID).Find(&catalog).Error; err != nil {
			log.Warn("owner manager: could not load catalog to adopt persisted expansion",
				"run_id", runID, "error", err)
		}
	}
	state.RehydrateInGroupEdges(rows, catalog)
}

// seedFanOutPolicies records each freshly-expanded group's fanOut.failurePolicy
// on the run state.  It is read from the catalog Task rows rather than carried
// on the expansion payload so there is exactly one source of truth for fanOut
// config, shared with the recovery path (RehydrateInGroupEdges).  A lookup
// failure leaves the policy unset, which normalizes to the schema default
// (fail_fast) rather than silently switching the group to `continue`.
func (m *OwnerManager) seedFanOutPolicies(state *RunState, exp *FanOutExpansion) {
	if state == nil || exp == nil || len(exp.Groups) == 0 || m.store == nil || m.store.DB() == nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(exp.Groups))
	for _, g := range exp.Groups {
		if g.TaskID != uuid.Nil {
			ids = append(ids, g.TaskID)
		}
	}
	if len(ids) == 0 {
		return
	}
	var tasks []models.Task
	if err := m.store.DB().Select("id", "fan_out_config").Where("id IN ?", ids).Find(&tasks).Error; err != nil {
		log.Warn("owner manager: fan-out policy lookup failed", "error", err)
		return
	}
	for i := range tasks {
		fo, err := decodeFanOutConfig(tasks[i].FanOutConfig)
		if err != nil || fo == nil {
			continue
		}
		state.SetGroupFailurePolicy(tasks[i].ID, fo.FailurePolicy)
	}
}

// jobAliasForRun resolves the run's job alias for metric labelling.  Metric
// labels must be bounded and stable, so an unresolvable alias falls back to
// "unknown" rather than a per-run UUID.
func (m *OwnerManager) jobAliasForRun(runID uuid.UUID) string {
	if m.store == nil || m.store.DB() == nil {
		return "unknown"
	}
	var row struct{ Alias string }
	err := m.store.DB().
		Table("job_runs").
		Select("jobs.alias AS alias").
		Joins("join jobs on jobs.id = job_runs.job_id").
		Where("job_runs.id = ?", runID).
		Take(&row).Error
	if err == nil && row.Alias != "" {
		return row.Alias
	}
	return "unknown"
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
	or.checkpointForce()
	or.mu.Unlock()
}

// Release forgets a run WITHOUT checkpointing its in-memory state and discards
// every checkpoint it has on disk. It is for the case where that state is
// known to be stale — the store refused the owner's completion because a
// per-partition retry reopened work the state never saw — and a recovering
// owner must rebuild from the rows instead.
//
// The order is load-bearing. The run is marked stale under its own lock
// first, so a completion that captured the pointer cannot checkpoint it
// afterwards; then the checkpoints are deleted while this owner still holds
// the run; only then is the run forgotten. Forgetting it first would open a
// window in which a recovery tick restores a checkpoint the invalidation has
// not reached yet, into a fresh owner that would never see the reset row.
func (m *OwnerManager) Release(runID uuid.UUID) error {
	m.mu.Lock()
	or, ok := m.runs[runID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	or.mu.Lock()
	or.stale = true
	or.mu.Unlock()
	if err := m.store.InvalidateRunCheckpoints(runID); err != nil {
		// The stale snapshot is still on disk. Keep the run (stale, so it
		// never checkpoints again) rather than let a recovery tick restore
		// it; the caller reports the error.
		return err
	}
	m.mu.Lock()
	delete(m.runs, runID)
	m.mu.Unlock()
	return nil
}
