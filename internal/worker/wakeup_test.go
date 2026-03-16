package worker

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
)

func TestSubscribeWakeups_NilBus(t *testing.T) {
	ch := SubscribeWakeups(context.Background(), nil)
	if ch != nil {
		t.Fatal("expected nil channel for nil bus")
	}
}

func TestSubscribeWakeups_WakesOnTaskReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := event.New()
	ch := SubscribeWakeups(ctx, b)

	b.Publish(event.Event{Type: event.TypeTaskReady})
	assertSignal(t, ch, "TypeTaskReady")
}

func TestSubscribeWakeups_WakesOnTaskLeaseExpired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := event.New()
	ch := SubscribeWakeups(ctx, b)

	b.Publish(event.Event{Type: event.TypeTaskLeaseExpired})
	assertSignal(t, ch, "TypeTaskLeaseExpired")
}

func TestSubscribeWakeups_WakesOnRunStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := event.New()
	ch := SubscribeWakeups(ctx, b)

	b.Publish(event.Event{Type: event.TypeRunStarted})
	assertSignal(t, ch, "TypeRunStarted")
}

func TestSubscribeWakeups_IgnoresIrrelevantEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := event.New()
	ch := SubscribeWakeups(ctx, b)

	b.Publish(event.Event{Type: event.TypeLogChunk})

	select {
	case <-ch:
		t.Fatal("received unexpected wakeup for irrelevant event")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestSubscribeWakeups_Coalesces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := event.New()
	ch := SubscribeWakeups(ctx, b)

	// Publish several events rapidly.
	for i := 0; i < 10; i++ {
		b.Publish(event.Event{Type: event.TypeTaskReady})
	}

	// Let the goroutine consume all bus events before we drain.
	time.Sleep(50 * time.Millisecond)

	// Drain the first signal.
	assertSignal(t, ch, "first coalesced signal")

	// No second signal should be immediately available.
	select {
	case <-ch:
		t.Fatal("expected events to be coalesced into a single signal")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestSubscribeWakeups_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	b := event.New()
	ch := SubscribeWakeups(ctx, b)

	cancel()

	// The channel should be closed after context cancellation.
	select {
	case _, ok := <-ch:
		if ok {
			// Got a signal before close — drain and wait for close.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Fatal("channel not closed after context cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for channel close")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func assertSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed unexpectedly", label)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s: timed out waiting for wakeup signal", label)
	}
}
