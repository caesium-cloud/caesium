package dqlite

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type migrationChild struct {
	ID       int `gorm:"primaryKey"`
	ParentID int `gorm:"column:parent_id;type:integer;not null"`
}

func (migrationChild) TableName() string { return "migration_child" }

// The actual native app supplies the production Dialector's connection. No
// sqlite3 replacement, foreign_keys setter or alternate migrator is involved.
func TestNativePopulatedSAMLReplayMigrationAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	dir := t.TempDir()
	require.Nil(t, currentApp.Load(), "do not replace a foreign native app")
	start := func() *gorm.DB {
		app := startTunedNode(t, ctx, dir, address, nil)
		currentApp.Store(app)
		t.Cleanup(func() {
			if currentApp.CompareAndSwap(app, nil) {
				require.NoError(t, app.Close())
			}
		})
		db, err := gorm.Open(Open("migration_native"), &gorm.Config{})
		require.NoError(t, err)
		pool, err := db.DB()
		require.NoError(t, err)
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, pool.Close()) })
		return db.WithContext(ctx)
	}
	db := start()
	assertDefaultGeneratedKeywordMigration(t, db)
	samlIndexes := assertPopulatedSAMLReplayMigration(t, db)
	require.NoError(t, db.Exec("CREATE TABLE migration_parent (id INTEGER PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_parent VALUES (1)").Error)
	require.NoError(t, db.Exec("CREATE TABLE migration_child (id INTEGER PRIMARY KEY,parent_id TEXT NOT NULL REFERENCES migration_parent(id) CHECK(parent_id > 0))").Error)
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_child_parent ON migration_child(parent_id)").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_child VALUES (1,1)").Error)
	indexes := migrationIndexSQL(t, db, "migration_child")
	require.NoError(t, db.Migrator().AlterColumn(&migrationChild{}, "ParentID"))
	require.Equal(t, indexes, migrationIndexSQL(t, db, "migration_child"))
	assertMigrationNativeFK(t, db)
	before, err := db.Migrator().(Migrator).getRawDDL("migration_child")
	require.NoError(t, err)
	failure := errors.New("native index replay refusal")
	require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register("native-migration:fail-index", func(tx *gorm.DB) {
		if strings.HasPrefix(tx.Statement.SQL.String(), "CREATE UNIQUE INDEX idx_child_parent") {
			tx.AddError(failure)
		}
	}))
	require.ErrorIs(t, db.Migrator().AlterColumn(&migrationChild{}, "ParentID"), failure)
	require.NoError(t, db.Callback().Raw().Remove("native-migration:fail-index"))
	after, err := db.Migrator().(Migrator).getRawDDL("migration_child")
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, indexes, migrationIndexSQL(t, db, "migration_child"))
	require.False(t, db.Migrator().HasTable("migration_child__temp"))
	pool, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, pool.Close())
	app := currentApp.Swap(nil)
	require.NoError(t, app.Close())
	db = start()
	assertSAMLReplayDates(t, db)
	require.Equal(t, samlIndexes, migrationIndexSQL(t, db, "saml_assertion_ids"))
	require.NoError(t, db.AutoMigrate(&models.SAMLAssertionReplay{}))
	assertSAMLReplayDates(t, db)
	require.Equal(t, samlIndexes, migrationIndexSQL(t, db, "saml_assertion_ids"))
	var row models.SAMLAssertionReplay
	require.NoError(t, db.First(&row).Error)
	require.Equal(t, "assertion", row.AssertionID)
	require.Equal(t, "issuer", row.Issuer)
	require.Error(t, db.Exec("INSERT INTO saml_assertion_ids VALUES (?, ?, ?, ?)", row.Issuer, row.AssertionID, row.ExpiresAt, row.CreatedAt).Error)
	assertMigrationNativeFK(t, db)
	require.Equal(t, indexes, migrationIndexSQL(t, db, "migration_child"))
	var child migrationChild
	require.NoError(t, db.First(&child).Error)
	require.Equal(t, 1, child.ParentID)
}

func assertMigrationNativeFK(t *testing.T, db *gorm.DB) {
	t.Helper()
	var enabled int
	require.NoError(t, db.Raw("PRAGMA foreign_keys").Scan(&enabled).Error)
	require.Equal(t, 1, enabled)
	require.Error(t, db.Exec("INSERT INTO migration_child VALUES (2,999)").Error)
	require.Error(t, db.Exec("INSERT INTO migration_child VALUES (2,0)").Error)
}
