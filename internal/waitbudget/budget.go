// Package waitbudget bounds a poll-and-wait loop with ONE deadline.
//
// The pattern it replaces is a sampling loop whose outer bound and whose
// per-check timeout are independent constants:
//
//	deadline := time.Now().Add(6 * time.Second)
//	for time.Now().Before(deadline) {
//	    for _, subject := range subjects {
//	        awaitSomething(subject, 3*time.Second) // its own timeout, every time
//	    }
//	}
//
// The outer condition is only re-tested between iterations, so the real worst
// case is the outer bound PLUS a whole final iteration of per-check timeouts —
// here 6s + 3×3s = 15s, two and a half times the number a reader takes from the
// code. Under continuous contention (every check hits its full timeout) that
// overshoot is what the loop actually costs, which is how a bounded-looking
// wait becomes an unbounded-in-practice one.
//
// A Budget makes the deadline the single source of truth: it is computed once,
// up front, and EVERY per-check timeout is a slice of what is left of it. The
// loop can then never run longer than the budget it was given, no matter how
// many checks it makes or how long each one blocks.
package waitbudget

import "time"

// Budget is one overall deadline that every wait in a loop draws from.
//
// It is not safe for concurrent use; a Budget belongs to the single goroutine
// running the loop it bounds.
type Budget struct {
	deadline time.Time
	now      func() time.Time
	sleep    func(time.Duration)
}

// Option customises a Budget.
type Option func(*Budget)

// WithClock injects the time source and the sleep function, so a test can prove
// a loop's total wait is bounded without actually waiting. Both must be
// non-nil to take effect.
func WithClock(now func() time.Time, sleep func(time.Duration)) Option {
	return func(b *Budget) {
		if now == nil || sleep == nil {
			return
		}
		b.now = now
		b.sleep = sleep
	}
}

// New starts a budget of total duration from now. A non-positive total yields a
// budget that is already expired, so a caller that mis-configures the total
// waits for nothing rather than forever.
func New(total time.Duration, opts ...Option) *Budget {
	b := &Budget{now: time.Now, sleep: time.Sleep}
	for _, opt := range opts {
		opt(b)
	}
	b.deadline = b.now().Add(total)
	return b
}

// Deadline is the single instant the budget was computed against.
func (b *Budget) Deadline() time.Time { return b.deadline }

// Remaining is how much of the budget is left, never negative.
func (b *Budget) Remaining() time.Duration {
	remaining := b.deadline.Sub(b.now())
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Expired reports whether the budget is spent.
func (b *Budget) Expired() bool { return b.Remaining() <= 0 }

// Slice is the timeout to give one check: at most max, and never more than what
// is left of the budget. It is 0 once the budget is spent, which callers should
// read as "stop, do not start another check".
func (b *Budget) Slice(max time.Duration) time.Duration {
	remaining := b.Remaining()
	if max <= 0 || max > remaining {
		return remaining
	}
	return max
}

// Sleep pauses for at most d, and never past the deadline, so a sampling
// cadence cannot overshoot the budget either.
func (b *Budget) Sleep(d time.Duration) {
	if slice := b.Slice(d); slice > 0 {
		b.sleep(slice)
	}
}
