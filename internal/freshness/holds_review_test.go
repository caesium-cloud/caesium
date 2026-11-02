package freshness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

// metadata.onUpstreamHold: run opts a job out of the breaker's gate. Readiness
// must honour it, or the evaluator refuses to derive a run the admission gate
// would admit; a consumer without the opt-out stays stale-upstream.
func TestEvaluatorHonoursOnUpstreamHoldRun(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	p1 := seedFreshnessJob(t, db, "stage-producer")
	monitor := seedFreshnessJob(t, db, "monitor")
	strict := seedFreshnessJob(t, db, "strict")
	if err := db.Model(&models.Job{}).Where("id = ?", monitor).Update("on_upstream_hold", "run").Error; err != nil {
		t.Fatalf("set opt-out: %v", err)
	}
	seedDeclarations(t, db,
		produceDecl(p1, "staging", "1h", ""),
		produceDecl(monitor, "monitor-out", "1h", ""),
		consumeDecl(monitor, "staging"),
		produceDecl(strict, "strict-out", "1h", ""),
		consumeDecl(strict, "staging"),
	)
	seedState(t, db, "staging", "2", now.Add(-5*time.Minute), nil)
	seedState(t, db, "monitor-out", "500", now.Add(-2*time.Hour), map[string]string{"staging": "1"})
	seedState(t, db, "strict-out", "500", now.Add(-2*time.Hour), map[string]string{"staging": "1"})
	seedActiveHold(t, db, "", "staging")

	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("started %d runs, want exactly the opted-out monitor's", starter.calls)
	}
	if got := heldStateRow(t, db, "strict-out").Status; got != models.DatasetStatusStaleUpstream {
		t.Fatalf("strict consumer status = %q, want stale-upstream", got)
	}
	var derived models.DatasetDerivation
	if err := db.Where("decision = ?", models.DatasetDecisionDerived).Take(&derived).Error; err != nil {
		t.Fatalf("load derivation: %v", err)
	}
	if derived.Name != "monitor-out" {
		t.Fatalf("derived %q, want monitor-out", derived.Name)
	}
}

// A hold opened before the dataset's first observation, still inside its SLO,
// records an admission skip that names the hold - not skipped_fresh "waiting for
// first observation". (The held write itself is covered above.)
func TestEvaluatorHeldBeforeFirstObservationRecordsAdmissionSkip(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	jobID := seedFreshnessJob(t, db, "unobserved-decision")
	decl := produceDecl(jobID, "out", "24h", "")
	seedDeclarations(t, db, decl)
	now := decl.CreatedAt.Add(time.Minute)
	hold := seedActiveHold(t, db, "", "out")

	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("started %d runs, want 0", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedFresh, 0)
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)
	var derivation models.DatasetDerivation
	if err := db.Where("decision = ?", models.DatasetDecisionSkippedAdmission).Take(&derivation).Error; err != nil {
		t.Fatalf("load derivation: %v", err)
	}
	if want := "dataset_hold:/out hold=" + hold.ID.String(); derivation.Reason != want {
		t.Fatalf("reason = %q, want %q", derivation.Reason, want)
	}
}

// A hold persisted before any observation must not leave "held" (and the
// released hold's reason) behind once it is released.
func TestEvaluatorReleaseBeforeFirstWatermarkClearsHeld(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	jobID := seedFreshnessJob(t, db, "unobserved")
	decl := produceDecl(jobID, "out", "24h", "")
	seedDeclarations(t, db, decl)
	// Within the SLO of the declaration's creation, never observed.
	now := decl.CreatedAt.Add(time.Minute)
	hold := seedActiveHold(t, db, "", "out")

	starter := &fakeRunStarter{t: t, db: db}
	eval := newHoldEvaluator(db, starter, now)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate held: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
		t.Fatalf("status = %q, want held", got)
	}

	releaseHold(t, db, hold)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate released: %v", err)
	}
	state := heldStateRow(t, db, "out")
	if state.Status != models.DatasetStatusUnknown {
		t.Fatalf("status after release = %q, want unknown", state.Status)
	}
	if strings.Contains(state.Reason, "dataset_hold") || state.Reason != unknownFreshnessReason {
		t.Fatalf("reason after release = %q, want %q", state.Reason, unknownFreshnessReason)
	}
}
