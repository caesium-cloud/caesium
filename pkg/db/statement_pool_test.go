package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// instrumentedGORM starts with the same instrumented pool in both GORM
// locations, so these tests observe the production installation boundary.
func instrumentedGORM(t *testing.T, pool gorm.ConnPool) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	native, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = native.Close() })
	gdb.ConnPool = pool
	gdb.Statement.ConnPool = pool
	return gdb
}

func TestRetryPluginRetriesRealGORMOperations(t *testing.T) {
	for _, operation := range []string{"exec", "query", "begin"} {
		t.Run(operation, func(t *testing.T) {
			pool := newCountingPool(t, 1, errors.New("checkpoint in progress"))
			if operation == "begin" {
				pool.failBefore = 0
				pool.beginFailBefore = 1
				pool.beginErr = errors.New("cannot start a transaction within a transaction")
			}
			gdb := instrumentedGORM(t, pool)
			require.NoError(t, gdb.Use(retryPlugin{}))
			// Exercise GORM's cloned statement and real SQL callbacks, rather
			// than calling the retry wrapper directly or checking Config only.
			query := gdb.WithContext(t.Context())
			var err error
			value := 0
			switch operation {
			case "exec":
				err = query.Exec("SELECT 1").Error
			case "query":
				err = query.Raw("SELECT 1").Scan(&value).Error
			case "begin":
				err = query.Transaction(func(tx *gorm.DB) error {
					require.IsType(t, &sql.Tx{}, tx.Statement.ConnPool)
					return tx.Raw("SELECT 1").Scan(&value).Error
				})
			}
			require.NoError(t, err)
			if operation == "begin" {
				require.Equal(t, int32(2), pool.beginAttempt.Load(), "poisoned BEGIN must be cleared and retried")
				require.Equal(t, int32(1), pool.execAttempt.Load(), "one recovery ROLLBACK; in-tx SQL bypasses pool retry")
			} else {
				require.Equal(t, int32(2), pool.execAttempt.Load())
			}
			if operation != "exec" {
				require.Equal(t, 1, value)
			}
		})
	}
}

func TestRetryPluginResynchronisesWithoutRewrapping(t *testing.T) {
	writer := newCountingPool(t, 0, nil)
	gdb := instrumentedGORM(t, writer)
	installed := newRetryConnPool(writer)
	// Simulate a previously configured pool with a stale active statement.
	// Reinitialisation must repair it without wrapping the pool a second time.
	gdb.ConnPool = installed
	require.NoError(t, retryPlugin{}.Initialize(gdb))
	var value int
	require.NoError(t, gdb.WithContext(t.Context()).Raw("SELECT 1").Scan(&value).Error)
	require.Equal(t, 1, value)
	require.Equal(t, int32(1), writer.execAttempt.Load())
	require.Same(t, installed, gdb.ConnPool)
	require.Same(t, installed, gdb.Statement.ConnPool)
}

func TestInstallReadWriteSplitRoutesFreshGORMOperations(t *testing.T) {
	// Each pool has independent state, and each reader connection shares that
	// pool's uniquely named memory database when its size exceeds one.
	gdb, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	writer, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := sql.Open("sqlite3", "file:"+uuid.NewString()+"?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	for sentinel, pool := range map[string]*sql.DB{"writer": writer, "reader": reader} {
		_, err := pool.ExecContext(t.Context(), "CREATE TABLE records (id INTEGER PRIMARY KEY, name TEXT)")
		require.NoError(t, err)
		_, err = pool.ExecContext(t.Context(), "INSERT INTO records (name) VALUES (?)", sentinel)
		require.NoError(t, err)
	}

	// This is the exact post-open installer used by the native backend, called
	// once on a fresh gorm.Open handle without preinstalling a wrapper.
	require.NoError(t, installReadWriteSplit(gdb, reader, 3, 2))
	var sentinel string
	require.NoError(t, gdb.WithContext(t.Context()).Raw("SELECT name FROM records WHERE id = 1").Scan(&sentinel).Error)
	require.Equal(t, "reader", sentinel, "GORM must use the freshly installed statement pool")
	require.NoError(t, gdb.WithContext(t.Context()).Exec("INSERT INTO records (name) VALUES ('exec')").Error)
	record := struct {
		ID   int
		Name string
	}{Name: "create"}
	// SQLite's real Create callback uses INSERT ... RETURNING through
	// QueryContext when SkipDefaultTransaction is true.
	require.NoError(t, gdb.WithContext(t.Context()).Table("records").Create(&record).Error)
	require.Equal(t, 3, record.ID)
	var writerRows, readerRows int
	require.NoError(t, writer.QueryRowContext(t.Context(), "SELECT count(*) FROM records").Scan(&writerRows))
	require.NoError(t, reader.QueryRowContext(t.Context(), "SELECT count(*) FROM records").Scan(&readerRows))
	require.Equal(t, 3, writerRows)
	require.Equal(t, 1, readerRows, "Exec and returning Create must only mutate the writer")
	require.IsType(t, &rwSplitConnPool{}, gdb.ConnPool)
	require.Same(t, gdb.ConnPool, gdb.Statement.ConnPool)
	require.Equal(t, 1, writer.Stats().MaxOpenConnections)
	require.Equal(t, 1, writer.Stats().Idle)
	require.Equal(t, 3, reader.Stats().MaxOpenConnections)

	// Hold the configured reader capacity open, prove all connections see the
	// same table, then check that idle connections are capped at two.
	connections := make([]*sql.Conn, 0, 3)
	for range 3 {
		connection, err := reader.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = connection.Close() })
		connections = append(connections, connection)
		var count int
		require.NoError(t, connection.QueryRowContext(t.Context(), "SELECT count(*) FROM records").Scan(&count))
		require.Equal(t, 1, count)
	}
	for _, connection := range connections {
		require.NoError(t, connection.Close())
	}
	require.Equal(t, 2, reader.Stats().Idle)
}

func TestReadWriteSplitRoutesRealGORMReadsAndTransactions(t *testing.T) {
	writePool := newCountingPool(t, 0, nil)
	readPool := newCountingPool(t, 1, errors.New("checkpoint in progress"))
	gdb := instrumentedGORM(t, writePool)
	installConnPool(gdb, newRWSplitConnPool(writePool, readPool))
	var value int
	require.NoError(t, gdb.WithContext(t.Context()).Raw("SELECT 1").Scan(&value).Error)
	require.Equal(t, 1, value)
	require.Equal(t, int32(2), readPool.execAttempt.Load())
	require.Equal(t, int32(0), writePool.execAttempt.Load())
	require.NoError(t, gdb.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		require.IsType(t, &sql.Tx{}, tx.Statement.ConnPool)
		return tx.Raw("SELECT 1").Scan(&value).Error
	}, &sql.TxOptions{ReadOnly: true}))
	require.Equal(t, int32(1), readPool.beginAttempt.Load())
	require.Equal(t, int32(0), writePool.beginAttempt.Load())
	require.NoError(t, gdb.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		return tx.Exec("SELECT 1").Error
	}))
	require.Equal(t, int32(1), writePool.beginAttempt.Load())
	require.NoError(t, gdb.WithContext(t.Context()).Exec("SELECT 1").Error)
	require.Equal(t, int32(1), writePool.execAttempt.Load())
}

func TestReadWriteSplitRoutesReturningMutationsToWriter(t *testing.T) {
	for _, queryRow := range []bool{false, true} {
		name := "gorm_create_query"
		if queryRow {
			name = "query_row"
		}
		t.Run(name, func(t *testing.T) {
			writer := newCountingPool(t, 0, nil)
			reader := newCountingPool(t, 0, nil)
			for _, pool := range []*countingConnPool{writer, reader} {
				pool.db.SetMaxOpenConns(1)
				_, err := pool.db.ExecContext(t.Context(), "CREATE TABLE records (id INTEGER PRIMARY KEY, name TEXT)")
				require.NoError(t, err)
			}
			gdb := instrumentedGORM(t, writer)
			installed := newRWSplitConnPool(writer, reader)
			installConnPool(gdb, installed)
			record := struct {
				ID   int
				Name string
			}{Name: "persisted"}
			if queryRow {
				require.NoError(t, installed.QueryRowContext(t.Context(), "INSERT INTO records (name) VALUES (?) RETURNING id", record.Name).Scan(&record.ID))
			} else {
				require.NoError(t, gdb.WithContext(t.Context()).Table("records").Create(&record).Error)
				require.Equal(t, int32(1), writer.execAttempt.Load())
				require.Equal(t, int32(0), reader.execAttempt.Load())
			}
			require.Equal(t, 1, record.ID)
			// Independent backing databases make a route error observable as
			// a real write on the wrong pool, even when SQL returns success.
			var readerRows, writerRows int
			require.NoError(t, reader.db.QueryRowContext(t.Context(), "SELECT count(*) FROM records").Scan(&readerRows))
			require.NoError(t, writer.db.QueryRowContext(t.Context(), "SELECT count(*) FROM records").Scan(&writerRows))
			require.Equal(t, 0, readerRows)
			require.Equal(t, 1, writerRows)
		})
	}
}

func TestReadWriteSplitRoutesQueryForms(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
		read  bool
	}{
		{name: "plain_select", query: "SELECT marker FROM markers", read: true},
		{name: "lowercase_select", query: "select marker FROM markers", read: true},
		{name: "leading_newline", query: "\n  SELECT marker FROM markers", read: true},
		{name: "select_parenthesis", query: "SELECT(SELECT marker FROM markers)", read: true},
		{name: "select_star", query: "SELECT* FROM markers"},
		{name: "trailing_semicolon", query: "SELECT marker FROM markers;"},
		{name: "comment_prefix", query: "/* prefix */ SELECT marker FROM markers"},
		{name: "read_cte", query: "WITH item AS (SELECT marker FROM markers) SELECT marker FROM item"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := newCountingPool(t, 0, nil)
			reader := newCountingPool(t, 0, nil)
			for marker, pool := range map[string]*countingConnPool{"writer": writer, "reader": reader} {
				pool.db.SetMaxOpenConns(1)
				_, err := pool.db.ExecContext(t.Context(), "CREATE TABLE markers (marker TEXT)")
				require.NoError(t, err)
				_, err = pool.db.ExecContext(t.Context(), "INSERT INTO markers VALUES (?)", marker)
				require.NoError(t, err)
			}
			installed := newRWSplitConnPool(writer, reader)
			rows, err := installed.QueryContext(t.Context(), test.query)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()
			require.True(t, rows.Next())
			var marker string
			require.NoError(t, rows.Scan(&marker))
			require.False(t, rows.Next())
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			if test.read {
				require.Equal(t, "reader", marker)
				require.Equal(t, int32(1), reader.execAttempt.Load())
				require.Zero(t, writer.execAttempt.Load())
			} else {
				require.Equal(t, "writer", marker)
				require.Equal(t, int32(1), writer.execAttempt.Load())
				require.Zero(t, reader.execAttempt.Load())
			}
		})
	}
}

func TestReadWriteSplitRoutesAmbiguousQuerySQLToWriter(t *testing.T) {
	for _, statement := range []string{
		"WITH item(value) AS (SELECT 1) SELECT value FROM item",
		"WITH item(value) AS (SELECT 'cte') INSERT INTO records (name) SELECT value FROM item RETURNING id",
		"PRAGMA user_version = 123",
		"SELECT 1; INSERT INTO records (name) VALUES ('appended')",
		"/* prefix */ INSERT INTO records (name) VALUES ('commented') RETURNING id",
	} {
		t.Run(statement, func(t *testing.T) {
			writer := newCountingPool(t, 0, nil)
			reader := newCountingPool(t, 0, nil)
			writer.db.SetMaxOpenConns(1)
			_, err := writer.db.ExecContext(t.Context(), "CREATE TABLE records (id INTEGER PRIMARY KEY, name TEXT)")
			require.NoError(t, err)
			installed := newRWSplitConnPool(writer, reader)
			rows, err := installed.QueryContext(t.Context(), statement)
			require.NoError(t, err)
			for rows.Next() {
				// Drain row-returning statements so mutations finish before Close.
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			require.Equal(t, int32(1), writer.execAttempt.Load())
			require.Equal(t, int32(0), reader.execAttempt.Load())
			if statement == "PRAGMA user_version = 123" {
				var readVersion, writeVersion int
				require.NoError(t, reader.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&readVersion))
				require.NoError(t, writer.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&writeVersion))
				require.Zero(t, readVersion)
				require.Equal(t, 123, writeVersion)
			}
		})
	}
}
