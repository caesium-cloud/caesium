package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Derivation ownership (identity-and-access B6): every decision is stamped
// with its producer and that producer's tenancy namespace at decision time,
// skips included (most skips have no run), so derivation history stays with
// the namespace that made the decision.

func moveJob(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace string) {
	t.Helper()
	if err := db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", namespace).Error; err != nil {
		t.Fatalf("move job: %v", err)
	}
}

func onlyDerivation(t *testing.T, db *gorm.DB, decision string) models.DatasetDerivation {
	t.Helper()
	var rows []models.DatasetDerivation
	if err := db.Where("decision = ?", decision).Find(&rows).Error; err != nil {
		t.Fatalf("load derivations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s derivations = %d, want 1", decision, len(rows))
	}
	return rows[0]
}

func requireOwner(t *testing.T, row models.DatasetDerivation, jobID uuid.UUID, namespace string) {
	t.Helper()
	if row.ProducerJobID == nil || *row.ProducerJobID != jobID {
		t.Fatalf("producer = %v, want %v", row.ProducerJobID, jobID)
	}
	if row.JobNamespace == nil || *row.JobNamespace != namespace {
		t.Fatalf("job namespace = %v, want %q", row.JobNamespace, namespace)
	}
}

func TestDerivationSkipIsStampedWithItsProducerNamespace(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "stamp-paused")
	moveJob(t, db, jobID, "marketing")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	if err := db.Model(&models.Job{}).Where("id = ?", jobID).Update("paused", true).Error; err != nil {
		t.Fatalf("pause job: %v", err)
	}
	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{DB: db, RunStore: starter, LaunchRun: starter.launch, MaxDerivationsPerTick: 50, Now: func() time.Time { return now }})
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	skip := onlyDerivation(t, db, models.DatasetDecisionSkippedAdmission)
	if skip.RunID != nil {
		t.Fatalf("a paused skip has no run, got %v", skip.RunID)
	}
	requireOwner(t, skip, jobID, "marketing")

	// The state's reason is recorded with the producer that wrote it.
	var state models.DatasetState
	if err := db.Where("name = ?", "out").Take(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Reason == "" || state.ReasonNamespace == nil || *state.ReasonNamespace != "marketing" {
		t.Fatalf("state reason %q owned by %v, want a marketing-owned reason", state.Reason, state.ReasonNamespace)
	}

	// After a move the next decision belongs to the new namespace; the old
	// one keeps its owner.
	moveJob(t, db, jobID, "finance")
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate after move: %v", err)
	}
	var rows []models.DatasetDerivation
	if err := db.Order("created_at, id").Find(&rows).Error; err != nil {
		t.Fatalf("load derivations: %v", err)
	}
	owners := map[string]int{}
	for _, row := range rows {
		owners[*row.JobNamespace]++
	}
	if owners["marketing"] != 1 || owners["finance"] != 1 {
		t.Fatalf("derivation owners = %v, want one marketing and one finance", owners)
	}
	if err := db.Where("name = ?", "out").Take(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if *state.ReasonNamespace != "finance" {
		t.Fatalf("reason owner after move = %q, want finance", *state.ReasonNamespace)
	}
}

// TestDecisionIsOwnedByItsDeclarationSnapshot: an apply that moves the
// producer after the evaluation read its declarations must not hand the
// decision (computed from the old declarations) to the new namespace.
func TestDecisionIsOwnedByItsDeclarationSnapshot(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "stamp-snapshot")
	moveJob(t, db, jobID, "marketing")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	if err := db.Model(&models.Job{}).Where("id = ?", jobID).Update("paused", true).Error; err != nil {
		t.Fatalf("pause job: %v", err)
	}
	prev := afterRegistrySnapshot
	afterRegistrySnapshot = func() { moveJob(t, db, jobID, "finance") }
	t.Cleanup(func() { afterRegistrySnapshot = prev })
	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{DB: db, RunStore: starter, LaunchRun: starter.launch, MaxDerivationsPerTick: 50, Now: func() time.Time { return now }})
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	requireOwner(t, onlyDerivation(t, db, models.DatasetDecisionSkippedAdmission), jobID, "marketing")
	var state models.DatasetState
	if err := db.Where("name = ?", "out").Take(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.ReasonNamespace == nil || *state.ReasonNamespace != "marketing" {
		t.Fatalf("reason owner = %v, want marketing (the snapshot's)", state.ReasonNamespace)
	}
}

// TestDerivedRunRefusedWhenTheProducerMovedSinceTheSnapshot: a run derived
// from one namespace's declarations is never admitted in another.
func TestDerivedRunRefusedWhenTheProducerMovedSinceTheSnapshot(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "stamp-fence")
	moveJob(t, db, jobID, "finance")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	prev := afterRegistrySnapshot
	afterRegistrySnapshot = func() { moveJob(t, db, jobID, "marketing") }
	t.Cleanup(func() { afterRegistrySnapshot = prev })
	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{DB: db, RunStore: starter, LaunchRun: starter.launch, MaxDerivationsPerTick: 50, Now: func() time.Time { return now }})
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(starter.runIDs) != 0 {
		t.Fatalf("a run was admitted after the producer moved: %v", starter.runIDs)
	}
	skip := onlyDerivation(t, db, models.DatasetDecisionSkippedAdmission)
	requireOwner(t, skip, jobID, "finance")

	// The next evaluation reads the moved job and derives in marketing.
	afterRegistrySnapshot = nil
	starter.namespace = "marketing"
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("re-evaluate: %v", err)
	}
	requireOwner(t, onlyDerivation(t, db, models.DatasetDecisionDerived), jobID, "marketing")
}

// TestOwnerlessSnapshotStaysOwnerless: declarations read while their job was
// deleted are owned by nobody, even when the alias is revived in another
// namespace before the decision is recorded.
func TestOwnerlessSnapshotStaysOwnerless(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "stamp-revived")
	moveJob(t, db, jobID, "finance")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	if err := db.Delete(&models.Job{}, "id = ?", jobID).Error; err != nil {
		t.Fatalf("soft-delete job: %v", err)
	}
	prev := afterRegistrySnapshot
	afterRegistrySnapshot = func() {
		if err := db.Unscoped().Model(&models.Job{}).Where("id = ?", jobID).Updates(map[string]any{"deleted_at": nil, "namespace": "marketing"}).Error; err != nil {
			t.Fatalf("revive job: %v", err)
		}
	}
	t.Cleanup(func() { afterRegistrySnapshot = prev })
	starter := &fakeRunStarter{t: t, db: db, namespace: "marketing"}
	eval := NewEvaluator(Config{DB: db, RunStore: starter, LaunchRun: starter.launch, MaxDerivationsPerTick: 50, Now: func() time.Time { return now }})
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(starter.runIDs) != 0 {
		t.Fatalf("a run was admitted from an ownerless snapshot: %v", starter.runIDs)
	}
	skip := onlyDerivation(t, db, models.DatasetDecisionSkippedAdmission)
	if skip.JobNamespace != nil {
		t.Fatalf("an ownerless decision was stamped %q", *skip.JobNamespace)
	}
	var state models.DatasetState
	if err := db.Where("name = ?", "out").Take(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.ReasonNamespace != nil {
		t.Fatalf("an ownerless reason was stamped %q", *state.ReasonNamespace)
	}
}

func TestDerivedDecisionIsOwnedByTheRunsNamespace(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "stamp-derived")
	moveJob(t, db, jobID, "marketing")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	// The job moves while its run is being admitted: the run was persisted
	// in marketing, so the decision is marketing's.
	starter := &fakeRunStarter{t: t, db: db, namespace: "marketing"}
	starter.afterCommit = func(uuid.UUID, uuid.UUID) { moveJob(t, db, jobID, "finance") }
	eval := NewEvaluator(Config{DB: db, RunStore: starter, LaunchRun: starter.launch, MaxDerivationsPerTick: 50, Now: func() time.Time { return now }})
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	derived := onlyDerivation(t, db, models.DatasetDecisionDerived)
	requireOwner(t, derived, jobID, "marketing")
}

func TestCronSkipIsStampedWithItsProducerNamespace(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(2 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "stamp-cron", models.TriggerTypeCron)
	moveJob(t, db, jobID, "finance")
	seedDeclarations(t, db, produceDecl(jobID, "mart.orders", "1h", ""), consumeDecl(jobID, "raw.orders"))
	seedState(t, db, "mart.orders", "100", now.Add(-10*time.Minute), map[string]string{"raw.orders": "42"})
	seedState(t, db, "raw.orders", "42", now.Add(-5*time.Minute), nil)
	decision, err := ShouldSkipCronRun(context.Background(), db, jobID, now)
	if err != nil || !decision.Skip {
		t.Fatalf("skip decision = %+v, %v", decision, err)
	}
	requireOwner(t, onlyDerivation(t, db, models.DatasetDecisionSkippedFresh), jobID, "finance")
}

func TestDerivationForAVanishedProducerHasNoNamespace(t *testing.T) {
	db := openRegistryDB(t)
	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{DB: db, RunStore: starter, LaunchRun: starter.launch, Now: func() time.Time { return t0 }})
	gone := uuid.New()
	decl := models.DatasetDeclaration{JobID: gone, Name: "out", Direction: models.DatasetDirectionProduces}
	if err := eval.recordDerivationTx(context.Background(), db, decl, models.DatasetDecisionSkippedFresh, "fresh", nil, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	row := onlyDerivation(t, db, models.DatasetDecisionSkippedFresh)
	if row.ProducerJobID == nil || *row.ProducerJobID != gone || row.JobNamespace != nil {
		t.Fatalf("producer %v namespace %v, want the job id and no namespace (a partially stamped row)", row.ProducerJobID, row.JobNamespace)
	}

	producers, err := ProducersTx(db, "", "out")
	if err != nil || len(producers) != 0 {
		t.Fatalf("producers of an undeclared dataset = %v, %v", producers, err)
	}
}

func TestProducersMatchANullDatasetNamespace(t *testing.T) {
	db := openRegistryDB(t)
	jobID := seedFreshnessJob(t, db, "producer-null-ns")
	moveJob(t, db, jobID, "marketing")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""), consumeDecl(jobID, "in"))
	producers, err := ProducersTx(db, "", "out")
	if err != nil {
		t.Fatalf("producers: %v", err)
	}
	if len(producers) != 1 || producers[0].JobID != jobID || producers[0].Namespace != "marketing" || producers[0].Alias != "producer-null-ns" {
		t.Fatalf("producers = %+v", producers)
	}
	if producers, err := ProducersTx(db, "", "in"); err != nil || len(producers) != 0 {
		t.Fatalf("a consumer is not a producer: %+v, %v", producers, err)
	}
}

// TestConsumedSnapshotRecordsTheRunThatWroteIt: the snapshot's owner moves
// only with the snapshot, not with every accepted observation.
func TestConsumedSnapshotRecordsTheRunThatWroteIt(t *testing.T) {
	db := openRegistryDB(t)
	store := NewStore(db)
	ctx := context.Background()
	first, second := uuid.New(), uuid.New()
	if _, err := store.Advance(ctx, AdvanceInput{Name: "out", Watermark: "1", RunID: first, CompletedAt: t0, Consumed: map[string]string{"in": "a"}}); err != nil {
		t.Fatalf("first advance: %v", err)
	}
	result, err := store.Advance(ctx, AdvanceInput{Name: "out", Watermark: "2", RunID: second, CompletedAt: t0.Add(time.Minute)})
	if err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if result.State.LastRunID == nil || *result.State.LastRunID != second {
		t.Fatalf("last run = %v, want %v", result.State.LastRunID, second)
	}
	var state models.DatasetState
	if err := db.Where("name = ?", "out").Take(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.ConsumedRunID == nil || *state.ConsumedRunID != first || len(state.ConsumedWatermarks) == 0 {
		t.Fatalf("consumed snapshot %s owned by %v, want the first run's", state.ConsumedWatermarks, state.ConsumedRunID)
	}
	if _, err := store.Advance(ctx, AdvanceInput{Name: "out", Watermark: "3", RunID: second, CompletedAt: t0.Add(2 * time.Minute), Consumed: map[string]string{"in": "b"}}); err != nil {
		t.Fatalf("third advance: %v", err)
	}
	if err := db.Where("name = ?", "out").Take(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.ConsumedRunID == nil || *state.ConsumedRunID != second {
		t.Fatalf("consumed owner = %v, want %v", state.ConsumedRunID, second)
	}
}
