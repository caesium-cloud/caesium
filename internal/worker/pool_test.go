package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolWaitsForSubmittedTasks(t *testing.T) {
	pool := NewPool(2)
	var completed int32

	for i := 0; i < 6; i++ {
		if err := pool.Submit(context.Background(), func() {
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&completed, 1)
		}); err != nil {
			t.Fatalf("submit failed: %v", err)
		}
	}

	pool.Wait()

	if got := atomic.LoadInt32(&completed); got != 6 {
		t.Fatalf("expected 6 completed tasks, got %d", got)
	}
}

func TestPoolSubmitHonorsContextCancelWhenFull(t *testing.T) {
	pool := NewPool(1)
	started := make(chan struct{})
	block := make(chan struct{})

	if err := pool.Submit(context.Background(), func() {
		close(started)
		<-block
	}); err != nil {
		t.Fatalf("first submit failed: %v", err)
	}

	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := pool.Submit(ctx, func() {}); err == nil {
		t.Fatal("expected context cancellation error")
	}

	close(block)
	pool.Wait()
}

// TestPoolReservationIsHandedToGo verifies the reservation half of the API:
// TryAcquire fills the semaphore, Go consumes the reservation the caller
// already holds (rather than taking a second one), and the slot comes back when
// the function returns.
func TestPoolReservationIsHandedToGo(t *testing.T) {
	pool := NewPool(1)

	if !pool.TryAcquire() {
		t.Fatal("first TryAcquire on an idle pool must succeed")
	}
	if pool.TryAcquire() {
		t.Fatal("TryAcquire must fail once the only slot is reserved")
	}

	block := make(chan struct{})
	done := make(chan struct{})
	pool.Go(func() {
		<-block
		close(done)
	})

	if pool.TryAcquire() {
		t.Fatal("Pool.Go must run on the caller's reservation, not take another slot")
	}

	close(block)
	<-done
	pool.Wait()

	if !pool.TryAcquire() {
		t.Fatal("slot must be released once the function returns")
	}
	pool.Release()
	if !pool.TryAcquire() {
		t.Fatal("Release must hand an unused reservation back")
	}
	pool.Release()
}

// TestPoolFreeSignalsReleasedSlot verifies Pool.Free wakes a caller parked on a
// full pool as soon as capacity appears, so a saturated worker does not have to
// idle out a full poll interval before it can claim again.
func TestPoolFreeSignalsReleasedSlot(t *testing.T) {
	pool := NewPool(1)

	block := make(chan struct{})
	if err := pool.Submit(context.Background(), func() { <-block }); err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	// Drain any token left by an earlier release so the wait below is about this
	// task's slot only.
	select {
	case <-pool.Free():
	default:
	}

	select {
	case <-pool.Free():
		t.Fatal("Free must not signal while the pool is saturated")
	case <-time.After(20 * time.Millisecond):
	}

	close(block)

	select {
	case <-pool.Free():
	case <-time.After(2 * time.Second):
		t.Fatal("Free was never signalled after the slot was released")
	}
	pool.Wait()
}
