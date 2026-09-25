package model_test

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
)

// These cases are the C3 sentinel tests. The validator runs them against the
// candidate and against versioned bad patches in an isolated checkout. They
// deliberately use the shipped model and its oracles, not a second checker.

func oracleRun(t *testing.T, dag model.DAG) *model.Run {
	t.Helper()
	r, err := model.New("acknowledged-run", "job", dag)
	if err != nil {
		t.Fatalf("admit fixture: %v", err)
	}
	return r
}

func TestOracleRegressionLostAcknowledgedState(t *testing.T) {
	r := oracleRun(t, model.DAG{Order: []model.TaskID{"task"}})
	before := r.Checkpoint()
	after := r.Checkpoint()
	after.Ack = model.Ack{} // a public read lost the UUID/job mapping
	if err := model.CheckNoTerminalRegression(before, after); err == nil || !strings.Contains(err.Error(), "DT-ADMIT-01") {
		t.Fatalf("lost acknowledged identity escaped DT-ADMIT-01: %v", err)
	}
	if err := model.CheckNoTerminalRegression(before, r.Checkpoint()); err != nil {
		t.Fatalf("unchanged acknowledgement rejected: %v", err)
	}
}

func TestOracleRegressionCrossEpochAcknowledgedState(t *testing.T) {
	r := oracleRun(t, model.DAG{Order: []model.TaskID{"task"}})
	id := model.Step("task")
	if !r.Dispatch(id, "worker", 1000) {
		t.Fatal("fixture task was not dispatchable")
	}
	if got := r.Complete(id, model.StatusFailed, r.Generation()); !got.Applied {
		t.Fatalf("fixture failure did not apply: %+v", got)
	}
	before := r.Checkpoint()
	if !r.Retry() {
		t.Fatal("fixture retry was rejected")
	}
	after := r.Checkpoint()
	if before.Epoch == after.Epoch || before.Status[id] != model.StatusFailed || after.Status[id] != model.StatusPending {
		t.Fatalf("fixture did not reopen failed work across an epoch: before=%+v after=%+v", before, after)
	}
	if err := model.CheckNoTerminalRegression(before, after); err != nil {
		t.Fatalf("legal retry with unchanged acknowledgement rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		ack  model.Ack
	}{
		{"lost", model.Ack{}},
		{"changed run", model.Ack{RunID: "other-run", JobID: before.Ack.JobID}},
		{"changed job", model.Ack{RunID: before.Ack.RunID, JobID: "other-job"}},
	} {
		changed := after
		changed.Ack = tc.ack
		if err := model.CheckNoTerminalRegression(before, changed); err == nil || !strings.Contains(err.Error(), "DT-ADMIT-01") {
			t.Fatalf("%s: cross-epoch acknowledged identity change escaped DT-ADMIT-01: %v", tc.name, err)
		}
	}
}

func TestOracleRegressionRetryRetainsAcknowledgedState(t *testing.T) {
	r := oracleRun(t, model.DAG{Order: []model.TaskID{"kept", "retried"}})
	kept, retried := model.Step("kept"), model.Step("retried")
	for _, id := range []model.InstanceID{kept, retried} {
		if !r.Dispatch(id, "worker", 1000) {
			t.Fatalf("fixture task %s was not dispatchable", id)
		}
	}
	if got := r.Complete(kept, model.StatusSucceeded, r.Generation()); !got.Applied {
		t.Fatalf("fixture success did not apply: %+v", got)
	}
	if got := r.Complete(retried, model.StatusFailed, r.Generation()); !got.Applied {
		t.Fatalf("fixture failure did not apply: %+v", got)
	}
	before := r.Checkpoint()
	if !r.Retry() {
		t.Fatal("fixture retry was rejected")
	}
	after := r.Checkpoint()
	if before.Epoch == after.Epoch || after.Status[kept] != model.StatusSucceeded || after.Status[retried] != model.StatusPending {
		t.Fatalf("fixture did not retain success and reopen failure: before=%+v after=%+v", before, after)
	}
	if err := model.CheckNoTerminalRegression(before, after); err != nil {
		t.Fatalf("retry lost acknowledged identity: %v", err)
	}
	if after.Ack != before.Ack {
		t.Fatalf("retry lost acknowledged identity: before=%+v after=%+v", before.Ack, after.Ack)
	}
}

func TestOracleRegressionStaleGeneration(t *testing.T) {
	r := oracleRun(t, model.DAG{Order: []model.TaskID{"task"}})
	id := model.Step("task")
	if !r.Dispatch(id, "old-owner", 1000) {
		t.Fatal("fixture task was not dispatchable")
	}
	r.AdoptGeneration(2)
	before := r.Checkpoint()
	got := r.Complete(id, model.StatusSucceeded, 1)
	after := r.Checkpoint()
	if got.Applied || got.Refused != model.RefusalStaleGeneration || got.Durable() {
		t.Fatalf("old owner completion was accepted or persisted: %+v", got)
	}
	if err := model.CheckRefusalInert(before, after); err != nil {
		t.Fatalf("rejected completion mutated state: %v", err)
	}
	if status, _ := r.StatusOf(id); status != model.StatusRunning {
		t.Fatalf("old owner changed task to %s", status)
	}
}

// The real stale-operation test above checks the model's fence. This fixture
// checks that the refusal oracle rejects the state a bad fence would produce.
func staleRefusalSnapshot(t *testing.T) (*model.Run, model.Snapshot, model.InstanceID) {
	t.Helper()
	r := oracleRun(t, model.DAG{Order: []model.TaskID{"task"}})
	id := model.Step("task")
	if !r.Dispatch(id, "old-owner", 1000) {
		t.Fatal("fixture task was not dispatchable")
	}
	r.AdoptGeneration(2)
	before := r.Checkpoint()
	if before.Status[id] != model.StatusRunning {
		t.Fatalf("fixture has unexpected status %s", before.Status[id])
	}
	return r, before, id
}

func TestOracleRegressionAcceptedStaleGenerationStateDiff(t *testing.T) {
	r, before, id := staleRefusalSnapshot(t)
	after := r.Checkpoint()
	after.Status[id] = model.StatusSucceeded
	if err := model.CheckRefusalInert(before, after); err == nil || !strings.Contains(err.Error(), "DT-COMPLETE-01") {
		t.Fatalf("accepted stale completion escaped refusal checker: %v", err)
	}
	if err := model.CheckRefusalInert(before, r.Checkpoint()); err != nil {
		t.Fatalf("unchanged state rejected by refusal checker: %v", err)
	}
}

// A stale completion could also move only the durable cursor. Keep this
// independent of the outcome check so neither clause masks the other.
func TestOracleRegressionStaleGenerationSequenceDiff(t *testing.T) {
	r, before, _ := staleRefusalSnapshot(t)
	after := r.Checkpoint()
	after.SequenceHigh++
	if err := model.CheckRefusalInert(before, after); err == nil || !strings.Contains(err.Error(), "DT-COMPLETE-01") {
		t.Fatalf("persisted stale completion escaped refusal checker: %v", err)
	}
}

func TestOracleRegressionWholeGroupFanIn(t *testing.T) {
	dag := model.DAG{
		Order: []model.TaskID{"fanned", "join"},
		Edges: [][2]model.TaskID{{"fanned", "join"}},
		Fan: map[model.TaskID]model.FanOut{
			"fanned": {Partitions: []model.PartitionKey{"first", "second"}, Policy: model.Continue},
		},
	}
	r := oracleRun(t, dag)
	first, second, join := model.Instance("fanned", 0), model.Instance("fanned", 1), model.Step("join")
	if !r.Dispatch(first, "worker", 1000) || !r.Dispatch(second, "worker", 1000) {
		t.Fatal("both predecessor partitions must dispatch")
	}
	if got := r.Complete(first, model.StatusSucceeded, r.Generation()); !got.Applied {
		t.Fatalf("first partition completion: %+v", got)
	}
	if r.Dispatch(join, "worker", 1000) || containsInstance(r.Ready(), join) {
		t.Fatal("join started after only one member of its predecessor group completed")
	}
	if err := model.CheckSafety(r); err != nil {
		t.Fatalf("valid partial fan-in rejected: %v", err)
	}
	if got := r.Complete(second, model.StatusSucceeded, r.Generation()); !got.Applied {
		t.Fatalf("second partition completion: %+v", got)
	}
	if !containsInstance(r.Ready(), join) {
		t.Fatal("join did not become ready after the whole predecessor group completed")
	}
}

func containsInstance(ids []model.InstanceID, want model.InstanceID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestOracleRegressionDurableCompletionReplay(t *testing.T) {
	r := oracleRun(t, model.DAG{Order: []model.TaskID{"first", "second"}, Edges: [][2]model.TaskID{{"first", "second"}}})
	id := model.Step("first")
	if !r.Dispatch(id, "worker", 1000) {
		t.Fatal("first task was not dispatchable")
	}
	first := r.Complete(id, model.StatusSucceeded, r.Generation())
	if !first.Applied || !first.Durable() || first.Sequence == 0 {
		t.Fatalf("first completion did not persist: %+v", first)
	}
	before := r.Checkpoint()
	repeat := r.Complete(id, model.StatusSucceeded, r.Generation())
	if repeat.Applied || repeat.Refused != model.RefusalNone || !repeat.Durable() || repeat.Sequence != first.Sequence || len(repeat.Ready) != 0 {
		t.Fatalf("duplicate delivery failed to replay the original durable effect: first=%+v repeat=%+v", first, repeat)
	}
	if err := model.CheckRefusalInert(before, r.Checkpoint()); err != nil {
		t.Fatalf("duplicate delivery mutated state: %v", err)
	}
}

func TestOracleRegressionLegalDuplicateDelivery(t *testing.T) {
	store := model.NewEventStore()
	first := store.Append("acknowledged-run", "task.started", model.Step("task"))
	second := store.Append("acknowledged-run", "task.completed", model.Step("task"))
	sub := model.NewSubscriber(0)
	sub.Deliver(second) // live delivery can arrive out of order
	sub.Deliver(first)
	sub.Deliver(first) // at-least-once delivery can duplicate it
	if err := model.CheckDelivery(store, sub, 0); err != nil {
		t.Fatalf("legal duplicate/out-of-order delivery rejected: %v", err)
	}
}
