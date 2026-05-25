package db

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// countingConnPool is a gorm.ConnPool that fails the first failBefore calls to
// Exec/Query with a contention error, then succeeds, counting total attempts.
// It embeds a real *sql.DB so BeginTx returns a genuine *sql.Tx.
type countingConnPool struct {
	db          *sql.DB
	execAttempt int32
	failBefore  int32
	failErr     error
}

func (c *countingConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return c.db.PrepareContext(ctx, query)
}

func (c *countingConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if n := atomic.AddInt32(&c.execAttempt, 1); n <= c.failBefore {
		return nil, c.failErr
	}
	return c.db.ExecContext(ctx, query, args...)
}

func (c *countingConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if n := atomic.AddInt32(&c.execAttempt, 1); n <= c.failBefore {
		return nil, c.failErr
	}
	return c.db.QueryContext(ctx, query, args...)
}

func (c *countingConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return c.db.QueryRowContext(ctx, query, args...)
}

func (c *countingConnPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return c.db.BeginTx(ctx, opts)
}

func newCountingPool(t *testing.T, failBefore int, failErr error) *countingConnPool {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &countingConnPool{db: sqlDB, failBefore: int32(failBefore), failErr: failErr}
}

// TestRetryConnPoolRetriesAutocommitContention proves a single autocommit
// statement is retried on a transient contention error and then succeeds.
func TestRetryConnPoolRetriesAutocommitContention(t *testing.T) {
	pool := newCountingPool(t, 2, errors.New("checkpoint in progress"))
	rp := newRetryConnPool(pool)

	_, err := rp.ExecContext(context.Background(), "SELECT 1")
	require.NoError(t, err)
	require.Equal(t, int32(3), atomic.LoadInt32(&pool.execAttempt),
		"expected 2 contention failures to be retried, succeeding on attempt 3")
}

// TestRetryConnPoolDoesNotRetryNonContention proves non-contention errors are
// returned immediately without retry.
func TestRetryConnPoolDoesNotRetryNonContention(t *testing.T) {
	pool := newCountingPool(t, 1, errors.New("syntax error"))
	rp := newRetryConnPool(pool)

	_, err := rp.QueryContext(context.Background(), "SELECT 1")
	require.Error(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&pool.execAttempt),
		"non-contention errors must not be retried")
}

// TestRetryConnPoolBeginTxReturnsRawTx is the critical safety guard: BeginTx
// must return a raw *sql.Tx, NOT a *retryConnPool. This is what guarantees that
// statements executed inside an explicit transaction bypass the per-statement
// retry — re-running one statement of a transaction the DB may have rolled back
// would corrupt state.
func TestRetryConnPoolBeginTxReturnsRawTx(t *testing.T) {
	pool := newCountingPool(t, 0, nil)
	rp := newRetryConnPool(pool)

	tx, err := rp.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	// The returned value is a *sql.Tx, which does not carry the retry wrapper.
	require.IsType(t, &sql.Tx{}, tx)
	_, isRetry := interface{}(tx).(*retryConnPool)
	require.False(t, isRetry, "BeginTx must not return a retry-wrapped pool")
}

// TestPluginInstallsRetryPool proves the plugin wraps the gorm connection pool
// exactly once and that the wrapped DB still works end-to-end.
func TestPluginInstallsRetryPool(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)

	require.NoError(t, gdb.Use(retryPlugin{}))
	_, ok := gdb.ConnPool.(*retryConnPool)
	require.True(t, ok, "plugin should wrap ConnPool in retryConnPool")

	// Idempotent: re-running Initialize on an already-wrapped DB must not
	// double-wrap (gorm itself rejects a second Use of the same plugin name,
	// but the Initialize guard protects against any direct re-invocation).
	require.NoError(t, retryPlugin{}.Initialize(gdb))
	rp := gdb.ConnPool.(*retryConnPool)
	_, doubleWrapped := rp.pool.(*retryConnPool)
	require.False(t, doubleWrapped, "plugin must not double-wrap the pool")

	// db.DB() must still resolve through the decorator's GetDBConn.
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.PingContext(context.Background()))
}

// TestInTransactionStatementNotIndividuallyRetried proves that when a statement
// runs inside an explicit gorm transaction, the per-statement retry is bypassed
// (the tx statements never touch retryConnPool). We assert this structurally:
// inside db.Transaction the statement's ConnPool is a *sql.Tx, not the
// retryConnPool, so a contention error there is owned by the surrounding
// closure-level retry, not silently re-run mid-transaction.
func TestInTransactionStatementNotIndividuallyRetried(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	require.NoError(t, gdb.Use(retryPlugin{}))

	// Outside a transaction the active ConnPool is the retry decorator.
	_, autocommitWrapped := gdb.ConnPool.(*retryConnPool)
	require.True(t, autocommitWrapped)

	var connPoolInsideTx interface{}
	require.NoError(t, gdb.Transaction(func(tx *gorm.DB) error {
		connPoolInsideTx = tx.Statement.ConnPool
		return nil
	}))

	// Inside the transaction the statement runs against a committer (*sql.Tx),
	// never the retry decorator — so individual in-tx statements are not
	// retried by the global mechanism.
	_, isRetryInsideTx := connPoolInsideTx.(*retryConnPool)
	require.False(t, isRetryInsideTx, "in-transaction statements must not run through the retry pool")
	_, isCommitter := connPoolInsideTx.(gorm.TxCommitter)
	require.True(t, isCommitter, "in-transaction ConnPool should be a TxCommitter (*sql.Tx)")
}
