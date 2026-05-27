package event

import (
	"context"
	"sync"
)

type deferredPublishKey struct{}

// deferredPublisher accumulates bus publishes so they can be dispatched after
// an enclosing transaction commits, instead of immediately. This keeps event
// delivery consistent with the transaction: an event only reaches the bus once
// the writes it describes have actually committed.
type deferredPublisher struct {
	mu      sync.Mutex
	pending []pendingPublish
}

type pendingPublish struct {
	bus    Bus
	store  *Store
	events []Event
}

func (d *deferredPublisher) record(bus Bus, store *Store, events ...Event) {
	if len(events) == 0 {
		return
	}
	d.mu.Lock()
	d.pending = append(d.pending, pendingPublish{bus: bus, store: store, events: events})
	d.mu.Unlock()
}

// WithDeferredPublish returns a child context in which
// PublishAndMarkBusDispatched accumulates events instead of dispatching them,
// plus a flush func that dispatches everything accumulated. Use it around a
// transaction whose events must not reach the bus until the transaction
// commits:
//
//	ctx, flush := event.WithDeferredPublish(ctx)
//	if err := db.Transaction(ctx, func(tx *gorm.DB) error {
//	    event.ResetDeferred(ctx) // drop a prior failed attempt's events
//	    ...
//	}); err != nil {
//	    return err // nothing published
//	}
//	flush() // commit succeeded — publish now
//
// flush dispatches against the parent context, so it performs the real publish
// rather than re-accumulating.
func WithDeferredPublish(ctx context.Context) (context.Context, func()) {
	parent := ctx
	d := &deferredPublisher{}
	child := context.WithValue(ctx, deferredPublishKey{}, d)
	flush := func() {
		d.mu.Lock()
		pending := d.pending
		d.pending = nil
		d.mu.Unlock()
		for _, p := range pending {
			PublishAndMarkBusDispatched(parent, p.bus, p.store, p.events...)
		}
	}
	return child, flush
}

// ResetDeferred discards any accumulated-but-unflushed publishes on ctx. Call
// it at the start of each transaction attempt so a rolled-back/retried
// attempt's events are dropped and only the committed attempt's are flushed.
func ResetDeferred(ctx context.Context) {
	if d := deferredFrom(ctx); d != nil {
		d.mu.Lock()
		d.pending = nil
		d.mu.Unlock()
	}
}

func deferredFrom(ctx context.Context) *deferredPublisher {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(deferredPublishKey{}).(*deferredPublisher)
	return d
}
