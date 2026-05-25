package run

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// OwnerTaskState is the per-task state the run owner holds in memory for a run
// it owns (run-owner mode).  It mirrors the subset of task_runs columns the
// owner needs to coordinate dispatch and reconstruct after a crash.
type OwnerTaskState struct {
	Status           TaskStatus `json:"status"`
	Attempt          int        `json:"attempt"`
	ClaimedBy        string     `json:"claimed_by,omitempty"`
	LeaseExpiresAtMs int64      `json:"lease_expires_at_ms,omitempty"`
}

// RunTopology is the immutable DAG shape for a run, loaded once at construction.
// Every task ID must appear as a key in Order (it is the authoritative task
// set); Adjacency/Predecessors/TriggerRule may omit tasks with no edges/rule.
type RunTopology struct {
	Adjacency    map[uuid.UUID][]uuid.UUID // task -> direct successors
	Predecessors map[uuid.UUID][]uuid.UUID // task -> direct predecessors
	TriggerRule  map[uuid.UUID]string      // task -> trigger rule ("" = all_success)
	Order        map[uuid.UUID]int         // task -> deterministic dispatch order
}

// SkippedTask records a task the owner transitioned to skipped (a DAG decision,
// never reported by a worker) along with the terminal_sequence stamped on it.
type SkippedTask struct {
	TaskID           uuid.UUID
	TerminalSequence int64
	Reason           string
}

// CompletionResult is what ApplyCompletion returns: the sequence stamped on the
// completed task, the tasks that newly became ready to dispatch, the tasks the
// owner skipped as a consequence, and whether the run is now complete.
type CompletionResult struct {
	TerminalSequence int64
	Ready            []uuid.UUID
	Skipped          []SkippedTask
	Complete         bool
}

// RunState is the run owner's authoritative in-memory DAG state for one run.
//
// It reproduces the advancement semantics of the local executor
// (internal/job): a completion decrements each successor's outstanding
// predecessor count, and when a successor reaches zero it either becomes ready
// (its trigger rule is satisfied by the predecessor outcomes) or is skipped and
// the skip propagates downstream.  The terminal vocabulary and trigger-rule
// evaluation are shared with the local path (run.IsTerminal,
// run.SatisfiesTriggerRule) so the two cannot diverge.
//
// Branch selection (a branch task choosing which successors run) is applied by
// the caller passing the non-selected successor IDs to ApplyCompletion as
// branchSkipped; resolving branch names to task IDs is the owner integration's
// responsibility, not this engine's.
//
// RunState is not safe for concurrent use; the owner serializes mutations.
type RunState struct {
	topo          RunTopology
	tasks         map[uuid.UUID]*OwnerTaskState
	indegree      map[uuid.UUID]int
	outcomes      map[uuid.UUID]TaskStatus // terminal outcomes, for trigger-rule eval
	ready         []uuid.UUID
	inReady       map[uuid.UUID]bool
	seq           int64 // per-run terminal_sequence cursor (monotonic, dense)
	terminalCount int
	total         int
}

// NewRunState builds a fresh RunState for a run that has not started executing:
// every task is pending with indegree equal to its predecessor count, and tasks
// with no predecessors are seeded into the ready queue.  startSeq is the
// terminal_sequence to count up from (0 for a new run; the checkpoint's
// sequence_high when seeding before replay).
func NewRunState(topo RunTopology, startSeq int64) *RunState {
	rs := &RunState{
		topo:     topo,
		tasks:    make(map[uuid.UUID]*OwnerTaskState, len(topo.Order)),
		indegree: make(map[uuid.UUID]int, len(topo.Order)),
		outcomes: make(map[uuid.UUID]TaskStatus),
		inReady:  make(map[uuid.UUID]bool),
		seq:      startSeq,
	}
	for id := range topo.Order {
		rs.tasks[id] = &OwnerTaskState{Status: TaskStatusPending, Attempt: 1}
		rs.indegree[id] = len(topo.Predecessors[id])
		rs.total++
	}
	for id := range rs.tasks {
		if rs.indegree[id] == 0 {
			rs.pushReady(id)
		}
	}
	return rs
}

// pushReady adds a pending task to the ready queue (idempotent), keeping the
// queue ordered by the topology's deterministic Order so dispatch is stable.
func (rs *RunState) pushReady(id uuid.UUID) {
	if rs.inReady[id] {
		return
	}
	if ts := rs.tasks[id]; ts == nil || IsTerminal(ts.Status) {
		return
	}
	rs.ready = append(rs.ready, id)
	rs.inReady[id] = true
	sort.SliceStable(rs.ready, func(i, j int) bool {
		return rs.topo.Order[rs.ready[i]] < rs.topo.Order[rs.ready[j]]
	})
}

func (rs *RunState) removeReady(id uuid.UUID) {
	if !rs.inReady[id] {
		return
	}
	delete(rs.inReady, id)
	for i, r := range rs.ready {
		if r == id {
			rs.ready = append(rs.ready[:i], rs.ready[i+1:]...)
			return
		}
	}
}

// markTerminal sets a task's terminal status, records its outcome, allocates and
// stamps the next terminal_sequence, removes it from the ready queue, and bumps
// the terminal count.  Returns the stamped sequence.  No-op (returns 0) if the
// task is unknown or already terminal.
func (rs *RunState) markTerminal(id uuid.UUID, status TaskStatus) int64 {
	ts := rs.tasks[id]
	if ts == nil || IsTerminal(ts.Status) {
		return 0
	}
	rs.seq++
	ts.Status = status
	ts.ClaimedBy = ""
	ts.LeaseExpiresAtMs = 0
	rs.outcomes[id] = status
	rs.removeReady(id)
	rs.terminalCount++
	return rs.seq
}

// ApplyCompletion records a worker-reported terminal outcome for taskID and
// advances the DAG.  branchSkipped lists the task's immediate successors that a
// branch decision excluded (nil for non-branch tasks); they are skipped and the
// skip propagates.  Remaining successors have their predecessor count
// decremented and, on reaching zero, are pushed ready or skipped per their
// trigger rule.  Returns the sequence stamped on taskID plus the newly ready and
// skipped tasks.  Applying a completion to an unknown or already-terminal task
// is a no-op.
func (rs *RunState) ApplyCompletion(taskID uuid.UUID, status TaskStatus, branchSkipped []uuid.UUID) CompletionResult {
	var res CompletionResult
	ts := rs.tasks[taskID]
	if ts == nil || IsTerminal(ts.Status) {
		res.Complete = rs.terminalCount >= rs.total
		return res
	}

	res.TerminalSequence = rs.markTerminal(taskID, status)

	seeds := []uuid.UUID{taskID}
	for _, b := range branchSkipped {
		st := rs.tasks[b]
		if st == nil || IsTerminal(st.Status) {
			continue
		}
		seq := rs.markTerminal(b, TaskStatusSkipped)
		res.Skipped = append(res.Skipped, SkippedTask{
			TaskID:           b,
			TerminalSequence: seq,
			Reason:           "branch not selected",
		})
		seeds = append(seeds, b)
	}

	ready, skipped := rs.advanceSuccessors(seeds)
	res.Ready = ready
	res.Skipped = append(res.Skipped, skipped...)
	res.Complete = rs.terminalCount >= rs.total
	return res
}

// advanceSuccessors performs the breadth-first DAG advancement shared by the
// success and skip paths: for each newly-terminal task, decrement each
// non-terminal successor's predecessor count, and when it reaches zero either
// push it ready (trigger rule satisfied) or skip it and enqueue the skip to
// propagate downstream.  This mirrors the local executor's propagateSkipped +
// successor-decrement loops exactly.
func (rs *RunState) advanceSuccessors(seeds []uuid.UUID) (ready []uuid.UUID, skipped []SkippedTask) {
	queue := append([]uuid.UUID{}, seeds...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, succ := range rs.topo.Adjacency[current] {
			st := rs.tasks[succ]
			if st == nil || IsTerminal(st.Status) {
				continue
			}
			if rs.indegree[succ] > 0 {
				rs.indegree[succ]--
			}
			if rs.indegree[succ] != 0 {
				continue
			}

			predStatuses := CollectPredecessorStatuses(rs.topo.Predecessors[succ], rs.outcomes)
			if SatisfiesTriggerRule(rs.topo.TriggerRule[succ], predStatuses) {
				rs.pushReady(succ)
				ready = append(ready, succ)
				continue
			}

			seq := rs.markTerminal(succ, TaskStatusSkipped)
			skipped = append(skipped, SkippedTask{
				TaskID:           succ,
				TerminalSequence: seq,
				Reason:           fmt.Sprintf("trigger rule %q not satisfied", rs.topo.TriggerRule[succ]),
			})
			queue = append(queue, succ)
		}
	}
	return ready, skipped
}

// MarkDispatched records that a ready task was pushed to a worker: it leaves the
// ready queue and becomes running with the given claim metadata.
func (rs *RunState) MarkDispatched(taskID uuid.UUID, claimedBy string, attempt int, leaseExpiresAtMs int64) {
	ts := rs.tasks[taskID]
	if ts == nil || IsTerminal(ts.Status) {
		return
	}
	ts.Status = TaskStatusRunning
	ts.ClaimedBy = claimedBy
	ts.Attempt = attempt
	ts.LeaseExpiresAtMs = leaseExpiresAtMs
	rs.removeReady(taskID)
}

// ReadyTasks returns a copy of the current ready queue in dispatch order.
func (rs *RunState) ReadyTasks() []uuid.UUID {
	out := make([]uuid.UUID, len(rs.ready))
	copy(out, rs.ready)
	return out
}

// TaskState returns a copy of a task's current state, or false if unknown.
func (rs *RunState) TaskState(id uuid.UUID) (OwnerTaskState, bool) {
	ts, ok := rs.tasks[id]
	if !ok {
		return OwnerTaskState{}, false
	}
	return *ts, true
}

// IsComplete reports whether every task in the run has reached a terminal state.
func (rs *RunState) IsComplete() bool { return rs.terminalCount >= rs.total }

// Sequence returns the current terminal_sequence cursor (the highest stamped).
func (rs *RunState) Sequence() int64 { return rs.seq }

// runStateSnapshot is the JSON-serializable form of a RunState's mutable state,
// persisted in a run_checkpoints row.  Topology is NOT serialized — it is
// reloaded from the catalog on recovery (constant for the run's lifetime).
type runStateSnapshot struct {
	Tasks         map[uuid.UUID]*OwnerTaskState `json:"tasks"`
	Indegree      map[uuid.UUID]int             `json:"indegree"`
	Outcomes      map[uuid.UUID]TaskStatus      `json:"outcomes"`
	Ready         []uuid.UUID                   `json:"ready"`
	Sequence      int64                         `json:"sequence"`
	TerminalCount int                           `json:"terminal_count"`
	Total         int                           `json:"total"`
}

// Snapshot serializes the mutable run state to a checkpoint blob (JSON in v1).
// The active-only / incremental size optimizations from the design are layered
// by the checkpoint writer; this produces a complete, self-contained snapshot.
func (rs *RunState) Snapshot() ([]byte, error) {
	snap := runStateSnapshot{
		Tasks:         rs.tasks,
		Indegree:      rs.indegree,
		Outcomes:      rs.outcomes,
		Ready:         rs.ready,
		Sequence:      rs.seq,
		TerminalCount: rs.terminalCount,
		Total:         rs.total,
	}
	return json.Marshal(snap)
}

// Restore rebuilds a RunState from a checkpoint blob produced by Snapshot,
// rehydrating the immutable topology from topo (reloaded from the catalog).
func Restore(topo RunTopology, blob []byte) (*RunState, error) {
	var snap runStateSnapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		return nil, fmt.Errorf("run state: unmarshal checkpoint: %w", err)
	}
	rs := &RunState{
		topo:          topo,
		tasks:         snap.Tasks,
		indegree:      snap.Indegree,
		outcomes:      snap.Outcomes,
		ready:         snap.Ready,
		inReady:       make(map[uuid.UUID]bool, len(snap.Ready)),
		seq:           snap.Sequence,
		terminalCount: snap.TerminalCount,
		total:         snap.Total,
	}
	if rs.tasks == nil {
		rs.tasks = make(map[uuid.UUID]*OwnerTaskState)
	}
	if rs.indegree == nil {
		rs.indegree = make(map[uuid.UUID]int)
	}
	if rs.outcomes == nil {
		rs.outcomes = make(map[uuid.UUID]TaskStatus)
	}
	for _, id := range rs.ready {
		rs.inReady[id] = true
	}
	return rs, nil
}
