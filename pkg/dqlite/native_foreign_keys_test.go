package dqlite

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/driver"
	"github.com/stretchr/testify/require"
)

// This test uses the actual native app, never an injected sqlite3 Dialector or
// a foreign_keys setter. Connection defaults must come from the pinned VFS.
func TestNativeForeignKeysOnBothPoolsAndReplacementConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	app := startTunedNode(t, ctx, t.TempDir(), address, nil)
	defer func() { require.NoError(t, app.Close()) }()

	for _, databaseName := range []string{"caesium", "caesium_hot_00", "caesium_history"} {
		t.Run(databaseName, func(t *testing.T) {
			writer, err := app.Open(ctx, databaseName)
			require.NoError(t, err)
			defer func() { require.NoError(t, writer.Close()) }()
			reader, err := app.Open(ctx, databaseName)
			require.NoError(t, err)
			defer func() { require.NoError(t, reader.Close()) }()
			_, err = writer.ExecContext(ctx, "CREATE TABLE fk_parent (id INTEGER PRIMARY KEY)")
			require.NoError(t, err)
			_, err = writer.ExecContext(ctx, "CREATE TABLE fk_child (parent_id INTEGER REFERENCES fk_parent(id))")
			require.NoError(t, err)
			_, err = writer.ExecContext(ctx, "INSERT INTO fk_parent VALUES (1)")
			require.NoError(t, err)
			for _, pool := range []*sql.DB{writer, reader} {
				assertNativeForeignKeys(t, ctx, pool)
				// With no retained idle connection, the next reservation must establish
				// another physical connection and receive the same native default.
				pool.SetMaxIdleConns(0)
				assertNativeForeignKeys(t, ctx, pool)
			}
		})
	}
}

func assertNativeForeignKeys(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	conn, err := pool.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	var enabled int
	require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled))
	require.Equal(t, 1, enabled)
	_, err = conn.ExecContext(ctx, "INSERT INTO fk_child VALUES (999)")
	require.Error(t, err)
	var nativeErr driver.Error
	require.True(t, errors.As(err, &nativeErr), "expected native SQLite error, got %T: %v", err, err)
	// SQLITE_CONSTRAINT_FOREIGNKEY = SQLITE_CONSTRAINT | (3 << 8).
	require.Equal(t, 19|(3<<8), nativeErr.Code)
	_, err = conn.ExecContext(ctx, "INSERT INTO fk_child VALUES (1)")
	require.NoError(t, err)
}
