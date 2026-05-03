package event

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return NewStore(db)
}

func TestAppendTx(t *testing.T) {
	s := openStore(t)

	jobID := uuid.New()
	runID := uuid.New()

	evt1 := &Event{
		Type:    TypeRunStarted,
		JobID:   jobID,
		RunID:   runID,
		Payload: json.RawMessage(`{"msg":"hello"}`),
	}

	tx := s.db.Begin()
	require.NoError(t, s.AppendTx(tx, evt1))
	require.NoError(t, tx.Commit().Error)

	require.Equal(t, uint64(1), evt1.Sequence, "first event should have sequence 1")
	require.False(t, evt1.Timestamp.IsZero(), "timestamp should be set")

	// Second event auto-increments.
	evt2 := &Event{
		Type:  TypeTaskStarted,
		JobID: jobID,
		RunID: runID,
	}

	tx = s.db.Begin()
	require.NoError(t, s.AppendTx(tx, evt2))
	require.NoError(t, tx.Commit().Error)

	require.Equal(t, uint64(2), evt2.Sequence, "second event should have sequence 2")
}

func TestAppendTxMarksEventPendingBusDispatch(t *testing.T) {
	s := openStore(t)

	evt := &Event{Type: TypeRunStarted, JobID: uuid.New()}
	tx := s.db.Begin()
	require.NoError(t, s.AppendTx(tx, evt))
	require.NoError(t, tx.Commit().Error)

	pending, err := s.ListPendingBusDispatch(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, evt.Sequence, pending[0].Sequence)

	var row models.ExecutionEvent
	require.NoError(t, s.db.First(&row, "sequence = ?", evt.Sequence).Error)
	require.True(t, row.BusDispatchPending)
	require.Nil(t, row.BusDispatchedAt)
}

func TestPublishAndMarkBusDispatchedPublishesAndMarksEvent(t *testing.T) {
	s := openStore(t)
	bus := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := bus.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	evt := &Event{Type: TypeRunStarted, JobID: uuid.New()}
	tx := s.db.Begin()
	require.NoError(t, s.AppendTx(tx, evt))
	require.NoError(t, tx.Commit().Error)

	PublishAndMarkBusDispatched(ctx, bus, s, *evt)

	select {
	case got := <-ch:
		require.Equal(t, evt.Sequence, got.Sequence)
		require.Equal(t, evt.Type, got.Type)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dispatched event")
	}

	pending, err := s.ListPendingBusDispatch(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)

	var row models.ExecutionEvent
	require.NoError(t, s.db.First(&row, "sequence = ?", evt.Sequence).Error)
	require.False(t, row.BusDispatchPending)
	require.NotNil(t, row.BusDispatchedAt)
}

func TestBusDispatcherDispatchOncePublishesPendingEvent(t *testing.T) {
	s := openStore(t)
	bus := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := bus.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	evt := &Event{Type: TypeTaskReady, JobID: uuid.New(), RunID: uuid.New(), TaskID: uuid.New()}
	tx := s.db.Begin()
	require.NoError(t, s.AppendTx(tx, evt))
	require.NoError(t, tx.Commit().Error)

	dispatcher := NewBusDispatcher(s, bus)
	require.NoError(t, dispatcher.DispatchOnce(ctx))

	select {
	case got := <-ch:
		require.Equal(t, evt.Sequence, got.Sequence)
		require.Equal(t, evt.Type, got.Type)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event bus dispatch")
	}

	pending, err := s.ListPendingBusDispatch(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestAppendTx_NilTransaction(t *testing.T) {
	s := openStore(t)
	err := s.AppendTx(nil, &Event{Type: TypeRunStarted})
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires transaction")
}

func TestAppendTx_NilEvent(t *testing.T) {
	s := openStore(t)
	tx := s.db.Begin()
	defer tx.Rollback()
	err := s.AppendTx(tx, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires event")
}

func TestListSince(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	jobA := uuid.New()
	jobB := uuid.New()
	runA := uuid.New()

	events := []Event{
		{Type: TypeRunStarted, JobID: jobA, RunID: runA},
		{Type: TypeTaskStarted, JobID: jobA, RunID: runA},
		{Type: TypeTaskSucceeded, JobID: jobA, RunID: runA},
		{Type: TypeRunStarted, JobID: jobB, RunID: uuid.New()},
	}
	for i := range events {
		tx := s.db.Begin()
		require.NoError(t, s.AppendTx(tx, &events[i]))
		require.NoError(t, tx.Commit().Error)
	}

	tests := []struct {
		name     string
		after    uint64
		filter   Filter
		wantLen  int
		wantSeqs []uint64
	}{
		{
			name:     "all from beginning",
			after:    0,
			filter:   Filter{},
			wantLen:  4,
			wantSeqs: []uint64{1, 2, 3, 4},
		},
		{
			name:     "cursor skips earlier events",
			after:    2,
			filter:   Filter{},
			wantLen:  2,
			wantSeqs: []uint64{3, 4},
		},
		{
			name:    "filter by JobID",
			after:   0,
			filter:  Filter{JobID: jobA},
			wantLen: 3,
		},
		{
			name:    "filter by RunID",
			after:   0,
			filter:  Filter{RunID: runA},
			wantLen: 3,
		},
		{
			name:    "filter by Types",
			after:   0,
			filter:  Filter{Types: []Type{TypeRunStarted}},
			wantLen: 2,
		},
		{
			name:    "combined JobID and Types",
			after:   0,
			filter:  Filter{JobID: jobA, Types: []Type{TypeTaskStarted, TypeTaskSucceeded}},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListSince(ctx, tt.after, 500, tt.filter)
			require.NoError(t, err)
			require.Len(t, got, tt.wantLen)

			// Verify ordering is ascending.
			for i := 1; i < len(got); i++ {
				require.Greater(t, got[i].Sequence, got[i-1].Sequence, "events must be ordered by sequence ASC")
			}

			if len(tt.wantSeqs) > 0 {
				seqs := make([]uint64, len(got))
				for i, e := range got {
					seqs[i] = e.Sequence
				}
				require.Equal(t, tt.wantSeqs, seqs)
			}
		})
	}
}

func TestListSince_EmptyStore(t *testing.T) {
	s := openStore(t)
	got, err := s.ListSince(context.Background(), 0, 100, Filter{})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestLatestSequence(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	// Empty store returns 0.
	seq, err := s.LatestSequence(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(0), seq)

	// After appending events, returns the highest sequence.
	for i := 0; i < 3; i++ {
		evt := &Event{Type: TypeRunStarted, JobID: uuid.New()}
		tx := s.db.Begin()
		require.NoError(t, s.AppendTx(tx, evt))
		require.NoError(t, tx.Commit().Error)
	}

	seq, err = s.LatestSequence(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(3), seq)
}
