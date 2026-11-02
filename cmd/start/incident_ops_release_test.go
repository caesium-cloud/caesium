package start

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	datasetsvc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func withAuthMode(t *testing.T, mode string) {
	t.Helper()
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_AUTH_MODE", mode)
	require.NoError(t, env.Process())
}

// releaseOps is the adapter over a real run store on a real catalog, with a held
// dataset.
func releaseOps(t *testing.T) (*incidentActionOps, *gorm.DB, *recordingBus, uuid.UUID) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	bus := &recordingBus{}
	store := run.NewStore(db)
	store.SetBus(bus)
	ops := newIncidentActionOps(db, bus, event.NewStore(db))
	ops.holdStore = store

	now := time.Now().UTC()
	holdID := uuid.New()
	key := models.DatasetHoldKey("", "orders")
	require.NoError(t, db.Create(&models.DatasetHold{
		ID: holdID, Name: "orders", Status: models.DatasetHoldStatusActive, ActiveKey: &key,
		Reason: "min", HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: now,
	}).Error)
	return ops, db, bus, holdID
}

func holdRelease(holdID uuid.UUID) incident.HoldRelease {
	return incident.HoldRelease{
		HoldID:    holdID,
		Principal: "user:ada",
		Reason:    "source backfilled",
		Tolerate:  map[string]string{"min": "24h"},
		Namespace: "finance",
	}
}

func holdAudits(t *testing.T, db *gorm.DB, holdID uuid.UUID) []models.AuditLog {
	t.Helper()
	var rows []models.AuditLog
	require.NoError(t, db.Where("resource_id = ?", holdID.String()).Find(&rows).Error)
	return rows
}

// TestReleaseHoldGoesThroughTheRESTReleaseService: the audit row, the release
// fields and the event are the endpoint's, because the path is.
func TestReleaseHoldGoesThroughTheRESTReleaseService(t *testing.T) {
	withAuthMode(t, "api-key")
	ops, db, bus, holdID := releaseOps(t)

	require.NoError(t, ops.ReleaseHold(context.Background(), holdRelease(holdID)))

	var hold models.DatasetHold
	require.NoError(t, db.First(&hold, "id = ?", holdID).Error)
	require.Equal(t, models.DatasetHoldStatusReleased, hold.Status)
	require.Equal(t, "user:ada", hold.ReleasedBy)
	require.Equal(t, models.DatasetHoldReleaseManualAck, hold.ReleaseReason)
	require.Equal(t, "source backfilled", hold.ReleaseNote)
	require.JSONEq(t, `{"min":"24h"}`, string(hold.Tolerances))

	audits := holdAudits(t, db, holdID)
	require.Len(t, audits, 1)
	require.Equal(t, "dataset.hold.release", audits[0].Action)
	require.Equal(t, "success", audits[0].Outcome)
	require.Equal(t, "user:ada", audits[0].Actor)
	require.Equal(t, "finance", audits[0].Namespace)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(audits[0].Metadata, &metadata))
	require.Equal(t, "source backfilled", metadata["reason"])

	require.Len(t, bus.published, 1)
	require.Equal(t, event.TypeDatasetReleased, bus.published[0].Type)
}

func TestReleaseHoldRefusedWithoutAnAuthMode(t *testing.T) {
	withAuthMode(t, "none")
	ops, db, bus, holdID := releaseOps(t)

	err := ops.ReleaseHold(context.Background(), holdRelease(holdID))
	require.ErrorIs(t, err, datasetsvc.ErrHoldReleaseUnauthenticated)

	var hold models.DatasetHold
	require.NoError(t, db.First(&hold, "id = ?", holdID).Error)
	require.Equal(t, models.DatasetHoldStatusActive, hold.Status)
	require.Empty(t, bus.published)
}

func TestReleaseHoldGuardAbortsTheReleaseTransaction(t *testing.T) {
	withAuthMode(t, "api-key")
	ops, db, bus, holdID := releaseOps(t)
	refused := errors.New("refused by the guard")

	release := holdRelease(holdID)
	release.Guard = func(*gorm.DB) error { return refused }
	require.ErrorIs(t, ops.ReleaseHold(context.Background(), release), refused)

	var hold models.DatasetHold
	require.NoError(t, db.First(&hold, "id = ?", holdID).Error)
	require.Equal(t, models.DatasetHoldStatusActive, hold.Status)
	audits := holdAudits(t, db, holdID)
	require.Len(t, audits, 1)
	require.Equal(t, "failure", audits[0].Outcome, "a refused attempt is recorded, as it is on the endpoint")
	require.Empty(t, bus.published)
}

func TestReleaseHoldAppliesTheRESTToleranceGrammar(t *testing.T) {
	withAuthMode(t, "api-key")
	ops, db, _, holdID := releaseOps(t)

	release := holdRelease(holdID)
	release.Tolerate = map[string]string{"unavailable": "1h"}
	require.ErrorContains(t, ops.ReleaseHold(context.Background(), release), "not an assertion kind")

	var hold models.DatasetHold
	require.NoError(t, db.First(&hold, "id = ?", holdID).Error)
	require.Equal(t, models.DatasetHoldStatusActive, hold.Status)
}

func TestReleaseHoldReportsAHoldThatEndedFirst(t *testing.T) {
	withAuthMode(t, "api-key")
	ops, db, _, holdID := releaseOps(t)

	require.NoError(t, ops.ReleaseHold(context.Background(), holdRelease(holdID)))
	err := ops.ReleaseHold(context.Background(), holdRelease(holdID))
	require.ErrorIs(t, err, incident.ErrHoldNotActive)

	successes := 0
	for _, audit := range holdAudits(t, db, holdID) {
		if audit.Outcome == "success" {
			successes++
		}
	}
	require.Equal(t, 1, successes, "one release, one success row")
}
