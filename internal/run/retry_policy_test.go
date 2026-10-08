package run

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestComputeRetryDelayPreservesLegacyFormula(t *testing.T) {
	for _, tc := range []struct {
		base    time.Duration
		backoff bool
		attempt int
		want    time.Duration
	}{
		{0, true, 2, 0}, {-time.Second, true, 2, 0}, {time.Second, false, 3, time.Second},
		{time.Second, true, 1, time.Second}, {time.Second, true, 3, 4 * time.Second},
		{time.Second, false, 0, time.Second}, {time.Second, true, 0, 0},
		{1, true, 64, time.Duration(-1 << 63)},
	} {
		require.Equal(t, tc.want, ComputeRetryDelay(tc.base, tc.backoff, tc.attempt))
	}
}
