package freshness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// seedProducingRun writes the minimal run-completion surface: a produced +
// consumed declaration, a task, its succeeded task run carrying the emitted
// ##caesium::output, and the job run row (with optional backfill).
func seedProducingRun(t *testing.T, db *gorm.DB, jobID, runID uuid.UUID, output map[string]string, backfill *uuid.UUID, watermarkKey string) {
	t.Helper()
	outBlob, _ := json.Marshal(output)
	seedProducingRunRaw(t, db, jobID, runID, outBlob, backfill, watermarkKey)
}

// seedProducingRunRaw is seedProducingRun with a raw ##caesium::output blob, so
// tests can seed non-scalar JSON values (null, object, array) that a
// map[string]string can't express.
func seedProducingRunRaw(t *testing.T, db *gorm.DB, jobID, runID uuid.UUID, rawOutput []byte, backfill *uuid.UUID, watermarkKey string) {
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

	must(db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: "docker", Image: "etl:1.4", Command: "run", Status: taskRunTerminalSucceeded,
		CacheHit: false, Output: datatypes.JSON(rawOutput), CreatedAt: now, UpdatedAt: now,
	}).Error)

	must(db.Create(&models.JobRun{
		ID: runID, JobID: jobID, TriggerID: uuid.New(), Status: "succeeded",
		BackfillID: backfill, StartedAt: t0, CompletedAt: &now, CreatedAt: t0, UpdatedAt: now,
	}).Error)

	must(db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: jobID, JobAlias: "orders-daily", StepName: "extract",
		Name: "staging.orders", Direction: models.DatasetDirectionProduces,
		Freshness: "8h", WatermarkKey: watermarkKey,
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
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": wm}, nil, "max_order_ts")

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

// consumedSnapshotOf reads back the consumed-watermark blob the capturer wrote
// onto a produced dataset's state row.
func consumedSnapshotOf(t *testing.T, c *Capturer, name string) map[string]string {
	t.Helper()
	st, ok, err := c.store.Get(context.Background(), nil, name)
	if err != nil || !ok {
		t.Fatalf("get produced %q: %v ok=%v", name, err, ok)
	}
	var consumed map[string]string
	if err := json.Unmarshal(st.ConsumedWatermarks, &consumed); err != nil {
		t.Fatalf("unmarshal consumed: %v", err)
	}
	return consumed
}

// TestCapturerConsumedSnapshotTakenAtRunStart is the regression for the
// completion-time capture bug: an input that advances WHILE the run is in flight
// must not be credited to that run. The produced dataset has to record the input
// view the run actually consumed at its start, otherwise a freshness comparison
// over-reports the output as caught-up with an input it never read.
func TestCapturerConsumedSnapshotTakenAtRunStart(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	wm := "2026-07-03T04:31:00Z"
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": wm}, nil, "max_order_ts")

	// The input's watermark as the run begins.
	if _, err := c.store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "vendor-key-1", RunID: uuid.New(), CompletedAt: t0,
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	c.handleRunStarted(ctx, event.Event{Type: event.TypeRunStarted, JobID: jobID, RunID: runID})

	// The input advances MID-RUN — this run never saw vendor-key-2.
	if _, err := c.store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "vendor-key-2", RunID: uuid.New(),
		RunOrder: t0.Add(time.Minute), CompletedAt: t0.Add(time.Minute),
	}); err != nil {
		t.Fatalf("advance upstream mid-run: %v", err)
	}

	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	consumed := consumedSnapshotOf(t, c, "staging.orders")
	if consumed["raw.vendor_x"] != "vendor-key-1" {
		t.Fatalf("consumed snapshot = %v, want the START-time raw.vendor_x=vendor-key-1", consumed)
	}
	if len(c.startSnapshots) != 0 {
		t.Fatalf("completion must evict the run's start snapshot, %d left", len(c.startSnapshots))
	}
}

// TestCapturerPrefersDerivedConsumedWatermarks proves the strongest source wins:
// a freshness-derived run carries the evaluator's start-time input view on its
// own job_runs row (_consumed_watermarks), which survives a restart the
// in-memory snapshot would not. It must beat both a start snapshot and a
// completion-time read.
func TestCapturerPrefersDerivedConsumedWatermarks(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": "2026-07-03T04:31:00Z"}, nil, "max_order_ts")

	// The evaluator's derivation view, stored the way derive() stores it: the
	// param VALUE is itself a JSON document.
	params, err := json.Marshal(map[string]string{
		freshnessConsumedWatermarksParam: `{"raw.vendor_x":"derived-key"}`,
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if err := db.Model(&models.JobRun{}).Where("id = ?", runID).
		Update("params", datatypes.JSON(params)).Error; err != nil {
		t.Fatalf("set run params: %v", err)
	}

	// A different, later value is what a completion-time read would pick up.
	if _, err := c.store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "completion-time-key", RunID: uuid.New(), CompletedAt: t0,
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}
	c.handleRunStarted(ctx, event.Event{Type: event.TypeRunStarted, JobID: jobID, RunID: runID})

	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	consumed := consumedSnapshotOf(t, c, "staging.orders")
	if consumed["raw.vendor_x"] != "derived-key" {
		t.Fatalf("consumed snapshot = %v, want the derived raw.vendor_x=derived-key", consumed)
	}
}

// TestCapturerStartSnapshotEvictedOnNonCompletion proves the bounded-memory
// claim on the paths that never reach handleRunCompleted: a failed or cancelled
// run releases its slot too, so the map cannot accumulate one entry per run that
// did not succeed.
func TestCapturerStartSnapshotEvictedOnNonCompletion(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": "x"}, nil, "max_order_ts")

	c.handleRunStarted(ctx, event.Event{Type: event.TypeRunStarted, JobID: jobID, RunID: runID})
	if len(c.startSnapshots) != 1 {
		t.Fatalf("run start should snapshot exactly one run, got %d", len(c.startSnapshots))
	}

	c.dropStartSnapshot(runID)
	if len(c.startSnapshots) != 0 {
		t.Fatalf("a non-completing run must release its slot, %d left", len(c.startSnapshots))
	}
}

// TestCapturerStartSnapshotCapEvictsOldest pins the hard cap that makes the
// in-memory map bounded even if a terminal event is lost for every run.
func TestCapturerStartSnapshotCapEvictsOldest(t *testing.T) {
	c := NewCapturer(event.New(), openRegistryDB(t))

	for i := 0; i < maxStartSnapshots+8; i++ {
		c.putStartSnapshot(uuid.New(), map[string]string{"raw.vendor_x": "v"})
	}
	if len(c.startSnapshots) > maxStartSnapshots {
		t.Fatalf("start snapshots = %d, must never exceed the cap %d", len(c.startSnapshots), maxStartSnapshots)
	}
}

// TestCapturerNoStartSnapshotFallsBackToCompletionRead covers the degraded path:
// a process that missed run_started (restart mid-run, leader change) still
// records a snapshot rather than none — exactly the pre-fix behaviour, never a
// missing row.
func TestCapturerNoStartSnapshotFallsBackToCompletionRead(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	if _, err := c.store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "vendor-key-1", RunID: uuid.New(), CompletedAt: t0,
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	jobID, runID := uuid.New(), uuid.New()
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": "2026-07-03T04:31:00Z"}, nil, "max_order_ts")

	// No handleRunStarted for this run.
	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	consumed := consumedSnapshotOf(t, c, "staging.orders")
	if consumed["raw.vendor_x"] != "vendor-key-1" {
		t.Fatalf("consumed snapshot = %v, want the completion-time fallback raw.vendor_x=vendor-key-1", consumed)
	}
}

func TestCapturerBackfillNeverAdvances(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	bf := uuid.New()
	seedProducingRun(t, db, jobID, runID, map[string]string{"max_order_ts": "2026-07-03T04:31:00Z"}, &bf, "max_order_ts")

	c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

	if _, ok, err := c.store.Get(ctx, nil, "staging.orders"); err != nil || ok {
		t.Fatalf("backfill run must not create/advance dataset state (ok=%v err=%v)", ok, err)
	}
}

// TestCapturerDegradedVerifiesWithoutWatermarkKey covers the legitimate degraded
// mode: the produced dataset declares NO watermark key, so a successful run
// refreshes verified_at against completion time (conflating "ran" with
// "advanced", the design's honest limitation).
func TestCapturerDegradedVerifiesWithoutWatermarkKey(t *testing.T) {
	db := openRegistryDB(t)
	c := NewCapturer(event.New(), db)
	ctx := context.Background()

	jobID, runID := uuid.New(), uuid.New()
	// No declared watermark key -> degraded verify.
	seedProducingRun(t, db, jobID, runID, map[string]string{"rows": "42"}, nil, "")

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

// TestCapturerMissingDeclaredWatermarkSkips covers a produced dataset that
// DECLARES a watermark key but whose step does not supply a usable value — the
// key is either absent OR emitted as an empty/whitespace string. None is
// degraded mode (that is only for datasets with no declared key), so refreshing
// verified_at would mark a stale value fresh. State must be left untouched.
func TestCapturerMissingDeclaredWatermarkSkips(t *testing.T) {
	cases := []struct {
		name   string
		output map[string]string
	}{
		{"absent key", map[string]string{"rows": "42"}},
		{"empty value", map[string]string{"max_order_ts": ""}},
		{"whitespace value", map[string]string{"max_order_ts": "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openRegistryDB(t)
			c := NewCapturer(event.New(), db)
			ctx := context.Background()

			jobID, runID := uuid.New(), uuid.New()
			// Declares max_order_ts; the step supplies no usable value.
			seedProducingRun(t, db, jobID, runID, tc.output, nil, "max_order_ts")

			c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

			if _, ok, err := c.store.Get(ctx, nil, "staging.orders"); err != nil || ok {
				t.Fatalf("declared-but-unusable watermark must leave state untouched (ok=%v err=%v)", ok, err)
			}
		})
	}
}

// TestDecodeOutputPreservesLargeInts proves the ##caesium::output decode keeps a
// large integer watermark exact (UseNumber), not rounded through float64.
func TestDecodeOutputPreservesLargeInts(t *testing.T) {
	raw := datatypes.JSON([]byte(`{"wm":9007199254740993,"name":"orders.parquet","flag":true}`))
	out := decodeOutput(raw)
	if out["wm"] != "9007199254740993" {
		t.Fatalf("large int watermark = %q, want 9007199254740993 (float64 would round)", out["wm"])
	}
	if out["name"] != "orders.parquet" {
		t.Fatalf("string value = %q, want orders.parquet", out["name"])
	}
	if out["flag"] != "true" {
		t.Fatalf("bool value = %q, want true", out["flag"])
	}
}

// TestDecodeOutputDropsNonScalar proves decodeOutput drops non-scalar JSON
// values (null, object, array): its type switch handles only string / number /
// bool, so any other JSON type matches no case and the key is omitted entirely
// (never coerced to a string like "<nil>"). A sibling scalar key still survives.
func TestDecodeOutputDropsNonScalar(t *testing.T) {
	raw := datatypes.JSON([]byte(`{"nul":null,"obj":{"x":1},"arr":[1,2],"ok":"v"}`))
	out := decodeOutput(raw)
	for _, k := range []string{"nul", "obj", "arr"} {
		if v, present := out[k]; present {
			t.Fatalf("non-scalar key %q should be dropped, got %q", k, v)
		}
	}
	if out["ok"] != "v" {
		t.Fatalf("scalar sibling = %q, want v", out["ok"])
	}
}

// TestCapturerNonScalarDeclaredWatermarkSkips drives the REAL producer surface:
// a step emits its DECLARED watermark key as a non-scalar JSON value (null,
// object, or array). The marker parser (task.ParseOutput) — which is what
// actually writes task_runs.output — DROPS such values, so the stored blob has
// the key absent. The capturer then sees it as not-emitted and leaves dataset
// state UNTOUCHED: no Advance, no verified_at refresh, no "<nil>" opaque
// watermark. This asserts the end-to-end marker -> stored blob -> capturer path,
// not a raw shape the pipeline can no longer produce.
func TestCapturerNonScalarDeclaredWatermarkSkips(t *testing.T) {
	cases := []struct {
		name       string
		markerJSON string
	}{
		{"null value", `{"max_order_ts": null, "rows": "5"}`},
		{"object value", `{"max_order_ts": {"x": 1}, "rows": "5"}`},
		{"array value", `{"max_order_ts": [1, 2], "rows": "5"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openRegistryDB(t)
			c := NewCapturer(event.New(), db)
			ctx := context.Background()

			// The marker parser is the real producer of task_runs.output.
			parsed, err := task.ParseOutput(strings.NewReader("##caesium::output " + tc.markerJSON))
			if err != nil {
				t.Fatalf("ParseOutput: %v", err)
			}
			if v, present := parsed["max_order_ts"]; present {
				t.Fatalf("producer must drop the non-scalar watermark, but stored %q", v)
			}
			blob, err := json.Marshal(parsed)
			if err != nil {
				t.Fatalf("marshal parsed output: %v", err)
			}

			jobID, runID := uuid.New(), uuid.New()
			// Seed EXACTLY what the producer path stores (watermark key absent).
			seedProducingRunRaw(t, db, jobID, runID, blob, nil, "max_order_ts")

			c.handleRunCompleted(ctx, event.Event{Type: event.TypeRunCompleted, JobID: jobID, RunID: runID})

			if _, ok, err := c.store.Get(ctx, nil, "staging.orders"); err != nil || ok {
				t.Fatalf("non-scalar declared watermark must leave state untouched (ok=%v err=%v)", ok, err)
			}
		})
	}
}
