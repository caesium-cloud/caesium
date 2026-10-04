package dbretry

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestRetryPolicy(t *testing.T) {
	busy := errors.New("busy")
	other := errors.New("other")
	for _, tc := range []struct {
		name            string
		results         []error
		retryable       func(error) bool
		want            error
		attempts, waits int
	}{
		{"success", []error{nil}, func(error) bool { return true }, nil, 1, 0},
		{"classification", []error{other}, func(err error) bool { return errors.Is(err, busy) }, other, 1, 0},
		{"missing classifier", []error{busy}, nil, busy, 1, 0},
		{"retry success", []error{busy, busy, nil}, func(err error) bool { return errors.Is(err, busy) }, nil, 3, 2},
		{"exhaustion", []error{busy, busy, busy}, func(error) bool { return true }, busy, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var gotWaits []time.Duration
			var order []string
			backoffs := []time.Duration{3 * time.Millisecond, 7 * time.Millisecond}
			err := Retry(context.Background(), Policy{
				Backoffs: backoffs, Retryable: tc.retryable,
				BeforeRetry: func(err error) {
					if err != busy {
						t.Fatalf("BeforeRetry error %v", err)
					}
					order = append(order, "before")
				},
				OnRetry: func(err error) {
					if err != busy {
						t.Fatalf("OnRetry error %v", err)
					}
					order = append(order, "on")
				},
				Wait: func(_ context.Context, d time.Duration) error {
					gotWaits = append(gotWaits, d)
					order = append(order, "wait")
					return nil
				},
			}, func() error { order = append(order, "attempt"); err := tc.results[calls]; calls++; return err })
			if !errors.Is(err, tc.want) || calls != tc.attempts || len(gotWaits) != tc.waits {
				t.Fatalf("err=%v calls=%d waits=%v", err, calls, gotWaits)
			}
			if tc.waits > 0 && !reflect.DeepEqual(gotWaits, backoffs[:tc.waits]) {
				t.Fatalf("schedule = %v", gotWaits)
			}
			wantOrder := []string{"attempt"}
			for range tc.waits {
				wantOrder = append(wantOrder, "before", "on", "wait", "attempt")
			}
			if !reflect.DeepEqual(order, wantOrder) {
				t.Fatalf("order = %v want %v", order, wantOrder)
			}
		})
	}
}

func TestRetryCancellationOrdering(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "operation first", true: "context first"}[before], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			calls, hooks := 0, 0
			err := Retry(ctx, Policy{Backoffs: []time.Duration{1}, BeforeAttempt: before, Retryable: func(error) bool { return true }, OnRetry: func(error) { hooks++ }, Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}, func() error { calls++; return errors.New("busy") })
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			want := 1
			if before {
				want = 0
			}
			if calls != want || hooks != want {
				t.Fatalf("calls=%d hooks=%d", calls, hooks)
			}
		})
	}
}

func TestRetryWaitErrorStopsAndNilWaitIsImmediate(t *testing.T) {
	waitErr := errors.New("wait failed")
	calls := 0
	err := Retry(nil, Policy{Backoffs: []time.Duration{1}, Retryable: func(error) bool { return true }, Wait: func(ctx context.Context, _ time.Duration) error {
		if ctx == nil {
			t.Fatal("nil context")
		}
		return waitErr
	}}, func() error { calls++; return errors.New("busy") })
	if !errors.Is(err, waitErr) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	calls = 0
	err = Retry(nil, Policy{Backoffs: []time.Duration{1}, BeforeAttempt: true, Retryable: func(error) bool { return true }}, func() error {
		calls++
		if calls == 1 {
			return errors.New("busy")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestRetryZeroBudgetHasNoHooks(t *testing.T) {
	busy := errors.New("busy")
	err := Retry(context.Background(), Policy{Retryable: func(error) bool { return true }, BeforeRetry: func(error) { t.Fatal("BeforeRetry") }, OnRetry: func(error) { t.Fatal("OnRetry") }, Wait: func(context.Context, time.Duration) error { t.Fatal("Wait"); return nil }}, func() error { return busy })
	if !errors.Is(err, busy) {
		t.Fatal(err)
	}
}
