package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

// An evaluation snapshots the active holds, then writes `held` later. If the
// hold is released in between (the release also clears any persisted `held`),
// the stale verdict must NOT be written back: the write re-checks the hold at
// write time. Without that, a dataset whose SLO is gone would keep `held` forever
// because no later pass revisits it.
func TestHeldWriteIsFencedOnTheHoldStillBeingActive(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "fence")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	hold := seedActiveHold(t, db, "", "out")

	released := false
	afterHoldsSnapshot = func() {
		// The pass already holds a snapshot saying "held". Release lands now,
		// and an apply drops the SLO, so no later pass will visit the dataset.
		releaseWithRealStore(t, db, hold)
		if err := db.Model(&models.DatasetDeclaration{}).Where("job_id = ?", jobID).Update("freshness", "").Error; err != nil {
			t.Errorf("drop SLO: %v", err)
		}
		released = true
	}
	t.Cleanup(func() { afterHoldsSnapshot = nil })

	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !released {
		t.Fatal("the interleaving hook never ran")
	}
	if got := heldStateRow(t, db, "out").Status; got == models.DatasetStatusHeld {
		t.Fatal("a held status was persisted from a snapshot whose hold had already been released")
	}

	// A later pass cannot resurrect it either (no SLO left to evaluate).
	afterHoldsSnapshot = nil
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate again: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got == models.DatasetStatusHeld {
		t.Fatal("held reappeared after release")
	}
}

// The fence only gates `held`; every other status is written as before.
func TestHeldFenceDoesNotGateOtherStatuses(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "plain")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	starter := &fakeRunStarter{t: t, db: db}
	if err := newHoldEvaluator(db, starter, now).EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusFresh {
		t.Fatalf("status = %q, want fresh", got)
	}
}
