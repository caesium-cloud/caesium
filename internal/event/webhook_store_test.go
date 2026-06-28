package event

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestWebhookEventStoreCreateTxValidatesAndDefaults(t *testing.T) {
	t.Parallel()

	db := openWebhookEventStoreTestDB(t)
	now := time.Date(2026, 6, 27, 13, 0, 0, 0, time.UTC)
	store := NewWebhookEventStore(db)
	store.now = func() time.Time { return now }

	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return store.CreateTx(tx, &models.WebhookEvent{})
	}))

	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return store.CreateTx(tx, &models.WebhookEvent{
			Path:           "github/push",
			HTTPTriggerIDs: datatypes.JSON(`{`),
		})
	}))

	evt := &models.WebhookEvent{
		Path:                 " github/push ",
		Source:               " github ",
		EventMatchedTriggers: 1,
		HTTPTriggersAccepted: 1,
		HTTPTriggerIDs:       datatypes.JSON(`["` + uuid.NewString() + `"]`),
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.CreateTx(tx, evt)
	}))
	require.NotEqual(t, uuid.Nil, evt.ID)
	require.Equal(t, now, evt.ReceivedAt)
	require.Equal(t, "github/push", evt.Path)
	require.Equal(t, "github", evt.Source)
	require.Equal(t, defaultWebhookEventStatus, evt.Status)

	var stored models.WebhookEvent
	require.NoError(t, db.First(&stored, "id = ?", evt.ID).Error)
	require.Equal(t, evt.ID, stored.ID)
	require.Equal(t, evt.EventMatchedTriggers, stored.EventMatchedTriggers)
}

func TestWebhookEventStorePrune(t *testing.T) {
	t.Parallel()

	db := openWebhookEventStoreTestDB(t)
	now := time.Date(2026, 6, 27, 14, 0, 0, 0, time.UTC)
	store := NewWebhookEventStore(db)
	store.now = func() time.Time { return now }

	old := &models.WebhookEvent{Path: "old", ReceivedAt: now.Add(-8 * 24 * time.Hour)}
	fresh := &models.WebhookEvent{Path: "fresh", ReceivedAt: now.Add(-6 * 24 * time.Hour)}
	require.NoError(t, store.Create(context.Background(), old))
	require.NoError(t, store.Create(context.Background(), fresh))

	count, err := store.Prune(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	var remaining []models.WebhookEvent
	require.NoError(t, db.Find(&remaining).Error)
	require.Len(t, remaining, 1)
	require.Equal(t, "fresh", remaining[0].Path)
}

func TestWebhookEventStorePruneDisabled(t *testing.T) {
	t.Parallel()

	db := openWebhookEventStoreTestDB(t)
	store := NewWebhookEventStore(db)
	require.NoError(t, store.Create(context.Background(), &models.WebhookEvent{Path: "keep"}))

	count, err := store.Prune(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	var rows int64
	require.NoError(t, db.Model(&models.WebhookEvent{}).Count(&rows).Error)
	require.EqualValues(t, 1, rows)
}

func openWebhookEventStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.WebhookEvent{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
