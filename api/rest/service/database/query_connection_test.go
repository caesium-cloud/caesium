package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestQueryLeavesPoolWritable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancelQuery bool
		refuseReset bool
		wantStage   string
	}{
		{"healthy query", false, false, ""},
		{"canceled query", true, false, queryStageExecute},
		{"reset failed after reading rows", false, true, queryStageCleanup},
		{"reset failed after query error", true, true, queryStageExecute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "catalog.db")), &gorm.Config{})
			require.NoError(t, err)
			pool, err := gdb.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = pool.Close() })
			pool.SetMaxOpenConns(1)
			_, err = pool.ExecContext(t.Context(), "CREATE TABLE probe (value INTEGER)")
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			conn, err := pool.Conn(ctx)
			require.NoError(t, err)
			var original *sqlite3.SQLiteConn
			require.NoError(t, conn.Raw(func(raw any) error {
				original = raw.(*sqlite3.SQLiteConn)
				original.RegisterAuthorizer(func(op int, name, arg, _ string) int {
					if tc.cancelQuery && op == sqlite3.SQLITE_SELECT {
						cancel()
						return sqlite3.SQLITE_DENY
					}
					if tc.refuseReset && op == sqlite3.SQLITE_PRAGMA && name == "query_only" && strings.EqualFold(arg, "off") {
						return sqlite3.SQLITE_DENY
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(t, conn.Close())

			resp, err := NewWithDB(ctx, gdb).Query(QueryRequest{SQL: "SELECT 1"})
			if tc.wantStage == "" {
				require.NoError(t, err)
				require.Equal(t, 1, resp.RowCount)
			} else {
				require.Equal(t, tc.wantStage, QueryStage(err))
				require.Nil(t, resp)
			}
			_, err = pool.ExecContext(t.Context(), "INSERT INTO probe VALUES (1)")
			require.NoError(t, err, "the next pool user must be able to write")
			next, err := pool.Conn(t.Context())
			require.NoError(t, err)
			defer next.Close()
			require.NoError(t, next.Raw(func(raw any) error {
				require.Equal(t, !tc.refuseReset, raw == original, "reuse only a restored connection")
				return nil
			}))
		})
	}
}
