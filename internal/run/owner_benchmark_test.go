package run

import (
	"testing"

	"github.com/google/uuid"
)

// Hot-path owner-engine benchmarks. Hermetic: in-memory RunState only, no
// store, no Docker. just unit-test compiles them with the rest of ./internal/run;
// run with -bench=BenchmarkOwner to measure.

func linearTopo(n int) (RunTopology, []uuid.UUID) {
	b := newTopoBuilder()
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		ids[i] = b.task("")
		if i > 0 {
			b.edge(ids[i-1], ids[i])
		}
	}
	return b.build(), ids
}

func wideTopo(width int) (RunTopology, uuid.UUID, []uuid.UUID, uuid.UUID) {
	b := newTopoBuilder()
	root := b.task("")
	leaves := make([]uuid.UUID, width)
	for i := range leaves {
		leaves[i] = b.task("")
		b.edge(root, leaves[i])
	}
	join := b.task("")
	for _, leaf := range leaves {
		b.edge(leaf, join)
	}
	return b.build(), root, leaves, join
}

func BenchmarkOwnerNewRunStateLinear64(b *testing.B) {
	topo, _ := linearTopo(64)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rs := NewRunState(topo, 0)
		if rs.total != 64 {
			b.Fatalf("total=%d", rs.total)
		}
	}
}

func BenchmarkOwnerApplyCompletionLinear64(b *testing.B) {
	topo, ids := linearTopo(64)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rs := NewRunState(topo, 0)
		for _, id := range ids {
			res := rs.ApplyCompletion(id, TaskStatusSucceeded, nil)
			if !res.Applied {
				b.Fatal("completion not applied")
			}
		}
		if !rs.IsComplete() {
			b.Fatal("run not complete")
		}
	}
}

func BenchmarkOwnerApplyCompletionWide32(b *testing.B) {
	topo, root, leaves, join := wideTopo(32)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rs := NewRunState(topo, 0)
		if res := rs.ApplyCompletion(root, TaskStatusSucceeded, nil); !res.Applied {
			b.Fatal("root not applied")
		}
		for _, id := range leaves {
			if res := rs.ApplyCompletion(id, TaskStatusSucceeded, nil); !res.Applied {
				b.Fatal("leaf not applied")
			}
		}
		if res := rs.ApplyCompletion(join, TaskStatusSucceeded, nil); !res.Applied {
			b.Fatal("join not applied")
		}
		if !rs.IsComplete() {
			b.Fatal("run not complete")
		}
	}
}

func BenchmarkOwnerDispatchCompleteCycleLinear32(b *testing.B) {
	topo, ids := linearTopo(32)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rs := NewRunState(topo, 0)
		for _, id := range ids {
			rs.MarkDispatched(id, "bench-node", 1, 1_000)
			if res := rs.ApplyCompletion(id, TaskStatusSucceeded, nil); !res.Applied {
				b.Fatal("completion not applied")
			}
		}
		if !rs.IsComplete() {
			b.Fatal("run not complete")
		}
	}
}

func BenchmarkOwnerReadyTasksWide32(b *testing.B) {
	topo, root, _, _ := wideTopo(32)
	rs := NewRunState(topo, 0)
	if res := rs.ApplyCompletion(root, TaskStatusSucceeded, nil); !res.Applied {
		b.Fatal("root not applied")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ready := rs.ReadyTasks()
		if len(ready) != 32 {
			b.Fatalf("ready=%d", len(ready))
		}
	}
}

func BenchmarkOwnerSnapshotLinear64(b *testing.B) {
	topo, ids := linearTopo(64)
	rs := NewRunState(topo, 0)
	for i := 0; i < 32; i++ {
		rs.MarkDispatched(ids[i], "bench-node", 1, 1_000)
		if res := rs.ApplyCompletion(ids[i], TaskStatusSucceeded, nil); !res.Applied {
			b.Fatal("completion not applied")
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		blob, err := rs.Snapshot()
		if err != nil {
			b.Fatal(err)
		}
		if len(blob) == 0 {
			b.Fatal("empty snapshot")
		}
	}
}
