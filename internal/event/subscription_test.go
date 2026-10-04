package event

import (
	"context"
	"errors"
	"testing"
)

type subscriptionBus struct {
	ch     <-chan Event
	err    error
	filter Filter
	ctx    context.Context
}

func (b *subscriptionBus) Publish(Event) {}
func (b *subscriptionBus) Subscribe(ctx context.Context, f Filter) (<-chan Event, error) {
	b.ctx = ctx
	b.filter = f
	return b.ch, b.err
}

func TestRunSubscriptionFailureDoesNotSignalOrCleanUp(t *testing.T) {
	failure := errors.New("subscribe")
	ready := make(chan struct{})
	b := &subscriptionBus{err: failure}
	err := RunSubscription(t.Context(), b, Filter{}, ready, func(Event) { t.Fatal("handle") }, func() error { t.Fatal("cleanup"); return nil })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	select {
	case <-ready:
		t.Fatal("ready signaled on failure")
	default:
	}
}
func TestRunSubscriptionLifetime(t *testing.T) {
	for _, stop := range []string{"cancel", "close"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ch := make(chan Event)
			ready := make(chan struct{})
			handled := make(chan Event)
			cleanup := errors.New("cleanup")
			done := make(chan error, 1)
			cleaned := 0
			b := &subscriptionBus{ch: ch}
			go func() {
				done <- RunSubscription(ctx, b, Filter{Types: []Type{TypeRunCompleted}}, ready, func(evt Event) { handled <- evt }, func() error { cleaned++; return cleanup })
			}()
			<-ready
			if b.ctx != ctx || len(b.filter.Types) != 1 {
				t.Fatal("subscription inputs lost")
			}
			ch <- Event{Type: TypeRunCompleted}
			if (<-handled).Type != TypeRunCompleted {
				t.Fatal("event lost")
			}
			// Unbuffered handler must finish before the subscription can stop.
			if stop == "cancel" {
				cancel()
			} else {
				close(ch)
			}
			if err := <-done; !errors.Is(err, cleanup) {
				t.Fatal(err)
			}
			if cleaned != 1 {
				t.Fatalf("cleanup count %d", cleaned)
			}
		})
	}
}
func TestRunSubscriptionWithoutCleanup(t *testing.T) {
	ch := make(chan Event)
	close(ch)
	if err := RunSubscription(t.Context(), &subscriptionBus{ch: ch}, Filter{}, nil, func(Event) { t.Fatal("handle") }, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunSubscriptionWaitsForSynchronousHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch := make(chan Event, 1)
	ch <- Event{Type: TypeRunCompleted}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunSubscription(ctx, &subscriptionBus{ch: ch}, Filter{}, nil, func(Event) { close(started); <-release }, nil)
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("subscription stopped before handler finished")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
