package freshness

import (
	"context"
	"testing"

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

	out, err := EnrichStartParams(ctx, db, jobID, map[string]string{"logical_date": "2026-07-03"})
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
	if _, err := EnrichStartParams(context.Background(), db, jobID, in); err != nil {
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
	})
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

	out, err := EnrichStartParams(context.Background(), db, jobID, nil)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if got := out[freshnessConsumedWatermarksParam]; got != `{}` {
		t.Fatalf("%s = %q, want an authoritative empty view", freshnessConsumedWatermarksParam, got)
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

			out, err := EnrichStartParams(ctx, db, jobID, map[string]string{"logical_date": "2026-07-03"})
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
