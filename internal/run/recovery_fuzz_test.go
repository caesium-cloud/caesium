package run

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// This file fuzzes the byte-encoding dimension of run-owner recovery:
// ValidateCheckpointBlob, Restore, and RecoverRunState (internal/run/owner_state.go,
// internal/run/recovery.go). C1's rapid-driven properties in
// recovery_properties_test.go already explore generated EXECUTION TRACES —
// realistic sequences of dispatch/complete/checkpoint over generated DAGs —
// exhaustively; a structured generator like that only ever produces
// well-formed checkpoint blobs (real output of RunState.Snapshot) and
// well-formed terminal-row sequences. It does not naturally reach a checkpoint
// torn by a partial write, a JSON document with a huge or negative sequence
// number, duplicate map keys, or a terminal-row list a corrupted replay could
// produce. That is the gap these fuzz targets are for.
//
// Both targets are hermetic: no DB, no cluster, no checkpoint table, and no
// build tag, exactly like recovery_properties_test.go — they run as ordinary
// seed-only-or-fuzzed regressions inside `just unit-test`.

// A small, fixed topology (A -> B -> C) used only to give the recovery
// functions something to reconstruct against. The DAG shape is not the
// subject of these fuzz targets (C1 already covers that with generated
// topologies); a small fixed one keeps every test case's fixture identical so
// failures are attributable to the encoded bytes alone.
var (
	fuzzTaskA = uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	fuzzTaskB = uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	fuzzTaskC = uuid.MustParse("00000000-0000-0000-0000-00000000000c")
)

func fuzzTopology() RunTopology {
	return RunTopology{
		Adjacency: map[uuid.UUID][]uuid.UUID{
			fuzzTaskA: {fuzzTaskB},
			fuzzTaskB: {fuzzTaskC},
		},
		Predecessors: map[uuid.UUID][]uuid.UUID{
			fuzzTaskB: {fuzzTaskA},
			fuzzTaskC: {fuzzTaskB},
		},
		TriggerRule: map[uuid.UUID]string{},
		Order: map[uuid.UUID]int{
			fuzzTaskA: 0,
			fuzzTaskB: 1,
			fuzzTaskC: 2,
		},
	}
}

// validCheckpointSeeds produces real Snapshot() blobs at several points of a
// real (hermetic, in-memory) execution over fuzzTopology — fresh, mid-run with
// one dispatch, and mid-run with a completion recorded — so the corpus starts
// from bytes the product itself would actually write, not just hand-typed
// JSON.
func validCheckpointSeeds() [][]byte {
	var out [][]byte
	topo := fuzzTopology()

	rs := NewRunState(topo, 0)
	blob, err := rs.Snapshot()
	if err != nil {
		panic(err)
	}
	out = append(out, blob)

	rs.MarkDispatched(fuzzTaskA, "node-a", 1, 60_000)
	blob, err = rs.Snapshot()
	if err != nil {
		panic(err)
	}
	out = append(out, blob)

	rs.ApplyCompletion(fuzzTaskA, TaskStatusSucceeded, nil)
	blob, err = rs.Snapshot()
	if err != nil {
		panic(err)
	}
	out = append(out, blob)

	return out
}

// FuzzValidateCheckpointBlob fuzzes the checkpoint decoder every recovering
// owner runs against bytes a previous owner's process wrote — including a
// torn write, since the checkpoint column is a plain blob with no atomicity
// guarantee stronger than the row's own transaction.
//
// Properties (never "did not panic" alone):
//
//  1. ValidateCheckpointBlob and Restore never disagree. owner_state.go's own
//     doc comment on ValidateCheckpointBlob depends on this: a recovering
//     owner validates BEFORE it drops the sequence filter on its terminal-tail
//     query, specifically so a checkpoint Restore will reject is never treated
//     as good. TestCheckpointValidationAgreesWithRestore already pins this
//     against Rapid-generated semantic mutations (truncation, corruption,
//     version skew); here it is checked against a JSON-grammar fuzzer's raw
//     byte mutations, which reach a different part of the input space (odd
//     Unicode, huge/negative numbers, duplicate keys, deep nesting).
//  2. RecoverRunState never errors and never panics on a corrupted checkpoint:
//     it is documented to fall back to a from-scratch replay instead, and a
//     rejected checkpoint's fallback must reconstruct EXACTLY what a genuine
//     from-scratch replay (checkpoint=nil) over the same terminal rows would
//     — the corrupted blob must never leak partial state into the recovered
//     RunState.
func FuzzValidateCheckpointBlob(f *testing.F) {
	for _, blob := range validCheckpointSeeds() {
		f.Add(blob)
	}
	f.Add([]byte("{}"))
	f.Add([]byte(""))
	f.Add([]byte("null"))
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":2,"tasks":{}}`))
	f.Add([]byte(`{"version":1,"tasks":null,"sequence":-1}`))
	f.Add([]byte(`{"version":1,"tasks":{"not-a-uuid":{}}}`))
	f.Add([]byte(`{"version":1.5}`))

	f.Fuzz(func(t *testing.T, blob []byte) {
		topo := fuzzTopology()

		validateErr := ValidateCheckpointBlob(blob)
		restored, restoreErr := Restore(topo, blob)

		if (validateErr == nil) != (restoreErr == nil) {
			t.Fatalf("ValidateCheckpointBlob=%v but Restore=%v for blob %q", validateErr, restoreErr, blob)
		}
		if validateErr == nil && restored == nil {
			t.Fatalf("Restore reported success but returned a nil RunState for blob %q", blob)
		}

		checkpoint := &models.RunCheckpoint{RunID: "fuzz-run", SequenceHigh: 7, StateBlob: blob}
		fallback, fallbackRes, err := RecoverRunState(topo, checkpoint, nil)
		if err != nil {
			t.Fatalf("RecoverRunState returned an error for blob %q (it is documented to never fail on a bad checkpoint, only fall back): %v", blob, err)
		}

		if validateErr != nil {
			scratch, scratchRes, err := RecoverRunState(topo, nil, nil)
			if err != nil {
				t.Fatalf("from-scratch RecoverRunState: %v", err)
			}
			if fallbackRes.MaxSequence != scratchRes.MaxSequence || fallbackRes.Complete != scratchRes.Complete {
				t.Fatalf("a rejected checkpoint's fallback (%+v) diverged from a from-scratch replay (%+v) for blob %q",
					fallbackRes, scratchRes, blob)
			}
			for id := range topo.Order {
				got, _ := fallback.TaskState(id)
				want, _ := scratch.TaskState(id)
				if got.Status != want.Status {
					t.Fatalf("blob %q: task %s: rejected-checkpoint fallback status %s, from-scratch %s",
						blob, id, got.Status, want.Status)
				}
			}
		}
	})
}

// fuzzTerminalRow is the fuzz corpus' encoding of one terminal task_runs row,
// deliberately looser than models.TaskRun: Task selects one of the three fixed
// topology tasks by index (out-of-range indices are dropped — see below), and
// Status/Seq are copied verbatim, unconstrained, so the fuzzer can produce
// duplicate sequences, negative sequences, and nonsense status strings.
type fuzzTerminalRow struct {
	Task   int    `json:"task"`
	Status string `json:"status"`
	Seq    int64  `json:"seq"`
}

// FuzzRecoverRunStateTerminalRows fuzzes RecoverRunState's terminal-row replay
// against row lists a real execution trace generator would never produce:
// duplicate sequences for the same task, negative or huge sequences,
// out-of-order rows, and unrecognized status strings. A real corrupted
// terminal-row read (a bad migration, a hand-edited row, replay from another
// build) can hand recovery exactly this kind of garbage, and it must survive
// without panicking or silently producing a different answer depending on
// nothing but happenstance.
//
// Property: determinism. Two recoveries over IDENTICAL (checkpoint=nil,
// terminalRows) inputs must reach the identical RecoveryResult and the
// identical reconstructed state for every task — TestRecoveryIsDeterministic's
// claim, re-checked here against adversarial rather than well-formed row
// lists, because determinism is exactly the property a takeover's second
// (or concurrently probing) owner depends on, and it is not supposed to be
// conditional on the row list being sane.
func FuzzRecoverRunStateTerminalRows(f *testing.F) {
	topo := fuzzTopology()
	seeds := []string{
		`[]`,
		`[{"task":0,"status":"succeeded","seq":1}]`,
		`[{"task":0,"status":"succeeded","seq":1},{"task":1,"status":"succeeded","seq":2},{"task":2,"status":"succeeded","seq":3}]`,
		`[{"task":0,"status":"succeeded","seq":1},{"task":0,"status":"failed","seq":1}]`,
		`[{"task":0,"status":"succeeded","seq":-5}]`,
		`[{"task":9,"status":"succeeded","seq":1}]`,
		`[{"task":0,"status":"not-a-real-status","seq":1}]`,
		`[{"task":2,"status":"succeeded","seq":1},{"task":0,"status":"succeeded","seq":100}]`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var rows []fuzzTerminalRow
		if err := json.Unmarshal(data, &rows); err != nil {
			// The property under test is recovery's replay, not the JSON
			// grammar; an input that doesn't even decode into a row list has
			// nothing to replay. Structural reason to return rather than
			// t.Skip, matching this repo's existing fuzz style
			// (internal/trigger/cron/fuzz_test.go).
			return
		}

		var terminal []models.TaskRun
		for _, r := range rows {
			var taskID uuid.UUID
			switch r.Task {
			case 0:
				taskID = fuzzTaskA
			case 1:
				taskID = fuzzTaskB
			case 2:
				taskID = fuzzTaskC
			default:
				continue
			}
			terminal = append(terminal, models.TaskRun{
				TaskID:           taskID,
				Status:           r.Status,
				TerminalSequence: r.Seq,
			})
		}

		first, firstRes, err := RecoverRunState(topo, nil, terminal)
		if err != nil {
			t.Fatalf("RecoverRunState errored for terminal rows %+v: %v", terminal, err)
		}
		second, secondRes, err := RecoverRunState(topo, nil, terminal)
		if err != nil {
			t.Fatalf("second RecoverRunState call errored for terminal rows %+v: %v", terminal, err)
		}

		if firstRes.MaxSequence != secondRes.MaxSequence || firstRes.Complete != secondRes.Complete {
			t.Fatalf("two recoveries from identical (adversarial) terminal rows %+v disagree:\n%+v\n%+v",
				terminal, firstRes, secondRes)
		}
		for id := range topo.Order {
			a, _ := first.TaskState(id)
			b, _ := second.TaskState(id)
			if a != b {
				t.Fatalf("two recoveries from terminal rows %+v reconstructed %s as %+v and %+v", terminal, id, a, b)
			}
		}
	})
}
