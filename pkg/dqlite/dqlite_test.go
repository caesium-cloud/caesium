package dqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/dbtrace"
	"github.com/canonical/go-dqlite/v3/client"
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

func TestClusterRequiresNativeApp(t *testing.T) {
	_, err := Cluster(context.Background())
	require.True(t, errors.Is(err, ErrNoNativeApp))

	isLeader, err := IsLocalLeader(context.Background())
	require.False(t, isLeader)
	require.True(t, errors.Is(err, ErrNoNativeApp))
}

func TestDqliteLogFieldsAttachRecentStatementsForUnknownDataTypeWarning(t *testing.T) {
	dbtrace.Reset()
	t.Cleanup(dbtrace.Reset)
	dbtrace.Record("select * from task_runs where id = 'abc'", 1, time.Millisecond, nil)

	fields := dqliteLogFields(client.LogWarn, "protocol warning: unknown data type: 0")

	fieldMap := make(map[string]interface{}, len(fields)/2)
	for idx := 0; idx < len(fields)-1; idx += 2 {
		key, ok := fields[idx].(string)
		require.True(t, ok)
		fieldMap[key] = fields[idx+1]
	}

	recent, ok := fieldMap["recent_db_statements"].([]dbtrace.Statement)
	require.True(t, ok)
	require.Len(t, recent, 1)
	require.Contains(t, recent[0].SQL, "task_runs")
}

func TestDqliteLogFieldsOnlyAttachContextToSpecificWarnings(t *testing.T) {
	dbtrace.Reset()
	t.Cleanup(dbtrace.Reset)
	dbtrace.Record("select 1", 1, time.Millisecond, nil)

	fields := dqliteLogFields(client.LogWarn, "leader changed")

	for idx := 0; idx < len(fields)-1; idx += 2 {
		require.NotEqual(t, "recent_db_statements", fields[idx])
	}
}
