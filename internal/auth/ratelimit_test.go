package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimiterAllowsUnderThreshold(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)

	require.False(t, rl.RecordFailure("1.2.3.4"))
	require.False(t, rl.RecordFailure("1.2.3.4"))
	require.False(t, rl.RecordFailure("1.2.3.4"))
	require.False(t, rl.IsLimited("1.2.3.4"))
}

func TestRateLimiterBlocksOverThreshold(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute)

	rl.RecordFailure("1.2.3.4")
	rl.RecordFailure("1.2.3.4")
	limited := rl.RecordFailure("1.2.3.4") // 3rd failure, limit is 2
	require.True(t, limited)
	require.True(t, rl.IsLimited("1.2.3.4"))
}

func TestRateLimiterIsolatesIPs(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)

	rl.RecordFailure("1.2.3.4")
	rl.RecordFailure("1.2.3.4") // over limit
	require.True(t, rl.IsLimited("1.2.3.4"))
	require.False(t, rl.IsLimited("5.6.7.8"))
}

func TestRateLimiterRetryAfter(t *testing.T) {
	rl := NewRateLimiter(1, 30*time.Second)

	rl.RecordFailure("1.2.3.4")
	rl.RecordFailure("1.2.3.4")

	retryAfter := rl.RetryAfter("1.2.3.4")
	require.Greater(t, retryAfter, 0)
	require.LessOrEqual(t, retryAfter, 31)
}

func TestRateLimiterCleanup(t *testing.T) {
	rl := NewRateLimiter(1, time.Millisecond)

	rl.RecordFailure("1.2.3.4")
	rl.RecordFailure("1.2.3.4")

	time.Sleep(5 * time.Millisecond)
	rl.Cleanup()
	require.False(t, rl.IsLimited("1.2.3.4"))
}
