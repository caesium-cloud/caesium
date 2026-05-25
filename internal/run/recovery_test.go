package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

func row(taskID uuid.UUID, status TaskStatus, seq int64) models.TaskRun {
	return models.TaskRun{TaskID: taskID, Status: string(status), TerminalSequence: seq}
}

// TestRecover_CheckpointPlusTail builds a linear run, checkpoints mid-flight,
// continues, then reconstructs from the checkpoint + the post-checkpoint
// terminal rows and asserts the recovered state matches the crash point.
func TestRecover_CheckpointPlusTail(t *testing.T) {
	b := newTopoBuilder()
	a, bb, c := b.task(""), b.task(""), b.task("")
	b.edge(a, bb)
	b.edge(bb, c)
	topo := b.build()

	// Live owner: complete a, dispatch b, then checkpoint at sequence 1.
	live := NewRunState(topo, 0)
	live.ApplyCompletion(a, TaskStatusSucceeded, nil) // seq 1, readies b
	live.MarkDispatched(bb, "node-1", 1, 1000)
	blob, err := live.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := &models.RunCheckpoint{SequenceHigh: 1, StateBlob: blob}

	// After the checkpoint: b completes (seq 2) and c gets dispatched, then the
	// owner crashes (c's dispatch is not yet a terminal row).
	tail := []models.TaskRun{row(bb, TaskStatusSucceeded, 2)}

	rs, res, err := RecoverRunState(topo, checkpoint, tail)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatal("run is not complete (c still pending)")
	}
	if len(res.Ready) != 1 || res.Ready[0] != c {
		t.Fatalf("c should be ready after replaying b's completion, got %v", res.Ready)
	}
	if len(res.ReDispatch) != 0 {
		t.Fatalf("nothing should need re-dispatch (b completed), got %v", res.ReDispatch)
	}
	if res.MaxSequence != 2 {
		t.Fatalf("max sequence should be 2, got %d", res.MaxSequence)
	}
	if st, _ := rs.TaskState(bb); st.Status != TaskStatusSucceeded {
		t.Fatalf("b should be reconstructed as succeeded, got %s", st.Status)
	}
}

// TestRecover_RunningTaskReDispatched covers the in-flight-loss case: a task was
// running in the checkpoint and never produced a terminal row.
func TestRecover_RunningTaskReDispatched(t *testing.T) {
	b := newTopoBuilder()
	a, bb := b.task(""), b.task("")
	b.edge(a, bb)
	topo := b.build()

	live := NewRunState(topo, 0)
	live.ApplyCompletion(a, TaskStatusSucceeded, nil) // seq 1, readies b
	live.MarkDispatched(bb, "dead-node", 1, 1000)     // b running
	blob, _ := live.Snapshot()
	checkpoint := &models.RunCheckpoint{SequenceHigh: 1, StateBlob: blob}

	// No terminal rows after the checkpoint — b's worker outcome was lost.
	_, res, err := RecoverRunState(topo, checkpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReDispatch) != 1 || res.ReDispatch[0] != bb {
		t.Fatalf("running b should be re-dispatched, got %v", res.ReDispatch)
	}
}

// TestRecover_FromScratch replays a finished run from all its terminal rows with
// no checkpoint, covering succeeded + cached + skipped statuses.
func TestRecover_FromScratch(t *testing.T) {
	// a -> b (all_success), a -> c (all_success); a fails so both b and c skip.
	b := newTopoBuilder()
	a := b.task("")
	bb := b.task(jobdefschema.TriggerRuleAllSuccess)
	c := b.task(jobdefschema.TriggerRuleAllSuccess)
	b.edge(a, bb)
	b.edge(a, c)
	topo := b.build()

	// Terminal rows as a finished run would have persisted them, in sequence.
	rows := []models.TaskRun{
		row(a, TaskStatusFailed, 1),
		row(bb, TaskStatusSkipped, 2),
		row(c, TaskStatusSkipped, 3),
	}

	rs, res, err := RecoverRunState(topo, nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete {
		t.Fatal("a failed and b,c skipped → run should be complete")
	}
	if len(res.Ready) != 0 || len(res.ReDispatch) != 0 {
		t.Fatalf("finished run should have nothing to do, ready=%v redispatch=%v", res.Ready, res.ReDispatch)
	}
	for _, id := range []uuid.UUID{bb, c} {
		if st, _ := rs.TaskState(id); st.Status != TaskStatusSkipped {
			t.Fatalf("task %v should be skipped, got %s", id, st.Status)
		}
	}
}

func TestRecover_CachedStatusSatisfiesSuccessors(t *testing.T) {
	b := newTopoBuilder()
	a, c := b.task(""), b.task(jobdefschema.TriggerRuleAllSuccess)
	b.edge(a, c)
	topo := b.build()

	// a was satisfied from cache (terminal success); c should become ready.
	rows := []models.TaskRun{row(a, TaskStatusCached, 1)}
	_, res, err := RecoverRunState(topo, nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ready) != 1 || res.Ready[0] != c {
		t.Fatalf("cached predecessor should ready c, got %v", res.Ready)
	}
}

func TestRecover_SequenceGapReported(t *testing.T) {
	b := newTopoBuilder()
	a, bb, c := b.task(""), b.task(""), b.task("")
	b.edge(a, bb)
	b.edge(a, c)
	topo := b.build()

	// Sequences 1 and 3 present, 2 missing → gap at 2.
	rows := []models.TaskRun{
		row(a, TaskStatusSucceeded, 1),
		row(bb, TaskStatusSucceeded, 3),
	}
	_, res, err := RecoverRunState(topo, nil, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SequenceGaps) != 1 || res.SequenceGaps[0] != 2 {
		t.Fatalf("expected gap at sequence 2, got %v", res.SequenceGaps)
	}
}

func TestRecover_CorruptCheckpointFallsBack(t *testing.T) {
	b := newTopoBuilder()
	a, c := b.task(""), b.task("")
	b.edge(a, c)
	topo := b.build()

	checkpoint := &models.RunCheckpoint{SequenceHigh: 1, StateBlob: []byte("{not valid json")}
	// Even with a corrupt checkpoint, a from-scratch replay over the terminal
	// rows must reconstruct without error.
	rows := []models.TaskRun{row(a, TaskStatusSucceeded, 1)}
	rs, res, err := RecoverRunState(topo, checkpoint, rows)
	if err != nil {
		t.Fatalf("corrupt checkpoint should fall back, not error: %v", err)
	}
	if len(res.Ready) != 1 || res.Ready[0] != c {
		t.Fatalf("fallback replay should ready c, got %v", res.Ready)
	}
	if st, _ := rs.TaskState(a); st.Status != TaskStatusSucceeded {
		t.Fatalf("a should be succeeded after fallback replay, got %s", st.Status)
	}
}
