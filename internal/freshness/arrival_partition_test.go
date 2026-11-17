package freshness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// TestArrivalPartitionWatermarkDoesNotCollide proves an arrival.partition
// path advances and verifies that partition only. The whole-dataset row is
// left untouched, a missing or blank partition records nothing, and scheduling
// is not woken — it still reads the empty-partition watermark.
func TestArrivalPartitionWatermarkDoesNotCollide(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	const (
		eventType = "partition.arrival"
		dataset   = "raw.parts"
	)
	binding, err := json.Marshal(map[string]any{
		"event":     map[string]any{"type": eventType},
		"watermark": "$.key",
		"partition": "$.partition",
	})
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	if err := db.Create(&models.DatasetDeclaration{
		ID:             uuid.New(),
		JobID:          uuid.New(),
		JobAlias:       "arrival-source",
		Name:           dataset,
		Direction:      models.DatasetDirectionSource,
		External:       true,
		ExpectedEvery:  "24h",
		ArrivalBinding: datatypes.JSON(binding),
		CreatedAt:      t0,
		UpdatedAt:      t0,
	}).Error; err != nil {
		t.Fatalf("seed source: %v", err)
	}

	bus := event.New()
	events, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeDatasetAdvanced}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	obs := NewArrivalObserver(db)
	obs.SetBus(bus)
	store := NewStore(db)

	ingest := func(at time.Time, payload string) ArrivalResult {
		t.Helper()
		res, err := obs.Observe(ctx, &models.IngestedEvent{
			ID:        uuid.New(),
			Type:      eventType,
			Data:      datatypes.JSON(payload),
			CreatedAt: at,
		})
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		return res
	}
	assertNoScheduleWake := func() {
		t.Helper()
		select {
		case got := <-events:
			t.Fatalf("partition arrival published %s; scheduling must keep reading the dataset row", got.Type)
		default:
		}
	}

	missed := ingest(t0, `{"key":"ignored"}`)
	if len(missed.Advances) != 0 {
		t.Fatalf("unresolved partition recorded %+v", missed.Advances)
	}
	blank := ingest(t0, `{"key":"ignored","partition":"  "}`)
	if len(blank.Advances) != 0 {
		t.Fatalf("blank partition recorded %+v", blank.Advances)
	}
	if _, ok, err := store.Get(ctx, nil, dataset); err != nil || ok {
		t.Fatalf("dataset row exists after a missed partition (ok=%v err=%v)", ok, err)
	}

	first := ingest(t0, `{"key":"e1","partition":"east"}`)
	if len(first.Advances) != 1 || first.Advances[0].Outcome != OutcomeAdvanced || first.Advances[0].Partition != "east" {
		t.Fatalf("east advance = %+v", first.Advances)
	}
	assertNoScheduleWake()

	west := ingest(t0.Add(time.Minute), `{"key":"w1","partition":"west"}`)
	if len(west.Advances) != 1 || west.Advances[0].Outcome != OutcomeAdvanced || west.Advances[0].Partition != "west" {
		t.Fatalf("west advance = %+v", west.Advances)
	}

	east, ok, err := store.GetPartition(ctx, nil, dataset, "east")
	if err != nil || !ok {
		t.Fatalf("east row: ok=%v err=%v", ok, err)
	}
	verified := ingest(t0.Add(2*time.Minute), `{"key":"e1","partition":"east"}`)
	if len(verified.Advances) != 1 || verified.Advances[0].Outcome != OutcomeVerified || verified.Advances[0].Partition != "east" {
		t.Fatalf("east verify = %+v", verified.Advances)
	}
	eastAfter, ok, err := store.GetPartition(ctx, nil, dataset, "east")
	if err != nil || !ok {
		t.Fatalf("east row after verify: ok=%v err=%v", ok, err)
	}
	if east.AdvancedAt == nil || eastAfter.AdvancedAt == nil || !eastAfter.AdvancedAt.Equal(*east.AdvancedAt) {
		t.Fatalf("verify moved advanced_at from %v to %v", east.AdvancedAt, eastAfter.AdvancedAt)
	}
	if eastAfter.VerifiedAt == nil || !eastAfter.VerifiedAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("verified_at = %v", eastAfter.VerifiedAt)
	}
	if eastAfter.Watermark != "e1" {
		t.Fatalf("east watermark = %q", eastAfter.Watermark)
	}

	gotWest, ok, err := store.GetPartition(ctx, nil, dataset, "west")
	if err != nil || !ok || gotWest.Watermark != "w1" {
		t.Fatalf("west row = %+v ok=%v err=%v", gotWest, ok, err)
	}
	if _, ok, err := store.Get(ctx, nil, dataset); err != nil || ok {
		t.Fatalf("named partitions collided with the dataset row (ok=%v err=%v)", ok, err)
	}
	assertNoScheduleWake()
}
