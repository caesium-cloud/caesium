package model

import (
	"fmt"
	"sort"
)

// Event is one persisted execution event.
//
// Sequence identifies an event WITHIN the selected store lifetime. It is not a
// gap-free global counter across clusters or shards, and no oracle here treats
// it as one — A1 is explicit about that, and a checker that assumed otherwise
// would reject legal histories.
type Event struct {
	Sequence int64
	RunID    string
	Kind     string
	Instance InstanceID
}

// EventStore is the durable half of the delivery model: an append-only log with
// a dense per-store sequence.
type EventStore struct {
	seq    int64
	events []Event
}

// NewEventStore builds an empty store.
func NewEventStore() *EventStore { return &EventStore{} }

// Append persists an event and returns it with its stamped sequence.
func (s *EventStore) Append(runID, kind string, instance InstanceID) Event {
	s.seq++
	e := Event{Sequence: s.seq, RunID: runID, Kind: kind, Instance: instance}
	s.events = append(s.events, e)
	return e
}

// Persisted returns the durable log.
func (s *EventStore) Persisted() []Event { return append([]Event(nil), s.events...) }

// Since returns the persisted events after a cursor, ascending — the catch-up
// query a reconnecting subscriber issues with Last-Event-ID.
func (s *EventStore) Since(cursor int64) []Event {
	var out []Event
	for _, e := range s.events {
		if e.Sequence > cursor {
			out = append(out, e)
		}
	}
	return out
}

// Subscriber is the live half: what one consumer actually received, in the
// order it received it.
//
// It models at-least-once delivery, so it may hold duplicates and it may hold
// events out of order. Those are legal, and CheckDelivery must not reject
// them: a checker that insists on exactly-once, in-order delivery would fail
// the product for behaving as documented.
type Subscriber struct {
	// Resume is the Last-Event-ID the subscriber first connected with.
	Resume int64
	// Cursor is the Last-Event-ID it would reconnect with NOW: the highest
	// sequence up to which it has received an unbroken run of events.
	//
	// Contiguity is the whole point. A cursor set to the highest sequence seen
	// would step over a hole — the client would resume past an event it never
	// received, and nothing would ever redeliver it. Resuming from the last
	// event actually received is what makes the drop repairable, and it is why
	// at-least-once delivery is a usable guarantee rather than a disclaimer.
	Cursor    int64
	Delivered []Event
}

// NewSubscriber starts a subscriber resuming after a cursor. A zero cursor is a
// fresh stream.
func NewSubscriber(cursor int64) *Subscriber {
	return &Subscriber{Resume: cursor, Cursor: cursor}
}

// Deliver records a live delivery.
func (s *Subscriber) Deliver(e Event) {
	s.Delivered = append(s.Delivered, e)
	s.recomputeCursor()
}

// CatchUp replays everything the store holds after the cursor, as a reconnect
// does.
func (s *Subscriber) CatchUp(store *EventStore) {
	s.Delivered = append(s.Delivered, store.Since(s.Cursor)...)
	s.recomputeCursor()
}

// Drop models the in-process subscriber buffer overflowing: a delivery is lost
// even though the row is persisted and marked dispatched. It is a real product
// behavior, which is why reconnect-and-catch-up is a separate mechanism from
// uninterrupted-stream reliability.
func (s *Subscriber) Drop(n int) {
	if n <= 0 || n > len(s.Delivered) {
		return
	}
	s.Delivered = s.Delivered[:len(s.Delivered)-n]
	s.recomputeCursor()
}

func (s *Subscriber) recomputeCursor() {
	seen := make(map[int64]bool, len(s.Delivered))
	for _, e := range s.Delivered {
		seen[e.Sequence] = true
	}
	cursor := s.Resume
	for seen[cursor+1] {
		cursor++
	}
	s.Cursor = cursor
}

// CheckDelivery is the DT-EVENT-01 oracle. It checks the three things the
// contract actually promises, and nothing more:
//
//  1. No fabrication. Every delivered event corresponds to a persisted one.
//     This is the half that catches a checker being fed a history it invented.
//  2. Completeness after catch-up. Every persisted event above the resume
//     cursor was delivered at least once. Duplicates and reordering pass.
//  3. Dense persisted sequence within the store lifetime, starting at 1.
//
// It deliberately does not check exactly-once delivery, global ordering across
// subscribers, or cross-store sequence continuity.
func CheckDelivery(store *EventStore, sub *Subscriber, resumeCursor int64) error {
	persisted := map[int64]Event{}
	for _, e := range store.Persisted() {
		persisted[e.Sequence] = e
	}
	for _, e := range sub.Delivered {
		p, ok := persisted[e.Sequence]
		if !ok {
			return fmt.Errorf("model: delivered event %d was never persisted", e.Sequence)
		}
		if p != e {
			return fmt.Errorf("model: delivered event %d does not match the persisted row", e.Sequence)
		}
	}

	seen := map[int64]bool{}
	for _, e := range sub.Delivered {
		seen[e.Sequence] = true
	}
	for _, e := range store.Since(resumeCursor) {
		if !seen[e.Sequence] {
			return fmt.Errorf("model: persisted event %d above cursor %d was never delivered",
				e.Sequence, resumeCursor)
		}
	}

	all := store.Persisted()
	sort.SliceStable(all, func(i, j int) bool { return all[i].Sequence < all[j].Sequence })
	for i, e := range all {
		if e.Sequence != int64(i+1) {
			return fmt.Errorf("model: persisted sequence is not dense: position %d holds %d", i+1, e.Sequence)
		}
	}
	return nil
}
