package model_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
	"pgregory.net/rapid"
)

// TestEventDeliveryProperties checks DT-EVENT-01 as a DELIVERY property, kept
// deliberately separate from the linearizability model.
//
// The contract is at-least-once, so the generator produces exactly the things a
// naive checker would call defects — duplicate deliveries, out-of-order live
// deliveries, and drops caused by a subscriber buffer overflowing even though
// the row is persisted and marked dispatched — and requires the oracle to
// accept every one of them after a reconnect and catch-up. A checker that
// demanded exactly-once in-order delivery would fail the product for behaving
// as documented, which is a worse outcome than having no checker.
func TestEventDeliveryProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		store := model.NewEventStore()
		resume := int64(rapid.IntRange(0, 3).Draw(t, "resume_cursor"))
		sub := model.NewSubscriber(resume)

		kinds := []string{"run.started", "task.started", "task.completed", "run.completed"}
		appended := 0

		t.Repeat(map[string]func(*rapid.T){
			"": func(t *rapid.T) {
				// Mid-stream the subscriber may legitimately be behind, so only
				// the no-fabrication half of the oracle holds continuously.
				for _, e := range sub.Delivered {
					if e.Sequence <= 0 || e.Sequence > int64(appended) {
						t.Fatalf("delivered event %d was never persisted", e.Sequence)
					}
				}
			},

			"append": func(t *rapid.T) {
				kind := rapid.SampledFrom(kinds).Draw(t, "kind")
				e := store.Append("run-1", kind, model.Step("s0"))
				appended++
				if e.Sequence != int64(appended) {
					t.Fatalf("append stamped sequence %d, expected %d", e.Sequence, appended)
				}
				// Live delivery: usually immediate, sometimes duplicated,
				// sometimes reordered behind the next one, sometimes lost.
				switch rapid.IntRange(0, 3).Draw(t, "delivery") {
				case 0:
					sub.Deliver(e)
				case 1:
					sub.Deliver(e)
					sub.Deliver(e) // at-least-once: a duplicate is legal
				case 2:
					// Out of order: held back, delivered after the next append.
				case 3:
					// Dropped by a full subscriber buffer.
				}
			},

			"deliver_backlog": func(t *rapid.T) {
				// Late live delivery of an arbitrary persisted event, which is
				// how reordering actually shows up.
				persisted := store.Persisted()
				if len(persisted) == 0 {
					return
				}
				sub.Deliver(rapid.SampledFrom(persisted).Draw(t, "event"))
			},

			"buffer_overflow": func(t *rapid.T) {
				sub.Drop(rapid.IntRange(1, 3).Draw(t, "lost"))
			},

			"reconnect": func(t *rapid.T) {
				// Reconnect and catch up from the resume cursor: after this the
				// full oracle must hold.
				sub.CatchUp(store)
				if err := model.CheckDelivery(store, sub, resume); err != nil {
					t.Fatalf("after catch-up: %v", err)
				}
			},
		})

		sub.CatchUp(store)
		if err := model.CheckDelivery(store, sub, resume); err != nil {
			t.Fatalf("%v", err)
		}
	})
}

// TestDeliveryOracleRejectsLoss and TestDeliveryOracleRejectsFabrication are
// the delivery checker's negative controls. At-least-once is a weak guarantee,
// and a weak guarantee is easy to "check" with an oracle that accepts
// everything; these two plant the defects it must still catch.
func TestDeliveryOracleRejectsLoss(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		store := model.NewEventStore()
		n := rapid.IntRange(1, 6).Draw(t, "events")
		var delivered []model.Event
		for i := range n {
			delivered = append(delivered, store.Append("run-1", "task.completed", model.Step(model.TaskID("s"+string(rune('0'+i))))))
		}
		lost := rapid.IntRange(0, n-1).Draw(t, "lost")

		sub := model.NewSubscriber(0)
		for i, e := range delivered {
			if i == lost {
				continue
			}
			sub.Deliver(e)
		}
		// No reconnect: the drop is never repaired, so completeness must fail.
		if err := model.CheckDelivery(store, sub, 0); err == nil {
			t.Fatalf("the oracle accepted a history missing persisted event %d", delivered[lost].Sequence)
		}
	})
}

func TestDeliveryOracleRejectsFabrication(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		store := model.NewEventStore()
		n := rapid.IntRange(1, 6).Draw(t, "events")
		for range n {
			store.Append("run-1", "task.completed", model.Step("s0"))
		}
		sub := model.NewSubscriber(0)
		sub.CatchUp(store)
		sub.Deliver(model.Event{
			Sequence: int64(n) + int64(rapid.IntRange(1, 5).Draw(t, "beyond")),
			RunID:    "run-1",
			Kind:     "task.completed",
			Instance: model.Step("s0"),
		})
		if err := model.CheckDelivery(store, sub, 0); err == nil {
			t.Fatalf("the oracle accepted a delivered event that was never persisted")
		}
	})
}
