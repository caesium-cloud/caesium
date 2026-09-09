package waitbudget

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock advances only when the code under test sleeps, so a test can run a
// wait loop to completion instantly and read off exactly how long it would have
// waited.
type fakeClock struct {
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	if d > 0 {
		c.now = c.now.Add(d)
	}
}

func (c *fakeClock) option() Option { return WithClock(c.Now, c.Sleep) }

// TestBudgetBoundsTotalWaitUnderContinuousContention is the regression this
// package exists for. The loop below is the shape of the claim-order pending
// wait: an outer sampling loop, and inside it one bounded wait per subject that
// ALWAYS blocks for its whole grace because the contention never clears. With
// per-check timeouts the total would be the outer bound plus a full final
// iteration (6s + 3×3s = 15s); with a single budget it is the budget, exactly.
func TestBudgetBoundsTotalWaitUnderContinuousContention(t *testing.T) {
	const (
		total   = 6 * time.Second
		grace   = 3 * time.Second
		cadence = 250 * time.Millisecond
	)

	clock := newFakeClock()
	start := clock.Now()
	budget := New(total, clock.option())

	subjects := []string{"high", "normal", "low"}
	samples := 0
	checks := 0
	for !budget.Expired() {
		samples++
		for range subjects {
			slice := budget.Slice(grace)
			if slice <= 0 {
				break // the budget is spent; do not start another check
			}
			checks++
			// Continuous contention: the subject never clears, so the wait
			// burns its whole slice.
			clock.Sleep(slice)
		}
		budget.Sleep(cadence)
	}

	elapsed := clock.Now().Sub(start)
	require.LessOrEqual(t, elapsed, total,
		"a budgeted loop must never wait longer than the budget it was given")
	require.Equal(t, total, elapsed,
		"and it must not exit early either: the whole budget is there to be spent")
	require.Positive(t, samples)
	require.Positive(t, checks)
}

// A loop whose checks all clear immediately must still spend its budget on
// sampling rather than spinning out early, and must still stop at the deadline.
func TestBudgetBoundsTotalWaitWhenChecksClearImmediately(t *testing.T) {
	const (
		total   = 5 * time.Second
		cadence = 100 * time.Millisecond
	)

	clock := newFakeClock()
	start := clock.Now()
	budget := New(total, clock.option())

	samples := 0
	for !budget.Expired() {
		samples++
		budget.Sleep(cadence)
	}

	require.Equal(t, total, clock.Now().Sub(start))
	require.Equal(t, int(total/cadence), samples)
}

func TestSliceNeverExceedsRemaining(t *testing.T) {
	clock := newFakeClock()
	budget := New(time.Second, clock.option())

	require.Equal(t, 400*time.Millisecond, budget.Slice(400*time.Millisecond))
	require.Equal(t, time.Second, budget.Slice(10*time.Second),
		"a per-check timeout larger than the budget is clamped to what is left")

	clock.Sleep(900 * time.Millisecond)
	require.Equal(t, 100*time.Millisecond, budget.Slice(400*time.Millisecond))

	clock.Sleep(500 * time.Millisecond)
	require.Zero(t, budget.Remaining(), "remaining is clamped at zero, never negative")
	require.Zero(t, budget.Slice(400*time.Millisecond))
	require.True(t, budget.Expired())
}

func TestSleepIsClampedToTheDeadline(t *testing.T) {
	clock := newFakeClock()
	start := clock.Now()
	budget := New(300*time.Millisecond, clock.option())

	budget.Sleep(time.Hour)
	require.Equal(t, 300*time.Millisecond, clock.Now().Sub(start))
	require.True(t, budget.Expired())

	budget.Sleep(time.Hour)
	require.Equal(t, 300*time.Millisecond, clock.Now().Sub(start),
		"sleeping on a spent budget must not wait at all")
}

func TestNonPositiveTotalIsAlreadyExpired(t *testing.T) {
	clock := newFakeClock()
	start := clock.Now()

	for _, total := range []time.Duration{0, -time.Second} {
		budget := New(total, clock.option())
		require.True(t, budget.Expired())
		require.Zero(t, budget.Slice(time.Second))
		budget.Sleep(time.Second)
	}
	require.Equal(t, time.Duration(0), clock.Now().Sub(start))
}

func TestDeadlineIsComputedOnceUpFront(t *testing.T) {
	clock := newFakeClock()
	budget := New(2*time.Second, clock.option())
	deadline := budget.Deadline()

	clock.Sleep(time.Second)
	require.Equal(t, deadline, budget.Deadline(),
		"the deadline must not drift as the loop makes progress")
	require.Equal(t, time.Second, budget.Remaining())
}

func TestDefaultClockIsWallClock(t *testing.T) {
	start := time.Now()
	budget := New(250 * time.Millisecond)

	budget.Sleep(time.Hour)
	require.True(t, budget.Expired())
	require.Less(t, time.Since(start), 30*time.Second,
		"the sleep must be clamped to the budget, not the requested hour")
}
