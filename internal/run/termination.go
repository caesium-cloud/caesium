package run

import (
	"context"
	"errors"
)

// RunCancellationError identifies cancellation of the authoritative whole-run
// context. A task/backend returning context.Canceled while that context is live
// is an ordinary failure and must not resolve unrelated unfinished tasks.
type RunCancellationError struct {
	cause error
}

func (e *RunCancellationError) Error() string { return e.Unwrap().Error() }
func (e *RunCancellationError) Unwrap() error {
	if e.cause == nil {
		return context.Canceled
	}
	return e.cause
}

// NewRunCancellationError preserves the whole-run context's original cause.
// The job owner selects this marker only after observing its context canceled.
func NewRunCancellationError(cause error) error {
	if cause == nil {
		cause = context.Canceled
	}
	return &RunCancellationError{cause: cause}
}

func IsRunCancellationError(err error) bool {
	_, ok := errors.AsType[*RunCancellationError](err)
	return ok
}
