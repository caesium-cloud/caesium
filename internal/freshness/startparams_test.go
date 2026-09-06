package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// seedEnricherJob writes the produces/consumes registry rows for a job, without
// the run surface seedProducingRun also needs, so the enricher can be exercised
// on its own.
func seedEnricherJob(t *testing.T, db *gorm.DB, jobID uuid.UUID, produces, consumes []string) {
	t.Helper()
	decls := make([]models.DatasetDeclaration, 0, len(produces)+len(consumes))
	for _, name := range produces {
		decls = append(decls, produceDecl(jobID, name, "8h", "24h"))
	}
	for _, name := range consumes {
		decls = append(decls, consumeDecl(jobID, name))
	}
	seedDeclarations(t, db, decls...)
}

func TestStartParamsEnricherStampsConsumedView(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	store := NewStore(db)

	if _, err := store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "vendor-key-1", RunID: uuid.New(), CompletedAt: t0,
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	out, err := EnrichStartParams(ctx, db, jobID, map[string]string{"logical_date": "2026-07-03"}, false)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if got := out[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"vendor-key-1"}` {
		t.Fatalf("%s = %q, want the creation-time view", freshnessConsumedWatermarksParam, got)
	}
	if out["logical_date"] != "2026-07-03" {
		t.Fatalf("enricher dropped a caller param: %v", out)
	}
}

// TestStartParamsEnricherDoesNotMutateCallerParams pins that the caller's map is
// left alone — run creation reuses it (and the evaluator's map is shared across
// its dedupe comparison).
func TestStartParamsEnricherDoesNotMutateCallerParams(t *testing.T) {
	db := openRegistryDB(t)
	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	in := map[string]string{"logical_date": "2026-07-03"}
	if _, err := EnrichStartParams(context.Background(), db, jobID, in, false); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(in) != 1 {
		t.Fatalf("caller params were mutated: %v", in)
	}
}

// TestStartParamsEnricherKeepsDerivedView proves the evaluator's view wins: a
// freshness-derived run already carries the exact inputs its derivation decision
// was made on, so the enricher must not restamp it with a later read.
func TestStartParamsEnricherKeepsDerivedView(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	store := NewStore(db)

	if _, err := store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "later-key", RunID: uuid.New(), CompletedAt: t0,
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	out, err := EnrichStartParams(ctx, db, jobID, map[string]string{
		freshnessConsumedWatermarksParam: `{"raw.vendor_x":"derived-key"}`,
	}, false)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if got := out[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"derived-key"}` {
		t.Fatalf("%s = %q, want the evaluator's view untouched", freshnessConsumedWatermarksParam, got)
	}
}

// TestStartParamsEnricherStampsEmptyView covers the input that has no state row
// yet: "nothing had a watermark when this run was created" is authoritative, and
// omitting the param would send the completion path back to the current-value
// read the capture exists to avoid.
func TestStartParamsEnricherStampsEmptyView(t *testing.T) {
	db := openRegistryDB(t)
	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	out, err := EnrichStartParams(context.Background(), db, jobID, nil, false)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if got := out[freshnessConsumedWatermarksParam]; got != `{}` {
		t.Fatalf("%s = %q, want an authoritative empty view", freshnessConsumedWatermarksParam, got)
	}
}

// TestStartParamsEnricherRefreshesOnQueuePromotion covers the queue-strategy
// run: it is enriched once when it is enqueued and again when the dequeuer
// promotes it, and the second view is the one it actually starts on. Keeping the
// admission-time view would credit the output to inputs the run never read and
// make freshness derive redundant work to "catch up" an output that is current.
func TestStartParamsEnricherRefreshesOnQueuePromotion(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	store := NewStore(db)

	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	if _, err := store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "admission-key", RunID: uuid.New(), CompletedAt: t0,
	}); err != nil {
		t.Fatalf("seed upstream: %v", err)
	}

	queued, err := EnrichStartParams(ctx, db, jobID, nil, false)
	if err != nil {
		t.Fatalf("enrich at admission: %v", err)
	}
	if got := queued[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("%s = %q, want the admission-time view", freshnessConsumedWatermarksParam, got)
	}

	// The input advances while the run sits in run_queue.
	if _, err := store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "promotion-key", RunID: uuid.New(), CompletedAt: t0.Add(time.Hour),
	}); err != nil {
		t.Fatalf("advance upstream: %v", err)
	}

	promoted, err := EnrichStartParams(ctx, db, jobID, queued, true)
	if err != nil {
		t.Fatalf("enrich at promotion: %v", err)
	}
	if got := promoted[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"promotion-key"}` {
		t.Fatalf("%s = %q, want the promotion-time view", freshnessConsumedWatermarksParam, got)
	}
	if got := queued[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("promotion mutated the queued params: %q", got)
	}
}

// TestStartParamsEnricherOmitsViewWhenTheReadFails pins the difference between
// "no input has a watermark" and "the read failed": a failure must leave the
// param ABSENT — run.enrichedStartParams then keeps the caller's params and
// completion falls back to its own read — never stamp {} as an authoritative
// "this run consumed nothing", which completion would believe.
func TestStartParamsEnricherOmitsViewWhenTheReadFails(t *testing.T) {
	db := openRegistryDB(t)
	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	// Break only the watermark read; the declaration read above must still work,
	// so the enricher reaches the snapshot it cannot take.
	if err := db.Migrator().DropTable(&models.DatasetState{}); err != nil {
		t.Fatalf("drop dataset_states: %v", err)
	}

	in := map[string]string{"logical_date": "2026-07-03"}
	out, err := EnrichStartParams(context.Background(), db, jobID, in, false)
	if err == nil {
		t.Fatal("a failed consumed-state read must be reported, not swallowed")
	}
	if _, ok := out[freshnessConsumedWatermarksParam]; ok {
		t.Fatalf("a failed read was written down as a view: %v", out)
	}
	if out["logical_date"] != "2026-07-03" {
		t.Fatalf("the caller's params must come back unchanged: %v", out)
	}
}

func TestStartParamsEnricherIgnoresJobsWithNothingToFreeze(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		produces []string
		consumes []string
	}{
		{name: "no declarations"},
		{name: "produces only", produces: []string{"staging.orders"}},
		{name: "consumes only", consumes: []string{"raw.vendor_x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobID := uuid.New()
			seedEnricherJob(t, db, jobID, tc.produces, tc.consumes)

			out, err := EnrichStartParams(ctx, db, jobID, map[string]string{"logical_date": "2026-07-03"}, false)
			if err != nil {
				t.Fatalf("enrich: %v", err)
			}
			if _, ok := out[freshnessConsumedWatermarksParam]; ok {
				t.Fatalf("nothing to freeze, but the enricher stamped %s: %v",
					freshnessConsumedWatermarksParam, out)
			}
		})
	}
}
