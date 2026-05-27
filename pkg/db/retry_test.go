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

	// BeginTx fails its first beginFailBefore calls with beginErr, then succeeds.
	beginAttempt    int32
	beginFailBefore int32
	beginErr        error
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
	if n := atomic.AddInt32(&c.beginAttempt, 1); n <= c.beginFailBefore {
		return nil, c.beginErr
	}
	return c.db.BeginTx(ctx, opts)
}

func (c *countingConnPool) GetDBConn() (*sql.DB, error) {
	return c.db, nil
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

// TestRetryConnPoolBeginTxRetriesPoisonedConnection proves a BEGIN that fails on
// a poisoned pooled connection ("cannot start a transaction within a
// transaction") is retried — with a best-effort ROLLBACK to clear the leftover
// BEGIN — and then succeeds, returning a raw *sql.Tx. This is what makes bare
// db.Transaction(fn) call sites (e.g. the job-apply path) recover from a
// poisoned BEGIN instead of surfacing a 500.
func TestRetryConnPoolBeginTxRetriesPoisonedConnection(t *testing.T) {
	pool := newCountingPool(t, 0, nil)
	pool.beginFailBefore = 2
	pool.beginErr = errors.New("cannot start a transaction within a transaction")
	rp := newRetryConnPool(pool)

	tx, err := rp.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	require.IsType(t, &sql.Tx{}, tx, "BeginTx must still return a raw *sql.Tx")
	require.Equal(t, int32(3), atomic.LoadInt32(&pool.beginAttempt),
		"BEGIN should be retried past 2 poisoned failures and succeed on attempt 3")
	require.Equal(t, int32(2), atomic.LoadInt32(&pool.execAttempt),
		"each of the 2 poisoned BEGINs should trigger exactly one best-effort ROLLBACK clear")
}

// TestRetryConnPoolBeginTxDoesNotRetryNonContention proves a non-contention
// BEGIN error returns immediately without retry.
func TestRetryConnPoolBeginTxDoesNotRetryNonContention(t *testing.T) {
	pool := newCountingPool(t, 0, nil)
	pool.beginFailBefore = 1
	pool.beginErr = errors.New("disk I/O error")
	rp := newRetryConnPool(pool)

	_, err := rp.BeginTx(context.Background(), nil)
	require.Error(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&pool.beginAttempt),
		"a non-contention BEGIN error must not be retried")
	require.Equal(t, int32(0), atomic.LoadInt32(&pool.execAttempt),
		"no ROLLBACK clear on a non-poisoned error")
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

// TestTransactionRetriesContentionThenCommits proves the whole-transaction
// helper re-runs the entire closure on transient contention and commits once it
// succeeds — the path the per-statement pool retry deliberately does not cover.
func TestTransactionRetriesContentionThenCommits(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)

	calls := 0
	err = transaction(context.Background(), gdb, func(tx *gorm.DB) error {
		calls++
		if calls < 3 {
			return errors.New("database is locked")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, calls, "expected two contention retries before the closure committed")
}

// TestTransactionDoesNotRetryNonContention proves a non-contention error from
// the closure is returned immediately without re-running the transaction.
func TestTransactionDoesNotRetryNonContention(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)

	calls := 0
	err = transaction(context.Background(), gdb, func(tx *gorm.DB) error {
		calls++
		return errors.New("not retryable")
	})
	require.Error(t, err)
	require.Equal(t, 1, calls, "non-contention errors must not be retried")
}

// TestTransactionSurfacesContextCancellation proves that when the context is
// cancelled during the retry backoff, the helper returns the context error
// rather than masking it as a DB contention error.
func TestTransactionSurfacesContextCancellation(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err = transaction(ctx, gdb, func(tx *gorm.DB) error {
		calls++
		cancel() // cancel before the backoff sleep on this contention error
		return errors.New("database is locked")
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls, "a cancelled context must stop the retry loop")
}

// TestRWSplitRoutesReadsAndWrites proves the splitter sends writes and
// transactions to the write pool and autocommit reads to the read pool — the
// routing that lets writes serialize on one connection while reads run
// concurrently on another.
func TestRWSplitRoutesReadsAndWrites(t *testing.T) {
	writePool := newCountingPool(t, 0, nil)
	readPool := newCountingPool(t, 0, nil)
	sp := newRWSplitConnPool(writePool, readPool)

	_, err := sp.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS t (id INTEGER)")
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&writePool.execAttempt), "write must hit the write pool")
	require.Equal(t, int32(0), atomic.LoadInt32(&readPool.execAttempt), "write must not hit the read pool")

	rows, err := sp.QueryContext(context.Background(), "SELECT 1")
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	require.Equal(t, int32(1), atomic.LoadInt32(&readPool.execAttempt), "read must hit the read pool")
	require.Equal(t, int32(1), atomic.LoadInt32(&writePool.execAttempt), "read must not touch the write pool")

	tx, err := sp.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.Equal(t, int32(1), atomic.LoadInt32(&writePool.beginAttempt), "transactions must begin on the write pool")
	require.Equal(t, int32(0), atomic.LoadInt32(&readPool.beginAttempt), "transactions must not begin on the read pool")
}

// TestRWSplitCloseClosesBothPools proves Close releases both pools — the read
// pool is otherwise unreachable via GetDBConn (which returns only the write
// pool), so without this it would leak on shutdown.
func TestRWSplitCloseClosesBothPools(t *testing.T) {
	writePool := newCountingPool(t, 0, nil)
	readPool := newCountingPool(t, 0, nil)
	sp := newRWSplitConnPool(writePool, readPool)

	require.NoError(t, sp.Close())

	_, werr := writePool.db.ExecContext(context.Background(), "SELECT 1")
	require.Error(t, werr, "write pool should be closed")
	_, rerr := readPool.db.ExecContext(context.Background(), "SELECT 1")
	require.Error(t, rerr, "read pool should be closed")
}
