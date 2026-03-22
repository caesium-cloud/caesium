package backfill

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRequestCancelMarksIntentWithoutTerminalTransition(t *testing.T) {
	store, db, backfillID := newBackfillTestStore(t)

	require.NoError(t, store.RequestCancel(backfillID))

	var backfill models.Backfill
	require.NoError(t, db.First(&backfill, "id = ?", backfillID).Error)
	require.Equal(t, string(models.BackfillStatusRunning), backfill.Status)
	require.NotNil(t, backfill.CancelRequestedAt)

	cancelRequested, err := store.IsCancelRequested(backfillID)
	require.NoError(t, err)
	require.True(t, cancelRequested)
}

func TestMarkCancelledTransitionsRunningBackfill(t *testing.T) {
	store, db, backfillID := newBackfillTestStore(t)

	require.NoError(t, store.RequestCancel(backfillID))
	require.NoError(t, store.MarkCancelled(backfillID))

	var backfill models.Backfill
	require.NoError(t, db.First(&backfill, "id = ?", backfillID).Error)
	require.Equal(t, string(models.BackfillStatusCancelled), backfill.Status)
	require.NotNil(t, backfill.CancelRequestedAt)
	require.NotNil(t, backfill.CompletedAt)

	cancelRequested, err := store.IsCancelRequested(backfillID)
	require.NoError(t, err)
	require.False(t, cancelRequested)
}

func TestCompleteDoesNotOverwriteCancelledBackfill(t *testing.T) {
	store, db, backfillID := newBackfillTestStore(t)

	require.NoError(t, store.RequestCancel(backfillID))
	require.NoError(t, store.MarkCancelled(backfillID))
	require.NoError(t, store.Complete(backfillID, false))

	var backfill models.Backfill
	require.NoError(t, db.First(&backfill, "id = ?", backfillID).Error)
	require.Equal(t, string(models.BackfillStatusCancelled), backfill.Status)
	require.NotNil(t, backfill.CompletedAt)
}

func newBackfillTestStore(t *testing.T) (*Store, *gorm.DB, uuid.UUID) {
	t.Helper()

	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})

	require.NoError(t, db.AutoMigrate(models.All...))

	triggerID := uuid.New()
	jobID := uuid.New()
	backfillID := uuid.New()

	require.NoError(t, db.Create(&models.Trigger{
		ID:        triggerID,
		Alias:     "trigger-" + triggerID.String(),
		Type:      models.TriggerTypeCron,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}).Error)

	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "job-" + jobID.String(),
		TriggerID: triggerID,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}).Error)

	require.NoError(t, db.Create(&models.Backfill{
		ID:            backfillID,
		JobID:         jobID,
		Status:        string(models.BackfillStatusRunning),
		Start:         time.Now().UTC().Add(-time.Hour),
		End:           time.Now().UTC(),
		MaxConcurrent: 1,
		Reprocess:     string(models.ReprocessNone),
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}).Error)

	return NewStore(db), db, backfillID
}
