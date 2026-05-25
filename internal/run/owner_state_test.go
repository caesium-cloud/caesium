package run

import (
	"sort"
	"testing"

	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// topoBuilder assembles a RunTopology from named tasks, edges, and rules.
type topoBuilder struct {
	ids   []uuid.UUID
	edges [][2]uuid.UUID
	rules map[uuid.UUID]string
}

func newTopoBuilder() *topoBuilder { return &topoBuilder{rules: map[uuid.UUID]string{}} }

func (b *topoBuilder) task(rule string) uuid.UUID {
	id := uuid.New()
	b.ids = append(b.ids, id)
	if rule != "" {
		b.rules[id] = rule
	}
	return id
}

func (b *topoBuilder) edge(from, to uuid.UUID) { b.edges = append(b.edges, [2]uuid.UUID{from, to}) }

func (b *topoBuilder) build() RunTopology {
	adj := make(map[uuid.UUID][]uuid.UUID, len(b.ids))
	pred := make(map[uuid.UUID][]uuid.UUID, len(b.ids))
	order := make(map[uuid.UUID]int, len(b.ids))
	for i, id := range b.ids {
		adj[id] = nil
		pred[id] = nil
		order[id] = i
	}
	for _, e := range b.edges {
		adj[e[0]] = append(adj[e[0]], e[1])
		pred[e[1]] = append(pred[e[1]], e[0])
	}
	return RunTopology{Adjacency: adj, Predecessors: pred, TriggerRule: b.rules, Order: order}
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestRunState_LinearChain(t *testing.T) {
	b := newTopoBuilder()
	a, c1, c2 := b.task(""), b.task(""), b.task("")
	b.edge(a, c1)
	b.edge(c1, c2)
	rs := NewRunState(b.build(), 0)

	if got := rs.ReadyTasks(); len(got) != 1 || got[0] != a {
		t.Fatalf("expected [a] ready at start, got %v", got)
	}

	r := rs.ApplyCompletion(a, TaskStatusSucceeded, nil)
	if !contains(r.Ready, c1) || len(r.Ready) != 1 {
		t.Fatalf("completing a should ready c1, got %v", r.Ready)
	}
	if r.TerminalSequence != 1 {
		t.Fatalf("first terminal sequence should be 1, got %d", r.TerminalSequence)
	}
	if r.Complete {
		t.Fatal("run not complete after a")
	}

	r = rs.ApplyCompletion(c1, TaskStatusSucceeded, nil)
	if !contains(r.Ready, c2) {
		t.Fatalf("completing c1 should ready c2, got %v", r.Ready)
	}

	r = rs.ApplyCompletion(c2, TaskStatusSucceeded, nil)
	if !r.Complete {
		t.Fatal("run should be complete after c2")
	}
	if r.TerminalSequence != 3 {
		t.Fatalf("third terminal sequence should be 3, got %d", r.TerminalSequence)
	}
}

func TestRunState_FanOutFanIn(t *testing.T) {
	b := newTopoBuilder()
	a, l, r2, j := b.task(""), b.task(""), b.task(""), b.task("")
	b.edge(a, l)
	b.edge(a, r2)
	b.edge(l, j)
	b.edge(r2, j)
	rs := NewRunState(b.build(), 0)

	res := rs.ApplyCompletion(a, TaskStatusSucceeded, nil)
	if len(res.Ready) != 2 || !contains(res.Ready, l) || !contains(res.Ready, r2) {
		t.Fatalf("fan-out should ready both l and r2, got %v", res.Ready)
	}

	// j has two predecessors; first completion must NOT ready it.
	res = rs.ApplyCompletion(l, TaskStatusSucceeded, nil)
	if contains(res.Ready, j) {
		t.Fatalf("j must wait for both predecessors, got %v", res.Ready)
	}
	res = rs.ApplyCompletion(r2, TaskStatusSucceeded, nil)
	if !contains(res.Ready, j) {
		t.Fatalf("j should ready after both predecessors, got %v", res.Ready)
	}
}

func TestRunState_AllSuccessSkipsOnFailure(t *testing.T) {
	b := newTopoBuilder()
	a, c := b.task(""), b.task(jobdefschema.TriggerRuleAllSuccess)
	b.edge(a, c)
	rs := NewRunState(b.build(), 0)

	res := rs.ApplyCompletion(a, TaskStatusFailed, nil)
	if len(res.Ready) != 0 {
		t.Fatalf("c must not be ready when predecessor failed (all_success), got %v", res.Ready)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].TaskID != c {
		t.Fatalf("c should be skipped, got %v", res.Skipped)
	}
	if !res.Complete {
		t.Fatal("run should be complete (a failed, c skipped)")
	}
	if res.Skipped[0].TerminalSequence != 2 {
		t.Fatalf("skip sequence should be 2 (after a=1), got %d", res.Skipped[0].TerminalSequence)
	}
}

func TestRunState_AllDoneRunsAfterFailure(t *testing.T) {
	b := newTopoBuilder()
	a, c := b.task(""), b.task(jobdefschema.TriggerRuleAllDone)
	b.edge(a, c)
	rs := NewRunState(b.build(), 0)

	res := rs.ApplyCompletion(a, TaskStatusFailed, nil)
	if !contains(res.Ready, c) {
		t.Fatalf("all_done successor should run even after failure, got ready=%v skipped=%v", res.Ready, res.Skipped)
	}
}

func TestRunState_OneSuccess(t *testing.T) {
	b := newTopoBuilder()
	a, a2, c := b.task(""), b.task(""), b.task(jobdefschema.TriggerRuleOneSuccess)
	b.edge(a, c)
	b.edge(a2, c)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(a, TaskStatusFailed, nil)
	res := rs.ApplyCompletion(a2, TaskStatusSucceeded, nil)
	if !contains(res.Ready, c) {
		t.Fatalf("one_success should run when at least one predecessor succeeded, got %v", res.Ready)
	}
}

func TestRunState_SkipPropagates(t *testing.T) {
	// a -> b (all_success) -> c (all_success); a fails => b skipped => c skipped.
	b := newTopoBuilder()
	a := b.task("")
	bb := b.task(jobdefschema.TriggerRuleAllSuccess)
	c := b.task(jobdefschema.TriggerRuleAllSuccess)
	b.edge(a, bb)
	b.edge(bb, c)
	rs := NewRunState(b.build(), 0)

	res := rs.ApplyCompletion(a, TaskStatusFailed, nil)
	if len(res.Skipped) != 2 {
		t.Fatalf("both b and c should skip-propagate, got %v", res.Skipped)
	}
	skippedIDs := []uuid.UUID{res.Skipped[0].TaskID, res.Skipped[1].TaskID}
	if !contains(skippedIDs, bb) || !contains(skippedIDs, c) {
		t.Fatalf("expected b and c skipped, got %v", skippedIDs)
	}
	if !res.Complete {
		t.Fatal("run should be complete")
	}
}

func TestRunState_BranchSkipped(t *testing.T) {
	// a -> b, a -> c.  a completes selecting only b; c is branch-skipped.
	b := newTopoBuilder()
	a, br, c := b.task(""), b.task(""), b.task("")
	b.edge(a, br)
	b.edge(a, c)
	rs := NewRunState(b.build(), 0)

	res := rs.ApplyCompletion(a, TaskStatusSucceeded, []uuid.UUID{c})
	if !contains(res.Ready, br) {
		t.Fatalf("selected branch b should be ready, got %v", res.Ready)
	}
	if contains(res.Ready, c) {
		t.Fatalf("non-selected branch c must not be ready, got %v", res.Ready)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].TaskID != c {
		t.Fatalf("c should be branch-skipped, got %v", res.Skipped)
	}
}

func TestRunState_MarkDispatchedLeavesReady(t *testing.T) {
	b := newTopoBuilder()
	a := b.task("")
	rs := NewRunState(b.build(), 0)

	rs.MarkDispatched(a, "10.0.0.1:9001", 1, 12345)
	if len(rs.ReadyTasks()) != 0 {
		t.Fatal("dispatched task should leave the ready queue")
	}
	st, ok := rs.TaskState(a)
	if !ok || st.Status != TaskStatusRunning || st.ClaimedBy != "10.0.0.1:9001" {
		t.Fatalf("dispatched task state wrong: %+v ok=%v", st, ok)
	}
}

func TestRunState_ApplyCompletionIdempotentOnTerminal(t *testing.T) {
	b := newTopoBuilder()
	a, c := b.task(""), b.task("")
	b.edge(a, c)
	rs := NewRunState(b.build(), 0)

	rs.ApplyCompletion(a, TaskStatusSucceeded, nil)
	seqBefore := rs.Sequence()
	// Re-applying a's completion must not double-advance or re-stamp.
	res := rs.ApplyCompletion(a, TaskStatusSucceeded, nil)
	if res.TerminalSequence != 0 {
		t.Fatalf("re-completing a terminal task should not stamp a sequence, got %d", res.TerminalSequence)
	}
	if rs.Sequence() != seqBefore {
		t.Fatalf("sequence should not advance on duplicate completion: before=%d after=%d", seqBefore, rs.Sequence())
	}
}

func TestRunState_SnapshotRestoreRoundTrip(t *testing.T) {
	b := newTopoBuilder()
	a, l, r2, j := b.task(""), b.task(""), b.task(""), b.task("")
	b.edge(a, l)
	b.edge(a, r2)
	b.edge(l, j)
	b.edge(r2, j)
	topo := b.build()
	rs := NewRunState(topo, 0)

	rs.ApplyCompletion(a, TaskStatusSucceeded, nil)
	rs.MarkDispatched(l, "node-1", 1, 999)
	rs.ApplyCompletion(r2, TaskStatusSucceeded, nil)

	blob, err := rs.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored, err := Restore(topo, blob)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if restored.Sequence() != rs.Sequence() {
		t.Fatalf("sequence mismatch: %d vs %d", restored.Sequence(), rs.Sequence())
	}
	if restored.IsComplete() != rs.IsComplete() {
		t.Fatal("completeness mismatch after restore")
	}

	// Ready sets must match immediately after restore (before any divergent
	// mutation): both have l running (dispatched), r2 + a terminal, j waiting.
	pre := rs.ReadyTasks()
	post := restored.ReadyTasks()
	sortIDs(pre)
	sortIDs(post)
	if len(pre) != len(post) {
		t.Fatalf("ready set size mismatch after restore: %d vs %d", len(pre), len(post))
	}
	for i := range pre {
		if pre[i] != post[i] {
			t.Fatalf("ready set mismatch after restore: %v vs %v", pre, post)
		}
	}

	// Advancement survives the round-trip: completing the still-running l on the
	// restored state should ready the join task j.
	res := restored.ApplyCompletion(l, TaskStatusSucceeded, nil)
	if !contains(res.Ready, j) {
		t.Fatalf("restored state should ready j after l completes, got %v", res.Ready)
	}
}

func sortIDs(ids []uuid.UUID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
}
