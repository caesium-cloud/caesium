package run

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// Hot-path recovery benchmarks. Hermetic: RecoverRunState / Restore over
// in-memory topologies and terminal rows, no store, no Docker.

func succeededRows(ids []uuid.UUID) []models.TaskRun {
	rows := make([]models.TaskRun, len(ids))
	for i, id := range ids {
		rows[i] = models.TaskRun{TaskID: id, Status: string(TaskStatusSucceeded), TerminalSequence: int64(i + 1)}
	}
	return rows
}

func BenchmarkRecoverFromScratchLinear64(b *testing.B) {
	topo, ids := linearTopo(64)
	rows := succeededRows(ids)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rs, res, err := RecoverRunState(topo, nil, rows)
		if err != nil {
			b.Fatal(err)
		}
		if !res.Complete || rs == nil {
			b.Fatal("expected a complete recovered run")
		}
	}
}

func BenchmarkRecoverFromScratchWide32(b *testing.B) {
	topo, root, leaves, join := wideTopo(32)
	ids := make([]uuid.UUID, 0, 2+len(leaves))
	ids = append(ids, root)
	ids = append(ids, leaves...)
	ids = append(ids, join)
	rows := succeededRows(ids)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, res, err := RecoverRunState(topo, nil, rows)
		if err != nil {
			b.Fatal(err)
		}
		if !res.Complete {
			b.Fatal("expected a complete recovered run")
		}
	}
}

func BenchmarkRecoverCheckpointPlusTailLinear64(b *testing.B) {
	topo, ids := linearTopo(64)
	live := NewRunState(topo, 0)
	for i := 0; i < 32; i++ {
		live.MarkDispatched(ids[i], "bench-node", 1, 1_000)
		if res := live.ApplyCompletion(ids[i], TaskStatusSucceeded, nil); !res.Applied {
			b.Fatal("completion not applied")
		}
	}
	blob, err := live.Snapshot()
	if err != nil {
		b.Fatal(err)
	}
	checkpoint := &models.RunCheckpoint{SequenceHigh: 32, StateBlob: blob}
	tail := succeededRows(ids[32:48])
	for i := range tail {
		tail[i].TerminalSequence = int64(33 + i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, res, err := RecoverRunState(topo, checkpoint, tail)
		if err != nil {
			b.Fatal(err)
		}
		if res.Complete {
			b.Fatal("tail is partial; run must not be complete")
		}
		if len(res.Ready) == 0 && len(res.ReDispatch) == 0 {
			b.Fatal("expected remaining work after partial recovery")
		}
	}
}

func BenchmarkRecoverRestoreLinear64(b *testing.B) {
	topo, ids := linearTopo(64)
	live := NewRunState(topo, 0)
	for i := 0; i < 40; i++ {
		if res := live.ApplyCompletion(ids[i], TaskStatusSucceeded, nil); !res.Applied {
			b.Fatal("completion not applied")
		}
	}
	blob, err := live.Snapshot()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rs, err := Restore(topo, blob)
		if err != nil {
			b.Fatal(err)
		}
		if rs.Sequence() != 40 {
			b.Fatalf("seq=%d", rs.Sequence())
		}
	}
}

func BenchmarkRecoverRunningReDispatchLinear32(b *testing.B) {
	topo, ids := linearTopo(32)
	live := NewRunState(topo, 0)
	for i := 0; i < 16; i++ {
		if res := live.ApplyCompletion(ids[i], TaskStatusSucceeded, nil); !res.Applied {
			b.Fatal("completion not applied")
		}
	}
	live.MarkDispatched(ids[16], "dead-node", 1, 1_000)
	blob, err := live.Snapshot()
	if err != nil {
		b.Fatal(err)
	}
	checkpoint := &models.RunCheckpoint{SequenceHigh: 16, StateBlob: blob}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, res, err := RecoverRunState(topo, checkpoint, nil)
		if err != nil {
			b.Fatal(err)
		}
		if len(res.ReDispatch) != 1 {
			b.Fatalf("redispatch=%d", len(res.ReDispatch))
		}
	}
}
