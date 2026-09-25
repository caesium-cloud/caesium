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
