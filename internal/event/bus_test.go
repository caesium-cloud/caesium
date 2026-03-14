package event

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
		return Event{}
	}
}

func noRecv(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("expected no event, got %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishSubscribeBasic(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	want := Event{
		Type:      TypeJobCreated,
		JobID:     uuid.New(),
		RunID:     uuid.New(),
		TaskID:    uuid.New(),
		Timestamp: time.Now(),
		Payload:   json.RawMessage(`{"key":"value"}`),
	}
	b.Publish(want)

	got := recv(t, ch)
	assert.Equal(t, want.Type, got.Type)
	assert.Equal(t, want.JobID, got.JobID)
	assert.Equal(t, want.RunID, got.RunID)
	assert.Equal(t, want.TaskID, got.TaskID)
	assert.Equal(t, want.Timestamp, got.Timestamp)
	assert.JSONEq(t, string(want.Payload), string(got.Payload))
}

func TestFilterByType(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{Types: []Type{TypeRunStarted}})
	require.NoError(t, err)

	b.Publish(Event{Type: TypeJobCreated, JobID: uuid.New(), Timestamp: time.Now()})
	b.Publish(Event{Type: TypeRunStarted, JobID: uuid.New(), Timestamp: time.Now()})

	got := recv(t, ch)
	assert.Equal(t, TypeRunStarted, got.Type)

	noRecv(t, ch)
}

func TestFilterByJobID(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targetJobID := uuid.New()
	otherJobID := uuid.New()

	ch, err := b.Subscribe(ctx, Filter{JobID: targetJobID})
	require.NoError(t, err)

	b.Publish(Event{Type: TypeJobCreated, JobID: otherJobID, Timestamp: time.Now()})
	b.Publish(Event{Type: TypeJobCreated, JobID: targetJobID, Timestamp: time.Now()})

	got := recv(t, ch)
	assert.Equal(t, targetJobID, got.JobID)

	noRecv(t, ch)
}

func TestFilterByRunID(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targetRunID := uuid.New()
	otherRunID := uuid.New()

	ch, err := b.Subscribe(ctx, Filter{RunID: targetRunID})
	require.NoError(t, err)

	b.Publish(Event{Type: TypeRunStarted, RunID: otherRunID, Timestamp: time.Now()})
	b.Publish(Event{Type: TypeRunStarted, RunID: targetRunID, Timestamp: time.Now()})

	got := recv(t, ch)
	assert.Equal(t, targetRunID, got.RunID)

	noRecv(t, ch)
}

func TestFilterCombination(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targetJobID := uuid.New()

	ch, err := b.Subscribe(ctx, Filter{
		JobID: targetJobID,
		Types: []Type{TypeRunStarted},
	})
	require.NoError(t, err)

	// Wrong type, right job
	b.Publish(Event{Type: TypeJobCreated, JobID: targetJobID, Timestamp: time.Now()})
	// Right type, wrong job
	b.Publish(Event{Type: TypeRunStarted, JobID: uuid.New(), Timestamp: time.Now()})
	// Both match
	b.Publish(Event{Type: TypeRunStarted, JobID: targetJobID, Timestamp: time.Now()})

	got := recv(t, ch)
	assert.Equal(t, TypeRunStarted, got.Type)
	assert.Equal(t, targetJobID, got.JobID)

	noRecv(t, ch)
}

func TestEmptyFilterMatchesAll(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	types := []Type{
		TypeJobCreated, TypeJobDeleted,
		TypeRunStarted, TypeRunCompleted, TypeRunFailed,
		TypeTaskStarted, TypeTaskSucceeded, TypeTaskFailed, TypeTaskSkipped,
		TypeLogChunk,
	}

	for _, typ := range types {
		b.Publish(Event{Type: typ, Timestamp: time.Now()})
	}

	for _, typ := range types {
		got := recv(t, ch)
		assert.Equal(t, typ, got.Type)
	}
}

func TestMultipleSubscribers(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobID := uuid.New()

	ch1, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)
	ch2, err := b.Subscribe(ctx, Filter{JobID: jobID})
	require.NoError(t, err)
	ch3, err := b.Subscribe(ctx, Filter{Types: []Type{TypeRunStarted}})
	require.NoError(t, err)

	evt := Event{Type: TypeRunStarted, JobID: jobID, Timestamp: time.Now()}
	b.Publish(evt)

	got1 := recv(t, ch1)
	got2 := recv(t, ch2)
	got3 := recv(t, ch3)

	assert.Equal(t, evt.Type, got1.Type)
	assert.Equal(t, evt.Type, got2.Type)
	assert.Equal(t, evt.Type, got3.Type)
}

func TestEventDropWhenBufferFull(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	// Publish 101 events without reading - should not deadlock or panic
	for i := 0; i < 101; i++ {
		b.Publish(Event{Type: TypeLogChunk, Timestamp: time.Now()})
	}

	// Drain and count
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	assert.Equal(t, 100, count)
}

func TestContextCancellationCleansUp(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	cancel()
	time.Sleep(50 * time.Millisecond)

	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after context cancellation")
}

func TestSubscribeAfterPublish(t *testing.T) {
	b := New()

	b.Publish(Event{Type: TypeJobCreated, Timestamp: time.Now()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	noRecv(t, ch)
}

func TestConcurrentPublishSafety(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	const goroutines = 100
	const eventsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				b.Publish(Event{Type: TypeLogChunk, Timestamp: time.Now()})
			}
		}()
	}

	// Drain in a separate goroutine
	received := make(chan int, 1)
	go func() {
		count := 0
		for {
			select {
			case <-ch:
				count++
			case <-time.After(2 * time.Second):
				received <- count
				return
			}
		}
	}()

	wg.Wait()
	count := <-received
	// Channel buffer is 100, total published is 10000.
	// Some will be dropped but we should receive many.
	assert.Greater(t, count, 0)
}

func TestConcurrentSubscribeUnsubscribe(t *testing.T) {
	b := New()

	const goroutines = 50
	var wg sync.WaitGroup

	// Continuously publish events
	publishCtx, publishCancel := context.WithCancel(context.Background())
	defer publishCancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-publishCtx.Done():
				return
			default:
				b.Publish(Event{Type: TypeLogChunk, Timestamp: time.Now()})
			}
		}
	}()

	// Goroutines subscribing and unsubscribing
	var subWg sync.WaitGroup
	subWg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer subWg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			ch, err := b.Subscribe(ctx, Filter{})
			if err != nil {
				cancel()
				return
			}
			// Read a few events
			for j := 0; j < 5; j++ {
				select {
				case <-ch:
				case <-time.After(100 * time.Millisecond):
				}
			}
			cancel()
			time.Sleep(10 * time.Millisecond)
		}()
	}

	subWg.Wait()
	publishCancel()
	wg.Wait()
	// Test passes if no panics or races occurred
}

func TestEventFieldsPreserved(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := b.Subscribe(ctx, Filter{})
	require.NoError(t, err)

	payload := json.RawMessage(`{"details":{"nested":true,"count":42}}`)
	taskID := uuid.New()
	jobID := uuid.New()
	runID := uuid.New()
	ts := time.Now().Truncate(time.Millisecond)

	want := Event{
		Type:      TypeTaskSucceeded,
		JobID:     jobID,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: ts,
		Payload:   payload,
	}
	b.Publish(want)

	got := recv(t, ch)
	assert.Equal(t, TypeTaskSucceeded, got.Type)
	assert.Equal(t, jobID, got.JobID)
	assert.Equal(t, runID, got.RunID)
	assert.Equal(t, taskID, got.TaskID)
	assert.Equal(t, ts, got.Timestamp)
	assert.JSONEq(t, string(payload), string(got.Payload))
}
