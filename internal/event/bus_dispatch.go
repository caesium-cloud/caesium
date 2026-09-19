package event

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/testfault"
	"github.com/caesium-cloud/caesium/pkg/log"
)

const (
	defaultBusDispatchBatchSize    = 100
	defaultBusDispatchPollInterval = 500 * time.Millisecond
)

type BusDispatcher struct {
	store    *Store
	bus      Bus
	interval time.Duration
	batch    int
}

type BusDispatcherOption func(*BusDispatcher)

func WithBusDispatcherInterval(interval time.Duration) BusDispatcherOption {
	return func(d *BusDispatcher) {
		if interval > 0 {
			d.interval = interval
		}
	}
}

func WithBusDispatcherBatchSize(batch int) BusDispatcherOption {
	return func(d *BusDispatcher) {
		if batch > 0 {
			d.batch = batch
		}
	}
}

func NewBusDispatcher(store *Store, bus Bus, opts ...BusDispatcherOption) *BusDispatcher {
	d := &BusDispatcher{
		store:    store,
		bus:      bus,
		interval: defaultBusDispatchPollInterval,
		batch:    defaultBusDispatchBatchSize,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

func (d *BusDispatcher) Start(ctx context.Context) error {
	if d == nil || d.store == nil || d.bus == nil {
		return nil
	}

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		if err := d.DispatchOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("event bus dispatch failed", "error", err)
		}
	}
}

func (d *BusDispatcher) DispatchOnce(ctx context.Context) error {
	if d == nil || d.store == nil || d.bus == nil {
		return nil
	}

	events, err := d.store.ListPendingBusDispatch(ctx, d.batch)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	for _, evt := range events {
		// distributed-testing B2 / A1 "Proxy and clock decision": the ONE
		// approved product boundary. The rows above are already durably
		// committed; this is the last point before their first publication,
		// and covering this path as well as PublishAndMarkBusDispatched is
		// what stops the background dispatcher bypassing an armed pause.
		// Compiled out of release builds (testfault.Enabled is a false
		// constant there), so nothing below exists in the release binary.
		if testfault.Enabled {
			testfault.BeforeBusPublish(ctx, testfault.PathDispatchOnce, string(evt.Type), evt.RunID.String(), evt.Sequence)
		}
		d.bus.Publish(evt)
	}
	// Marking stays AFTER publication: a held event's row remains
	// bus_dispatch_pending so a survivor replays it durably.
	return d.store.MarkBusDispatched(ctx, events...)
}

func PublishAndMarkBusDispatched(ctx context.Context, bus Bus, store *Store, events ...Event) {
	if bus == nil || len(events) == 0 {
		return
	}

	if busDispatchDeferred(ctx) {
		// Inside a transaction (see WithDeferredBusDispatch): skip the immediate
		// publish. The event row is written transactionally and the BusDispatcher
		// delivers it after the transaction commits — and never if it rolls back
		// — keeping bus delivery consistent with the commit.
		return
	}

	for _, evt := range events {
		// Same A1-approved boundary on the immediate-publication path: the
		// event row is already written, and this is the last point before its
		// first bus delivery. Removed entirely from release builds.
		if testfault.Enabled {
			testfault.BeforeBusPublish(ctx, testfault.PathPublishAndMark, string(evt.Type), evt.RunID.String(), evt.Sequence)
		}
		bus.Publish(evt)
	}
	if store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	if err := store.MarkBusDispatched(ctx, events...); err != nil {
		log.Warn("failed to mark events dispatched to bus", "error", err)
	}
}

func (s *Store) ListPendingBusDispatch(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = defaultBusDispatchBatchSize
	}

	var rows []models.ExecutionEvent
	if err := s.db.WithContext(ctx).
		Model(&models.ExecutionEvent{}).
		Where("bus_dispatch_pending = ? AND bus_dispatched_at IS NULL", true).
		Order("sequence ASC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, modelToEvent(row))
	}
	return events, nil
}

func (s *Store) MarkBusDispatched(ctx context.Context, events ...Event) error {
	if s == nil || len(events) == 0 {
		return nil
	}

	sequences := make([]uint64, 0, len(events))
	seen := make(map[uint64]struct{}, len(events))
	for _, evt := range events {
		if evt.Sequence == 0 {
			continue
		}
		if _, ok := seen[evt.Sequence]; ok {
			continue
		}
		seen[evt.Sequence] = struct{}{}
		sequences = append(sequences, evt.Sequence)
	}
	if len(sequences) == 0 {
		return nil
	}

	now := time.Now().UTC()
	return s.db.WithContext(ctx).
		Model(&models.ExecutionEvent{}).
		Where("sequence IN ? AND bus_dispatch_pending = ?", sequences, true).
		Updates(map[string]any{
			"bus_dispatch_pending": false,
			"bus_dispatched_at":    now,
		}).Error
}
