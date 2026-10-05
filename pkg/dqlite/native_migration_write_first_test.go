package dqlite

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/driver"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type nativeConcurrentMigrationRow struct {
	ID       int `gorm:"primaryKey"`
	ParentID int
	Value    string `gorm:"type:blob;not null"`
}

func (nativeConcurrentMigrationRow) TableName() string { return "native_migration_concurrent" }

// This is the old transaction ordering on the actual native engine. Scan has
// returned (and closed its rows) before the independent writer commits. The
// failure therefore characterizes snapshot upgrade, not a dangling Rows leak.
func TestNativeReadFirstMigrationRejectsConcurrentCommit(t *testing.T) {
	migration, writer := nativeConcurrentMigrationDBs(t)
	before, err := migration.Migrator().(Migrator).getRawDDL("native_migration_concurrent")
	require.NoError(t, err)
	indexes := migrationIndexSQL(t, migration, "native_migration_concurrent")
	createSQL, err := alterColumnDDL(before, "value", "blob NOT NULL")
	require.NoError(t, err)
	createSQL = strings.Replace(createSQL, "CREATE TABLE native_migration_concurrent ", "CREATE TABLE native_migration_concurrent__temp ", 1)
	require.Contains(t, createSQL, "CREATE TABLE native_migration_concurrent__temp ")
	var peerErr error
	err = migration.Transaction(func(tx *gorm.DB) error {
		var captured []string
		if err := tx.Raw("SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL ORDER BY name", "native_migration_concurrent").Scan(&captured).Error; err != nil {
			return err
		}
		require.Equal(t, indexes, captured)
		peerErr = commitNativeMigrationPeer(t, writer)
		if peerErr != nil {
			return peerErr
		}
		return tx.Exec(createSQL).Error
	})
	require.NoError(t, peerErr, "the independent writer must actually commit after the read snapshot")
	require.Error(t, err)
	var nativeErr driver.Error
	require.True(t, errors.As(err, &nativeErr), "expected native SQLite error: %T %v", err, err)
	require.Equal(t, 5, nativeErr.Code&0xff, "SQLITE_BUSY, including its extended snapshot code")
	t.Logf("closed-row read-first snapshot refused upgrade: native code=%d error=%v", nativeErr.Code, nativeErr)
	after, err := migration.Migrator().(Migrator).getRawDDL("native_migration_concurrent")
	require.NoError(t, err)
	require.Equal(t, before, after)
	assertNativeConcurrentMigrationPreserved(t, migration, indexes)
}

// The callback targets the real production AlterColumn's first CREATE. A peer
// commits at exactly the former snapshot-upgrade window. The write-first path
// has no preceding transactional read, so migration must succeed and retain
// both the committed peer value and the table's indexes/constraints.
func TestNativeAlterColumnPreservesConcurrentCommitBeforeCreate(t *testing.T) {
	migration, writer := nativeConcurrentMigrationDBs(t)
	indexes := migrationIndexSQL(t, migration, "native_migration_concurrent")
	var peerErr error
	calls := 0
	require.NoError(t, migration.Callback().Raw().Before("gorm:raw").Register("native-migration:peer-before-create", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if !strings.HasPrefix(sql, "CREATE TABLE ") || !strings.Contains(sql, "native_migration_concurrent__temp") {
			return
		}
		calls++
		peerErr = commitNativeMigrationPeer(t, writer)
		if peerErr != nil {
			tx.AddError(peerErr)
		}
	}))
	t.Cleanup(func() { require.NoError(t, migration.Callback().Raw().Remove("native-migration:peer-before-create")) })
	err := migration.Migrator().AlterColumn(&nativeConcurrentMigrationRow{}, "Value")
	require.Equal(t, 1, calls, "must exercise the actual temporary-table CREATE")
	require.NoError(t, peerErr, "the independent writer must actually commit before CREATE")
	require.NoError(t, err)
	var columns []struct{ Name, Type string }
	require.NoError(t, migration.Raw("PRAGMA table_info(native_migration_concurrent)").Scan(&columns).Error)
	require.Equal(t, []struct{ Name, Type string }{{"id", "INTEGER"}, {"parent_id", "INTEGER"}, {"value", "BLOB"}}, columns)
	assertNativeConcurrentMigrationPreserved(t, migration, indexes)
}

func nativeConcurrentMigrationDBs(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	require.Nil(t, currentApp.Load(), "do not replace another native fixture")
	app := startTunedNode(t, ctx, t.TempDir(), address, nil)
	currentApp.Store(app)
	t.Cleanup(func() {
		require.True(t, currentApp.CompareAndSwap(app, nil))
		require.NoError(t, app.Close())
	})
	open := func() *gorm.DB {
		db, err := gorm.Open(Open("migration_concurrent"), &gorm.Config{})
		require.NoError(t, err)
		pool, err := db.DB()
		require.NoError(t, err)
		pool.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, pool.Close()) })
		return db.WithContext(ctx)
	}
	migration, writer := open(), open() // distinct native connection pools
	require.NoError(t, migration.Exec("CREATE TABLE native_migration_parent (id INTEGER PRIMARY KEY)").Error)
	require.NoError(t, migration.Exec("INSERT INTO native_migration_parent VALUES (1)").Error)
	require.NoError(t, migration.Exec("CREATE TABLE native_migration_concurrent (id INTEGER PRIMARY KEY,parent_id INTEGER NOT NULL REFERENCES native_migration_parent(id),value TEXT NOT NULL CHECK(length(value)>0))").Error)
	require.NoError(t, migration.Exec("CREATE UNIQUE INDEX idx_native_value ON native_migration_concurrent(value)").Error)
	require.NoError(t, migration.Exec("CREATE INDEX idx_native_partial ON native_migration_concurrent(value) WHERE id > 0").Error)
	require.NoError(t, migration.Exec("CREATE INDEX idx_native_expression ON native_migration_concurrent(lower(value))").Error)
	require.NoError(t, migration.Exec("INSERT INTO native_migration_concurrent VALUES (1,1,'before')").Error)
	return migration, writer
}

func commitNativeMigrationPeer(t *testing.T, writer *gorm.DB) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- writer.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Exec("UPDATE native_migration_concurrent SET value='peer-committed' WHERE id=1").Error
		})
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		// The driver receives the finite context; join rather than leave a peer
		// writer racing the fixture's pool and app cleanup.
		return errors.Join(ctx.Err(), <-result)
	}
}

func assertNativeConcurrentMigrationPreserved(t *testing.T, db *gorm.DB, indexes []string) {
	t.Helper()
	var rows []nativeConcurrentMigrationRow
	require.NoError(t, db.Find(&rows).Error)
	require.Equal(t, []nativeConcurrentMigrationRow{{ID: 1, ParentID: 1, Value: "peer-committed"}}, rows)
	require.Equal(t, indexes, migrationIndexSQL(t, db, "native_migration_concurrent"))
	require.False(t, db.Migrator().HasTable("native_migration_concurrent__temp"))
	var enabled int
	require.NoError(t, db.Raw("PRAGMA foreign_keys").Scan(&enabled).Error)
	require.Equal(t, 1, enabled)
	require.Error(t, db.Exec("INSERT INTO native_migration_concurrent VALUES (2,999,'orphan')").Error)
	require.Error(t, db.Exec("INSERT INTO native_migration_concurrent VALUES (2,1,'')").Error)
	require.Error(t, db.Exec("INSERT INTO native_migration_concurrent VALUES (2,1,'peer-committed')").Error)
}
