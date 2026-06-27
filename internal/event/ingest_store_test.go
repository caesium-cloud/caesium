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

func TestIngestStoreCreateTxValidatesAndDefaults(t *testing.T) {
	t.Parallel()

	db := openIngestStoreTestDB(t)
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	store := NewIngestStore(db)
	store.now = func() time.Time { return now }

	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return store.CreateTx(tx, &models.IngestedEvent{Data: datatypes.JSON(`{}`)})
	}))

	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return store.CreateTx(tx, &models.IngestedEvent{Type: "webhook.github", Data: datatypes.JSON(`{`)})
	}))

	evt := &models.IngestedEvent{Type: " webhook.github ", Source: " github "}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.CreateTx(tx, evt)
	}))
	require.NotEqual(t, uuid.Nil, evt.ID)
	require.Equal(t, now, evt.CreatedAt)
	require.Equal(t, "webhook.github", evt.Type)
	require.Equal(t, "github", evt.Source)
	require.JSONEq(t, `{}`, string(evt.Data))

	var stored models.IngestedEvent
	require.NoError(t, db.First(&stored, "id = ?", evt.ID).Error)
	require.Equal(t, evt.ID, stored.ID)
}

func TestIngestStoreRecordMatchesTxValidatesAndDefaults(t *testing.T) {
	t.Parallel()

	db := openIngestStoreTestDB(t)
	now := time.Date(2026, 6, 27, 12, 30, 0, 0, time.UTC)
	store := NewIngestStore(db)
	store.now = func() time.Time { return now }

	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		return store.RecordMatchesTx(tx, []*models.EventTriggerMatch{nil})
	}))

	evt := &models.IngestedEvent{Type: "webhook.github"}
	require.NoError(t, store.Create(context.Background(), evt))

	row := &models.EventTriggerMatch{
		EventID:     evt.ID,
		TriggerID:   uuid.New(),
		RunsStarted: datatypes.JSON(`["` + uuid.NewString() + `"]`),
	}
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.RecordMatchesTx(tx, []*models.EventTriggerMatch{row})
	}))
	require.NotEqual(t, uuid.Nil, row.ID)
	require.Equal(t, now, row.MatchedAt)

	var stored models.EventTriggerMatch
	require.NoError(t, db.First(&stored, "id = ?", row.ID).Error)
	require.Equal(t, row.EventID, stored.EventID)
	require.Equal(t, row.TriggerID, stored.TriggerID)
	require.Equal(t, now, stored.MatchedAt)
}

func openIngestStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.IngestedEvent{}, &models.EventTriggerMatch{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
