package run

import (
	"fmt"
	"sort"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/test/model"
	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// These properties drive the real recovery path — RecoverRunState and its
// fan-out variant, the code a surviving owner runs after a takeover — against
// generated executions, with hermetic fixtures: no database, no cluster, no
// checkpoint table, just the topology, a snapshot blob and terminal rows.
//
// The limit is worth naming. This establishes that the RECONSTRUCTION is
// correct given durable inputs. It says nothing about whether those inputs are
// durable: that the checkpoint actually committed, that the terminal row landed
// in the same transaction as the decision, or that a real surviving node
// acquires the lease and reaches this code at all. DT-RECOVER-01 is only
// established by a real crash on a real cluster.

// recoveryTrace records what an owner made durable while executing, so a
// property can crash it at any point and hand the successor exactly the rows
// that would have survived.
type recoveryTrace struct {
	bridge *modelBridge
	rs     *RunState
	// journal is every terminal row, in allocation order.
	journal []models.TaskRun
	// checkpoints are the snapshot blobs the owner could have written, one per
	// step of the execution.
	checkpoints []models.RunCheckpoint
}

func newRecoveryTrace(t *rapid.T, bridge *modelBridge) *recoveryTrace {
	t.Helper()
	tr := &recoveryTrace{bridge: bridge, rs: NewRunState(bridge.topo, 0)}
	tr.checkpoint(t)
	return tr
}

func (tr *recoveryTrace) checkpoint(t *rapid.T) {
	t.Helper()
	blob, err := tr.rs.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	tr.checkpoints = append(tr.checkpoints, models.RunCheckpoint{
		RunID:        "run-1",
		SequenceHigh: tr.rs.Sequence(),
		StateBlob:    blob,
	})
}

// record appends the durable rows one completion produced, exactly as
// CompleteTaskOwner persists them: the completing task's row plus a row for
// every skip that completion decided, each carrying its own sequence.
func (tr *recoveryTrace) record(taskID uuid.UUID, status TaskStatus, res CompletionResult) {
	if res.TerminalSequence > 0 {
		tr.journal = append(tr.journal, models.TaskRun{
			TaskID:           taskID,
			Status:           string(status),
			TerminalSequence: res.TerminalSequence,
		})
	}
	for _, skip := range res.Skipped {
		tr.journal = append(tr.journal, models.TaskRun{
			TaskID:           skip.TaskID,
			Status:           string(TaskStatusSkipped),
			TerminalSequence: skip.TerminalSequence,
		})
	}
	sort.SliceStable(tr.journal, func(i, j int) bool {
		return tr.journal[i].TerminalSequence < tr.journal[j].TerminalSequence
	})
}

// tailSince is the post-checkpoint terminal tail query, including its
// strictly-greater predicate.
func (tr *recoveryTrace) tailSince(sequenceHigh int64) []models.TaskRun {
	var out []models.TaskRun
	for _, row := range tr.journal {
		if row.TerminalSequence > sequenceHigh {
			out = append(out, row)
		}
	}
	return out
}

// drive advances the owner through a generated prefix of its execution,
// checkpointing after every step.
func (tr *recoveryTrace) drive(t *rapid.T, steps int) {
	t.Helper()
	for range steps {
		ready := tr.rs.ReadyTasks()
		var running []uuid.UUID
		for _, id := range tr.bridge.dag.AllInstances() {
			productID := tr.bridge.instance[id]
			if state, ok := tr.rs.TaskState(productID); ok && state.Status == TaskStatusRunning {
				running = append(running, productID)
			}
		}
		switch {
		case len(ready) > 0 && (len(running) == 0 || rapid.Bool().Draw(t, "dispatch_next")):
			id := ready[rapid.IntRange(0, len(ready)-1).Draw(t, "dispatch")]
			state, _ := tr.rs.TaskState(id)
			tr.rs.MarkDispatched(id, "node-a", state.Attempt, 60_000)
		case len(running) > 0:
			id := running[rapid.IntRange(0, len(running)-1).Draw(t, "complete")]
			outcome := TaskStatus(model.OutcomeGen().Draw(t, "outcome"))
			res := tr.rs.ApplyCompletion(id, outcome, nil)
			tr.record(id, outcome, res)
		default:
			return
		}
		tr.checkpoint(t)
	}
}

func (tr *recoveryTrace) liveStatuses() map[uuid.UUID]TaskStatus {
	out := map[uuid.UUID]TaskStatus{}
	for _, id := range tr.bridge.dag.AllInstances() {
		productID := tr.bridge.instance[id]
		if state, ok := tr.rs.TaskState(productID); ok {
			out[productID] = state.Status
		}
	}
	return out
}

func (tr *recoveryTrace) runningNow() []uuid.UUID {
	var out []uuid.UUID
	for id, status := range tr.liveStatuses() {
		if status == TaskStatusRunning {
			out = append(out, id)
		}
	}
	return out
}

func containsUUID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestRecoveryReconstructsEveryCheckpoint is the core DT-RECOVER-01 property on
// the real engine: for EVERY point the owner could have checkpointed,
// reconstructing from that checkpoint plus the post-checkpoint tail reproduces
// the terminal state the owner held, and loses no in-flight work.
//
// Quantifying over every checkpoint index is what makes this worth running.
// Recovery defects cluster at the boundary — a row stamped exactly at
// sequence_high, a checkpoint taken between a sequence being allocated and its
// row landing — and a test that checkpoints once in the middle sails past them.
func TestRecoveryReconstructsEveryCheckpoint(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(0, 16).Draw(t, "steps"))

		live := trace.liveStatuses()
		inFlight := trace.runningNow()

		for i, checkpoint := range trace.checkpoints {
			recovered, res, err := RecoverRunState(bridge.topo, &checkpoint, trace.tailSince(checkpoint.SequenceHigh))
			if err != nil {
				t.Fatalf("checkpoint %d: %v", i, err)
			}

			for productID, want := range live {
				got, ok := recovered.TaskState(productID)
				if !ok {
					t.Fatalf("checkpoint %d lost %s entirely", i, bridge.back[productID])
				}
				if IsTerminal(want) && got.Status != want {
					t.Fatalf("checkpoint %d reconstructed %s as %s, owner held %s",
						i, bridge.back[productID], got.Status, want)
				}
			}
			if res.MaxSequence != trace.rs.Sequence() {
				t.Fatalf("checkpoint %d recovered sequence %d, owner held %d",
					i, res.MaxSequence, trace.rs.Sequence())
			}
			if len(res.SequenceGaps) != 0 {
				t.Fatalf("checkpoint %d reported gaps %v over a complete journal", i, res.SequenceGaps)
			}
			if res.Complete != trace.rs.IsComplete() {
				t.Fatalf("checkpoint %d reports complete=%v, owner %v",
					i, res.Complete, trace.rs.IsComplete())
			}

			// Nothing the owner had in flight may be lost: it comes back either
			// as an explicit re-dispatch (the checkpoint recorded it running) or
			// on the ready queue (it was dispatched after the checkpoint, so
			// nothing durable recorded it at all).
			pickup := map[uuid.UUID]bool{}
			for _, id := range res.Ready {
				pickup[id] = true
			}
			for _, id := range res.ReDispatch {
				pickup[id] = true
				if !containsUUID(inFlight, id) {
					t.Fatalf("checkpoint %d re-dispatches %s, which the owner was not running",
						i, bridge.back[id])
				}
			}
			for _, id := range inFlight {
				if !pickup[id] {
					t.Fatalf("checkpoint %d loses in-flight %s", i, bridge.back[id])
				}
			}
			// A re-dispatched task must come back with a fresh attempt, or a
			// retry policy keyed on attempt silently stops counting.
			for _, id := range res.ReDispatch {
				state, _ := recovered.TaskState(id)
				if state.Status != TaskStatusPending {
					t.Fatalf("checkpoint %d re-dispatches %s still marked %s",
						i, bridge.back[id], state.Status)
				}
				if state.Attempt < 2 {
					t.Fatalf("checkpoint %d re-dispatches %s at attempt %d",
						i, bridge.back[id], state.Attempt)
				}
			}
		}
	})
}

// TestRecoveryAgreesWithReferenceModel runs the same reconstruction through the
// independent model and requires the two to reach the same conclusion about
// what is terminal, what must be re-dispatched, and where the sequence cursor
// stands.
func TestRecoveryAgreesWithReferenceModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(0, 16).Draw(t, "steps"))

		ack := model.Ack{RunID: "run-1", JobID: "job-1"}
		for i, checkpoint := range trace.checkpoints {
			tail := trace.tailSince(checkpoint.SequenceHigh)
			recovered, res, err := RecoverRunState(bridge.topo, &checkpoint, tail)
			if err != nil {
				t.Fatalf("checkpoint %d: %v", i, err)
			}

			refSnap, refTail := bridge.referenceInputs(t, trace, checkpoint)
			refRun, refRes, err := model.Recover(dag, ack, refSnap, refTail)
			if err != nil {
				t.Fatalf("checkpoint %d: reference recovery: %v", i, err)
			}

			if res.MaxSequence != refRes.MaxSequence {
				t.Fatalf("checkpoint %d: RunState sequence %d, model %d",
					i, res.MaxSequence, refRes.MaxSequence)
			}
			if res.Complete != refRes.Complete {
				t.Fatalf("checkpoint %d: RunState complete=%v, model %v",
					i, res.Complete, refRes.Complete)
			}
			if !sameSet(sortedNames(bridge.names(res.ReDispatch)), sortedNames(refRes.ReDispatch)) {
				t.Fatalf("checkpoint %d: RunState re-dispatches %v, model %v",
					i, sortedNames(bridge.names(res.ReDispatch)), sortedNames(refRes.ReDispatch))
			}
			if !sameSet(sortedNames(bridge.names(res.Ready)), sortedNames(refRes.Ready)) {
				t.Fatalf("checkpoint %d: RunState ready %v, model ready %v",
					i, sortedNames(bridge.names(res.Ready)), sortedNames(refRes.Ready))
			}
			for _, id := range dag.AllInstances() {
				productID := bridge.instance[id]
				got, _ := recovered.TaskState(productID)
				want, _ := refRun.StatusOf(id)
				if string(got.Status) != string(want) {
					t.Fatalf("checkpoint %d: %s is %q in RunState and %q in the model",
						i, id, got.Status, want)
				}
			}
		}
	})
}

// referenceInputs translates the product's durable recovery inputs — a
// checkpoint blob and terminal rows — into the reference model's equivalents,
// by replaying the trace up to the same point.
//
// It deliberately reconstructs the model's snapshot from the same journal
// rather than reading the product's blob: the model must not learn the
// product's internal encoding, or the comparison stops being independent.
func (b *modelBridge) referenceInputs(t *rapid.T, trace *recoveryTrace, checkpoint models.RunCheckpoint) (*model.Snapshot, []model.TerminalRecord) {
	t.Helper()
	ref, err := model.New("run-1", "job-1", b.dag)
	if err != nil {
		t.Fatalf("reference model rejected a generated DAG: %v", err)
	}
	// Replay the journal up to the checkpoint, dispatching as needed so the
	// model reaches the same point the product's snapshot captured.
	for _, row := range trace.journal {
		if row.TerminalSequence > checkpoint.SequenceHigh {
			continue
		}
		id := b.back[row.TaskID]
		if s, _ := ref.StatusOf(id); s == model.StatusPending {
			ref.Dispatch(id, "node-a", 60_000)
		}
		ref.Complete(id, model.TaskStatus(row.Status), ref.Generation())
	}
	// Re-establish whatever was in flight at the checkpoint, which the
	// product's blob records and the journal does not.
	restored, err := Restore(b.topo, checkpoint.StateBlob)
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	for _, productID := range restored.RunningTasks() {
		ref.Dispatch(b.back[productID], "node-a", 60_000)
	}

	snap := ref.Checkpoint()
	var tail []model.TerminalRecord
	for _, row := range trace.tailSince(checkpoint.SequenceHigh) {
		tail = append(tail, model.TerminalRecord{
			Instance: b.back[row.TaskID],
			Status:   model.TaskStatus(row.Status),
			Sequence: row.TerminalSequence,
			Epoch:    1,
		})
	}
	return &snap, tail
}

// TestCheckpointValidationAgreesWithRestore pins an invariant the product
// documents and depends on: ValidateCheckpointBlob accepts exactly the blobs
// Restore accepts.
//
// The two must never disagree, and the reason is specific. A recovering owner
// validates BEFORE it queries the terminal tail, because the tail query is
// filtered by the checkpoint's sequence_high. If validation said yes and Restore
// then said no, recovery would fall back to a from-scratch replay holding only
// the post-checkpoint rows — every transition at or below the cut silently
// lost, and finished work resurrected.
func TestCheckpointValidationAgreesWithRestore(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(0, 10).Draw(t, "steps"))

		blob := trace.checkpoints[len(trace.checkpoints)-1].StateBlob
		if mutate := rapid.IntRange(0, 3).Draw(t, "mutation"); mutate > 0 {
			blob = corruptBlob(t, blob, mutate)
		}

		validateErr := ValidateCheckpointBlob(blob)
		_, restoreErr := Restore(bridge.topo, blob)
		if (validateErr == nil) != (restoreErr == nil) {
			t.Fatalf("ValidateCheckpointBlob=%v but Restore=%v for blob %q",
				validateErr, restoreErr, string(blob))
		}
	})
}

// corruptBlob damages a checkpoint the three ways a real one can arrive
// damaged: truncated by a partial write, structurally invalid, or carrying a
// version this build does not know.
func corruptBlob(t *rapid.T, blob []byte, kind int) []byte {
	t.Helper()
	switch kind {
	case 1:
		if len(blob) <= 1 {
			return []byte("")
		}
		cut := rapid.IntRange(0, len(blob)-1).Draw(t, "cut")
		return blob[:cut]
	case 2:
		return []byte("{not json")
	default:
		return []byte(fmt.Sprintf(`{"version":%d}`, rapid.IntRange(2, 99).Draw(t, "version")))
	}
}

// TestCorruptCheckpointFallsBackToFullReplay checks the documented fallback: a
// rejected checkpoint must replay from scratch, and the caller must drop the
// sequence filter in the same breath. Given the complete journal the fallback
// reaches exactly the same state a clean from-scratch replay does.
func TestCorruptCheckpointFallsBackToFullReplay(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(1, 12).Draw(t, "steps"))

		last := trace.checkpoints[len(trace.checkpoints)-1]
		corrupt := models.RunCheckpoint{
			RunID:        last.RunID,
			SequenceHigh: last.SequenceHigh,
			StateBlob:    corruptBlob(t, last.StateBlob, rapid.IntRange(1, 3).Draw(t, "mutation")),
		}
		if err := ValidateCheckpointBlob(corrupt.StateBlob); err == nil {
			return // the mutation happened to produce a valid blob
		}

		fallback, fallbackRes, err := RecoverRunState(bridge.topo, &corrupt, trace.journal)
		if err != nil {
			t.Fatalf("fallback recovery: %v", err)
		}
		scratch, scratchRes, err := RecoverRunState(bridge.topo, nil, trace.journal)
		if err != nil {
			t.Fatalf("from-scratch recovery: %v", err)
		}

		if fallbackRes.MaxSequence != scratchRes.MaxSequence {
			t.Fatalf("fallback sequence %d, from-scratch %d",
				fallbackRes.MaxSequence, scratchRes.MaxSequence)
		}
		if fallbackRes.Complete != scratchRes.Complete {
			t.Fatalf("fallback complete=%v, from-scratch %v",
				fallbackRes.Complete, scratchRes.Complete)
		}
		for _, id := range dag.AllInstances() {
			productID := bridge.instance[id]
			got, _ := fallback.TaskState(productID)
			want, _ := scratch.TaskState(productID)
			if got.Status != want.Status {
				t.Fatalf("%s is %s after the fallback and %s after a clean replay",
					id, got.Status, want.Status)
			}
		}
	})
}

// TestDroppedTerminalRowIsReportedAsAGap covers the crash-mid-write case: the
// dead owner allocated a sequence and died before its row landed. Recovery must
// name the hole and must not treat the affected work as finished — the row's
// absence is the only evidence that work needs re-running.
func TestDroppedTerminalRowIsReportedAsAGap(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(2, 16).Draw(t, "steps"))

		if len(trace.journal) < 2 {
			return
		}
		drop := rapid.IntRange(0, len(trace.journal)-2).Draw(t, "drop")
		lost := trace.journal[drop]

		tail := make([]models.TaskRun, 0, len(trace.journal)-1)
		for i, row := range trace.journal {
			if i != drop {
				tail = append(tail, row)
			}
		}

		recovered, res, err := RecoverRunState(bridge.topo, nil, tail)
		if err != nil {
			t.Fatalf("recovery: %v", err)
		}
		found := false
		for _, gap := range res.SequenceGaps {
			if gap == lost.TerminalSequence {
				found = true
			}
		}
		if !found {
			t.Fatalf("dropping sequence %d produced gaps %v", lost.TerminalSequence, res.SequenceGaps)
		}
		state, _ := recovered.TaskState(lost.TaskID)
		if string(state.Status) == lost.Status {
			t.Fatalf("recovery treated %s as %s with no persisted row",
				bridge.back[lost.TaskID], state.Status)
		}
	})
}

// TestRecoveryIsDeterministic requires two owners handed the same durable
// inputs to reach the same conclusion. Recovery ordered by wall clock rather
// than by terminal sequence would fail this under any clock skew; ordering by
// sequence is what makes the guarantee hold without a synchronized clock.
func TestRecoveryIsDeterministic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(0, 12).Draw(t, "steps"))

		checkpoint := trace.checkpoints[rapid.IntRange(0, len(trace.checkpoints)-1).Draw(t, "checkpoint")]
		tail := trace.tailSince(checkpoint.SequenceHigh)

		first, firstRes, err := RecoverRunState(bridge.topo, &checkpoint, tail)
		if err != nil {
			t.Fatalf("%v", err)
		}
		second, secondRes, err := RecoverRunState(bridge.topo, &checkpoint, tail)
		if err != nil {
			t.Fatalf("%v", err)
		}

		if firstRes.MaxSequence != secondRes.MaxSequence || firstRes.Complete != secondRes.Complete {
			t.Fatalf("two recoveries from identical inputs disagree:\n%+v\n%+v", firstRes, secondRes)
		}
		if !sameSet(sortedNames(bridge.names(firstRes.Ready)), sortedNames(bridge.names(secondRes.Ready))) {
			t.Fatalf("two recoveries offered different ready queues")
		}
		if !sameSet(sortedNames(bridge.names(firstRes.ReDispatch)), sortedNames(bridge.names(secondRes.ReDispatch))) {
			t.Fatalf("two recoveries re-dispatched different work")
		}
		for _, id := range dag.AllInstances() {
			productID := bridge.instance[id]
			a, _ := first.TaskState(productID)
			b, _ := second.TaskState(productID)
			if a != b {
				t.Fatalf("two recoveries reconstructed %s as %+v and %+v", id, a, b)
			}
		}
	})
}

// TestFanOutRecoveryRebuildsPartitionAccounting checks the half of recovery the
// checkpoint deliberately does NOT carry: a fanned group's instance identities,
// its in-group edges, its parallelism cap and its failure policy are all
// rebuilt from the durable instance rows and the catalog, because the catalog
// is their single source of truth.
//
// Getting this wrong is quiet rather than loud. A takeover that silently drops
// a group's maxParallel does not fail anything; it just stops throttling, and
// the run that used to fit in its cluster stops fitting.
func TestFanOutRecoveryRebuildsPartitionAccounting(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := fannedDAGGen().Draw(t, "dag")
		var fanned model.TaskID
		for step := range dag.Fan {
			fanned = step
		}
		fx := newModelFanOutFixture(t, dag, fanned)
		fan := dag.Fan[fanned]
		catalogID := fx.bridge.taskID[fanned]

		// The owner crashes with nothing yet terminal: the successor has only
		// the durable instance rows and the catalog to work from.
		recovered, res, err := RecoverRunStateWithFanOut(fx.bridge.topo, nil, nil, fx.rows, fx.catalog)
		if err != nil {
			t.Fatalf("fan-out recovery: %v", err)
		}

		for i := range fan.Partitions {
			rowID := fx.rows[i].ID
			if _, ok := recovered.TaskState(rowID); !ok {
				t.Fatalf("recovery lost partition %d of %s", i, fanned)
			}
			got, isInstance := recovered.CatalogTaskID(rowID)
			if !isInstance || got != catalogID {
				t.Fatalf("recovery mapped partition %d of %s to catalog %v (instance=%v)",
					i, fanned, got, isInstance)
			}
		}
		if _, templatePresent := recovered.TaskState(catalogID); templatePresent {
			t.Fatalf("recovery left the unexpanded template node for %s", fanned)
		}

		// The parallelism cap must survive the takeover, even though the
		// checkpoint never carried it.
		offered := 0
		for _, id := range res.Ready {
			if _, isInstance := recovered.CatalogTaskID(id); isInstance {
				offered++
			}
		}
		if fan.MaxParallel > 0 && offered > fan.MaxParallel {
			t.Fatalf("recovery offered %d instances of %s against a cap of %d",
				offered, fanned, fan.MaxParallel)
		}

		// In-group edges must survive too: an instance that waits on a sibling
		// may not be offered before that sibling is terminal.
		for i, key := range fan.Partitions {
			if len(fan.DependsOn[key]) == 0 {
				continue
			}
			rowID := fx.rows[i].ID
			for _, id := range res.Ready {
				if id == rowID {
					t.Fatalf("recovery offered partition %d of %s while its in-group predecessor was pending",
						i, fanned)
				}
			}
		}
	})
}

// TestFanOutRecoveryReplaysTerminalInstanceRows checks the fanned counterpart
// of the plain replay: instance rows that reached a terminal state before the
// crash are replayed under their INSTANCE identity, not their catalog task id.
// Replaying them under the catalog id would resolve an arbitrary sibling, and
// for a group of one that mistake is invisible.
func TestFanOutRecoveryReplaysTerminalInstanceRows(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := fannedDAGGen().Draw(t, "dag")
		var fanned model.TaskID
		for step := range dag.Fan {
			fanned = step
		}
		fx := newModelFanOutFixture(t, dag, fanned)
		fan := dag.Fan[fanned]

		// Resolve a prefix of the group durably.
		resolved := rapid.IntRange(0, len(fan.Partitions)).Draw(t, "resolved")
		var tail []models.TaskRun
		var seq int64
		for i := range resolved {
			outcome := TaskStatus(model.OutcomeGen().Draw(t, "outcome"))
			seq++
			row := fx.rows[i]
			row.Status = string(outcome)
			row.TerminalSequence = seq
			tail = append(tail, row)
		}

		recovered, res, err := RecoverRunStateWithFanOut(fx.bridge.topo, nil, tail, fx.rows, fx.catalog)
		if err != nil {
			t.Fatalf("fan-out recovery: %v", err)
		}
		if res.MaxSequence != seq {
			t.Fatalf("recovery observed sequence %d, the journal reached %d", res.MaxSequence, seq)
		}
		for i := range resolved {
			state, ok := recovered.TaskState(fx.rows[i].ID)
			if !ok {
				t.Fatalf("recovery lost partition %d", i)
			}
			if string(state.Status) != tail[i].Status {
				t.Fatalf("partition %d replayed as %s, its row said %s", i, state.Status, tail[i].Status)
			}
		}
		for i := resolved; i < len(fan.Partitions); i++ {
			state, ok := recovered.TaskState(fx.rows[i].ID)
			if !ok {
				t.Fatalf("recovery lost partition %d", i)
			}
			if IsTerminal(state.Status) {
				t.Fatalf("recovery resolved partition %d as %s with no terminal row", i, state.Status)
			}
		}
	})
}

// TestSnapshotRoundTripPreservesDecisions checks that a snapshot survives the
// round trip with enough fidelity that the restored state makes the SAME next
// decisions — including replaying a re-delivered completion's durable rows,
// which lives in the snapshot's completions map and is easy to drop.
func TestSnapshotRoundTripPreservesDecisions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		trace := newRecoveryTrace(t, bridge)
		trace.drive(t, rapid.IntRange(1, 12).Draw(t, "steps"))

		blob, err := trace.rs.Snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		restored, err := Restore(bridge.topo, blob)
		if err != nil {
			t.Fatalf("restore: %v", err)
		}

		if restored.Sequence() != trace.rs.Sequence() {
			t.Fatalf("restored sequence %d, original %d", restored.Sequence(), trace.rs.Sequence())
		}
		if restored.IsComplete() != trace.rs.IsComplete() {
			t.Fatalf("restored complete=%v, original %v", restored.IsComplete(), trace.rs.IsComplete())
		}
		if !sameSet(sortedNames(bridge.names(restored.ReadyTasks())),
			sortedNames(bridge.names(trace.rs.ReadyTasks()))) {
			t.Fatalf("restored ready queue %v, original %v",
				sortedNames(bridge.names(restored.ReadyTasks())),
				sortedNames(bridge.names(trace.rs.ReadyTasks())))
		}
		for _, id := range dag.AllInstances() {
			productID := bridge.instance[id]
			got, _ := restored.TaskState(productID)
			want, _ := trace.rs.TaskState(productID)
			if got != want {
				t.Fatalf("restored %s as %+v, original %+v", id, got, want)
			}
		}

		// A re-delivered completion must replay the same durable rows on both.
		if len(trace.journal) == 0 {
			return
		}
		row := trace.journal[rapid.IntRange(0, len(trace.journal)-1).Draw(t, "row")]
		original := trace.rs.ApplyCompletion(row.TaskID, TaskStatus(row.Status), nil)
		replayed := restored.ApplyCompletion(row.TaskID, TaskStatus(row.Status), nil)
		if original.TerminalSequence != replayed.TerminalSequence {
			t.Fatalf("re-delivery replayed sequence %d before the round trip and %d after",
				original.TerminalSequence, replayed.TerminalSequence)
		}
		if len(original.Skipped) != len(replayed.Skipped) {
			t.Fatalf("re-delivery replayed %d skips before the round trip and %d after",
				len(original.Skipped), len(replayed.Skipped))
		}
	})
}
