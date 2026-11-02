package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"gorm.io/gorm"
)

// The release transaction itself takes the persisted `held` status off the
// dataset's state row (internal/run releaseDatasetHoldTx), so it cannot outlive
// the hold when nothing ever re-evaluates the dataset: the producer dropped its
// freshness SLO while the dataset was held (the evaluator skips datasets with no
// SLO), or freshness scheduling is off entirely. No evaluator pass runs between
// the release and the assertions below.
func TestReleaseTransactionClearsHeldWithoutAnEvaluatorPass(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "drops-slo")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	hold := seedActiveHold(t, db, "", "out")
	starter := &fakeRunStarter{t: t, db: db}
	eval := newHoldEvaluator(db, starter, now)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate held: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
		t.Fatalf("status = %q, want held", got)
	}

	// The SLO is dropped while the dataset is held; an evaluator pass no longer
	// visits it, and a live hold keeps the row held.
	if err := db.Model(&models.DatasetDeclaration{}).Where("job_id = ?", jobID).Update("freshness", "").Error; err != nil {
		t.Fatalf("drop SLO: %v", err)
	}
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate held without SLO: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
		t.Fatalf("an active hold without an SLO changed the row to %q, want held", got)
	}

	releaseWithRealStore(t, db, hold)
	state := heldStateRow(t, db, "out")
	if state.Status != models.DatasetStatusUnknown || state.Reason != "" || state.ReasonNamespace != nil {
		t.Fatalf("after release status=%q reason=%q reason_namespace=%v, want unknown with no reason", state.Status, state.Reason, state.ReasonNamespace)
	}
	if state.Watermark != "100" {
		t.Fatalf("release touched the watermark: %q", state.Watermark)
	}
}

// Only the released dataset's own row is touched, and only if it is held.
func TestReleaseTransactionLeavesOtherRowsAlone(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	seedState(t, db, "released", "1", now.Add(-time.Minute), nil)
	seedState(t, db, "other-held", "1", now.Add(-time.Minute), nil)
	seedState(t, db, "fresh-one", "1", now.Add(-time.Minute), nil)
	for name, status := range map[string]string{"released": models.DatasetStatusHeld, "other-held": models.DatasetStatusHeld, "fresh-one": models.DatasetStatusFresh} {
		if err := db.Model(&models.DatasetState{}).Where("name = ?", name).Updates(map[string]any{"status": status, "reason": "r"}).Error; err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	hold := seedActiveHold(t, db, "", "released")
	// An unrelated hold on a same-named dataset in another namespace.
	seedActiveHold(t, db, "tenant", "released")

	releaseWithRealStore(t, db, hold)

	for name, want := range map[string]string{
		"released":   models.DatasetStatusUnknown,
		"other-held": models.DatasetStatusHeld,
		"fresh-one":  models.DatasetStatusFresh,
	} {
		if got := heldStateRow(t, db, name).Status; got != want {
			t.Fatalf("%s status = %q, want %q", name, got, want)
		}
	}
}

func releaseWithRealStore(t *testing.T, db *gorm.DB, hold models.DatasetHold) {
	t.Helper()
	if _, err := runstorage.NewStore(db).ReleaseHold(context.Background(), hold.ID, runstorage.ReleaseHoldParams{ReleasedBy: "operator", Note: "test"}); err != nil {
		t.Fatalf("release hold: %v", err)
	}
}
