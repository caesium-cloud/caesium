// Package dbretry provides retry mechanics without choosing a database policy.
package dbretry

import (
	"context"
	"time"
)

// Policy keeps classification, budget, waiting, and side effects at the caller.
// A nil Wait retries immediately. Hooks run only when a retry remains.
type Policy struct {
	Backoffs      []time.Duration
	Retryable     func(error) bool
	Wait          func(context.Context, time.Duration) error
	BeforeAttempt bool
	BeforeRetry   func(error)
	OnRetry       func(error)
}

// Retry calls fn at most len(policy.Backoffs)+1 times. With BeforeAttempt false,
// fn runs before any context check, preserving callers that own cancellation
// inside their operation. Wait errors, including cancellation, are returned.
func Retry(ctx context.Context, policy Policy, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 0; ; attempt++ {
		if policy.BeforeAttempt {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		err := fn()
		if err == nil || policy.Retryable == nil || !policy.Retryable(err) || attempt >= len(policy.Backoffs) {
			return err
		}
		if policy.BeforeRetry != nil {
			policy.BeforeRetry(err)
		}
		if policy.OnRetry != nil {
			policy.OnRetry(err)
		}
		if policy.Wait != nil {
			if waitErr := policy.Wait(ctx, policy.Backoffs[attempt]); waitErr != nil {
				return waitErr
			}
		}
	}
}
