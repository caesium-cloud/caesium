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
	"gorm.io/gorm"
)

// seedProducingRun writes the minimal run-completion surface: a produced +
// consumed declaration, a task, its succeeded task run carrying the emitted
// ##caesium::output, and the job run row (with optional backfill).
func seedProducingRun(t *testing.T, db *gorm.DB, jobID, runID uuid.UUID, output map[string]string, backfill *uuid.UUID) {
	t.Helper()
	taskID := uuid.New()
	now := t0.Add(time.Hour)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	must(db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: uuid.New(), Name: "extract"}).Error)

	outBlob, _ := json.Marshal(output)
	must(db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: "docker", Image: "etl:1.4", Command: "run", Status: taskRunTerminalSucceeded,
		CacheHit: false, Output: datatypes.JSON(outBlob), CreatedAt: now, UpdatedAt: now,
	}).Error)

	must(db.Create(&models.JobRun{
		ID: runID, JobID: jobID, TriggerID: uuid.New(), Status: "succeeded",
		BackfillID: backfill, StartedAt: t0, CompletedAt: &now, CreatedAt: t0, UpdatedAt: now,
	}).Error)

	must(db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: jobID, JobAlias: "orders-daily", StepName: "extract",
		Name: "staging.orders", Direction: models.DatasetDirectionProduces,
		Freshness: "8h", WatermarkKey: "max_order_ts",
	}).Error)
	must(db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: jobID, JobAlias: "orders-daily", StepName: "extract",
		Name: "raw.vendor_x", Direction: models.DatasetDirectionConsumes,
	}).Error)
}

func TestCapturerAdvancesAndSnapshotsConsumed(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	// A consumed upstream already has a known watermark.
	if _, err := c.store.Advance(ctx, AdvanceInput{Name: "raw.vendor_x", Watermark: "vendor-key-1", RunID: uuid.New(), CompletedAt: t0}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	jobID, runID := uuid.New(), uuid.New()
	wm := "2026-07-03T04:31:00Z"
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": wm}, nil)

	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	st, ok, err := c.store.Get(ctx, nil, "staging.orders")
	if err != nil || !ok {
		t.Fatalf("get produced: %v ok=%v", err, ok)
	}
	if st.Watermark != wm {
		t.Fatalf("produced watermark = %q, want %q", st.Watermark, wm)
	}
	if st.AdvancedAt == nil {
		t.Fatalf("expected advanced_at set on produced dataset")
	}
	if st.LastRunID == nil || *st.LastRunID != runID {
		t.Fatalf("last_run_id = %v, want %v", st.LastRunID, runID)
	}
	var consumed map[string]string
	if err := json.Unmarshal(st.ConsumedWatermarks, &consumed); err != nil {
		t.Fatalf("unmarshal consumed: %v", err)
	}
	if consumed["raw.vendor_x"] != "vendor-key-1" {
		t.Fatalf("consumed snapshot = %v, want raw.vendor_x=vendor-key-1", consumed)
	}
}

func TestCapturerBackfillNeverAdvances(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	bf := uuid.New()
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": "2026-07-03T04:31:00Z"}, &bf)

	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	if _, ok, err := c.store.Get(ctx, nil, "staging.orders"); err != nil || ok {
		t.Fatalf("backfill run must not create/advance dataset state (ok=%v err=%v)", ok, err)
	}
}

func TestCapturerDegradedVerifiesWithoutWatermarkKey(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	// The step emits no max_order_ts key: degraded mode.
	seedProducingRun(t, db, jobID, runID, map[string]string{"rows": "42"}, nil)

	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	st, ok, err := c.store.Get(ctx, nil, "staging.orders")
	if err != nil || !ok {
		t.Fatalf("get produced: %v ok=%v", err, ok)
	}
	if st.Watermark != "" {
		t.Fatalf("degraded mode should not set a watermark, got %q", st.Watermark)
	}
	if st.VerifiedAt == nil {
		t.Fatalf("degraded mode should refresh verified_at")
	}
	if st.AdvancedAt != nil {
		t.Fatalf("degraded mode must not advance")
	}
}
