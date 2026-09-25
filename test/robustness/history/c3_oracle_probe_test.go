// C3 tests the real B3 history and raw-effect checkers as an ordinary package
// test, preserving the independent reference model's import boundary.
package history

import "testing"

func c3Scope() Scope     { return Scope{Store: "c3", Min: 1, Max: 20, Complete: true} }
func c3Open() Connection { return Connection{Established: true, Status: 200} }

func TestC3MissingReplay(t *testing.T) {
	persisted := []Persisted{
		{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "first"},
		{Sequence: 9, Type: "task_succeeded", RunID: "r", TaskID: "second"},
	}
	first := []Delivered{{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "first"}}
	// The resumed connection was healthy and delivered something, but omitted
	// the durable row above Last-Event-ID. A live-stream drop would be legal;
	// a missing durable catch-up row is a replay defect.
	resumed := []Delivered{{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "first"}}
	rep := CompareReconnect(first, resumed, persisted, 5, c3Scope(), c3Open())
	if !rep.Conclusive() || len(rep.Defects()) == 0 || len(rep.MissingFromResumed) != 1 || rep.MissingFromResumed[0] != 9 {
		t.Fatalf("missing replay escaped checker: %+v", rep)
	}
}

func TestC3UnaccountedExternalEffect(t *testing.T) {
	// Aggregate counts tie (two raw completions, two persisted events), but
	// the external process for "second" never completed. The independent raw
	// ledger must prevent "first"'s extra effect from hiding that phantom.
	effects := []Effect{
		{RunID: "r", Step: "first", Nonce: "a", Kind: "start"},
		{RunID: "r", Step: "first", Nonce: "a", Kind: "complete"},
		{RunID: "r", Step: "first", Nonce: "b", Kind: "complete"},
	}
	persisted := []Persisted{
		{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t1"},
		{Sequence: 9, Type: "task_succeeded", RunID: "r", TaskID: "t2"},
	}
	rep := CorrelateEffects("r", effects, nil, persisted, []string{"task_succeeded"}, map[string]string{"t1": "first", "t2": "second"})
	if !rep.Conclusive() || rep.RawCompletions != rep.PersistedCompletionEvents || rep.PerStep["second"].PhantomCompletions != 1 || len(rep.Defects()) == 0 {
		t.Fatalf("unaccounted completion escaped per-step effect checker: %+v", rep)
	}
}

func TestC3LegalDuplicateDelivery(t *testing.T) {
	persisted := []Persisted{
		{Sequence: 3, Type: "task_started", RunID: "r"},
		{Sequence: 17, Type: "task_succeeded", RunID: "r"},
	}
	delivered := []Delivered{
		{Sequence: 17, Type: "task_succeeded", RunID: "r"},
		{Sequence: 3, Type: "task_started", RunID: "r"},
		{Sequence: 3, Type: "task_started", RunID: "r"},
	}
	rep := Compare(delivered, persisted, c3Scope(), c3Open())
	if !rep.Conclusive() || len(rep.Defects()) != 0 || rep.Duplicates[3] != 2 || rep.OutOfOrder != 1 {
		t.Fatalf("legal duplicate/sparse/out-of-order delivery rejected: %+v", rep)
	}
}

func TestC3MissingEvidenceFailsClosed(t *testing.T) {
	rep := Compare(nil, nil, Scope{Store: "c3", Min: 1, Max: 20, Complete: false}, Connection{})
	if rep.Conclusive() {
		t.Fatalf("empty, incomplete histories became conclusive: %+v", rep)
	}
	effects := CorrelateEffects("r", nil, nil, []Persisted{{Sequence: 5, Type: "task_succeeded", RunID: "r", TaskID: "t1"}}, []string{"task_succeeded"}, map[string]string{"t1": "first"})
	if effects.Conclusive() {
		t.Fatalf("absent external ledger became conclusive: %+v", effects)
	}
}
