package run

import "time"

// ComputeRetryDelay preserves the execution lanes' constant or exponential
// retry formula. Attempt selection and cancellation belong to the caller.
func ComputeRetryDelay(base time.Duration, backoff bool, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	if backoff {
		return base * (1 << uint(attempt-1))
	}
	return base
}
