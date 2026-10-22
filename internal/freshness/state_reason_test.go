package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TestAdvanceNeverWritesBackTheEvaluatorsReason (PR #683 greptile P1): on
// PostgreSQL an accepting advance holds no lock on the row it re-read, so an
// evaluation can commit between that re-read and the advance's write. The
// advance must not restore the reason it read (an old producer's) under the
// owner the evaluation just stamped. The hook commits the evaluation at
// exactly that point.
func TestAdvanceNeverWritesBackTheEvaluatorsReason(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	store := NewStore(db)
	if _, err := store.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "w1", RunID: uuid.New(), CompletedAt: t0}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	finance := "finance"
	if err := db.Model(&models.DatasetState{}).Where("name = ?", "staging.orders").
		Updates(map[string]any{"status": "stale", "reason": "raw.finance_ledger is behind", "reason_namespace": finance}).Error; err != nil {
		t.Fatalf("seed finance's evaluation: %v", err)
	}

	evaluated := false
	prev := afterAdvanceReread
	afterAdvanceReread = func(tx *gorm.DB) {
		if evaluated {
			return
		}
		evaluated = true
		// The producer moved to marketing and its evaluation commits now.
		if err := tx.Model(&models.DatasetState{}).Where("name = ?", "staging.orders").
			Updates(map[string]any{"status": "fresh", "reason": "", "reason_namespace": "marketing"}).Error; err != nil {
			t.Errorf("marketing's evaluation: %v", err)
		}
	}
	t.Cleanup(func() { afterAdvanceReread = prev })

	res, err := store.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "w2", RunID: uuid.New(), RunOrder: t0.Add(time.Minute), CompletedAt: t0.Add(time.Minute)})
	if err != nil || res.Outcome != OutcomeAdvanced {
		t.Fatalf("advance: %v (%s)", err, res.Outcome)
	}
	if !evaluated {
		t.Fatal("precondition: the advance took the conflict path")
	}
	var row models.DatasetState
	if err := db.Where("name = ?", "staging.orders").Take(&row).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	if row.Watermark != "w2" {
		t.Fatalf("watermark = %q, want the advance's", row.Watermark)
	}
	if row.Reason != "" || row.Status != "fresh" || row.ReasonNamespace == nil || *row.ReasonNamespace != "marketing" {
		t.Fatalf("reason %q / status %q owned by %v: the advance wrote back finance's evaluation", row.Reason, row.Status, row.ReasonNamespace)
	}
}
