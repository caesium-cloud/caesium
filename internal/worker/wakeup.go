package worker

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/event"
)

func SubscribeWakeups(ctx context.Context, bus event.Bus) <-chan struct{} {
	if bus == nil {
		return nil
	}

	events, err := bus.Subscribe(ctx, event.Filter{
		Types: []event.Type{
			event.TypeTaskReady,
			event.TypeTaskLeaseExpired,
			event.TypeRunStarted,
		},
	})
	if err != nil {
		return nil
	}

	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-events:
				if !ok {
					return
				}
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()

	return ch
}
