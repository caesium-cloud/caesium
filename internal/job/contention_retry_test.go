package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

// withFastRunStartBackoffs shrinks the run-start retry schedule to n×1ms for
// the duration of a test so exhaustion cases don't sleep the real ~630ms
// budget. Tests using it must not run in parallel while the package var is
// swapped.
func withFastRunStartBackoffs(t *testing.T, n int) {
	t.Helper()
	orig := runStartReadBackoffs
	fast := make([]time.Duration, n)
	for i := range fast {
		fast[i] = time.Millisecond
	}
	runStartReadBackoffs = fast
	t.Cleanup(func() { runStartReadBackoffs = orig })
}

func TestRetryOnContention_SucceedsFirstTry(t *testing.T) {
	withFastRunStartBackoffs(t, 5)
	calls := 0
	err := retryOnContention(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetryOnContention_RetriesThenSucceeds(t *testing.T) {
	withFastRunStartBackoffs(t, 5)
	calls := 0
	err := retryOnContention(context.Background(), func() error {
		calls++
		if calls <= 2 {
			return errors.New("checkpoint in progress")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error after retries, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (two contention then success), got %d", calls)
	}
}

func TestRetryOnContention_NonContentionNotRetried(t *testing.T) {
	withFastRunStartBackoffs(t, 5)
	sentinel := errors.New("record not found")
	calls := 0
	err := retryOnContention(context.Background(), func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a non-contention error must not be retried; got %d calls", calls)
	}
}

func TestRetryOnContention_ExhaustsAndReturnsError(t *testing.T) {
	withFastRunStartBackoffs(t, 3)
	calls := 0
	err := retryOnContention(context.Background(), func() error {
		calls++
		return errors.New("checkpoint in progress")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if want := len(runStartReadBackoffs) + 1; calls != want {
		t.Fatalf("expected %d calls (initial + one per backoff), got %d", want, calls)
	}
}

func TestRetryOnContention_ContextCancelStops(t *testing.T) {
	withFastRunStartBackoffs(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryOnContention(ctx, func() error {
		calls++
		return errors.New("database is locked")
	})
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if calls != 1 {
		t.Fatalf("a pre-cancelled context must stop after the first attempt; got %d calls", calls)
	}
}
