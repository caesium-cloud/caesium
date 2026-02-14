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
