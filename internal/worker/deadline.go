package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/run"
)

type executionTimeouts struct {
	taskTimeout time.Duration
	runTimeout  time.Duration
	runDeadline time.Time
	taskID      string
}

type taskDeadlineError struct {
	taskID  string
	timeout time.Duration
}

func (t executionTimeouts) runDeadlineError(now time.Time) error {
	if t.runDeadline.IsZero() || now.Before(t.runDeadline) {
		return nil
	}
	return run.NewRunDeadlineError(t.runTimeout)
}

func (e *taskDeadlineError) Error() string {
	return fmt.Sprintf("task %s timed out after %s", e.taskID, e.timeout)
}

func (e *taskDeadlineError) Unwrap() error {
	return context.DeadlineExceeded
}

// attemptContext applies metadata.taskTimeout per attempt and
// metadata.runTimeout against the durable run-window anchor. The earlier
// deadline wins. Zero values preserve the parent's existing deadline.
func (t executionTimeouts) attemptContext(parent context.Context, now time.Time) (context.Context, context.CancelFunc) {
	deadline := time.Time{}
	var cause error
	if t.taskTimeout > 0 {
		deadline = now.Add(t.taskTimeout)
		cause = &taskDeadlineError{taskID: t.taskID, timeout: t.taskTimeout}
	}
	if !t.runDeadline.IsZero() && (deadline.IsZero() || !t.runDeadline.After(deadline)) {
		deadline = t.runDeadline
		cause = run.NewRunDeadlineError(t.runTimeout)
	}
	if deadline.IsZero() {
		return context.WithCancel(parent)
	}
	return context.WithDeadlineCause(parent, deadline, cause)
}
