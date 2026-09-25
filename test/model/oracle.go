package model

import (
	"fmt"
)

// CheckSafety is the per-run safety oracle. Every clause here corresponds to a
// resolved A1 contract; none of them asserts a guarantee A1 marks unresolved.
//
// It is written to be run after EVERY operation in a generated sequence, not
// once at the end: an invariant that only holds in final states is not an
// invariant, and the interesting violations (a step dispatched before its
// fan-in is satisfied, a group over its parallelism cap) are transient.
func CheckSafety(r *Run) error {
	if r.ack.RunID == "" {
		return fmt.Errorf("safety: acknowledged run identity was lost (DT-ADMIT-01)")
	}

	// DT-DAG-01: nothing is dispatchable whose gates are unresolved or whose
	// trigger rule is unsatisfied. This is the clause that fails when a fan-in
	// is satisfied from one member of a predecessor group instead of all of it.
	for _, id := range r.ReadyUncapped() {
		if !r.gatingResolved(id) {
			return fmt.Errorf("safety: %s is dispatchable with unresolved predecessors (DT-DAG-01)", id)
		}
		if !r.dag.Rule(id.Task).Satisfied(r.predecessorStatuses(id)) {
			return fmt.Errorf("safety: %s is dispatchable with an unsatisfied %s rule (DT-DAG-01)",
				id, r.dag.Rule(id.Task))
		}
	}

	// Fan-out parallelism: in-flight plus newly dispatchable instances of one
	// group must never exceed its cap.
	inFlight := map[TaskID]int{}
	for id, s := range r.status {
		if s == StatusRunning {
			inFlight[id.Task]++
		}
	}
	for t, fan := range r.dag.Fan {
		if fan.MaxParallel > 0 && inFlight[t] > fan.MaxParallel {
			return fmt.Errorf("safety: %s has %d instances in flight over a cap of %d",
				t, inFlight[t], fan.MaxParallel)
		}
	}
	for _, id := range r.Ready() {
		inFlight[id.Task]++
	}
	for t, fan := range r.dag.Fan {
		if fan.MaxParallel > 0 && inFlight[t] > fan.MaxParallel {
			return fmt.Errorf("safety: dispatching every ready instance would put %s at %d over a cap of %d",
				t, inFlight[t], fan.MaxParallel)
		}
	}

	// Terminal sequences: unique, dense from 1, and agreeing with the journal.
	if err := CheckSequenceDensity(r); err != nil {
		return err
	}

	// Owner decisions carry a reason; worker outcomes do not need one.
	for id, s := range r.status {
		if s == StatusSkipped && r.reason[id] == "" {
			return fmt.Errorf("safety: %s was skipped with no recorded reason", id)
		}
		if Terminal(s) && r.seqOf[id] == 0 {
			return fmt.Errorf("safety: %s is terminal with no stamped sequence", id)
		}
		if Terminal(s) {
			if _, held := r.claim[id]; held {
				return fmt.Errorf("safety: %s is terminal but still holds a dispatch claim", id)
			}
		}
	}

	// Run status agrees with the instance outcomes once everything is terminal.
	if r.IsComplete() && r.runStatus == RunRunning {
		return fmt.Errorf("safety: every task is terminal but the run is still running")
	}
	return nil
}

// CheckSequenceDensity checks the property the whole recovery design rests on:
// terminal sequences are unique, strictly increasing in allocation order, and
// dense from 1.
//
// Density is not cosmetic. Recovery reads the terminal tail with a strictly
// greater-than predicate, so a hole means a sequence was allocated but its row
// never landed, and the affected work must be re-dispatched rather than assumed
// finished. A model that allocated sparse sequences would hide that.
func CheckSequenceDensity(r *Run) error {
	seen := map[int64]InstanceID{}
	var last int64
	for _, rec := range r.journal {
		if rec.Sequence <= 0 {
			return fmt.Errorf("safety: journal row for %s carries sequence %d", rec.Instance, rec.Sequence)
		}
		if prev, dup := seen[rec.Sequence]; dup {
			return fmt.Errorf("safety: sequence %d stamped on both %s and %s", rec.Sequence, prev, rec.Instance)
		}
		seen[rec.Sequence] = rec.Instance
		if rec.Sequence <= last {
			return fmt.Errorf("safety: journal is not strictly increasing at %s (%d after %d)",
				rec.Instance, rec.Sequence, last)
		}
		last = rec.Sequence
	}
	if r.densityExempt {
		// The journal was reconstructed from a tail already known to have
		// holes; reporting them again here would blame the state for an input
		// defect that RecoveryResult.SequenceGaps already named.
		return nil
	}
	for i := r.densityFrom + 1; i <= last; i++ {
		if _, ok := seen[i]; !ok {
			return fmt.Errorf("safety: terminal sequence %d is missing from a journal covering (%d, %d]",
				i, r.densityFrom, last)
		}
	}
	return nil
}

// CheckLiveness is the progress oracle: a run that is neither complete nor
// cancelled must have somewhere to go — something in flight, or something
// dispatchable.
//
// This is the clause that catches the stranded-consumer class of defect, where
// a fan-in downstream of a failed predecessor is never dispatched and never
// skipped, so the run hangs until a harness timeout calls it a flake. It is a
// statement about the decision function only. It does not promise that a real
// run terminates: that needs a live worker, capacity, a reachable runtime and a
// quorum, none of which a pure model can supply.
func CheckLiveness(r *Run) error {
	if r.IsComplete() || IsTerminalRun(r.runStatus) {
		return nil
	}
	if len(r.Running()) > 0 || len(r.Ready()) > 0 {
		return nil
	}
	var stuck []string
	for _, id := range r.dag.AllInstances() {
		if !Terminal(r.status[id]) {
			stuck = append(stuck, fmt.Sprintf("%s=%s", id, r.status[id]))
		}
	}
	return fmt.Errorf("liveness: run has nothing in flight and nothing dispatchable, stranded: %v", stuck)
}

// CheckRefusalInert verifies DT-COMPLETE-01's safety half: a refused operation
// changed nothing observable. Callers snapshot before the refused call and pass
// both snapshots.
func CheckRefusalInert(before, after Snapshot) error {
	if before.SequenceHigh != after.SequenceHigh {
		return fmt.Errorf("safety: a refused operation moved the sequence cursor %d -> %d (DT-COMPLETE-01)",
			before.SequenceHigh, after.SequenceHigh)
	}
	if before.RunStatus != after.RunStatus {
		return fmt.Errorf("safety: a refused operation changed the run status %s -> %s (DT-COMPLETE-01)",
			before.RunStatus, after.RunStatus)
	}
	if len(before.Status) != len(after.Status) {
		return fmt.Errorf("safety: a refused operation changed the instance set (DT-COMPLETE-01)")
	}
	for id, s := range before.Status {
		if after.Status[id] != s {
			return fmt.Errorf("safety: a refused operation moved %s from %s to %s (DT-COMPLETE-01)",
				id, s, after.Status[id])
		}
	}
	for id, a := range before.Attempt {
		if after.Attempt[id] != a {
			return fmt.Errorf("safety: a refused operation changed %s's attempt %d -> %d (DT-COMPLETE-01)",
				id, a, after.Attempt[id])
		}
	}
	return nil
}

// CheckNoTerminalRegression verifies DT-ADMIT-01 across every observation of
// an acknowledged run, including retries. It also verifies DT-TERMINAL-01
// WITHIN one execution epoch: a terminal identity keeps the same outcome.
//
// Scoping to an epoch is not a convenience. A1 records that the product has no
// durable run-execution epoch on its completion fence, so a retry legitimately
// moves a failed task back to pending. Only terminal-status non-regression is
// epoch-scoped; an acknowledged run UUID and job identity remain durable.
func CheckNoTerminalRegression(before, after Snapshot) error {
	if after.Ack != before.Ack {
		return fmt.Errorf("safety: the acknowledged run identity changed from %+v to %+v (DT-ADMIT-01)",
			before.Ack, after.Ack)
	}
	if before.Epoch != after.Epoch {
		return nil
	}
	for id, s := range before.Status {
		if !Terminal(s) {
			continue
		}
		got, ok := after.Status[id]
		if !ok {
			return fmt.Errorf("safety: terminal %s disappeared from the run (DT-TERMINAL-01)", id)
		}
		if got != s {
			return fmt.Errorf("safety: %s regressed from terminal %s to %s (DT-TERMINAL-01)", id, s, got)
		}
	}
	return nil
}
