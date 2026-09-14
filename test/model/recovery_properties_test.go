package model_test

import (
	"reflect"
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
	"pgregory.net/rapid"
)

// drivePrefix advances a run through a generated prefix of its execution and
// returns a checkpoint taken at every step, so a property can crash the owner
// at any point rather than only at the end.
func drivePrefix(t *rapid.T, run *model.Run, steps int) []model.Snapshot {
	snaps := []model.Snapshot{run.Checkpoint()}
	for range steps {
		if run.IsComplete() {
			break
		}
		ready := run.Ready()
		running := run.Running()
		switch {
		case len(ready) > 0 && rapid.Bool().Draw(t, "dispatch_next"):
			id := rapid.SampledFrom(ready).Draw(t, "dispatch")
			run.Dispatch(id, "node-a", 60_000)
			if rapid.Bool().Draw(t, "started") {
				run.MarkStarted(id)
			}
		case len(running) > 0:
			id := rapid.SampledFrom(running).Draw(t, "complete")
			run.Complete(id, model.OutcomeGen().Draw(t, "outcome"), run.Generation())
		case len(ready) > 0:
			id := rapid.SampledFrom(ready).Draw(t, "dispatch")
			run.Dispatch(id, "node-a", 60_000)
		default:
			return snaps
		}
		snaps = append(snaps, run.Checkpoint())
	}
	return snaps
}

func containsID(ids []model.InstanceID, want model.InstanceID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func terminalStatuses(run *model.Run, dag model.DAG) map[model.InstanceID]model.TaskStatus {
	out := map[model.InstanceID]model.TaskStatus{}
	for _, id := range dag.AllInstances() {
		if s, ok := run.StatusOf(id); ok && model.Terminal(s) {
			out[id] = s
		}
	}
	return out
}

// TestCheckpointReplayEquivalence is the core DT-RECOVER-01 property: for EVERY
// point at which the owner could have checkpointed, reconstructing from that
// checkpoint plus the post-checkpoint terminal tail reproduces the terminal
// state the owner actually held.
//
// Quantifying over every checkpoint index matters. Recovery bugs cluster at the
// boundary — a row exactly at sequence_high, a checkpoint taken between a
// sequence being allocated and its row landing — and a test that checkpoints
// once in the middle will miss them.
func TestCheckpointReplayEquivalence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.FannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}
		snaps := drivePrefix(t, run, rapid.IntRange(0, 16).Draw(t, "steps"))

		live := terminalStatuses(run, dag)
		inFlight := run.Running()

		for i, snap := range snaps {
			tail := run.TailSince(snap.SequenceHigh)
			recovered, res, err := model.Recover(dag, run.Acknowledged(), &snap, tail)
			if err != nil {
				t.Fatalf("checkpoint %d: recovery failed: %v", i, err)
			}
			got := terminalStatuses(recovered, dag)
			if !reflect.DeepEqual(got, live) {
				t.Fatalf("checkpoint %d reconstructed %v, owner held %v", i, got, live)
			}
			if res.MaxSequence != run.Sequence() {
				t.Fatalf("checkpoint %d recovered sequence %d, owner held %d",
					i, res.MaxSequence, run.Sequence())
			}
			if len(res.SequenceGaps) != 0 {
				t.Fatalf("checkpoint %d reported gaps %v over a complete journal", i, res.SequenceGaps)
			}
			// Re-dispatch covers work the CHECKPOINT recorded as in flight and
			// that never produced a terminal row. Work dispatched after the
			// checkpoint is recorded nowhere, so it comes back as pending and
			// is picked up through Ready instead. What DT-RECOVER-01 actually
			// requires is that neither category is lost: every identity the
			// crashed owner had in flight is dispatchable again.
			//
			// Ready is compared uncapped here: a fan-out parallelism cap
			// legitimately holds instances back from the first dispatch wave,
			// and that is throttling, not loss.
			pickup := map[model.InstanceID]bool{}
			for _, id := range recovered.ReadyUncapped() {
				pickup[id] = true
			}
			for _, id := range res.ReDispatch {
				pickup[id] = true
				if !containsID(inFlight, id) {
					t.Fatalf("checkpoint %d re-dispatches %s, which the owner was not running (%v)",
						i, id, inFlight)
				}
			}
			for _, id := range inFlight {
				if !pickup[id] {
					t.Fatalf("checkpoint %d loses in-flight %s: ready=%v redispatch=%v",
						i, id, res.Ready, res.ReDispatch)
				}
			}
			if res.Complete != run.IsComplete() {
				t.Fatalf("checkpoint %d reports complete=%v, owner %v", i, res.Complete, run.IsComplete())
			}
			if err := model.CheckSafety(recovered); err != nil {
				t.Fatalf("checkpoint %d: %v", i, err)
			}
		}
	})
}

// TestReplayFromScratchMatchesCheckpointedReplay pins the equivalence the
// checkpoint is an optimization OF: replaying the complete journal with no
// checkpoint at all must land in the same place as replaying a checkpoint plus
// its tail. If the two ever disagree, the checkpoint is not a snapshot of the
// replay, it is a second source of truth.
func TestReplayFromScratchMatchesCheckpointedReplay(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.FannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}
		snaps := drivePrefix(t, run, rapid.IntRange(0, 16).Draw(t, "steps"))

		scratch, scratchRes, err := model.Recover(dag, run.Acknowledged(), nil, run.TailSince(0))
		if err != nil {
			t.Fatalf("from-scratch replay failed: %v", err)
		}
		for i, snap := range snaps {
			recovered, res, err := model.Recover(dag, run.Acknowledged(), &snap, run.TailSince(snap.SequenceHigh))
			if err != nil {
				t.Fatalf("checkpoint %d: %v", i, err)
			}
			if !reflect.DeepEqual(terminalStatuses(recovered, dag), terminalStatuses(scratch, dag)) {
				t.Fatalf("checkpoint %d disagrees with the from-scratch replay", i)
			}
			if res.MaxSequence != scratchRes.MaxSequence {
				t.Fatalf("checkpoint %d recovered sequence %d, from-scratch %d",
					i, res.MaxSequence, scratchRes.MaxSequence)
			}
		}
	})
}

// TestDroppedCheckpointWithFilteredTailLosesState is a negative control, and it
// is here because the product documents this exact trap: the terminal tail is
// filtered by the checkpoint's sequence_high, so a recovery that rejects the
// checkpoint but keeps the filtered tail silently loses every transition at or
// below the cut and resurrects finished work.
//
// The property asserts that the loss is real and observable. A checker that
// could not tell these two recoveries apart would be unable to catch the bug,
// so this also validates the oracle rather than only the model.
func TestDroppedCheckpointWithFilteredTailLosesState(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}
		drivePrefix(t, run, rapid.IntRange(1, 16).Draw(t, "steps"))

		cut := run.Checkpoint()
		if cut.SequenceHigh == 0 {
			return // nothing was covered by the checkpoint; nothing to lose
		}
		covered := 0
		for _, rec := range run.Journal() {
			if rec.Sequence <= cut.SequenceHigh {
				covered++
			}
		}
		if covered == 0 {
			return
		}

		lossy, _, err := model.Recover(dag, run.Acknowledged(), nil, run.TailSince(cut.SequenceHigh))
		if err != nil {
			t.Fatalf("recovery failed: %v", err)
		}
		live := terminalStatuses(run, dag)
		got := terminalStatuses(lossy, dag)
		if len(got) >= len(live) {
			t.Fatalf("dropping a checkpoint covering %d rows lost nothing: %v vs %v", covered, got, live)
		}
		for id, s := range got {
			if live[id] != s {
				t.Fatalf("the lossy recovery invented an outcome for %s: %s", id, s)
			}
		}
	})
}

// TestSequenceGapIsReported covers the crash-mid-write case: the previous owner
// allocated a terminal sequence and died before its row landed. Recovery must
// report the hole and must not treat the affected work as finished.
func TestSequenceGapIsReported(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}
		drivePrefix(t, run, rapid.IntRange(2, 16).Draw(t, "steps"))

		journal := run.Journal()
		if len(journal) < 2 {
			return
		}
		drop := rapid.IntRange(0, len(journal)-1).Draw(t, "drop")
		lost := journal[drop]

		tail := make([]model.TerminalRecord, 0, len(journal)-1)
		for i, rec := range journal {
			if i != drop {
				tail = append(tail, rec)
			}
		}

		recovered, res, err := model.Recover(dag, run.Acknowledged(), nil, tail)
		if err != nil {
			t.Fatalf("recovery failed: %v", err)
		}
		if lost.Sequence < journal[len(journal)-1].Sequence {
			// A hole below the highest observed row is detectable; a row lost
			// from the very end is indistinguishable from one never allocated,
			// which is why the check is conditional rather than unconditional.
			found := false
			for _, gap := range res.SequenceGaps {
				if gap == lost.Sequence {
					found = true
				}
			}
			if !found {
				t.Fatalf("dropping sequence %d produced gaps %v", lost.Sequence, res.SequenceGaps)
			}
		}
		if s, _ := recovered.StatusOf(lost.Instance); model.Terminal(s) && s == lost.Status {
			t.Fatalf("recovery treated %s as %s with no persisted row", lost.Instance, s)
		}
	})
}

// TestRecoveryIsDeterministic checks that two owners handed the same durable
// inputs reach the same conclusion. Recovery ordered by wall clock instead of
// by terminal sequence would fail this under clock skew; ordering by sequence
// is what makes it hold.
func TestRecoveryIsDeterministic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.FannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}
		drivePrefix(t, run, rapid.IntRange(0, 12).Draw(t, "steps"))

		snap := run.Checkpoint()
		tail := run.TailSince(snap.SequenceHigh)
		a, resA, err := model.Recover(dag, run.Acknowledged(), &snap, tail)
		if err != nil {
			t.Fatalf("%v", err)
		}
		b, resB, err := model.Recover(dag, run.Acknowledged(), &snap, tail)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !reflect.DeepEqual(resA, resB) {
			t.Fatalf("two recoveries from identical inputs disagree:\n%+v\n%+v", resA, resB)
		}
		if !reflect.DeepEqual(terminalStatuses(a, dag), terminalStatuses(b, dag)) {
			t.Fatalf("two recoveries from identical inputs reconstructed different states")
		}
	})
}

// TestPartitionAccounting checks the bookkeeping a fanned step introduces: the
// scheduled identity set is exactly one per declared partition, a fanned
// predecessor contributes ONE fan-in edge rather than one per partition, and a
// group's parallelism cap is never exceeded.
//
// The "one edge per group" clause is the one worth stating: counting a fanned
// predecessor once per instance is how a consumer ends up dispatched while its
// producer is still running, and it is invisible in any single-partition test.
func TestPartitionAccounting(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.FannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}

		want := 0
		for _, step := range dag.Order {
			n := len(dag.Fan[step].Partitions)
			if n == 0 {
				n = 1
			}
			want += n
		}
		if got := len(dag.AllInstances()); got != want {
			t.Fatalf("DAG schedules %d identities, declared %d", got, want)
		}

		drivePrefix(t, run, rapid.IntRange(0, 20).Draw(t, "steps"))

		for _, step := range dag.Order {
			ids := dag.Instances(step)
			terminal := 0
			for _, id := range ids {
				if s, _ := run.StatusOf(id); model.Terminal(s) {
					terminal++
				}
			}
			// A successor may only have observed this step as resolved if every
			// one of its instances is terminal.
			resolved := terminal == len(ids)
			for _, succ := range dag.Succ(step) {
				for _, sid := range dag.Instances(succ) {
					s, _ := run.StatusOf(sid)
					if s == model.StatusRunning && !resolved {
						t.Fatalf("%s is in flight while predecessor group %s has %d/%d terminal",
							sid, step, terminal, len(ids))
					}
				}
			}
		}
		if err := model.CheckSafety(run); err != nil {
			t.Fatalf("%v", err)
		}
	})
}
