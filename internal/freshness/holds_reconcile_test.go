package freshness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// seedHeldDataset persists the state a held, SLO-tracked dataset has: observed,
// then evaluated while its hold was active.
func seedHeldDataset(t *testing.T, db *gorm.DB, eval *Evaluator, now time.Time) (uuid.UUID, models.DatasetHold) {
	t.Helper()
	jobID := seedFreshnessJob(t, db, "drops-slo")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-10*time.Minute), nil)
	hold := seedActiveHold(t, db, "", "out")
	if err := eval.EvaluateOnce(context.Background()); err != nil {
		t.Fatalf("evaluate held: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
		t.Fatalf("status = %q, want held", got)
	}
	return jobID, hold
}

func dropSLO(t *testing.T, db *gorm.DB, jobID uuid.UUID) {
	t.Helper()
	if err := db.Model(&models.DatasetDeclaration{}).Where("job_id = ?", jobID).Update("freshness", "").Error; err != nil {
		t.Fatalf("drop SLO: %v", err)
	}
}

// A producer that drops its freshness SLO while the dataset is held is no longer
// visited by per-dataset evaluation. Releasing the hold must still take the row
// out of `held`, on the periodic pass AND on the dataset_released event.
func TestReleaseAfterDroppingTheSLOClearsHeld(t *testing.T) {
	for _, path := range []string{"tick", "event"} {
		t.Run(path, func(t *testing.T) {
			setDataAssertions(t, true)
			db := openRegistryDB(t)
			ctx := context.Background()
			now := t0.Add(3 * time.Hour)
			starter := &fakeRunStarter{t: t, db: db}
			eval := newHoldEvaluator(db, starter, now)
			jobID, hold := seedHeldDataset(t, db, eval, now)

			dropSLO(t, db, jobID)
			// Still held with no SLO: the live hold keeps the row held.
			if err := eval.EvaluateOnce(ctx); err != nil {
				t.Fatalf("evaluate held without SLO: %v", err)
			}
			if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusHeld {
				t.Fatalf("an active hold without an SLO changed the row to %q, want held", got)
			}

			releaseHold(t, db, hold)
			if path == "tick" {
				if err := eval.EvaluateOnce(ctx); err != nil {
					t.Fatalf("evaluate released: %v", err)
				}
			} else {
				payload, err := json.Marshal(map[string]any{"hold_id": hold.ID, "namespace": "", "dataset": "out"})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if err := eval.EvaluateEvent(ctx, event.Event{Type: event.TypeDatasetReleased, Payload: payload}); err != nil {
					t.Fatalf("evaluate released event: %v", err)
				}
			}
			state := heldStateRow(t, db, "out")
			if state.Status != models.DatasetStatusUnknown || state.Reason != "" {
				t.Fatalf("after release status=%q reason=%q, want unknown with an empty reason", state.Status, state.Reason)
			}
			if state.Watermark != "100" {
				t.Fatalf("reconcile touched the watermark: %q", state.Watermark)
			}
		})
	}
}

// A declaration that is gone entirely (job removed) is reconciled too, and with
// the circuit breaker off a leftover `held` row is not trusted.
func TestReconcileHeldRowsWithoutDeclarationOrGate(t *testing.T) {
	setDataAssertions(t, true)
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	starter := &fakeRunStarter{t: t, db: db}
	eval := newHoldEvaluator(db, starter, now)
	jobID, hold := seedHeldDataset(t, db, eval, now)
	if err := db.Where("job_id = ?", jobID).Delete(&models.DatasetDeclaration{}).Error; err != nil {
		t.Fatalf("delete declaration: %v", err)
	}
	releaseHold(t, db, hold)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusUnknown {
		t.Fatalf("status without a declaration = %q, want unknown", got)
	}

	// Gate off: a leftover persisted `held` with a stale active row resets.
	if err := db.Model(&models.DatasetState{}).Where("name = ?", "out").Updates(map[string]any{"status": models.DatasetStatusHeld, "reason": "dataset_hold:/out hold=x"}).Error; err != nil {
		t.Fatalf("re-hold: %v", err)
	}
	seedActiveHold(t, db, "", "out")
	setDataAssertions(t, false)
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate gate off: %v", err)
	}
	if got := heldStateRow(t, db, "out").Status; got != models.DatasetStatusUnknown {
		t.Fatalf("status with the gate off = %q, want unknown", got)
	}
}
