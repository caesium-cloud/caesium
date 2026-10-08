package event

import "context"

// RunSubscription handles events synchronously for the lifetime of a subscription.
// Cleanup runs only once the successfully established subscription stops.
func RunSubscription(ctx context.Context, bus Bus, filter Filter, ready chan<- struct{}, handle func(Event), onStop func() error) error {
	ch, err := bus.Subscribe(ctx, filter)
	if err != nil {
		return err
	}
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			if onStop != nil {
				return onStop()
			}
			return nil
		case evt, ok := <-ch:
			if !ok {
				if onStop != nil {
					return onStop()
				}
				return nil
			}
			handle(evt)
		}
	}
}
