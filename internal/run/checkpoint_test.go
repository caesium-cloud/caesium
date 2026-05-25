package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func TestCheckpointStore_WriteLoadLatest(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID := uuid.New()

	if cp, err := store.LatestFullCheckpoint(runID); err != nil || cp != nil {
		t.Fatalf("expected no checkpoint yet, got cp=%v err=%v", cp, err)
	}

	if err := store.WriteCheckpoint(runID, 5, 1, []byte(`{"sequence":5}`), false); err != nil {
		t.Fatalf("write checkpoint 5: %v", err)
	}
	if err := store.WriteCheckpoint(runID, 12, 1, []byte(`{"sequence":12}`), false); err != nil {
		t.Fatalf("write checkpoint 12: %v", err)
	}

	cp, err := store.LatestFullCheckpoint(runID)
	if err != nil || cp == nil {
		t.Fatalf("expected a checkpoint, got cp=%v err=%v", cp, err)
	}
	if cp.SequenceHigh != 12 {
		t.Fatalf("latest full should be seq 12, got %d", cp.SequenceHigh)
	}
	if string(cp.StateBlob) != `{"sequence":12}` {
		t.Fatalf("blob mismatch: %s", cp.StateBlob)
	}
}

func TestCheckpointStore_RewriteSameSequence(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID := uuid.New()

	if err := store.WriteCheckpoint(runID, 7, 1, []byte("v1"), false); err != nil {
		t.Fatal(err)
	}
	// Re-writing the same (run_id, sequence_high) overwrites, not errors.
	if err := store.WriteCheckpoint(runID, 7, 2, []byte("v2"), false); err != nil {
		t.Fatalf("rewrite at same sequence should upsert: %v", err)
	}
	cp, _ := store.LatestFullCheckpoint(runID)
	if cp == nil || string(cp.StateBlob) != "v2" || cp.OwnerGeneration != 2 {
		t.Fatalf("expected overwritten checkpoint v2/gen2, got %+v", cp)
	}
}

func TestCheckpointStore_Prune(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID := uuid.New()

	for _, seq := range []int64{1, 2, 3, 4, 5} {
		if err := store.WriteCheckpoint(runID, seq, 1, []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PruneCheckpoints(runID, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// Only the 2 most recent fulls (4, 5) should remain.
	var remaining []int64
	if err := db.Model(&models.RunCheckpoint{}).
		Where("run_id = ?", runID.String()).
		Order("sequence_high ASC").
		Pluck("sequence_high", &remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0] != 4 || remaining[1] != 5 {
		t.Fatalf("expected [4 5] after prune keep=2, got %v", remaining)
	}

	if err := store.DeleteCheckpoints(runID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if cp, _ := store.LatestFullCheckpoint(runID); cp != nil {
		t.Fatal("expected no checkpoints after delete")
	}
}

// fakePersister records WriteCheckpoint/PruneCheckpoints calls for writer tests.
type fakePersister struct {
	writes  int
	prunes  int
	lastSeq int64
}

func (f *fakePersister) WriteCheckpoint(_ uuid.UUID, seq, _ int64, _ []byte, _ bool) error {
	f.writes++
	f.lastSeq = seq
	return nil
}
func (f *fakePersister) PruneCheckpoints(_ uuid.UUID, _ int) error {
	f.prunes++
	return nil
}

func singleTaskState(t *testing.T) *RunState {
	t.Helper()
	b := newTopoBuilder()
	b.task("")
	b.task("")
	rs := NewRunState(b.build(), 0)
	return rs
}

func TestCheckpointWriter_DueByEvents(t *testing.T) {
	f := &fakePersister{}
	w := NewCheckpointWriter(f, uuid.New(), CheckpointConfig{Events: 2, Interval: time.Hour, KeepFulls: 3})

	rs := singleTaskState(t)
	ids := rs.ReadyTasks()

	// 1 terminal transition: below the events threshold (2), not due.
	rs.ApplyCompletion(ids[0], TaskStatusSucceeded, nil)
	if err := w.Maybe(rs, 1); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatalf("should not checkpoint after 1 event (threshold 2), writes=%d", f.writes)
	}

	// 2nd terminal transition reaches the threshold → checkpoint.
	rs.ApplyCompletion(ids[1], TaskStatusSucceeded, nil)
	if err := w.Maybe(rs, 1); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 || f.prunes != 1 {
		t.Fatalf("should checkpoint+prune at the events threshold, writes=%d prunes=%d", f.writes, f.prunes)
	}
	if f.lastSeq != 2 {
		t.Fatalf("checkpoint should cover sequence 2, got %d", f.lastSeq)
	}
}

func TestCheckpointWriter_DueByInterval(t *testing.T) {
	f := &fakePersister{}
	w := NewCheckpointWriter(f, uuid.New(), CheckpointConfig{Events: 1000, Interval: time.Second, KeepFulls: 3})

	clock := time.Now()
	w.now = func() time.Time { return clock }
	w.lastAt = clock

	rs := singleTaskState(t)
	ids := rs.ReadyTasks()
	rs.ApplyCompletion(ids[0], TaskStatusSucceeded, nil)

	// Below the events threshold; no time elapsed → not due.
	if err := w.Maybe(rs, 1); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatalf("not due before interval elapses, writes=%d", f.writes)
	}

	// Advance the clock past the interval → due by time.
	clock = clock.Add(2 * time.Second)
	if err := w.Maybe(rs, 1); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatalf("should checkpoint once the interval elapses, writes=%d", f.writes)
	}
}

func TestCheckpointWriter_ForceAlwaysWrites(t *testing.T) {
	f := &fakePersister{}
	w := NewCheckpointWriter(f, uuid.New(), CheckpointConfig{Events: 1000, Interval: time.Hour, KeepFulls: 3})
	rs := singleTaskState(t)

	if err := w.Force(rs, 1); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatalf("Force should always write, writes=%d", f.writes)
	}
}
