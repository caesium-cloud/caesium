package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

func TestShouldSkipCronRunRecordsSkippedFresh(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(2 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "cron-skip", models.TriggerTypeCron)
	seedDeclarations(t, db,
		produceDecl(jobID, "mart.orders", "1h", ""),
		consumeDecl(jobID, "raw.orders"),
	)
	seedState(t, db, "mart.orders", "100", now.Add(-10*time.Minute), map[string]string{"raw.orders": "42"})
	seedState(t, db, "raw.orders", "42", now.Add(-5*time.Minute), nil)

	decision, err := ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil {
		t.Fatalf("skip decision: %v", err)
	}
	if !decision.Skip {
		t.Fatalf("skip = false, reason = %q", decision.Reason)
	}
	if len(decision.Datasets) != 1 || decision.Datasets[0] != "mart.orders" {
		t.Fatalf("datasets = %v, want [mart.orders]", decision.Datasets)
	}

	var derivation models.DatasetDerivation
	if err := db.Where("name = ? AND decision = ?", "mart.orders", models.DatasetDecisionSkippedFresh).Take(&derivation).Error; err != nil {
		t.Fatalf("load skipped derivation: %v", err)
	}
	if derivation.RunID != nil {
		t.Fatalf("skipped derivation run_id = %v, want nil", derivation.RunID)
	}
	if got := string(derivation.ConsumedWatermarks); got != `{"raw.orders":"42"}` {
		t.Fatalf("consumed watermarks = %s, want raw.orders snapshot", got)
	}
}

func TestShouldSkipCronRunRunsWhenConsumedWatermarkAdvanced(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(2 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "cron-advanced", models.TriggerTypeCron)
	seedDeclarations(t, db,
		produceDecl(jobID, "mart.orders", "1h", ""),
		consumeDecl(jobID, "raw.orders"),
	)
	seedState(t, db, "mart.orders", "100", now.Add(-10*time.Minute), map[string]string{"raw.orders": "42"})
	seedState(t, db, "raw.orders", "43", now.Add(-5*time.Minute), nil)

	decision, err := ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil {
		t.Fatalf("skip decision: %v", err)
	}
	if decision.Skip {
		t.Fatalf("skip = true, want false")
	}
	if decision.Reason != "consumed watermark advanced for raw.orders" {
		t.Fatalf("reason = %q, want consumed watermark advanced", decision.Reason)
	}
	var count int64
	if err := db.Model(&models.DatasetDerivation{}).Count(&count).Error; err != nil {
		t.Fatalf("count derivations: %v", err)
	}
	if count != 0 {
		t.Fatalf("derivations = %d, want 0", count)
	}
}

func TestShouldSkipCronRunHonorsSkipWhenFreshOptOut(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(2 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "cron-optout", models.TriggerTypeCron)
	produced := produceDecl(jobID, "mart.orders", "1h", "")
	optOut := false
	produced.SkipWhenFresh = &optOut
	seedDeclarations(t, db, produced)
	seedState(t, db, "mart.orders", "100", now.Add(-10*time.Minute), nil)

	decision, err := ShouldSkipCronRun(ctx, db, jobID, now)
	if err != nil {
		t.Fatalf("skip decision: %v", err)
	}
	if decision.Skip {
		t.Fatalf("skip = true, want false")
	}
	if decision.Reason != "metadata.datasets.skipWhenFresh is false" {
		t.Fatalf("reason = %q, want opt-out reason", decision.Reason)
	}
}
