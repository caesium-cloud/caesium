package model_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
)

// The retained corpus.
//
// Every test below is a counterexample Rapid found and minimized while this
// model was being written, transcribed into a deterministic scenario. Each one
// failed before the fix named in its comment and passes after it.
//
// This file is the durable half of the retention workflow described in doc.go.
// Rapid's own .fail artifacts are committed only while a defect is open,
// because they are opaque bitstreams tied to the exact draw sequence a property
// made: the first change to a generator turns one into a "no longer valid" log
// line that executes nothing. A minimized case is only worth keeping in a form
// that still runs after the code around it moves, and that still says what the
// defect was.
//
// When a property fails: keep Rapid's .fail file while you investigate, fix the
// defect, add the minimized case here, then delete the artifact.

// TestRegressionRepeatCompletionOnCancelledRun.
//
// Found by TestRunLifecycleProperties. A worker re-delivered a completion after
// the run had been cancelled. The replay path returned the FIRST delivery's
// result verbatim, including Applied=true, so the caller was told a completion
// it had already processed had just advanced the DAG again.
//
// Why it matters: re-delivery is the normal response to the owner answering a
// completion with a retryable error, so this is a routine path, not an edge.
// A caller that trusts Applied would double-count the transition and, on the
// product's side, re-emit its events.
func TestRegressionRepeatCompletionOnCancelledRun(t *testing.T) {
	dag := model.DAG{Order: []model.TaskID{"s0", "s1"}}
	run, err := model.New("run-1", "job-1", dag)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	s0 := model.Step("s0")
	if !run.Dispatch(s0, "node-a", 60_000) {
		t.Fatal("s0 should be dispatchable")
	}
	first := run.Complete(s0, model.StatusSucceeded, run.Generation())
	if !first.Applied || first.Sequence != 1 {
		t.Fatalf("first delivery: %+v", first)
	}
	run.Cancel()

	before := run.Checkpoint()
	repeat := run.Complete(s0, model.StatusSucceeded, run.Generation())
	after := run.Checkpoint()

	if repeat.Applied {
		t.Fatalf("a repeat completion on a cancelled run reported Applied")
	}
	if repeat.Sequence != first.Sequence {
		t.Fatalf("repeat replayed sequence %d, first stamped %d", repeat.Sequence, first.Sequence)
	}
	if len(repeat.Ready) != 0 {
		t.Fatalf("repeat replayed live ready work %v", repeat.Ready)
	}
	if err := model.CheckRefusalInert(before, after); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestRegressionReapedClaimCannotResurrectCancelledWork.
//
// Found by TestRunLifecycleProperties. A worker claim lapsed on a run that had
// already been cancelled, and the reaper moved the task back to pending — so a
// settled run grew a dispatchable task again, and the stale container's later
// report could walk it forward a second time.
//
// This is the missing-terminal-guard shape, and it is worth a named regression
// because it is invisible in any test that cancels and then stops: the
// resurrection needs a reap AFTER the cancellation to show up at all.
func TestRegressionReapedClaimCannotResurrectCancelledWork(t *testing.T) {
	dag := model.DAG{Order: []model.TaskID{"s0"}}
	run, err := model.New("run-1", "job-1", dag)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	s0 := model.Step("s0")
	if !run.Dispatch(s0, "node-a", 1_000) {
		t.Fatal("s0 should be dispatchable")
	}
	run.MarkStarted(s0) // started work survives cancellation
	run.Cancel()

	if run.ExpireClaim(s0, 10_000) {
		t.Fatal("reaping a lapsed claim on a cancelled run must be a no-op")
	}
	if got, _ := run.StatusOf(s0); got == model.StatusPending {
		t.Fatal("a cancelled run grew a pending task again")
	}
	if len(run.Ready()) != 0 {
		t.Fatalf("a cancelled run reports dispatchable work: %v", run.Ready())
	}
}

// TestRegressionFailFastSurvivorIsNotStrandedAfterRecovery.
//
// Found by TestCheckpointReplayEquivalence. A fan-out group with fail_fast had
// one instance fail while a sibling was already executing. The sibling could
// not be cancelled — its container was live — so no terminal row was ever
// written for it. After an owner crash, the recovering owner saw a failed
// instance and a sibling with no row, and treating the failure policy as a
// readiness gate left that sibling neither terminal nor dispatchable: a run
// that hangs forever and surfaces as a harness timeout.
//
// The resolution is recorded in Run.dispatchable: the failure policy is a
// DECISION, not a gate. Re-running one partition of a failed group is a worse
// outcome than a correct one; a stranded run is a worse outcome than both.
func TestRegressionFailFastSurvivorIsNotStrandedAfterRecovery(t *testing.T) {
	dag := model.DAG{
		Order: []model.TaskID{"fanned"},
		Fan: map[model.TaskID]model.FanOut{
			"fanned": {Partitions: []model.PartitionKey{"p0", "p1"}, Policy: model.FailFast},
		},
	}
	run, err := model.New("run-1", "job-1", dag)
	if err != nil {
		t.Fatalf("admission: %v", err)
	}

	p0, p1 := model.Instance("fanned", 0), model.Instance("fanned", 1)
	fresh := run.Checkpoint() // the owner's last checkpoint: nothing dispatched yet

	if !run.Dispatch(p0, "node-a", 60_000) || !run.Dispatch(p1, "node-a", 60_000) {
		t.Fatal("both partitions should be dispatchable")
	}
	run.MarkStarted(p1) // p1 is live and cannot be cancelled
	run.Complete(p0, model.StatusFailed, run.Generation())

	if got, _ := run.StatusOf(p1); got != model.StatusRunning {
		t.Fatalf("fail_fast resolved started work: p1 is %s", got)
	}

	// The owner now crashes. Recovery has the pre-dispatch checkpoint and the
	// one terminal row p0 produced.
	tail := run.TailSince(fresh.SequenceHigh)
	recovered, res, err := model.Recover(dag, run.Acknowledged(), &fresh, tail)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if err := model.CheckLiveness(recovered); err != nil {
		t.Fatalf("%v", err)
	}
	dispatchable := false
	for _, id := range recovered.ReadyUncapped() {
		if id == p1 {
			dispatchable = true
		}
	}
	if got, _ := recovered.StatusOf(p1); !dispatchable && !model.Terminal(got) {
		t.Fatalf("p1 recovered as %s and is not dispatchable: ready=%v redispatch=%v",
			got, res.Ready, res.ReDispatch)
	}
}

// TestRegressionDeliveryCursorDoesNotStepOverALostEvent.
//
// Found by TestEventDeliveryProperties. A subscriber advanced its resume cursor
// to the highest sequence it had seen rather than to the end of the unbroken
// run it had received. A buffer overflow then left a hole below the cursor, and
// the reconnect fetched only events ABOVE it — so the lost event was never
// redelivered and at-least-once delivery quietly became at-most-once.
func TestRegressionDeliveryCursorDoesNotStepOverALostEvent(t *testing.T) {
	store := model.NewEventStore()
	for range 3 {
		store.Append("run-1", "task.completed", model.Step("s0"))
	}

	sub := model.NewSubscriber(0)
	sub.CatchUp(store)
	if sub.Cursor != 3 {
		t.Fatalf("cursor after a clean catch-up is %d, want 3", sub.Cursor)
	}

	sub.Drop(2) // events 2 and 3 are lost from the subscriber's buffer
	if sub.Cursor != 1 {
		t.Fatalf("cursor after losing events 2 and 3 is %d, want 1", sub.Cursor)
	}

	sub.CatchUp(store)
	if err := model.CheckDelivery(store, sub, 0); err != nil {
		t.Fatalf("a reconnect did not repair the loss: %v", err)
	}
}
