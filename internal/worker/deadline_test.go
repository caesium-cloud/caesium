package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

func TestAttemptContextAppliesTaskTimeout(t *testing.T) {
	now := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)
	timeouts := executionTimeouts{
		taskTimeout: 15 * time.Second,
		taskID:      "transform",
	}

	ctx, cancel := timeouts.attemptContext(context.Background(), now)
	defer cancel()

	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, now.Add(15*time.Second), deadline)
	requireContextDone(t, ctx)
	requireTaskDeadlineCause(t, context.Cause(ctx), "transform", 15*time.Second)
}

func TestAttemptContextChoosesExhaustedBudgetCause(t *testing.T) {
	now := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name           string
		runDeadline    time.Time
		wantDeadline   time.Time
		wantRunTimeout bool
	}{
		{
			name:           "run deadline earlier",
			runDeadline:    now.Add(5 * time.Second),
			wantDeadline:   now.Add(5 * time.Second),
			wantRunTimeout: true,
		},
		{
			name:           "run deadline later",
			runDeadline:    now.Add(20 * time.Second),
			wantDeadline:   now.Add(10 * time.Second),
			wantRunTimeout: false,
		},
		{
			name:           "equal deadline exhausts run budget",
			runDeadline:    now.Add(10 * time.Second),
			wantDeadline:   now.Add(10 * time.Second),
			wantRunTimeout: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			timeouts := executionTimeouts{
				taskTimeout: 10 * time.Second,
				runTimeout:  time.Minute,
				runDeadline: tc.runDeadline,
				taskID:      "load",
			}

			ctx, cancel := timeouts.attemptContext(context.Background(), now)
			defer cancel()

			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.Equal(t, tc.wantDeadline, deadline)
			requireContextDone(t, ctx)
			if tc.wantRunTimeout {
				requireRunDeadlineCause(t, context.Cause(ctx), time.Minute)
				return
			}
			requireTaskDeadlineCause(t, context.Cause(ctx), "load", 10*time.Second)
		})
	}
}

func TestAttemptContextWithoutConfiguredTimeoutPreservesParent(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		parentDeadline := time.Date(2099, time.January, 2, 3, 4, 5, 0, time.UTC)
		parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
		defer cancelParent()

		ctx, cancel := (executionTimeouts{}).attemptContext(parent, time.Time{})
		defer cancel()

		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Equal(t, parentDeadline, deadline)
	})

	t.Run("cancellation cause", func(t *testing.T) {
		parent, cancelParent := context.WithCancelCause(context.Background())
		ctx, cancel := (executionTimeouts{}).attemptContext(parent, time.Time{})
		defer cancel()

		parentErr := errors.New("worker shutting down")
		cancelParent(parentErr)
		requireContextDone(t, ctx)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		require.ErrorIs(t, context.Cause(ctx), parentErr)
	})
}

func TestAttemptContextWithExpiredRunDeadline(t *testing.T) {
	now := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)
	timeouts := executionTimeouts{
		runTimeout:  30 * time.Second,
		runDeadline: now.Add(-time.Nanosecond),
	}

	ctx, cancel := timeouts.attemptContext(context.Background(), now)
	defer cancel()

	requireContextDone(t, ctx)
	requireRunDeadlineCause(t, context.Cause(ctx), 30*time.Second)
}

func TestRunDeadlineErrorBoundary(t *testing.T) {
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name        string
		runDeadline time.Time
		wantError   bool
	}{
		{name: "not configured"},
		{name: "before deadline", runDeadline: now.Add(time.Nanosecond)},
		{name: "at deadline", runDeadline: now, wantError: true},
		{name: "after deadline", runDeadline: now.Add(-time.Nanosecond), wantError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (executionTimeouts{
				runTimeout:  45 * time.Second,
				runDeadline: tc.runDeadline,
			}).runDeadlineError(now)
			if !tc.wantError {
				require.NoError(t, err)
				return
			}
			requireRunDeadlineCause(t, err, 45*time.Second)
		})
	}
}

func requireContextDone(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("context remains active after its deadline")
	}
}

func requireTaskDeadlineCause(t *testing.T, err error, taskID string, timeout time.Duration) {
	t.Helper()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, run.IsRunDeadlineError(err))
	var taskErr *taskDeadlineError
	require.ErrorAs(t, err, &taskErr)
	require.Equal(t, taskID, taskErr.taskID)
	require.Equal(t, timeout, taskErr.timeout)
}

func requireRunDeadlineCause(t *testing.T, err error, timeout time.Duration) {
	t.Helper()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, run.IsRunDeadlineError(err))
	var runErr *run.RunDeadlineError
	require.ErrorAs(t, err, &runErr)
	require.Equal(t, timeout, runErr.Timeout)
}
