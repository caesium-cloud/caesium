package worker

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/event"
)

type WakeupSignaler struct {
	ch chan struct{}
}

func NewWakeupSignaler() *WakeupSignaler {
	return &WakeupSignaler{ch: make(chan struct{}, 1)}
}

func (s *WakeupSignaler) Signal() {
	if s == nil {
		return
	}
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

func (s *WakeupSignaler) C() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.ch
}

func SubscribeWakeups(ctx context.Context, bus event.Bus, extra ...<-chan struct{}) <-chan struct{} {
	hasSource := bus != nil
	for _, ch := range extra {
		if ch != nil {
			hasSource = true
			break
		}
	}
	if !hasSource {
		return nil
	}

	signals := make(chan struct{}, 1)

	if bus != nil {
		events, err := bus.Subscribe(ctx, event.Filter{Types: wakeupEventTypes()})
		if err != nil {
			return nil
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-events:
					if !ok {
						return
					}
					sendWakeupSignal(signals)
				}
			}
		}()
	}

	for _, source := range extra {
		source := source
		if source == nil {
			continue
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-source:
					if !ok {
						return
					}
					sendWakeupSignal(signals)
				}
			}
		}()
	}

	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()

	return ch
}

func sendWakeupSignal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func wakeupEventTypes() []event.Type {
	return []event.Type{
		event.TypeTaskReady,
		event.TypeTaskLeaseExpired,
		event.TypeRunStarted,
	}
}
