package dqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SQLite refreshes a prepared statement's schema on its first step. Native
// dqlite must send the refreshed column names/count with the resulting values,
// including joined aliases after a wildcard (#613's job_labels scan failure).
func TestNativePreparedQueryRefreshesSchemaMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	app := startTunedNode(t, ctx, t.TempDir(), address, nil)
	defer func() { require.NoError(t, app.Close()) }()
	reader, err := app.Open(ctx, "query_schema")
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	writer, err := app.Open(ctx, "query_schema")
	require.NoError(t, err)
	defer func() { require.NoError(t, writer.Close()) }()
	reader.SetMaxOpenConns(1)
	writer.SetMaxOpenConns(1)

	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			table := fmt.Sprintf("schema_rows_%t", empty)
			_, err := writer.ExecContext(ctx, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY, status TEXT)")
			require.NoError(t, err)
			if !empty {
				// Force several protocol batches; the first row must neither be
				// dropped nor repeated when stepping moves ahead of the header.
				_, err = writer.ExecContext(ctx, "WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<200) INSERT INTO "+table+" SELECT i, ? FROM n", strings.Repeat("running", 100))
				require.NoError(t, err)
			}
			stmt, err := reader.PrepareContext(ctx, "SELECT r.*, 'lifecycle-queue' AS job_alias, '{\"lane\":\"lifecycle\"}' AS job_labels FROM "+table+" r WHERE id > ? ORDER BY id")
			require.NoError(t, err)
			defer func() { require.NoError(t, stmt.Close()) }()
			// Prepare is complete on the reader before an independent migration
			// adds a column. No timing window or retry is needed to reproduce it.
			_, err = writer.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default'")
			require.NoError(t, err)
			for range 2 {
				rows, err := stmt.QueryContext(ctx, 0)
				require.NoError(t, err)
				assertNativeSchemaRows(t, rows, empty)
			}
		})
	}
}

func assertNativeSchemaRows(t *testing.T, rows *sql.Rows, empty bool) {
	t.Helper()
	defer func() { require.NoError(t, rows.Close()) }()
	columns, err := rows.Columns()
	require.NoError(t, err)
	require.Equal(t, []string{"id", "status", "namespace", "job_alias", "job_labels"}, columns)
	count := 0
	for rows.Next() {
		var id int
		var status, namespace, alias, labels string
		require.NoError(t, rows.Scan(&id, &status, &namespace, &alias, &labels))
		count++
		require.Equal(t, count, id)
		require.Equal(t, strings.Repeat("running", 100), status)
		require.Equal(t, "default", namespace)
		require.Equal(t, "lifecycle-queue", alias)
		require.JSONEq(t, `{"lane":"lifecycle"}`, labels)
	}
	require.NoError(t, rows.Err())
	if empty {
		require.Zero(t, count)
	} else {
		require.Equal(t, 200, count)
	}
}
