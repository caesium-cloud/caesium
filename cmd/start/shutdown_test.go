package start

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownCoordinatorIdempotentAndWaitsForAsync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	var apiShutdowns int32
	var internalShutdowns int32
	var closeDBs int32

	coordinator := newShutdownCoordinator(shutdownConfig{
		cancel:      cancel,
		gracePeriod: time.Second,
		apiShutdown: func(context.Context) error {
			atomic.AddInt32(&apiShutdowns, 1)
			return nil
		},
		internalShutdown: func(context.Context) error {
			atomic.AddInt32(&internalShutdowns, 1)
			return nil
		},
		closeDB: func() error {
			atomic.AddInt32(&closeDBs, 1)
			return nil
		},
	})
	deactivate := activateShutdownCoordinator(coordinator)
	defer deactivate()

	coordinator.runAsync(func() {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		close(finished)
	})
	<-started

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- shutdown()
	}()
	go func() {
		secondDone <- shutdown()
	}()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel root context")
	}

	select {
	case err := <-firstDone:
		t.Fatalf("shutdown returned before tracked goroutine stopped: %v", err)
	case err := <-secondDone:
		t.Fatalf("second shutdown returned before tracked goroutine stopped: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("tracked goroutine did not finish")
	}

	for _, ch := range []chan error{firstDone, secondDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("shutdown returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not return")
		}
	}

	if got := atomic.LoadInt32(&apiShutdowns); got != 1 {
		t.Fatalf("api shutdown count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&internalShutdowns); got != 1 {
		t.Fatalf("internal shutdown count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&closeDBs); got != 1 {
		t.Fatalf("db close count = %d, want 1", got)
	}
}
