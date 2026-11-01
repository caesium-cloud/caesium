package freshness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// setDataAssertions flips CAESIUM_DATA_ASSERTIONS_ENABLED for one test and
// restores the process-wide env afterwards.
func setDataAssertions(t *testing.T, on bool) {
	t.Helper()
	value := "false"
	if on {
		value = "true"
	}
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", value)
	if err := env.Process(); err != nil {
		t.Fatalf("env.Process: %v", err)
	}
	t.Cleanup(func() {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
		_ = env.Process()
	})
}

func seedActiveHold(t *testing.T, db *gorm.DB, namespace, name string) models.DatasetHold {
	t.Helper()
	now := time.Now().UTC()
	key := models.DatasetHoldKey(namespace, name)
	hold := models.DatasetHold{
		ID:              uuid.New(),
		Namespace:       namespace,
		Name:            name,
		Status:          models.DatasetHoldStatusActive,
		ActiveKey:       &key,
		Reason:          "min",
		HeldByJobID:     uuid.New(),
		OccurrenceCount: 1,
		OpenedAt:        now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(&hold).Error; err != nil {
		t.Fatalf("create hold: %v", err)
	}
	return hold
}

func releaseHold(t *testing.T, db *gorm.DB, hold models.DatasetHold) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Model(&models.DatasetHold{}).Where("id = ?", hold.ID).Updates(map[string]any{
		"status":         models.DatasetHoldStatusReleased,
		"active_key":     nil,
		"released_at":    now,
		"released_by":    "system",
		"release_reason": models.DatasetHoldReleaseCleanRun,
	}).Error; err != nil {
		t.Fatalf("release hold: %v", err)
	}
}

func heldStateRow(t *testing.T, db *gorm.DB, name string) models.DatasetState {
	t.Helper()
	var state models.DatasetState
	if err := db.Where("namespace = ? AND name = ?", "", name).Take(&state).Error; err != nil {
		t.Fatalf("load state %s: %v", name, err)
	}
	return state
}

func newHoldEvaluator(db *gorm.DB, starter *fakeRunStarter, now time.Time) *Evaluator {
	return NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
}

func TestStatusForHeldOverridesComputedStatusAndNamesTheHold(t *testing.T) {
	e := &Evaluator{}
	now := t0.Add(3 * time.Hour)
	decl := produceDecl(uuid.New(), "out", "1h", "")
	freshAt := now.Add(-10 * time.Minute)
	state := models.DatasetState{Name: "out", AdvancedAt: &freshAt, Status: models.DatasetStatusUnknown}
	hold := &heldDataset{id: uuid.New(), name: "out"}

	status, _, _, seen := e.statusFor(decl, state, now, time.Hour, 0, true, "", nil)
	if status != models.DatasetStatusFresh || !seen {
		t.Fatalf("unheld status = %q seen=%v, want fresh", status, seen)
	}

	status, reason, staleness, seen := e.statusFor(decl, state, now, time.Hour, 0, true, "", hold)
	if status != models.DatasetStatusHeld {
		t.Fatalf("held status = %q, want %q", status, models.DatasetStatusHeld)
	}
	if want := "dataset_hold:/out hold=" + hold.id.String(); reason != want {
		t.Fatalf("held reason = %q, want %q", reason, want)
	}
	if !seen || staleness != 10*time.Minute {
		t.Fatalf("held staleness = %s seen=%v, want the computed 10m", staleness, seen)
	}

	// A replay-run quarantine is a different disposition and is not overridden.
	state.Status = models.DatasetStatusQuarantined
	status, _, _, _ = e.statusFor(decl, state, now, time.Hour, 0, true, "", hold)
	if status != models.DatasetStatusQuarantined {
		t.Fatalf("quarantined with a hold = %q, want quarantined", status)
	}
}

// A held, recent dataset reports held (not fresh), derives nothing, and the
// decision log does not claim skipped_fresh. Release restores the computed
// status on the next evaluation with no manual step, and the row is not left
// at held.
func TestEvaluatorHeldDatasetIsNotFreshAndReleaseRestoresIt(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "held-out")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	hold := seedActiveHold(t, db, "", "out")

	starter := &fakeRunStarter{t: t, db: db}
	eval := newHoldEvaluator(db, starter, now)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate held: %v", err)
	}
	state := heldStateRow(t, db, "out")
	if state.Status != models.DatasetStatusHeld {
		t.Fatalf("status = %q, want held", state.Status)
	}
	if !strings.Contains(state.Reason, "dataset_hold:/out") || !strings.Contains(state.Reason, hold.ID.String()) {
		t.Fatalf("reason = %q, want it to name the hold", state.Reason)
	}
	if starter.calls != 0 {
		t.Fatalf("a held, computed-fresh dataset started %d runs, want 0", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedFresh, 0)
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)

	releaseHold(t, db, hold)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate released: %v", err)
	}
	state = heldStateRow(t, db, "out")
	if state.Status != models.DatasetStatusFresh {
		t.Fatalf("status after release = %q, want fresh", state.Status)
	}
	if strings.Contains(state.Reason, "dataset_hold") {
		t.Fatalf("reason after release = %q, still names the hold", state.Reason)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedFresh, 1)
}

// With the data circuit breaker off, a stale `active` row must not change
// freshness - and the evaluator must not even read the table.
func TestEvaluatorIgnoresHoldsWhenAssertionsAreDisabled(t *testing.T) {
	setDataAssertions(t, false)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "gate-off")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	seedActiveHold(t, db, "", "out")
	if err := db.Migrator().DropTable(&models.DatasetHold{}); err != nil {
		t.Fatalf("drop holds: %v", err)
	}

	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusFresh {
		t.Fatalf("status with the gate off = %q, want fresh", got)
	}
}

// A dataset identity is the exact (namespace, name) pair. "warehouse/orders"
// stored whole in Name must not be confused with namespace "warehouse" + name
// "orders" (their concatenated hold keys are identical), in either direction.
func TestEvaluatorHoldMatchesExactNamespaceAndName(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobA := seedFreshnessJob(t, db, "slash-name")
	jobB := seedFreshnessJob(t, db, "split-ns")
	ns := "warehouse"
	other := produceDecl(jobB, "orders", "1h", "")
	other.Namespace = &ns
	seedDeclarations(t, db, produceDecl(jobA, "warehouse/orders", "1h", ""), other)
	seedState(t, db, "warehouse/orders", "100", now.Add(-10*time.Minute), nil)
	fresh := now.Add(-10 * time.Minute)
	if err := db.Create(&models.DatasetState{ID: uuid.New(), Namespace: "warehouse", Name: "orders", Watermark: "7",
		AdvancedAt: &fresh, Status: models.DatasetStatusUnknown, CreatedAt: fresh, UpdatedAt: fresh}).Error; err != nil {
		t.Fatalf("create namespaced state: %v", err)
	}

	// The hold is on the slash-named, empty-namespace dataset only.
	seedActiveHold(t, db, "", "warehouse/orders")

	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := heldStateRow(t, db, "warehouse/orders").Status; got != models.DatasetStatusHeld {
		t.Fatalf("slash-named dataset status = %q, want held", got)
	}
	var namespaced models.DatasetState
	if err := db.Where("namespace = ? AND name = ?", "warehouse", "orders").Take(&namespaced).Error; err != nil {
		t.Fatalf("load namespaced state: %v", err)
	}
	if namespaced.Status != models.DatasetStatusFresh {
		t.Fatalf("namespaced dataset status = %q, want fresh (a different dataset)", namespaced.Status)
	}
}

// A consumer of a held dataset is never derived from it, and reports why. Its
// own data is not poisoned, so the honest status is stale-upstream (with the
// hold named), not held. Releasing the hold lets it derive.
func TestEvaluatorHeldUpstreamBlocksDownstreamDerivation(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	p1 := seedFreshnessJob(t, db, "stage-producer")
	p2 := seedFreshnessJob(t, db, "mart-producer")
	seedDeclarations(t, db,
		produceDecl(p1, "staging", "1h", ""),
		produceDecl(p2, "mart", "1h", ""),
		consumeDecl(p2, "staging"),
	)
	// mart is stale and consumed staging="1"; staging has advanced to "2", so
	// without a hold mart derives.
	seedState(t, db, "mart", "500", now.Add(-2*time.Hour), map[string]string{"staging": "1"})
	seedState(t, db, "staging", "2", now.Add(-5*time.Minute), nil)
	hold := seedActiveHold(t, db, "", "staging")

	starter := &fakeRunStarter{t: t, db: db}
	eval := newHoldEvaluator(db, starter, now)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate held upstream: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("downstream of a held dataset started %d runs, want 0", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
	mart := heldStateRow(t, db, "mart")
	if mart.Status != models.DatasetStatusStaleUpstream {
		t.Fatalf("mart status = %q, want stale-upstream", mart.Status)
	}
	if !strings.Contains(mart.Reason, "held upstream dataset") || !strings.Contains(mart.Reason, "dataset_hold:/staging") {
		t.Fatalf("mart reason = %q, want it to name the held upstream", mart.Reason)
	}
	if got := heldStateRow(t, db, "staging").Status; got != models.DatasetStatusHeld {
		t.Fatalf("staging status = %q, want held", got)
	}

	releaseHold(t, db, hold)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate released: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("after release started %d runs, want 1 derived mart run", starter.calls)
	}
	if got := heldStateRow(t, db, "staging").Status; got != models.DatasetStatusFresh {
		t.Fatalf("staging status after release = %q, want fresh", got)
	}
}

// A held dataset whose SLO is really breached still re-derives its producer:
// that re-run is the only way a freshness-triggered producer's clean run can
// release an auto-release hold. The hold changes what is REPORTED, not whether a
// genuinely stale dataset is repaired.
func TestEvaluatorStaleHeldProducerStillDerivesForRecovery(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "stale-held")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	seedActiveHold(t, db, "", "out")

	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
		t.Fatalf("status = %q, want held", got)
	}
	if starter.calls != 1 {
		t.Fatalf("a stale held producer started %d runs, want 1 recovery run", starter.calls)
	}
}

// The cron skip-when-fresh decision treats a held produced dataset as not
// fresh, so the tick runs (and a clean run can release the hold).
func TestShouldSkipCronRunRunsWhenProducedDatasetIsHeld(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(2 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "cron-held", models.TriggerTypeCron)
	seedDeclarations(t, db, produceDecl(jobID, "mart.orders", "1h", ""))
	seedState(t, db, "mart.orders", "100", now.Add(-10*time.Minute), nil)

	decision, err := ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil || !decision.Skip {
		t.Fatalf("baseline decision = %+v err=%v, want skip", decision, err)
	}

	hold := seedActiveHold(t, db, "", "mart.orders")
	decision, err = ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil {
		t.Fatalf("held decision: %v", err)
	}
	if decision.Skip {
		t.Fatalf("a held dataset let the cron tick skip: %+v", decision)
	}
	if !strings.Contains(decision.Reason, "dataset_hold:/mart.orders") {
		t.Fatalf("skip reason = %q, want it to name the hold", decision.Reason)
	}

	releaseHold(t, db, hold)
	decision, err = ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil || !decision.Skip {
		t.Fatalf("decision after release = %+v err=%v, want skip", decision, err)
	}

	setDataAssertions(t, false)
	seedActiveHold(t, db, "", "mart.orders")
	decision, err = ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil || !decision.Skip {
		t.Fatalf("decision with the gate off = %+v err=%v, want skip despite the stale hold row", decision, err)
	}
}

// dataset_held and dataset_released are reactive, and their payload keys the
// dataset under "dataset" rather than "name".
func TestEvaluatorReactsToHoldEvents(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "reactive-hold")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	hold := seedActiveHold(t, db, "", "out")

	starter := &fakeRunStarter{t: t, db: db}
	eval := newHoldEvaluator(db, starter, now)
	payload, err := json.Marshal(map[string]any{"hold_id": hold.ID, "namespace": "", "dataset": "out"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, typ := range []event.Type{event.TypeDatasetHeld, event.TypeDatasetReleased} {
		if !eval.isReactiveType(typ) {
			t.Fatalf("%s is not a reactive type", typ)
		}
	}
	if err := eval.EvaluateEvent(ctx, event.Event{Type: event.TypeDatasetHeld, Payload: payload}); err != nil {
		t.Fatalf("evaluate held event: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
		t.Fatalf("status after dataset_held = %q, want held", got)
	}

	releaseHold(t, db, hold)
	if err := eval.EvaluateEvent(ctx, event.Event{Type: event.TypeDatasetReleased, Payload: payload}); err != nil {
		t.Fatalf("evaluate released event: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusFresh {
		t.Fatalf("status after dataset_released = %q, want fresh", got)
	}
}

// One read per pass: the evaluator must not query dataset_holds per dataset.
func TestEvaluatorReadsActiveHoldsOncePerPass(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "many")
	var decls []models.DatasetDeclaration
	for _, name := range []string{"a", "b", "c", "d"} {
		decls = append(decls, produceDecl(jobID, name, "1h", ""))
		seedState(t, db, name, "1", now.Add(-10*time.Minute), nil)
	}
	seedDeclarations(t, db, decls...)

	queries := 0
	if err := db.Callback().Query().After("gorm:query").Register("test:count_hold_queries", func(tx *gorm.DB) {
		if tx.Statement.Table == "dataset_holds" {
			queries++
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if queries != 1 {
		t.Fatalf("dataset_holds queries per pass = %d, want exactly 1", queries)
	}
}
