package freshness

import (
	"context"
	"errors"
	"sync"

	"github.com/caesium-cloud/caesium/internal/runlife"
)

// ErrNoRunReservation identifies launchers invoked without evaluator admission.
var ErrNoRunReservation = errors.New("freshness run reservation is missing")

var errRunReservationConsumed = errors.New("freshness run reservation already consumed or released")

type runReservationKey struct{}
type runReservation struct {
	mu       sync.Mutex
	workCtx  context.Context
	release  func()
	consumed bool
}

// reserveRunLaunch leaves admission subject to the original tick/leadership
// context. Only the eventual execution uses the server-owned detached context.
func reserveRunLaunch(ctx context.Context) (context.Context, func(), error) {
	owner := runlife.FromContext(ctx)
	if owner == nil {
		// Direct synchronous evaluator injections retain their existing contract.
		return ctx, func() {}, nil
	}
	workCtx, release, err := owner.Reserve(ctx)
	if err != nil {
		return nil, nil, err
	}
	r := &runReservation{workCtx: workCtx, release: release}
	return context.WithValue(ctx, runReservationKey{}, r), func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.consumed {
			r.consumed = true
			r.release()
		}
	}, nil
}

// TakeRunReservation transfers the evaluator's pre-admission reservation to its
// launcher. The caller must release it after execution and cancellation cleanup.
func TakeRunReservation(ctx context.Context) (context.Context, func(), error) {
	r, _ := ctx.Value(runReservationKey{}).(*runReservation)
	if r == nil {
		return nil, nil, ErrNoRunReservation
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.consumed {
		return nil, nil, errRunReservationConsumed
	}
	r.consumed = true
	return r.workCtx, r.release, nil
}
