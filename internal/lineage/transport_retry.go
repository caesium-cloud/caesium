package lineage

import (
	"context"
	"time"

	"github.com/Rican7/retry"
	"github.com/Rican7/retry/backoff"
	"github.com/Rican7/retry/strategy"
	"github.com/caesium-cloud/caesium/pkg/log"
)

type retryTransport struct {
	base     Transport
	attempts uint
}

// NewRetryTransport wraps an existing transport with retry logic.
func NewRetryTransport(base Transport, attempts uint) Transport {
	if attempts == 0 {
		attempts = 3
	}
	return &retryTransport{
		base:     base,
		attempts: attempts,
	}
}

func (t *retryTransport) Emit(ctx context.Context, event RunEvent) error {
	return retry.Retry(
		func(attempt uint) error {
			err := t.base.Emit(ctx, event)
			if err != nil {
				log.Warn("lineage: emit attempt failed",
					"attempt", attempt+1,
					"max_attempts", t.attempts,
					"error", err,
				)
			}
			return err
		},
		strategy.Limit(t.attempts),
		strategy.Backoff(backoff.Exponential(100*time.Millisecond, 2.0)),
	)
}

func (t *retryTransport) Close() error {
	return t.base.Close()
}
