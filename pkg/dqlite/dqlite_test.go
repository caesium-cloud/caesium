package dqlite

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDialectorAppliesConnectionPragmas(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	_, err = gorm.Open(Dialector{Conn: conn}, &gorm.Config{})
	require.NoError(t, err)

	var busyTimeout int
	require.NoError(t, conn.QueryRowContext(context.Background(), "SELECT * FROM pragma_busy_timeout").Scan(&busyTimeout))
	require.Equal(t, 5000, busyTimeout)

	var synchronous int
	require.NoError(t, conn.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous))
	require.Equal(t, 1, synchronous)
}
