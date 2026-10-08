package contract

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestExistingAliasLookupPolicy(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.Exec("CREATE TABLE jobs (id TEXT, alias TEXT, deleted_at DATETIME)").Error)
	active, deleted := uuid.New(), uuid.New()
	require.NoError(t, db.Exec("INSERT INTO jobs (id,alias,deleted_at) VALUES (?, ?, NULL), (?, ?, CURRENT_TIMESTAMP), (?, ?, NULL)", active, " active ", deleted, "deleted", uuid.Nil, "nil").Error)
	store := GORMStore{DB: db}
	ids, err := store.ExistingJobIDsByAlias(t.Context(), map[string]struct{}{" active ": {}, "deleted": {}, "nil": {}})
	require.NoError(t, err)
	require.Equal(t, map[string]uuid.UUID{"active": active}, ids)
	ids, err = store.ExistingJobIDsByAlias(t.Context(), nil)
	require.NoError(t, err)
	require.Nil(t, ids)
	require.NoError(t, db.Exec("DROP TABLE jobs").Error)
	_, err = store.ExistingJobIDsByAlias(context.Background(), map[string]struct{}{"active": {}})
	require.Error(t, err)
}
