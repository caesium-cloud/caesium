package event

import "context"

type deferBusDispatchKey struct{}

// WithDeferredBusDispatch marks ctx so that PublishAndMarkBusDispatched skips
// the immediate in-memory publish for events emitted within it.
//
// Use it around a transaction. Events are still written to the store
// transactionally (AppendTx) with bus_dispatch_pending=true; once the
// transaction commits, the BusDispatcher delivers each pending event to the bus
// exactly once and marks it dispatched, using its own live connection. If the
// transaction rolls back or is retried, the event rows never commit, so nothing
// is published — which avoids the orphan events (on rollback) and duplicate
// events (on retry) that an immediate, tx-scoped publish would cause.
func WithDeferredBusDispatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferBusDispatchKey{}, true)
}

func busDispatchDeferred(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	deferred, _ := ctx.Value(deferBusDispatchKey{}).(bool)
	return deferred
}
