package run

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
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
	TaskID           uuid.UUID `json:"task_id"`
	TerminalSequence int64     `json:"terminal_sequence"`
	Reason           string    `json:"reason,omitempty"`
}

// CompletionResult is what ApplyCompletion returns: the sequence stamped on the
// completed task, the tasks that newly became ready to dispatch, the tasks the
// owner skipped as a consequence, and whether the run is now complete.
//
// Applied distinguishes a completion this call actually advanced from one that
// was already applied and is being replayed (see ApplyCompletion).  Callers
// decide whether to persist with Durable, never by testing TerminalSequence
// against zero.
type CompletionResult struct {
	TerminalSequence int64
	Ready            []uuid.UUID
	Skipped          []SkippedTask
	Complete         bool
	Applied          bool
}

// Durable reports whether the result carries terminal rows the owner must write:
// a sequence stamped on the completing task, or owner-decided skips.  A result
// that is not Durable must NOT be persisted — CompleteTaskOwner would stamp
// terminal_sequence = 0 on the row, and recovery reads the terminal tail with a
// strictly-greater predicate (`terminal_sequence > ?`), so a zero-stamped row is
// invisible to replay after a takeover.
func (r CompletionResult) Durable() bool {
	return r.TerminalSequence > 0 || len(r.Skipped) > 0
}

// completionRecord is the durable effect one worker completion had: the sequence
// stamped on the completing task plus every skip that completion decided.  It is
// retained per task so a repeat completion for the same task replays the same
// durable write instead of a zero-valued one.
type completionRecord struct {
	TerminalSequence int64         `json:"terminal_sequence"`
	Skipped          []SkippedTask `json:"skipped,omitempty"`
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
	// completions records, per completed task, the durable effect its completion
	// had, so a repeat delivery of the same completion replays that effect rather
	// than a zero-valued one.  See ApplyCompletion.
	completions map[uuid.UUID]completionRecord

	// Fan-out instance maps. Catalog edges stay on topo; in-group edges live
	// here and are enumerated by traverseSuccessors. Not snapshotted — recovery
	// rebuilds them from TaskRun partition columns.
	catalogOf     map[uuid.UUID]uuid.UUID   // instance TaskRun ID -> catalog Task ID
	instancesOf   map[uuid.UUID][]uuid.UUID // catalog Task ID -> instance TaskRun IDs
	inGroupAdj    map[uuid.UUID][]uuid.UUID // instance -> in-group dependents
	instanceOrder map[uuid.UUID]int
	maxParallel   map[uuid.UUID]int // catalog Task ID -> fanOut.maxParallel (0 = unlimited)
	// partitionKeys is the instance -> partition key index.  It is what makes an
	// owner-mode in-group skip reason read the same as the SQL cascade's
	// ("fan-out dependency <key> failed", internal/run/fanout.go), instead of a
	// TaskRun UUID no operator can map back to a unit of work.
	partitionKeys map[uuid.UUID]string
	// failurePolicy is the catalog Task ID -> fanOut.failurePolicy index. It
	// decides what one instance's failure does to the rest of its group, and it
	// lives only in memory, so recovery re-seeds it from the catalog.
	failurePolicy map[uuid.UUID]string
	// groupNames / groupStarted / groupObserved back the
	// caesium_fanout_group_duration_seconds observation: the owner engine is the
	// only place that knows both when a group's first instance was dispatched
	// and when its last instance reached a terminal state.
	groupNames    map[uuid.UUID]string    // catalog Task ID -> step name
	groupStarted  map[uuid.UUID]time.Time // catalog Task ID -> first instance dispatch
	groupObserved map[uuid.UUID]bool      // catalog Task ID -> duration already reported
}

// ResolvedGroup is one fan-out group that reached a fully terminal state, with
// the wall-clock duration from its first instance's dispatch.  Duration is zero
// when the start is unknown (a group whose first dispatch predates a takeover).
type ResolvedGroup struct {
	TaskID   uuid.UUID
	TaskName string
	Duration time.Duration
}

// NewRunState builds a fresh RunState for a run that has not started executing:
// every task is pending with indegree equal to its predecessor count, and tasks
// with no predecessors are seeded into the ready queue.  startSeq is the
// terminal_sequence to count up from (0 for a new run; the checkpoint's
// sequence_high when seeding before replay).
func NewRunState(topo RunTopology, startSeq int64) *RunState {
	rs := &RunState{
		topo:          topo,
		tasks:         make(map[uuid.UUID]*OwnerTaskState, len(topo.Order)),
		indegree:      make(map[uuid.UUID]int, len(topo.Order)),
		outcomes:      make(map[uuid.UUID]TaskStatus),
		inReady:       make(map[uuid.UUID]bool),
		seq:           startSeq,
		completions:   make(map[uuid.UUID]completionRecord),
		catalogOf:     make(map[uuid.UUID]uuid.UUID),
		instancesOf:   make(map[uuid.UUID][]uuid.UUID),
		inGroupAdj:    make(map[uuid.UUID][]uuid.UUID),
		instanceOrder: make(map[uuid.UUID]int),
		maxParallel:   make(map[uuid.UUID]int),
		partitionKeys: make(map[uuid.UUID]string),
		failurePolicy: make(map[uuid.UUID]string),
		groupNames:    make(map[uuid.UUID]string),
		groupStarted:  make(map[uuid.UUID]time.Time),
		groupObserved: make(map[uuid.UUID]bool),
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
		return rs.orderOf(rs.ready[i]) < rs.orderOf(rs.ready[j])
	})
}

func (rs *RunState) orderOf(id uuid.UUID) int {
	if n, ok := rs.instanceOrder[id]; ok {
		return n
	}
	return rs.topo.Order[id]
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
// skipped tasks, with Applied true.
//
// A completion can be delivered more than once for the same task: a worker
// re-POSTs the identical /internal/complete envelope when the owner answers 503
// after transient dqlite contention, and by then the DAG has already advanced in
// memory even though the durable write did not land.  Such a repeat delivery
// does not advance anything (Applied false) but still returns the sequence and
// skips the first delivery decided, so the caller re-persists the *same* terminal
// rows.  Returning a zero sequence here would let the caller stamp
// terminal_sequence = 0, which recovery's `terminal_sequence > ?` replay filters
// out — the row would be invisible to a future takeover.  Ready is deliberately
// not replayed: the ready queue lives in this state and the dispatch loop polls
// it, so a re-delivery must not look like fresh work.
//
// A completion for a task this state does not know, or one made terminal by
// recovery replay (which adopts the row's stored sequence rather than stamping
// one), has no recorded effect: the result is not Durable and must not be
// persisted.
func (rs *RunState) ApplyCompletion(taskID uuid.UUID, status TaskStatus, branchSkipped []uuid.UUID) CompletionResult {
	var res CompletionResult
	ts := rs.tasks[taskID]
	if ts == nil || IsTerminal(ts.Status) {
		if rec, ok := rs.completions[taskID]; ok {
			res.TerminalSequence = rec.TerminalSequence
			res.Skipped = append([]SkippedTask(nil), rec.Skipped...)
		}
		res.Complete = rs.terminalCount >= rs.total
		return res
	}

	res.Applied = true
	res.TerminalSequence = rs.markTerminal(taskID, status)

	seeds := []uuid.UUID{taskID}
	if status == TaskStatusFailed && rs.groupFailsFast(taskID) {
		// fail_fast (the schema DEFAULT — pkg/jobdef validateSteps applies it
		// whenever fanOut.failurePolicy is omitted): the first instance failure
		// stops the whole group. Resolving a sibling here both removes it from
		// the ready queue (so it is never dispatched) and gives it a terminal row
		// carrying its own sequence, so a takeover does not re-dispatch it. This
		// is a superset of the `continue` cascade below, so the two are mutually
		// exclusive.
		//
		// PENDING siblings only, matching the design ("at first failure,
		// cancelling pending siblings" — docs/design-dynamic-fanout.md:324, :691,
		// :1111) and the SQL lane's failFastSkipSiblingsTx. A RUNNING sibling is
		// deliberately left alone: Caesium cannot kill its container, so marking
		// the row skipped would claim a terminal state for live work and let the
		// worker's later completion contradict it. It runs to its own terminal
		// state, the group resolves failed on that transition, and the fan-in is
		// skipped by its trigger rule — later, but never wrong.
		catalogID := rs.catalogOf[taskID]
		for _, sib := range rs.instancesOf[catalogID] {
			if sib == taskID {
				continue
			}
			st := rs.tasks[sib]
			if st == nil || st.Status != TaskStatusPending {
				continue
			}
			seq := rs.markTerminal(sib, TaskStatusSkipped)
			res.Skipped = append(res.Skipped, SkippedTask{
				TaskID:           sib,
				TerminalSequence: seq,
				Reason:           "fan-out group failed fast",
			})
			seeds = append(seeds, sib)
		}
	} else if status == TaskStatusFailed {
		// In-group skip cascade, the owner-side counterpart of
		// skipInGroupDependentsTx: a failed instance skips its *transitive*
		// dependents, not just its direct ones. Walking only one level leaves a
		// grandchild with a decremented counter and a satisfied cross-step
		// trigger rule, so it would be dispatched even though its in-group
		// prerequisite never ran. The reason names the failed instance's
		// partition key for every member of the cascade, byte-identical to the
		// SQL path's single `reason` string.
		reason := fmt.Sprintf("fan-out dependency %s failed", rs.partitionKey(taskID))
		queue := append([]uuid.UUID(nil), rs.inGroupAdj[taskID]...)
		seen := map[uuid.UUID]bool{taskID: true}
		for len(queue) > 0 {
			dep := queue[0]
			queue = queue[1:]
			if seen[dep] {
				continue
			}
			seen[dep] = true
			queue = append(queue, rs.inGroupAdj[dep]...)

			st := rs.tasks[dep]
			if st == nil || IsTerminal(st.Status) {
				continue
			}
			seq := rs.markTerminal(dep, TaskStatusSkipped)
			res.Skipped = append(res.Skipped, SkippedTask{
				TaskID:           dep,
				TerminalSequence: seq,
				Reason:           reason,
			})
			seeds = append(seeds, dep)
		}
	}
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

	rs.completions[taskID] = completionRecord{
		TerminalSequence: res.TerminalSequence,
		Skipped:          append([]SkippedTask(nil), res.Skipped...),
	}
	return res
}

func (rs *RunState) prependCompletionSkips(id uuid.UUID, extra []SkippedTask) {
	if rs == nil || len(extra) == 0 {
		return
	}
	rec := rs.completions[id]
	rec.Skipped = append(append([]SkippedTask(nil), extra...), rec.Skipped...)
	rs.completions[id] = rec
}

// advanceSuccessors performs the breadth-first DAG advancement shared by the
// success and skip paths: for each newly-terminal task, decrement each
// non-terminal successor's predecessor count, and when it reaches zero either
// push it ready (trigger rule satisfied) or skip it and enqueue the skip to
// propagate downstream.  This mirrors the local executor's propagateSkipped +
// successor-decrement loops exactly.
type unsatisfiedTriggerPolicy int

const (
	skipUnsatisfied unsatisfiedTriggerPolicy = iota
	leaveUnsatisfiedPending
)

func (rs *RunState) advanceSuccessors(seeds []uuid.UUID) (ready []uuid.UUID, skipped []SkippedTask) {
	return rs.traverseSuccessors(seeds, skipUnsatisfied)
}

// traverseSuccessors is the single successor-walk kernel used by live
// ApplyCompletion (skipUnsatisfied) and recovery ApplyTerminalRow
// (leaveUnsatisfiedPending). The unsatisfied-trigger policy stays a parameter
// so replay does not double-handle persisted skips.
func (rs *RunState) traverseSuccessors(seeds []uuid.UUID, policy unsatisfiedTriggerPolicy) (ready []uuid.UUID, skipped []SkippedTask) {
	queue := append([]uuid.UUID{}, seeds...)
	// A fan-out group's cross-step edge belongs to the *group*, not to each
	// instance: the successor inherited one indegree unit for the whole group
	// (its catalog predecessor count), so the group must decrement it exactly
	// once, on the transition that resolves the last instance. One traversal can
	// carry several members of the same group — a failed instance plus every
	// dependent its skip cascade resolved — and each one sees a now-resolved
	// group. Without this guard each seed decrements the successor again, so it
	// reaches zero while other predecessors are still running (dispatching it
	// early) and is appended to `ready` once per seed.
	walkedGroups := make(map[uuid.UUID]bool, len(seeds))
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		successors := rs.successorIDs(current, walkedGroups)
		for _, succ := range successors {
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

			predStatuses := rs.predecessorStatuses(succ)
			rule := rs.triggerRule(succ)
			if SatisfiesTriggerRule(rule, predStatuses) {
				rs.pushReady(succ)
				ready = append(ready, succ)
				continue
			}
			if policy == leaveUnsatisfiedPending {
				continue
			}

			seq := rs.markTerminal(succ, TaskStatusSkipped)
			skipped = append(skipped, SkippedTask{
				TaskID:           succ,
				TerminalSequence: seq,
				Reason:           fmt.Sprintf("trigger rule %q not satisfied", rule),
			})
			queue = append(queue, succ)
		}
	}
	return ready, skipped
}

// ApplyTerminalRow applies a terminal task_runs row observed during recovery
// replay.  Unlike ApplyCompletion it does NOT allocate a new sequence (it adopts
// the row's stored sequence) and does NOT auto-skip unsatisfied successors —
// every skip was itself persisted as a terminal row and arrives in sequence
// order, so re-deriving skips here would double-handle them.  It sets the task's
// terminal status, advances the cursor, and pushes any successor whose trigger
// rule is now satisfied.  Returns the newly-ready successors.  Idempotent for a
// task already terminal in the restored snapshot.
func (rs *RunState) ApplyTerminalRow(taskID uuid.UUID, status TaskStatus, storedSeq int64) []uuid.UUID {
	if storedSeq > rs.seq {
		rs.seq = storedSeq
	}
	ts := rs.tasks[taskID]
	if ts == nil {
		return nil
	}
	wasTerminal := IsTerminal(ts.Status)
	ts.Status = status
	ts.ClaimedBy = ""
	ts.LeaseExpiresAtMs = 0
	rs.outcomes[taskID] = status
	rs.removeReady(taskID)
	if !wasTerminal {
		rs.terminalCount++
	} else {
		// Already counted in the snapshot; nothing new to advance.
		return nil
	}

	ready, _ := rs.traverseSuccessors([]uuid.UUID{taskID}, leaveUnsatisfiedPending)
	return ready
}

// RunningTasks returns the IDs of tasks currently in the running state — used
// during recovery to identify in-flight work a dead owner had dispatched, which
// the new owner must re-dispatch.
func (rs *RunState) RunningTasks() []uuid.UUID {
	var out []uuid.UUID
	for id, ts := range rs.tasks {
		if ts.Status == TaskStatusRunning {
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return rs.orderOf(out[i]) < rs.orderOf(out[j])
	})
	return out
}

// requeueRunning moves every task left running (in-flight when the previous
// owner died, with no terminal row) back to pending and onto the ready queue
// with an incremented attempt, returning their IDs in dispatch order.  Recovery
// calls this so lost in-flight work is re-dispatched fresh.
func (rs *RunState) requeueRunning() []uuid.UUID {
	out := rs.RunningTasks()
	for _, id := range out {
		ts := rs.tasks[id]
		ts.Status = TaskStatusPending
		ts.Attempt++
		ts.ClaimedBy = ""
		ts.LeaseExpiresAtMs = 0
		rs.pushReady(id)
	}
	return out
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
	// A group's clock starts at its first instance's dispatch (the design's
	// "first instance start to group resolve"); TakeResolvedGroups closes it.
	if cat, ok := rs.catalogOf[taskID]; ok {
		if _, started := rs.groupStarted[cat]; !started {
			rs.groupStarted[cat] = time.Now()
		}
	}
}

// TakeResolvedGroups returns every fan-out group that has become fully terminal
// and has not been reported before, in deterministic dispatch order.  It is the
// owner engine's observation point for caesium_fanout_group_duration_seconds:
// the caller records the duration and this state never reports that group again.
func (rs *RunState) TakeResolvedGroups() []ResolvedGroup {
	if rs == nil || len(rs.instancesOf) == 0 {
		return nil
	}
	var out []ResolvedGroup
	for catalogID := range rs.instancesOf {
		if rs.groupObserved[catalogID] || !rs.groupResolved(catalogID) {
			continue
		}
		rs.groupObserved[catalogID] = true
		var d time.Duration
		if start, ok := rs.groupStarted[catalogID]; ok && !start.IsZero() {
			d = time.Since(start)
		}
		out = append(out, ResolvedGroup{
			TaskID:   catalogID,
			TaskName: rs.groupNames[catalogID],
			Duration: d,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return rs.topo.Order[out[i].TaskID] < rs.topo.Order[out[j].TaskID]
	})
	return out
}

// ReadyTasks returns a copy of the current ready queue in dispatch order,
// applying per-group fanOut.maxParallel so in-flight instances never exceed
// the cap.
func (rs *RunState) ReadyTasks() []uuid.UUID {
	inFlight := make(map[uuid.UUID]int)
	for id, ts := range rs.tasks {
		if ts != nil && ts.Status == TaskStatusRunning {
			cat := id
			if c, ok := rs.catalogOf[id]; ok {
				cat = c
			}
			inFlight[cat]++
		}
	}
	out := make([]uuid.UUID, 0, len(rs.ready))
	for _, id := range rs.ready {
		cat := id
		if c, ok := rs.catalogOf[id]; ok {
			cat = c
		}
		if capn := rs.maxParallel[cat]; capn > 0 && inFlight[cat] >= capn {
			continue
		}
		out = append(out, id)
		inFlight[cat]++
	}
	return out
}

// SetGroupFailurePolicy records a fanned step's fanOut.failurePolicy.  The
// owner learns it from the catalog (at expansion and again on recovery), never
// from the checkpoint: two copies of one config can disagree after a partial
// write, and the catalog row is authoritative.
func (rs *RunState) SetGroupFailurePolicy(catalogID uuid.UUID, policy string) {
	if rs == nil || catalogID == uuid.Nil {
		return
	}
	if rs.failurePolicy == nil {
		rs.failurePolicy = make(map[uuid.UUID]string)
	}
	rs.failurePolicy[catalogID] = normalizeFanOutFailurePolicy(policy)
}

// normalizeFanOutFailurePolicy resolves an unset policy to fail_fast, matching
// pkg/jobdef validateSteps, which stamps that default onto the stored config.
// Both lanes must normalize identically or a job that omits the key behaves one
// way locally and another under the run owner.
func normalizeFanOutFailurePolicy(policy string) string {
	if policy == jobdefschema.FanOutFailureContinue {
		return jobdefschema.FanOutFailureContinue
	}
	return jobdefschema.FanOutFailureFailFast
}

// groupFailsFast reports whether a failed instance must stop its whole group.
// False for anything that is not a fan-out instance, so unfanned tasks are
// completely unaffected.
func (rs *RunState) groupFailsFast(id uuid.UUID) bool {
	catalogID, isInstance := rs.catalogOf[id]
	if !isInstance {
		return false
	}
	return normalizeFanOutFailurePolicy(rs.failurePolicy[catalogID]) == jobdefschema.FanOutFailureFailFast
}

// CatalogTaskID maps a ready/running identity back to its catalog task.  ok is
// false when the identity *is* a catalog task (an unfanned step), which is how
// callers tell an instance from a plain task without reaching into the state.
func (rs *RunState) CatalogTaskID(id uuid.UUID) (uuid.UUID, bool) {
	catalogID, ok := rs.catalogOf[id]
	return catalogID, ok
}

// CompletionIdentity resolves the key THIS state stores for a completion that
// names both a catalog task and the row that executed it.
//
// The two sides of the wire disagree about identity on purpose, and this is the
// single place that reconciles them:
//
//   - Dispatch is precise. ReadyForDispatch sets DispatchableTask.TaskRunID only
//     for a real instance and leaves it uuid.Nil for an unfanned step, because
//     that is exactly how this state is keyed — ExpandTask deletes the template
//     node and inserts one node per TaskRunID, and everything else stays keyed by
//     its catalog task id.
//   - Completion is not. ownerSink.send stamps req.TaskRunID = taskRun.ID for
//     every route and every task (internal/worker/completion_sink.go), which is
//     right for the SQL fallback — loadTaskRunByIDOrUnique takes a primary key or
//     a catalog id interchangeably — and is what makes a fanned sibling's
//     completion unambiguous.
//
// Preferring TaskRunID unconditionally therefore looked an unfanned task's
// primary key up in a map keyed by its catalog id, missed, and ApplyCompletion
// reported Applied=false with sequence 0: nothing persisted, no successor
// released, and the run hung until its harness timed out. Every job has an
// unfanned task, so that stalled the entire owner in-memory lane.
//
// Asking the state rather than trusting either side keeps both properties: a
// fanned instance still resolves to its own row (falling back to the catalog id
// there would resolve an arbitrary sibling), and an unfanned task resolves to
// the catalog id this state actually stored. The completions map is consulted
// too so a RE-DELIVERED completion lands on the same key the first delivery
// stamped and replays its sequence instead of starting a second one.
func (rs *RunState) CompletionIdentity(taskID, taskRunID uuid.UUID) uuid.UUID {
	if rs == nil || taskRunID == uuid.Nil || taskRunID == taskID {
		return taskID
	}
	if rs.knows(taskRunID) {
		return taskRunID
	}
	if rs.knows(taskID) {
		return taskID
	}
	// Neither is a node here (a run this owner does not really track). Name the
	// row rather than the catalog task: a catalog id is ambiguous for a fanned
	// group, and the caller's Applied=false path is what handles it.
	return taskRunID
}

// knows reports whether an identity is one this state has a node or a recorded
// completion for.
func (rs *RunState) knows(id uuid.UUID) bool {
	if _, ok := rs.tasks[id]; ok {
		return true
	}
	_, ok := rs.completions[id]
	return ok
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

// HasFailures reports whether any task reached the failed terminal state — used
// to decide the run's final status when the DAG completes.
func (rs *RunState) HasFailures() bool {
	for _, status := range rs.outcomes {
		if status == TaskStatusFailed {
			return true
		}
	}
	return false
}

// Sequence returns the current terminal_sequence cursor (the highest stamped).
func (rs *RunState) Sequence() int64 { return rs.seq }

// ExpandTask replaces the single catalog-keyed entry for taskID with N
// instance-keyed entries (TaskRun IDs), raising total in the same critical
// section. Unfanned runs never call this.
func (rs *RunState) ExpandTask(taskID uuid.UUID, instances []ExpandedInstance) {
	if rs == nil || len(instances) == 0 {
		return
	}
	rs.removeReady(taskID)
	wasTerminal := false
	if ts := rs.tasks[taskID]; ts != nil && IsTerminal(ts.Status) {
		wasTerminal = true
	}
	delete(rs.tasks, taskID)
	delete(rs.indegree, taskID)
	delete(rs.inReady, taskID)
	if rs.total > 0 {
		rs.total--
	}
	if wasTerminal && rs.terminalCount > 0 {
		rs.terminalCount--
	}
	catalogOrder := rs.topo.Order[taskID]
	ids := make([]uuid.UUID, 0, len(instances))
	for _, inst := range instances {
		id := inst.TaskRunID
		if id == uuid.Nil {
			continue
		}
		rs.tasks[id] = &OwnerTaskState{Status: TaskStatusPending, Attempt: 1}
		rs.indegree[id] = inst.OutstandingPredecessors
		rs.catalogOf[id] = taskID
		rs.instanceOrder[id] = catalogOrder*10000 + inst.PartitionIndex
		rs.partitionKeys[id] = inst.Partition.Key
		rs.total++
		ids = append(ids, id)
		if inst.OutstandingPredecessors == 0 {
			rs.pushReady(id)
		}
	}
	rs.instancesOf[taskID] = ids
}

// ApplyExpansion materializes fanned successor groups and in-group adjacency.
func (rs *RunState) ApplyExpansion(exp *FanOutExpansion) []SkippedTask {
	if rs == nil || exp == nil {
		return nil
	}
	var skipped []SkippedTask
	for _, g := range exp.Groups {
		if g.MaxParallel > 0 {
			rs.maxParallel[g.TaskID] = g.MaxParallel
		}
		if g.TaskName != "" {
			rs.groupNames[g.TaskID] = g.TaskName
		}
		if g.Skipped {
			if ts := rs.tasks[g.TaskID]; ts != nil && !IsTerminal(ts.Status) {
				seq := rs.markTerminal(g.TaskID, TaskStatusSkipped)
				skipped = append(skipped, SkippedTask{
					TaskID:           g.TaskID,
					TerminalSequence: seq,
					Reason:           "fan-out produced no partitions",
				})
				ready, more := rs.advanceSuccessors([]uuid.UUID{g.TaskID})
				_ = ready
				skipped = append(skipped, more...)
			}
			continue
		}
		if len(g.Instances) == 0 {
			continue
		}
		rs.ExpandTask(g.TaskID, g.Instances)
		keyToID := make(map[string]uuid.UUID, len(g.Instances))
		for _, inst := range g.Instances {
			keyToID[inst.Partition.Key] = inst.TaskRunID
		}
		for fromKey, deps := range g.Dependents {
			fromID := keyToID[fromKey]
			if fromID == uuid.Nil {
				continue
			}
			for _, toKey := range deps {
				toID := keyToID[toKey]
				if toID == uuid.Nil {
					continue
				}
				rs.inGroupAdj[fromID] = append(rs.inGroupAdj[fromID], toID)
			}
		}
	}
	return skipped
}

// successorIDs enumerates the edges leaving `current`: the catalog (step-level)
// successors when `current` is an unfanned task or the transition that resolved
// its whole group, plus its instance-level in-group dependents.  walkedGroups
// (may be nil) records which catalog groups this traversal has already walked,
// so a group contributes its cross-step decrement exactly once.
func (rs *RunState) successorIDs(current uuid.UUID, walkedGroups map[uuid.UUID]bool) []uuid.UUID {
	var out []uuid.UUID
	catalogID, isInstance := rs.catalogOf[current]
	if !isInstance {
		catalogID = current
	}
	walkCatalog := !isInstance
	if isInstance && rs.groupResolved(catalogID) {
		walkCatalog = true
	}
	if walkCatalog && walkedGroups != nil {
		if walkedGroups[catalogID] {
			walkCatalog = false
		} else {
			walkedGroups[catalogID] = true
		}
	}
	if walkCatalog {
		for _, succ := range rs.topo.Adjacency[catalogID] {
			if insts := rs.instancesOf[succ]; len(insts) > 0 {
				out = append(out, insts...)
			} else {
				out = append(out, succ)
			}
		}
	}
	out = append(out, rs.inGroupAdj[current]...)
	return out
}

func (rs *RunState) groupResolved(catalogID uuid.UUID) bool {
	insts := rs.instancesOf[catalogID]
	if len(insts) == 0 {
		ts := rs.tasks[catalogID]
		return ts != nil && IsTerminal(ts.Status)
	}
	for _, id := range insts {
		ts := rs.tasks[id]
		if ts == nil || !IsTerminal(ts.Status) {
			return false
		}
	}
	return true
}

func (rs *RunState) predecessorStatuses(id uuid.UUID) []TaskStatus {
	catalogID := id
	if c, ok := rs.catalogOf[id]; ok {
		catalogID = c
	}
	preds := rs.topo.Predecessors[catalogID]
	out := make([]TaskStatus, 0, len(preds))
	for _, p := range preds {
		if insts := rs.instancesOf[p]; len(insts) > 0 {
			rows := make([]models.TaskRun, 0, len(insts))
			for _, inst := range insts {
				st := TaskStatusPending
				if ts := rs.tasks[inst]; ts != nil {
					st = ts.Status
				}
				rows = append(rows, models.TaskRun{Status: string(st)})
			}
			out = append(out, groupStatusFromInstances(rows))
			continue
		}
		if st, ok := rs.outcomes[p]; ok {
			out = append(out, st)
			continue
		}
		if ts := rs.tasks[p]; ts != nil {
			out = append(out, ts.Status)
		}
	}
	return out
}

func (rs *RunState) triggerRule(id uuid.UUID) string {
	catalogID := id
	if c, ok := rs.catalogOf[id]; ok {
		catalogID = c
	}
	return rs.topo.TriggerRule[catalogID]
}

// partitionKey resolves an instance's partition key — the operator-facing name
// for a unit of work.  Falls back to the TaskRun UUID only for an identity this
// state has no expansion for (an unfanned task), which no fan-out message uses.
func (rs *RunState) partitionKey(id uuid.UUID) string {
	if key, ok := rs.partitionKeys[id]; ok && key != "" {
		return key
	}
	return id.String()
}

// RehydrateInGroupEdges rebuilds fan-out maps from durable instance rows.
// Called on recovery after Restore, before replaying the terminal tail.
//
// catalog is the run's catalog Task rows; they carry the FanOutConfig that
// fanOut.maxParallel is re-seeded from (the cap lives only in memory, so
// without this a takeover silently drops it for the rest of the run) and the
// step names the group-duration metric is labelled with.  It may be nil, in
// which case only the edges and partition keys are rebuilt.
func (rs *RunState) RehydrateInGroupEdges(rows []models.TaskRun, catalog []models.Task) {
	if rs == nil || len(rows) == 0 {
		return
	}
	if rs.catalogOf == nil {
		rs.catalogOf = make(map[uuid.UUID]uuid.UUID)
	}
	if rs.instancesOf == nil {
		rs.instancesOf = make(map[uuid.UUID][]uuid.UUID)
	}
	if rs.inGroupAdj == nil {
		rs.inGroupAdj = make(map[uuid.UUID][]uuid.UUID)
	}
	if rs.instanceOrder == nil {
		rs.instanceOrder = make(map[uuid.UUID]int)
	}
	if rs.maxParallel == nil {
		rs.maxParallel = make(map[uuid.UUID]int)
	}
	if rs.partitionKeys == nil {
		rs.partitionKeys = make(map[uuid.UUID]string)
	}
	if rs.failurePolicy == nil {
		rs.failurePolicy = make(map[uuid.UUID]string)
	}
	if rs.groupNames == nil {
		rs.groupNames = make(map[uuid.UUID]string)
	}
	if rs.groupStarted == nil {
		rs.groupStarted = make(map[uuid.UUID]time.Time)
	}
	if rs.groupObserved == nil {
		rs.groupObserved = make(map[uuid.UUID]bool)
	}

	// Re-seed the scheduling metadata the checkpoint deliberately does not carry:
	// the catalog rows are authoritative for fanOut, so there is only ever one
	// copy of it.
	catalogMaxParallel := make(map[uuid.UUID]int, len(catalog))
	catalogFailurePolicy := make(map[uuid.UUID]string, len(catalog))
	for i := range catalog {
		task := &catalog[i]
		if task.Name != "" {
			rs.groupNames[task.ID] = task.Name
		}
		fo, err := decodeFanOutConfig(task.FanOutConfig)
		if err != nil || fo == nil {
			continue
		}
		catalogFailurePolicy[task.ID] = fo.FailurePolicy
		if fo.MaxParallel <= 0 {
			continue
		}
		catalogMaxParallel[task.ID] = fo.MaxParallel
	}

	grouped := make(map[uuid.UUID][]models.TaskRun)
	for i := range rows {
		row := rows[i]
		if row.PartitionCount == 0 && row.PartitionValue == "" {
			continue
		}
		grouped[row.TaskID] = append(grouped[row.TaskID], row)
	}
	for catalogID, insts := range grouped {
		if len(insts) == 0 {
			continue
		}
		if n := catalogMaxParallel[catalogID]; n > 0 {
			rs.maxParallel[catalogID] = n
		}
		if policy, ok := catalogFailurePolicy[catalogID]; ok {
			rs.SetGroupFailurePolicy(catalogID, policy)
		}
		if _, hasCatalog := rs.tasks[catalogID]; hasCatalog {
			expanded := make([]ExpandedInstance, 0, len(insts))
			base := rs.indegree[catalogID]
			for _, row := range insts {
				var deps []string
				if len(row.PartitionDependsOn) > 0 {
					_ = json.Unmarshal(row.PartitionDependsOn, &deps)
				}
				indegree := 0
				if len(deps) > 0 {
					indegree = len(deps)
				}
				expanded = append(expanded, ExpandedInstance{
					TaskRunID:               row.ID,
					TaskID:                  catalogID,
					PartitionIndex:          row.PartitionIndex,
					Partition:               pkgtask.Partition{Key: row.PartitionValue, Fingerprint: row.PartitionFingerprint, DependsOn: deps},
					OutstandingPredecessors: base + indegree,
				})
			}
			rs.ExpandTask(catalogID, expanded)
			keyToID := make(map[string]uuid.UUID, len(insts))
			for _, row := range insts {
				keyToID[row.PartitionValue] = row.ID
			}
			for _, row := range insts {
				var deps []string
				if len(row.PartitionDependsOn) > 0 {
					_ = json.Unmarshal(row.PartitionDependsOn, &deps)
				}
				for _, d := range deps {
					if from := keyToID[d]; from != uuid.Nil {
						rs.inGroupAdj[from] = append(rs.inGroupAdj[from], row.ID)
					}
				}
			}
			continue
		}
		ids := make([]uuid.UUID, 0, len(insts))
		keyToID := make(map[string]uuid.UUID, len(insts))
		for _, row := range insts {
			ids = append(ids, row.ID)
			keyToID[row.PartitionValue] = row.ID
			rs.catalogOf[row.ID] = catalogID
			rs.instanceOrder[row.ID] = rs.topo.Order[catalogID]*10000 + row.PartitionIndex
			rs.partitionKeys[row.ID] = row.PartitionValue
		}
		rs.instancesOf[catalogID] = ids
		for _, row := range insts {
			var deps []string
			if len(row.PartitionDependsOn) > 0 {
				_ = json.Unmarshal(row.PartitionDependsOn, &deps)
			}
			for _, d := range deps {
				from := keyToID[d]
				if from != uuid.Nil {
					rs.inGroupAdj[from] = append(rs.inGroupAdj[from], row.ID)
				}
			}
		}
	}
}

const runStateSnapshotVersion = 1

var errUnknownCheckpointVersion = fmt.Errorf("run state: unknown or missing checkpoint version")

// runStateSnapshot is the JSON-serializable form of a RunState's mutable state,
// persisted in a run_checkpoints row.  Topology is NOT serialized — it is
// reloaded from the catalog on recovery (constant for the run's lifetime).
type runStateSnapshot struct {
	Version       int                            `json:"version"`
	Tasks         map[uuid.UUID]*OwnerTaskState  `json:"tasks"`
	Indegree      map[uuid.UUID]int              `json:"indegree"`
	Outcomes      map[uuid.UUID]TaskStatus       `json:"outcomes"`
	Ready         []uuid.UUID                    `json:"ready"`
	Sequence      int64                          `json:"sequence"`
	TerminalCount int                            `json:"terminal_count"`
	Total         int                            `json:"total"`
	Completions   map[uuid.UUID]completionRecord `json:"completions,omitempty"`
}

// Snapshot serializes the mutable run state to a checkpoint blob (JSON in v1).
// The active-only / incremental size optimizations from the design are layered
// by the checkpoint writer; this produces a complete, self-contained snapshot.
func (rs *RunState) Snapshot() ([]byte, error) {
	snap := runStateSnapshot{
		Version:       runStateSnapshotVersion,
		Tasks:         rs.tasks,
		Indegree:      rs.indegree,
		Outcomes:      rs.outcomes,
		Ready:         rs.ready,
		Sequence:      rs.seq,
		TerminalCount: rs.terminalCount,
		Total:         rs.total,
		Completions:   rs.completions,
	}
	return json.Marshal(snap)
}

// Restore rebuilds a RunState from a checkpoint blob produced by Snapshot,
// rehydrating the immutable topology from topo (reloaded from the catalog).
func Restore(topo RunTopology, blob []byte) (*RunState, error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(blob, &probe); err != nil {
		return nil, fmt.Errorf("run state: unmarshal checkpoint: %w", err)
	}
	if probe.Version != runStateSnapshotVersion {
		return nil, fmt.Errorf("%w: got %d", errUnknownCheckpointVersion, probe.Version)
	}
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
		completions:   snap.Completions,
		catalogOf:     make(map[uuid.UUID]uuid.UUID),
		instancesOf:   make(map[uuid.UUID][]uuid.UUID),
		inGroupAdj:    make(map[uuid.UUID][]uuid.UUID),
		instanceOrder: make(map[uuid.UUID]int),
		maxParallel:   make(map[uuid.UUID]int),
		partitionKeys: make(map[uuid.UUID]string),
		failurePolicy: make(map[uuid.UUID]string),
		groupNames:    make(map[uuid.UUID]string),
		groupStarted:  make(map[uuid.UUID]time.Time),
		groupObserved: make(map[uuid.UUID]bool),
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
	if rs.completions == nil {
		rs.completions = make(map[uuid.UUID]completionRecord)
	}
	for _, id := range rs.ready {
		rs.inReady[id] = true
	}
	return rs, nil
}
