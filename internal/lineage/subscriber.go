package lineage

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
)

type Subscriber struct {
	bus           event.Bus
	transport     Transport
	transportName string
	namespace     string
	db            *gorm.DB
	mapper        *mapper
}

func NewSubscriber(bus event.Bus, transport Transport, namespace string, db *gorm.DB) *Subscriber {
	return &Subscriber{
		bus:       bus,
		transport: transport,
		namespace: namespace,
		db:        db,
		mapper:    newMapper(namespace, db),
	}
}

func (s *Subscriber) SetTransportName(name string) {
	s.transportName = name
}

func (s *Subscriber) Start(ctx context.Context) error {
	filter := event.Filter{
		Types: []event.Type{
			event.TypeRunStarted,
			event.TypeRunCompleted,
			event.TypeRunFailed,
			event.TypeTaskStarted,
			event.TypeTaskSucceeded,
			event.TypeTaskFailed,
			event.TypeTaskSkipped,
		},
	}

	ch, err := s.bus.Subscribe(ctx, filter)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return s.transport.Close()
		case evt, ok := <-ch:
			if !ok {
				return s.transport.Close()
			}
			s.handleEvent(ctx, evt)
		}
	}
}

func (s *Subscriber) handleEvent(ctx context.Context, evt event.Event) {
	olEvent, err := s.mapper.mapEvent(evt)
	if err != nil {
		log.Error("lineage: failed to map event",
			"event_type", string(evt.Type),
			"error", err,
		)
		return
	}

	if olEvent == nil {
		return
	}

	start := time.Now()
	emitErr := s.transport.Emit(ctx, *olEvent)
	duration := time.Since(start)

	transportLabel := s.transportName
	if transportLabel == "" {
		transportLabel = "unknown"
	}
	LineageEmitDuration.WithLabelValues(transportLabel).Observe(duration.Seconds())

	if emitErr != nil {
		LineageEventsEmitted.WithLabelValues(string(olEvent.EventType), "error").Inc()
		log.Error("lineage: failed to emit event",
			"event_type", string(olEvent.EventType),
			"job", olEvent.Job.Name,
			"error", emitErr,
		)
	} else {
		LineageEventsEmitted.WithLabelValues(string(olEvent.EventType), "success").Inc()
	}
}
