package model

import (
	"fmt"
	"sort"
)

// Skip reasons an owner decision records. They are operator-facing strings in
// the product; here they exist so an oracle can tell WHY a decision was made
// and a differential test can compare decision kinds rather than prose.
const (
	ReasonTriggerRule = "trigger rule not satisfied"
	ReasonFailFast    = "fan-out group failed fast"
	ReasonInGroup     = "fan-out dependency failed"
	ReasonCancelled   = "run cancelled"
)

// RunStatus is the modelled run-level state.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// IsTerminalRun reports whether a run status will not transition again within
// its current execution epoch.
func IsTerminalRun(s RunStatus) bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

// TerminalRecord is one durable terminal row: the identity that reached a
// terminal state, the outcome, the sequence stamped on it, and — for an owner
// decision — why.
//
// The journal of these records is the model's analogue of the task_runs
// terminal tail, and it is what recovery replays.
type TerminalRecord struct {
	Instance InstanceID
	Status   TaskStatus
	Sequence int64
	Reason   string
	Epoch    int
}

// Claim is the owner's bookkeeping for a dispatched instance.
type Claim struct {
	Node        string
	Attempt     int
	ExpiresAtMs int64
	// Started records that the work actually began (a container exists), which
	// is the single thing that decides whether cancellation can still resolve
	// it without stranding live work.
	Started bool
}

// RefusalKind classifies a rejected operation. A refusal must mutate nothing —
// that is DT-COMPLETE-01's safety half, and CheckRefusalInert verifies it.
type RefusalKind string

const (
	RefusalNone            RefusalKind = ""
	RefusalStaleGeneration RefusalKind = "stale_generation"
	RefusalUnknownInstance RefusalKind = "unknown_instance"
	RefusalRunTerminal     RefusalKind = "run_terminal"
	RefusalBadOutcome      RefusalKind = "bad_outcome"
)

// CompletionResult is what a modelled completion produced.
type CompletionResult struct {
	// Applied is false for a refusal AND for a repeat delivery of a completion
	// already applied. Refused reports which of the two it was.
	Applied  bool
	Refused  RefusalKind
	Sequence int64
	// Decided are the owner decisions this completion forced, in allocation
	// order: skips from trigger rules, fan-out failure policy and in-group
	// cascades. It does NOT include the completing instance itself.
	Decided []TerminalRecord
	// Ready are instances that newly became dispatchable.
	Ready    []InstanceID
	Complete bool
}

// Durable reports whether the result carries terminal rows a caller must
// persist. A repeat delivery is not Applied but IS Durable, and replays the
// first delivery's rows: persisting a zero sequence instead would write a row
// that recovery's strictly-greater replay filter can never see.
func (r CompletionResult) Durable() bool {
	return r.Sequence > 0 || len(r.Decided) > 0
}

// Ack is what a caller retains from a successful admission. A bare
// acknowledgement with no run identity is a different outcome and is not an
// Ack — DT-ADMIT-01.
type Ack struct {
	RunID string
	JobID string
}

// Run is the modelled lifecycle of one run.
//
// Readiness is recomputed declaratively from the outcome set rather than
// maintained as decremented predecessor counters. That is deliberate: it is the
// one design choice that makes this a second opinion instead of a second copy.
type Run struct {
	ack   Ack
	dag   DAG
	epoch int

	status  map[InstanceID]TaskStatus
	attempt map[InstanceID]int
	claim   map[InstanceID]Claim
	reason  map[InstanceID]string

	seq     int64
	seqOf   map[InstanceID]int64
	journal []TerminalRecord
	// densityFrom is the sequence below which this run's journal deliberately
	// holds no rows: a recovered run starts at its checkpoint's sequence_high,
	// and a retry drops the rows its reset invalidated. Sequences ABOVE it must
	// still be dense, which is the property recovery depends on.
	densityFrom int64
	// densityExempt marks a run reconstructed from a journal that was already
	// known to have holes, so the density clause would be re-reporting an input
	// defect as a state defect.
	densityExempt bool

	// replay records the durable effect of each completion so a repeat
	// delivery replays it rather than producing a fresh, zero-valued one.
	replay map[InstanceID]CompletionResult

	runStatus RunStatus
	// generation is the owner generation completions are fenced against.
	generation int64
}

// New admits a run: it validates the frozen DAG, seeds every identity pending,
// and resolves whatever the empty outcome set already decides (a step whose
// rule cannot be satisfied by a run that has not started — all_failed under no
// predecessors is satisfiable, so in practice this is a no-op for valid DAGs).
//
// Admission returns an Ack carrying the run identity. A caller that gets an
// error got no run, and must not treat the attempt as idempotently retryable:
// A1 records that a lost response is "possibly committed", which is a property
// of the transport, not something admission can promise away.
func New(runID, jobID string, dag DAG) (*Run, error) {
	if err := dag.Validate(); err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, fmt.Errorf("model: admission requires a run id")
	}
	r := &Run{
		ack:        Ack{RunID: runID, JobID: jobID},
		dag:        dag,
		epoch:      1,
		status:     map[InstanceID]TaskStatus{},
		attempt:    map[InstanceID]int{},
		claim:      map[InstanceID]Claim{},
		reason:     map[InstanceID]string{},
		seqOf:      map[InstanceID]int64{},
		replay:     map[InstanceID]CompletionResult{},
		runStatus:  RunRunning,
		generation: 1,
	}
	for _, id := range dag.AllInstances() {
		r.status[id] = StatusPending
		r.attempt[id] = 1
	}
	r.resolve()
	r.settle()
	return r, nil
}

// Acknowledged returns the run identity a successful admission handed back. It
// stays readable for the run's whole lifetime, including after cancellation and
// across a retry, and it never becomes a different job.
func (r *Run) Acknowledged() Ack { return r.ack }

// DAG returns the frozen run shape.
func (r *Run) DAG() DAG { return r.dag }

// Epoch is the current execution epoch: 1 for the original admission, one more
// for each retry. Terminal non-regression is scoped to an epoch because A1
// records across-retry fencing as unresolved.
func (r *Run) Epoch() int { return r.epoch }

// Generation is the owner generation completions are fenced against.
func (r *Run) Generation() int64 { return r.generation }

// AdoptGeneration records that a new owner took the run's lease over. It is the
// in-run half of DT-OWNER-01: a completion carrying the old generation is
// refused from here on.
func (r *Run) AdoptGeneration(generation int64) {
	if generation > r.generation {
		r.generation = generation
	}
}

// Status returns the run-level status.
func (r *Run) Status() RunStatus { return r.runStatus }

// Sequence returns the highest stamped terminal sequence.
func (r *Run) Sequence() int64 { return r.seq }

// StatusOf returns an identity's status, and false when it is not a node.
func (r *Run) StatusOf(id InstanceID) (TaskStatus, bool) {
	s, ok := r.status[id]
	return s, ok
}

// AttemptOf returns an identity's attempt counter.
func (r *Run) AttemptOf(id InstanceID) int { return r.attempt[id] }

// ClaimOf returns an identity's dispatch claim, if it holds one.
func (r *Run) ClaimOf(id InstanceID) (Claim, bool) {
	c, ok := r.claim[id]
	return c, ok
}

// ReasonOf returns the recorded reason for an owner-decided terminal state.
func (r *Run) ReasonOf(id InstanceID) string { return r.reason[id] }

// Journal returns the durable terminal rows in allocation order.
func (r *Run) Journal() []TerminalRecord {
	return append([]TerminalRecord(nil), r.journal...)
}

// IsComplete reports whether every identity has reached a terminal state.
func (r *Run) IsComplete() bool {
	for _, s := range r.status {
		if !Terminal(s) {
			return false
		}
	}
	return true
}

// Running returns the identities currently in flight, in dispatch order.
func (r *Run) Running() []InstanceID {
	var out []InstanceID
	for id, s := range r.status {
		if s == StatusRunning {
			out = append(out, id)
		}
	}
	r.dag.SortInstances(out)
	return out
}

// ReadyUncapped returns every dispatchable identity in dispatch order, ignoring
// fan-out parallelism caps.
func (r *Run) ReadyUncapped() []InstanceID {
	var out []InstanceID
	for id, s := range r.status {
		if s == StatusPending && r.dispatchable(id) {
			out = append(out, id)
		}
	}
	r.dag.SortInstances(out)
	return out
}

// Ready returns the dispatchable identities a scheduler may actually push now,
// with each fanned step's maxParallel applied against its in-flight instances.
func (r *Run) Ready() []InstanceID {
	inFlight := map[TaskID]int{}
	for id, s := range r.status {
		if s == StatusRunning {
			inFlight[id.Task]++
		}
	}
	var out []InstanceID
	for _, id := range r.ReadyUncapped() {
		if capn := r.dag.Fan[id.Task].MaxParallel; capn > 0 && inFlight[id.Task] >= capn {
			continue
		}
		out = append(out, id)
		inFlight[id.Task]++
	}
	return out
}

// Dispatch pushes a ready identity to a worker. It reports false when the
// identity was not dispatchable, which a scheduler must treat as a scheduling
// error rather than retrying blind.
func (r *Run) Dispatch(id InstanceID, node string, leaseExpiresAtMs int64) bool {
	if r.status[id] != StatusPending || !r.dispatchable(id) {
		return false
	}
	r.status[id] = StatusRunning
	r.claim[id] = Claim{Node: node, Attempt: r.attempt[id], ExpiresAtMs: leaseExpiresAtMs}
	return true
}

// MarkStarted records that a dispatched identity's work actually began. After
// this, cancellation can no longer resolve it without stranding live work.
func (r *Run) MarkStarted(id InstanceID) {
	c, ok := r.claim[id]
	if !ok || r.status[id] != StatusRunning {
		return
	}
	c.Started = true
	r.claim[id] = c
}

// ExpireClaim models an in-flight worker claim lapsing: the identity returns to
// pending with an incremented attempt, and its claim is cleared. Only work this
// run still believes is in flight is affected, so a lease that lapses under a
// completion that already landed is inert.
func (r *Run) ExpireClaim(id InstanceID, nowMs int64) bool {
	if IsTerminalRun(r.runStatus) {
		// The terminal guard that a real scheduler needs and that is easy to
		// omit: reaping a lapsed claim on a run that is already cancelled or
		// finished would move settled work back to pending and let a stale
		// container's later report walk it forward again. A terminal run is
		// nobody's to re-dispatch.
		return false
	}
	c, ok := r.claim[id]
	if !ok || r.status[id] != StatusRunning {
		return false
	}
	if c.ExpiresAtMs > 0 && c.ExpiresAtMs >= nowMs {
		return false
	}
	r.status[id] = StatusPending
	r.attempt[id]++
	delete(r.claim, id)
	return true
}

// AnyClaimOverdue reports whether some in-flight claim's lease has lapsed, or
// was never recorded. It is a NECESSARY condition for reaping, never a
// sufficient one: the owner's copy of a lease is never renewed, so it can call
// a healthy task overdue, but it can never call a lapsed one fresh.
func (r *Run) AnyClaimOverdue(nowMs int64) bool {
	for id, s := range r.status {
		if s != StatusRunning {
			continue
		}
		c, ok := r.claim[id]
		if !ok || c.ExpiresAtMs <= 0 || c.ExpiresAtMs < nowMs {
			return true
		}
	}
	return false
}

// Complete applies a worker-reported terminal outcome and advances the DAG.
//
// generation fences the call: a completion carrying a generation older than the
// run's current one is refused and mutates nothing (DT-COMPLETE-01). A repeat
// delivery of an already-applied completion is not a refusal — it replays the
// first delivery's durable rows so the caller re-persists the same effect.
func (r *Run) Complete(id InstanceID, outcome TaskStatus, generation int64) CompletionResult {
	if generation < r.generation {
		return CompletionResult{Refused: RefusalStaleGeneration, Complete: r.IsComplete()}
	}
	if _, known := r.status[id]; !known {
		return CompletionResult{Refused: RefusalUnknownInstance, Complete: r.IsComplete()}
	}
	if IsTerminalRun(r.runStatus) {
		// A cancelled or finished run does not accept new outcomes. The
		// in-flight container may still exist and still report; refusing here
		// is what stops it rewriting a terminal run.
		if prev, ok := r.replayOf(id); ok {
			return prev
		}
		return CompletionResult{Refused: RefusalRunTerminal, Complete: r.IsComplete()}
	}
	if Terminal(r.status[id]) {
		if prev, ok := r.replayOf(id); ok {
			return prev
		}
		return CompletionResult{Complete: r.IsComplete()}
	}
	if !Terminal(outcome) {
		return CompletionResult{Refused: RefusalBadOutcome, Complete: r.IsComplete()}
	}

	before := r.readySet()
	res := CompletionResult{Applied: true}
	res.Sequence = r.markTerminal(id, outcome, "")
	res.Decided = r.resolve()
	res.Ready = r.newlyReady(before)
	res.Complete = r.IsComplete()
	r.settle()
	r.replay[id] = res
	return res
}

// replayOf returns the durable effect a previous delivery of this completion
// had, shaped as a REPLAY rather than a fresh application: never Applied, and
// never carrying Ready.
//
// Ready is deliberately dropped. The ready queue is live scheduler state that
// the first delivery already handed over; replaying it would present work that
// is by now dispatched or finished as if it were fresh. The durable rows are
// replayed, because the caller's whole reason for re-delivering is that its
// first attempt to persist them may not have landed.
func (r *Run) replayOf(id InstanceID) (CompletionResult, bool) {
	prev, ok := r.replay[id]
	if !ok {
		return CompletionResult{}, false
	}
	prev.Applied = false
	prev.Ready = nil
	prev.Complete = r.IsComplete()
	return prev, true
}

// Cancel resolves the run: every identity that has not started becomes
// cancelled, and the run itself becomes cancelled.
//
// Work that HAS started is deliberately left running. The product cannot kill a
// container it did not start in-process, so claiming a terminal state for live
// work would let that work's own later report contradict the record. A 202 for
// a replacement run is therefore not an acknowledgement that the old external
// process is already dead — DT-CANCEL-01.
func (r *Run) Cancel() []TerminalRecord {
	if IsTerminalRun(r.runStatus) {
		return nil
	}
	ids := make([]InstanceID, 0, len(r.status))
	for id := range r.status {
		ids = append(ids, id)
	}
	r.dag.SortInstances(ids)

	var out []TerminalRecord
	for _, id := range ids {
		if !r.cancellableBeforeStart(id) {
			continue
		}
		seq := r.markTerminal(id, StatusCancelled, ReasonCancelled)
		out = append(out, TerminalRecord{
			Instance: id, Status: StatusCancelled, Sequence: seq,
			Reason: ReasonCancelled, Epoch: r.epoch,
		})
	}
	r.runStatus = RunCancelled
	return out
}

// Retry reopens the run under a new execution epoch: succeeded work is
// retained, every other terminal identity is reset to pending with attempt 1,
// and claims and owner decisions are cleared.
//
// The terminal-sequence cursor keeps counting rather than restarting, so a
// sequence identifies a transition uniquely for the run's whole life. Note what
// this does NOT promise: A1 records that the product supplies no durable
// run-execution epoch to its completion fence, so a coordinator holding a
// pre-retry view is not guaranteed to be rejected. The model therefore scopes
// terminal non-regression per epoch and makes no cross-epoch fencing claim.
func (r *Run) Retry() bool {
	if r.runStatus == RunRunning || r.runStatus == RunPending {
		return false
	}
	r.epoch++
	r.runStatus = RunRunning
	for id, s := range r.status {
		if Succeeded(s) {
			continue
		}
		r.status[id] = StatusPending
		r.attempt[id] = 1
		delete(r.claim, id)
		delete(r.reason, id)
		delete(r.seqOf, id)
		delete(r.replay, id)
	}
	// Drop the journal rows the reset invalidated. Leaving them would let a
	// recovery replay re-apply a previous epoch's outcome over work that is
	// pending again — the durable half of the same reset.
	retained := r.journal[:0]
	for _, rec := range r.journal {
		if Terminal(r.status[rec.Instance]) && r.seqOf[rec.Instance] == rec.Sequence {
			retained = append(retained, rec)
		}
	}
	r.journal = retained
	r.densityFrom = r.seq
	r.resolve()
	r.settle()
	return true
}

// markTerminal stamps the next sequence on an identity, records the outcome and
// appends the durable row. It returns 0 for an identity that is already
// terminal, which is what makes every terminal transition stamp exactly one
// sequence.
func (r *Run) markTerminal(id InstanceID, status TaskStatus, reason string) int64 {
	if s, ok := r.status[id]; !ok || Terminal(s) {
		return 0
	}
	r.seq++
	r.status[id] = status
	r.seqOf[id] = r.seq
	delete(r.claim, id)
	if reason != "" {
		r.reason[id] = reason
	}
	r.journal = append(r.journal, TerminalRecord{
		Instance: id, Status: status, Sequence: r.seq, Reason: reason, Epoch: r.epoch,
	})
	return r.seq
}

// resolve applies every owner decision the current outcome set forces, to a
// fixpoint, and returns the rows it produced in allocation order.
//
// This is the declarative counterpart of the product's incremental traversal:
// instead of walking edges out of a newly terminal task and decrementing
// counters, it asks of every pending identity "is this one's fate already
// decided?" and repeats until nothing changes. The two must agree, and
// internal/run/model_properties_test.go is where that is checked.
func (r *Run) resolve() []TerminalRecord {
	var out []TerminalRecord
	ids := make([]InstanceID, 0, len(r.status))
	for id := range r.status {
		ids = append(ids, id)
	}
	r.dag.SortInstances(ids)

	for {
		progressed := false
		for _, id := range ids {
			status, reason := r.decide(id)
			if status == "" {
				continue
			}
			seq := r.markTerminal(id, status, reason)
			if seq == 0 {
				continue
			}
			out = append(out, TerminalRecord{
				Instance: id, Status: status, Sequence: seq, Reason: reason, Epoch: r.epoch,
			})
			progressed = true
		}
		if !progressed {
			return out
		}
	}
}

// decide reports the terminal state an identity's fate is already sealed into,
// or "" when it is still pending on something or is dispatchable.
//
// The order of the three tests is the product's: fail_fast pre-empts the
// in-group cascade (it is a superset of it), and both pre-empt the ordinary
// trigger-rule evaluation.
func (r *Run) decide(id InstanceID) (TaskStatus, string) {
	s, known := r.status[id]
	if !known || Terminal(s) {
		return "", ""
	}

	if id.IsInstance() {
		if r.dag.Policy(id.Task) == FailFast && r.groupHasFailure(id.Task) {
			// fail_fast resolves every sibling that has not started. A sibling
			// that is genuinely running is left alone: its container cannot be
			// killed, so a terminal row for it would be a claim the worker can
			// contradict. fail_fast is a superset of the in-group cascade, so
			// the two are mutually exclusive.
			if r.cancellableBeforeStart(id) {
				return StatusSkipped, ReasonFailFast
			}
			return "", ""
		}
		// Under `continue`, a failed instance still resolves its transitive
		// in-group dependents: they wait on work that will never run. This is
		// not restricted to pending dependents, matching the product, though an
		// in-group dependent cannot in fact be in flight before its in-group
		// predecessor is terminal.
		if r.inGroupAncestorFailed(id) {
			return StatusSkipped, ReasonInGroup
		}
	}
	if s != StatusPending {
		return "", ""
	}
	if !r.gatingResolved(id) {
		return "", ""
	}
	if !r.dag.Rule(id.Task).Satisfied(r.predecessorStatuses(id)) {
		return StatusSkipped, ReasonTriggerRule
	}
	return "", ""
}

// dispatchable reports whether a pending identity may be pushed to a worker:
// every gate it waits on is resolved and its trigger rule is satisfied.
func (r *Run) dispatchable(id InstanceID) bool {
	if r.status[id] != StatusPending || IsTerminalRun(r.runStatus) {
		return false
	}
	if !r.gatingResolved(id) {
		return false
	}
	// Fan-out failure policy is deliberately NOT a readiness gate here, only a
	// decision in decide(). In a live run the distinction is invisible: the
	// failure that triggers the policy resolves every affected sibling in the
	// same breath, so none of them is ever pending-and-ready afterwards.
	//
	// It matters exactly once, and that case is the reason for the comment. A
	// sibling that was already executing when the group failed is left alone —
	// its container cannot be killed — so no terminal row is written for it. If
	// the owner then crashes, the recovering owner sees a failed instance and a
	// sibling with no row at all. Gating readiness on the policy would leave
	// that sibling neither dispatchable nor terminal: a stranded run, which is
	// a strictly worse outcome than re-running one partition of a failed group.
	return r.dag.Rule(id.Task).Satisfied(r.predecessorStatuses(id))
}

// gatingResolved reports whether everything an identity waits on has reached a
// terminal state: every cross-step predecessor GROUP (a fanned predecessor is
// one edge, not one per partition) and every in-group predecessor instance.
func (r *Run) gatingResolved(id InstanceID) bool {
	for _, pred := range r.dag.Pred(id.Task) {
		if !r.groupTerminal(pred) {
			return false
		}
	}
	for _, pred := range r.dag.InGroupPred(id) {
		if !Terminal(r.status[pred]) {
			return false
		}
	}
	return true
}

// predecessorStatuses collapses each cross-step predecessor to the single
// status the trigger rule is evaluated against. In-group predecessors are
// deliberately absent: they gate readiness and cascade failure, but they are
// not part of the step's fan-in rule.
func (r *Run) predecessorStatuses(id InstanceID) []TaskStatus {
	preds := r.dag.Pred(id.Task)
	out := make([]TaskStatus, 0, len(preds))
	for _, pred := range preds {
		out = append(out, r.groupStatus(pred))
	}
	return out
}

// groupStatus is a catalog step's status as its successors see it.
func (r *Run) groupStatus(t TaskID) TaskStatus {
	ids := r.dag.Instances(t)
	members := make([]TaskStatus, 0, len(ids))
	for _, id := range ids {
		members = append(members, r.status[id])
	}
	if len(ids) == 1 && !ids[0].IsInstance() {
		return members[0]
	}
	return GroupStatus(members)
}

func (r *Run) groupTerminal(t TaskID) bool {
	for _, id := range r.dag.Instances(t) {
		if !Terminal(r.status[id]) {
			return false
		}
	}
	return true
}

func (r *Run) groupHasFailure(t TaskID) bool {
	for _, id := range r.dag.Instances(t) {
		if r.status[id] == StatusFailed {
			return true
		}
	}
	return false
}

// inGroupAncestorFailed reports whether some in-group ancestor of an instance
// failed. The product cascades transitively from the failed instance for
// exactly this reason: stopping at direct dependents leaves a grandchild with a
// satisfied cross-step rule and an in-group prerequisite that never ran.
func (r *Run) inGroupAncestorFailed(id InstanceID) bool {
	seen := map[InstanceID]bool{id: true}
	queue := append([]InstanceID(nil), r.dag.InGroupPred(id)...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if r.status[cur] == StatusFailed {
			return true
		}
		queue = append(queue, r.dag.InGroupPred(cur)...)
	}
	return false
}

// cancellableBeforeStart reports whether an identity can still be resolved
// terminal without stranding live work: it is pending, or it was dispatched and
// its worker has not begun executing it.
func (r *Run) cancellableBeforeStart(id InstanceID) bool {
	switch r.status[id] {
	case StatusPending:
		return true
	case StatusRunning:
		c, ok := r.claim[id]
		return !ok || !c.Started
	default:
		return false
	}
}

// settle folds the instance outcomes up into the run status once every identity
// is terminal.
func (r *Run) settle() {
	if IsTerminalRun(r.runStatus) || !r.IsComplete() {
		return
	}
	for _, s := range r.status {
		if s == StatusFailed {
			r.runStatus = RunFailed
			return
		}
	}
	for _, s := range r.status {
		if s == StatusCancelled {
			r.runStatus = RunCancelled
			return
		}
	}
	r.runStatus = RunSucceeded
}

func (r *Run) readySet() map[InstanceID]bool {
	out := map[InstanceID]bool{}
	for _, id := range r.ReadyUncapped() {
		out[id] = true
	}
	return out
}

func (r *Run) newlyReady(before map[InstanceID]bool) []InstanceID {
	var out []InstanceID
	for _, id := range r.ReadyUncapped() {
		if !before[id] {
			out = append(out, id)
		}
	}
	return out
}

// Snapshot is the model's checkpoint: the mutable state, without the DAG (which
// is frozen and reloaded from the catalog on recovery).
type Snapshot struct {
	SequenceHigh int64
	Epoch        int
	Generation   int64
	Ack          Ack
	RunStatus    RunStatus
	Status       map[InstanceID]TaskStatus
	Attempt      map[InstanceID]int
	Claim        map[InstanceID]Claim
	Reason       map[InstanceID]string
	SeqOf        map[InstanceID]int64
}

// Checkpoint captures the current state.
func (r *Run) Checkpoint() Snapshot {
	snap := Snapshot{
		SequenceHigh: r.seq,
		Epoch:        r.epoch,
		Generation:   r.generation,
		Ack:          r.ack,
		RunStatus:    r.runStatus,
		Status:       map[InstanceID]TaskStatus{},
		Attempt:      map[InstanceID]int{},
		Claim:        map[InstanceID]Claim{},
		Reason:       map[InstanceID]string{},
		SeqOf:        map[InstanceID]int64{},
	}
	for k, v := range r.status {
		snap.Status[k] = v
	}
	for k, v := range r.attempt {
		snap.Attempt[k] = v
	}
	for k, v := range r.claim {
		snap.Claim[k] = v
	}
	for k, v := range r.reason {
		snap.Reason[k] = v
	}
	for k, v := range r.seqOf {
		snap.SeqOf[k] = v
	}
	return snap
}

// TailSince returns the durable terminal rows after a sequence, ascending — the
// model's analogue of the post-checkpoint terminal tail query, including its
// strictly-greater predicate.
func (r *Run) TailSince(sequenceHigh int64) []TerminalRecord {
	var out []TerminalRecord
	for _, rec := range r.journal {
		if rec.Sequence > sequenceHigh {
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// RecoveryResult summarizes what a recovering owner must do.
type RecoveryResult struct {
	// Ready are pending identities whose gates are satisfied.
	Ready []InstanceID
	// ReDispatch are identities the previous owner left in flight with no
	// terminal row: their worker outcome was lost.
	ReDispatch []InstanceID
	// MaxSequence is the highest sequence observed.
	MaxSequence int64
	// SequenceGaps are sequences missing between the checkpoint and the highest
	// observed row: allocated by the dead owner but never persisted. Reported
	// for observability; the affected work is recovered by re-dispatch.
	SequenceGaps []int64
	Complete     bool
}

// Recover reconstructs a run from a checkpoint plus the post-checkpoint
// terminal tail, then classifies the leftovers. A nil snapshot replays from
// scratch, which is only correct when the tail is the COMPLETE journal — the
// same trap the product documents: dropping a rejected checkpoint without also
// dropping the sequence filter silently loses every transition below it.
func Recover(dag DAG, ack Ack, snap *Snapshot, tail []TerminalRecord) (*Run, RecoveryResult, error) {
	r, err := New(ack.RunID, ack.JobID, dag)
	if err != nil {
		return nil, RecoveryResult{}, err
	}
	var start int64
	if snap != nil {
		start = snap.SequenceHigh
		r.restore(*snap)
	}

	var res RecoveryResult
	expected := start + 1
	for _, rec := range tail {
		r.applyTerminalRow(rec)
		for expected < rec.Sequence {
			res.SequenceGaps = append(res.SequenceGaps, expected)
			expected++
		}
		if rec.Sequence >= expected {
			expected = rec.Sequence + 1
		}
	}

	res.MaxSequence = r.seq
	res.ReDispatch = r.requeueRunning()
	res.Ready = r.Ready()
	res.Complete = r.IsComplete()
	r.densityExempt = len(res.SequenceGaps) > 0
	r.settle()
	return r, res, nil
}

func (r *Run) restore(snap Snapshot) {
	r.seq = snap.SequenceHigh
	r.epoch = snap.Epoch
	r.generation = snap.Generation
	r.ack = snap.Ack
	r.runStatus = snap.RunStatus
	r.status = map[InstanceID]TaskStatus{}
	r.attempt = map[InstanceID]int{}
	r.claim = map[InstanceID]Claim{}
	r.reason = map[InstanceID]string{}
	r.seqOf = map[InstanceID]int64{}
	r.journal = nil
	r.densityFrom = snap.SequenceHigh
	r.replay = map[InstanceID]CompletionResult{}
	for k, v := range snap.Status {
		r.status[k] = v
	}
	for k, v := range snap.Attempt {
		r.attempt[k] = v
	}
	for k, v := range snap.Claim {
		r.claim[k] = v
	}
	for k, v := range snap.Reason {
		r.reason[k] = v
	}
	for k, v := range snap.SeqOf {
		r.seqOf[k] = v
	}
}

// applyTerminalRow replays one persisted terminal row. Unlike a live
// completion it adopts the row's stored sequence rather than stamping a new
// one, and it derives NO new owner decisions: every decision was itself
// persisted as a row and arrives in sequence order, so re-deriving them here
// would double-handle them.
func (r *Run) applyTerminalRow(rec TerminalRecord) {
	if rec.Sequence > r.seq {
		r.seq = rec.Sequence
	}
	if _, known := r.status[rec.Instance]; !known {
		return
	}
	if Terminal(r.status[rec.Instance]) {
		return
	}
	r.status[rec.Instance] = rec.Status
	r.seqOf[rec.Instance] = rec.Sequence
	delete(r.claim, rec.Instance)
	if rec.Reason != "" {
		r.reason[rec.Instance] = rec.Reason
	}
	r.journal = append(r.journal, rec)
}

// requeueRunning moves every identity the previous owner left in flight back to
// pending with an incremented attempt, in dispatch order.
func (r *Run) requeueRunning() []InstanceID {
	out := r.Running()
	for _, id := range out {
		r.status[id] = StatusPending
		r.attempt[id]++
		delete(r.claim, id)
	}
	return out
}
