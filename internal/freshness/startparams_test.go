package freshness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/datatypes"
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
	if got := out[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"vendor-key-1"}` {
		t.Fatalf("%s = %q, want the creation-time view", ConsumedWatermarksStartParam, got)
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

// TestStartParamsEnricherNeverTouchesTheDerivationView pins the key separation:
// _consumed_watermarks is the evaluator's DECISION-time view and belongs to the
// evaluator alone, so the enricher must leave it exactly as it found it while
// stamping its own start-time view beside it.
func TestStartParamsEnricherNeverTouchesTheDerivationView(t *testing.T) {
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
	if got := out[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"later-key"}` {
		t.Fatalf("%s = %q, want the start-time view read now", ConsumedWatermarksStartParam, got)
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
	if got := out[ConsumedWatermarksStartParam]; got != `{}` {
		t.Fatalf("%s = %q, want an authoritative empty view", ConsumedWatermarksStartParam, got)
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
	if got := queued[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("%s = %q, want the admission-time view", ConsumedWatermarksStartParam, got)
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
	if got := promoted[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"promotion-key"}` {
		t.Fatalf("%s = %q, want the promotion-time view", ConsumedWatermarksStartParam, got)
	}
	if got := queued[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("promotion mutated the queued params: %q", got)
	}
}

// TestQueuePromotionKeepsTheDerivationDedupe is the regression for a duplicate
// freshness run. hasActiveOrQueuedRun recognises work it already scheduled by
// comparing exactly two params (sameDerivationParams): _derived_from_dataset and
// _consumed_watermarks. Promotion deletes the run_queue row, so the running row
// is the only thing left to match against — and when the enricher's refresh
// shared that key it overwrote the derivation view, the match failed, and the
// next tick derived a second run for work already in flight. The start-time view
// having its own key is what keeps the match.
func TestQueuePromotionKeepsTheDerivationDedupe(t *testing.T) {
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

	// Exactly what derive() builds, and what a later tick evaluating the same
	// unchanged inputs will build again and compare with.
	derivation := map[string]string{
		freshnessDerivedFromDatasetParam: "staging.orders",
		freshnessConsumedWatermarksParam: `{"raw.vendor_x":"admission-key"}`,
	}

	queued, err := EnrichStartParams(ctx, db, jobID, derivation, false)
	if err != nil {
		t.Fatalf("enrich at admission: %v", err)
	}

	// The input advances while the run waits in run_queue, so the promotion read
	// returns a different view from the derivation one.
	if _, err := store.Advance(ctx, AdvanceInput{
		Name: "raw.vendor_x", Watermark: "promotion-key", RunID: uuid.New(), CompletedAt: t0.Add(time.Hour),
	}); err != nil {
		t.Fatalf("advance upstream: %v", err)
	}

	promoted, err := EnrichStartParams(ctx, db, jobID, queued, true)
	if err != nil {
		t.Fatalf("enrich at promotion: %v", err)
	}
	if got := promoted[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("promotion overwrote the derivation view: %s = %q", freshnessConsumedWatermarksParam, got)
	}
	if got := promoted[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"promotion-key"}` {
		t.Fatalf("%s = %q, want the promotion-time view", ConsumedWatermarksStartParam, got)
	}

	// The dequeuer deletes the run_queue row after StartQueuedRun succeeds, so
	// only the running job_runs row remains for the dedupe to see.
	seedRunningRun(t, db, jobID, promoted)

	eval := NewEvaluator(Config{DB: db, RunStore: &fakeRunAdmitter{t: t, db: db}})
	active, err := eval.hasActiveOrQueuedRun(ctx, jobID, derivation)
	if err != nil {
		t.Fatalf("hasActiveOrQueuedRun: %v", err)
	}
	if !active {
		t.Fatal("the promoted run no longer matches its derivation view; freshness would derive a duplicate")
	}
}

// seedRunningRun writes a running job_runs row carrying params, the way run
// creation persists them.
func seedRunningRun(t *testing.T, db *gorm.DB, jobID uuid.UUID, params map[string]string) {
	t.Helper()
	blob, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	now := t0.Add(time.Hour)
	if err := db.Create(&models.JobRun{
		ID: uuid.New(), JobID: jobID, TriggerID: uuid.New(),
		Status: string(runstorage.StatusRunning), Params: datatypes.JSON(blob),
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed running run: %v", err)
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
	if _, ok := out[ConsumedWatermarksStartParam]; ok {
		t.Fatalf("a failed read was written down as a view: %v", out)
	}
	if out["logical_date"] != "2026-07-03" {
		t.Fatalf("the caller's params must come back unchanged: %v", out)
	}
}

// TestStartParamsEnricherDropsTheStaleViewWhenThePromotionReadFails is the other
// half of that rule, and the one a failing promotion actually depends on: the
// params arrive off the run_queue row still carrying the ADMISSION-time view, so
// "leave it absent" has to mean "make it absent". Anything else persists a view
// the run did not start on, and completion believes it. The evaluator's
// derivation view is not the enricher's to retract and must survive.
func TestStartParamsEnricherDropsTheStaleViewWhenThePromotionReadFails(t *testing.T) {
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

	queued, err := EnrichStartParams(ctx, db, jobID, map[string]string{
		freshnessConsumedWatermarksParam: `{"raw.vendor_x":"admission-key"}`,
	}, false)
	if err != nil {
		t.Fatalf("enrich at admission: %v", err)
	}
	if _, ok := queued[ConsumedWatermarksStartParam]; !ok {
		t.Fatalf("admission did not stamp a start view: %v", queued)
	}

	// The promotion-time read fails.
	if err := db.Migrator().DropTable(&models.DatasetState{}); err != nil {
		t.Fatalf("drop dataset_states: %v", err)
	}

	promoted, err := EnrichStartParams(ctx, db, jobID, queued, true)
	if err == nil {
		t.Fatal("a failed promotion read must be reported, not swallowed")
	}
	if got, ok := promoted[ConsumedWatermarksStartParam]; ok {
		t.Fatalf("the stale admission-time view survived a failed promotion read: %q", got)
	}
	if got := promoted[freshnessConsumedWatermarksParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("%s = %q, want the evaluator's view left intact", freshnessConsumedWatermarksParam, got)
	}
	if got := queued[ConsumedWatermarksStartParam]; got != `{"raw.vendor_x":"admission-key"}` {
		t.Fatalf("the retraction mutated the caller's params: %q", got)
	}
}

// TestStartParamsEnricherDropsAnInheritedViewWhenTheReadFails covers the other
// way a run is handed someone else's view: a retry re-runs with the params of
// the run it is retrying (cmd/run/retry.go, api/rest/controller/job/run/retry.go
// both pass a prior JobRun's Params), so the stale value arrives on an ordinary
// creation, with fromQueue false. Retraction cannot be gated on the queue path.
func TestStartParamsEnricherDropsAnInheritedViewWhenTheReadFails(t *testing.T) {
	db := openRegistryDB(t)
	jobID := uuid.New()
	seedEnricherJob(t, db, jobID, []string{"staging.orders"}, []string{"raw.vendor_x"})

	if err := db.Migrator().DropTable(&models.DatasetState{}); err != nil {
		t.Fatalf("drop dataset_states: %v", err)
	}

	// Exactly what a retry passes: the retried run's persisted params.
	out, err := EnrichStartParams(context.Background(), db, jobID, map[string]string{
		"logical_date":               "2026-07-03",
		ConsumedWatermarksStartParam: `{"raw.vendor_x":"the-previous-run's-view"}`,
	}, false)
	if err == nil {
		t.Fatal("a failed consumed-state read must be reported, not swallowed")
	}
	if got, ok := out[ConsumedWatermarksStartParam]; ok {
		t.Fatalf("an inherited view survived a failed read: %q", got)
	}
	if out["logical_date"] != "2026-07-03" {
		t.Fatalf("the caller's other params must survive: %v", out)
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
			if _, ok := out[ConsumedWatermarksStartParam]; ok {
				t.Fatalf("nothing to freeze, but the enricher stamped %s: %v",
					ConsumedWatermarksStartParam, out)
			}
		})
	}
}
