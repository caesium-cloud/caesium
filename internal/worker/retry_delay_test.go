package worker

import (
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestWorkerRetryDelayFrozenSelectionAndLegacyFallback(t *testing.T) {
	model := &models.Task{RetryDelay: time.Second, RetryBackoff: true}
	for _, tc := range []struct {
		name    string
		delay   time.Duration
		backoff bool
		want    time.Duration
	}{
		{"frozen constant", 2 * time.Second, false, 2 * time.Second},
		{"frozen exponential", 2 * time.Second, true, 8 * time.Second},
		{"zero frozen fallback", 0, false, 4 * time.Second},
		{"negative frozen fallback", -time.Second, false, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := &models.TaskExecutionDescriptor{Runtime: models.TaskExecutionRuntime{RetryDelay: tc.delay, RetryBackoff: tc.backoff}}
			require.Equal(t, tc.want, workerRetryDelay(desc, model, 3))
		})
	}
	require.Equal(t, 4*time.Second, workerRetryDelay(nil, model, 3))
	require.Zero(t, workerRetryDelay(nil, nil, 3))
}
