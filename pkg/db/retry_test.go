package db

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/testutil"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// countingConnPool is a gorm.ConnPool that fails the first failBefore calls to
// Exec/Query with a contention error, then succeeds, counting total attempts.
// It embeds a real *sql.DB so BeginTx returns a genuine *sql.Tx.
type countingConnPool struct {
	db          *sql.DB
	execAttempt atomic.Int32
	failBefore  int32
	failErr     error

	// BeginTx fails its first beginFailBefore calls with beginErr, then succeeds.
	beginAttempt    atomic.Int32
	beginFailBefore int32
	beginErr        error
}

func (c *countingConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return c.db.PrepareContext(ctx, query)
}

func (c *countingConnPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if n := c.execAttempt.Add(1); n <= c.failBefore {
		return nil, c.failErr
	}
	return c.db.ExecContext(ctx, query, args...)
}

func (c *countingConnPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if n := c.execAttempt.Add(1); n <= c.failBefore {
		return nil, c.failErr
	}
	return c.db.QueryContext(ctx, query, args...)
}

func (c *countingConnPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.db.QueryRowContext(ctx, query, args...)
}

func (c *countingConnPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	if n := c.beginAttempt.Add(1); n <= c.beginFailBefore {
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
	require.Equal(t, int32(3), pool.execAttempt.Load(),
		"expected 2 contention failures to be retried, succeeding on attempt 3")
}

// TestRetryConnPoolDoesNotRetryNonContention proves non-contention errors are
// returned immediately without retry.
func TestRetryConnPoolDoesNotRetryNonContention(t *testing.T) {
	pool := newCountingPool(t, 1, errors.New("syntax error"))
	rp := newRetryConnPool(pool)

	_, err := rp.QueryContext(context.Background(), "SELECT 1")
	require.Error(t, err)
	require.Equal(t, int32(1), pool.execAttempt.Load(),
		"non-contention errors must not be retried")
}

// The completion guard delegates statements directly to its raw *sql.Tx,
// bypassing the pool's per-statement retry.
func TestRetryConnPoolTransactionStatementsBypassPoolRetry(t *testing.T) {
	pool := newCountingPool(t, 0, nil)
	rp := newRetryConnPool(pool)

	tx, err := rp.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.(gorm.Tx).Rollback() })

	guard, ok := tx.(*guardedTx)
	require.True(t, ok)
	require.IsType(t, &sql.Tx{}, guard.Tx)
	_, err = tx.ExecContext(t.Context(), "SELECT 1")
	require.NoError(t, err)
	require.Zero(t, pool.execAttempt.Load(), "transaction statements must bypass the retry pool")
	_, isRetry := any(tx).(*retryConnPool)
	require.False(t, isRetry, "BeginTx must not return a retry-wrapped pool")
}

// TestRetryConnPoolBeginTxRetriesPoisonedConnection proves a BEGIN that fails on
// a poisoned connection ("cannot start a transaction within a transaction") is
// discarded and retried, and then succeeds. This is what makes bare
// db.Transaction(fn) call sites (e.g. the job-apply path) recover from a
// poisoned BEGIN instead of surfacing a 500.
func TestRetryConnPoolBeginTxRetriesPoisonedConnection(t *testing.T) {
	_, _, pool, state := faultSQLite(t)
	state.err = errors.New("cannot start a transaction within a transaction")
	state.arm("begin", 2)
	rp := newRetryConnPool(pool)

	tx, err := rp.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.(gorm.Tx).Rollback() })

	require.IsType(t, &guardedTx{}, tx)
	require.Equal(t, 3, state.count("begin"),
		"BEGIN should be retried past 2 poisoned failures and succeed on attempt 3")
	require.True(t, state.wasClosed(1))
	require.True(t, state.wasClosed(2), "each failed BEGIN must discard its own physical connection")
	require.Zero(t, state.count("rollback"), "recovery must not issue ROLLBACK through another pool member")
}

// TestRetryConnPoolBeginTxDoesNotRetryNonContention proves a non-contention
// BEGIN error returns immediately without retry.
func TestRetryConnPoolBeginTxDoesNotRetryNonContention(t *testing.T) {
	_, _, pool, state := faultSQLite(t)
	state.err = errors.New("disk I/O error")
	state.arm("begin", 1)
	rp := newRetryConnPool(pool)

	_, err := rp.BeginTx(context.Background(), nil)
	require.Error(t, err)
	require.Equal(t, 1, state.count("begin"),
		"a non-contention BEGIN error must not be retried")
	require.True(t, state.wasClosed(1))
	require.Zero(t, state.count("rollback"), "no pool-wide ROLLBACK clear")
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

	var connPoolInsideTx any
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

// TestTransactionOnRetriesContentionThenCommits: the exported variant over a
// caller's connection retries the whole closure the same way.
func TestTransactionOnRetriesContentionThenCommits(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)

	calls := 0
	require.NoError(t, TransactionOn(context.Background(), gdb, func(tx *gorm.DB) error {
		calls++
		if calls < 2 {
			return errors.New("database is locked")
		}
		return nil
	}))
	require.Equal(t, 2, calls)
}

// TestTransactionOnCarriesOptionsToEveryAttempt: a retried read-only snapshot
// stays read-only, so it keeps its read view and never reaches for the writer.
func TestTransactionOnCarriesOptionsToEveryAttempt(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	conn, faults := testutil.FailCommits(t, gdb, errors.New("checkpoint in progress"), 1)
	options := sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}

	calls := 0
	require.NoError(t, TransactionOn(t.Context(), conn, func(*gorm.DB) error {
		calls++
		return nil
	}, &options))

	require.Equal(t, 2, calls)
	require.Equal(t, []sql.TxOptions{options, options}, faults.Options)
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
	require.Equal(t, int32(1), writePool.execAttempt.Load(), "write must hit the write pool")
	require.Equal(t, int32(0), readPool.execAttempt.Load(), "write must not hit the read pool")

	rows, err := sp.QueryContext(context.Background(), "SELECT 1")
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	require.Equal(t, int32(1), readPool.execAttempt.Load(), "read must hit the read pool")
	require.Equal(t, int32(1), writePool.execAttempt.Load(), "read must not touch the write pool")

	tx, err := sp.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, writePool.db.Stats().InUse, "transactions must pin the write pool")
	require.Zero(t, readPool.db.Stats().InUse)
	require.NoError(t, tx.(gorm.Tx).Rollback())
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

func TestRetryBudgetsExhaustWithoutExtraAttempt(t *testing.T) {
	old := BusyRetryBackoffs
	BusyRetryBackoffs = []time.Duration{0, 0}
	t.Cleanup(func() { BusyRetryBackoffs = old })
	busy := errors.New("database is locked")
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	calls := 0
	require.ErrorIs(t, transaction(context.Background(), gdb, func(tx *gorm.DB) error { calls++; return busy }), busy)
	require.Equal(t, 3, calls)
	pool := newCountingPool(t, 99, busy)
	_, err = newRetryConnPool(pool).ExecContext(context.Background(), "SELECT 1")
	require.ErrorIs(t, err, busy)
	require.Equal(t, int32(3), pool.execAttempt.Load())
}

func TestTransactionRetriesRollbackAllAttemptWrites(t *testing.T) {
	old := BusyRetryBackoffs
	BusyRetryBackoffs = []time.Duration{0, 0}
	t.Cleanup(func() { BusyRetryBackoffs = old })
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, gdb.Exec("CREATE TABLE retry_writes (attempt INTEGER)").Error)
	calls := 0
	require.NoError(t, transaction(context.Background(), gdb, func(tx *gorm.DB) error {
		calls++
		require.NoError(t, tx.Exec("INSERT INTO retry_writes VALUES (?)", calls).Error)
		if calls < 3 {
			return errors.New("database is locked")
		}
		return nil
	}))
	var attempts []int
	require.NoError(t, gdb.Raw("SELECT attempt FROM retry_writes").Scan(&attempts).Error)
	require.Equal(t, []int{3}, attempts)
}

func TestJitterRetryBackoffBounds(t *testing.T) {
	for _, base := range []time.Duration{-1, 0, 4, 10 * time.Millisecond, time.Second} {
		for range 100 {
			got := jitterRetryBackoff(base)
			if base <= 0 {
				require.Zero(t, got)
			} else {
				require.GreaterOrEqual(t, got, base-base/5)
				require.LessOrEqual(t, got, base)
			}
		}
	}
}

// Pool cancellation retains both the backoff error and its last contention.
func TestRetryPoolSurfacesWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busy := errors.New("database is locked")
	p := newRetryConnPool(newCountingPool(t, 0, nil))
	calls := 0
	err := p.retry(ctx, func() error { calls++; cancel(); return busy })
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, busy)
	require.True(t, dqlite.IsContentionError(err))
	require.Equal(t, 1, calls)
}

func TestRetryPoolCancellationPreservesLastContention(t *testing.T) {
	for _, stopped := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(stopped.Error(), func(t *testing.T) {
			first := errors.New("checkpoint in progress: first")
			last := errors.New("database is locked: last")
			p := newRetryConnPool(newCountingPool(t, 0, nil))
			waits := 0
			p.wait = func(context.Context, time.Duration) error {
				waits++
				if waits == 2 {
					return stopped
				}
				return nil
			}
			calls := 0
			err := p.retry(t.Context(), func() error {
				calls++
				if calls == 1 {
					return first
				}
				return last
			})
			require.ErrorIs(t, err, stopped)
			require.ErrorIs(t, err, last)
			require.NotErrorIs(t, err, first)
			require.True(t, dqlite.IsContentionError(err))
			require.Equal(t, 2, calls)
			// The next operation must not inherit a previous operation's error.
			require.ErrorIs(t, p.retry(t.Context(), func() error { return stopped }), stopped)
			require.False(t, dqlite.IsContentionError(p.retry(t.Context(), func() error { return stopped })))
			require.NoError(t, p.retry(t.Context(), func() error { return nil }))
		})
	}
}

func TestRWSplitReadOnlyTransactionDoesNotWaitForSerializedWriter(t *testing.T) {
	writePool := newCountingPool(t, 0, nil)
	writePool.db.SetMaxOpenConns(1)
	readPool := newCountingPool(t, 0, nil)
	split := newRWSplitConnPool(writePool, readPool)
	writer, err := split.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.(gorm.Tx).Rollback() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reader, err := split.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err, "read-only acquisition must succeed while the only writer is held")
	t.Cleanup(func() { _ = reader.(gorm.Tx).Rollback() })
	var value int
	require.NoError(t, reader.QueryRowContext(ctx, "SELECT 1").Scan(&value))
	require.Equal(t, 1, value)
	require.Equal(t, 1, writePool.db.Stats().InUse)
	require.Equal(t, 1, readPool.db.Stats().InUse)
	require.IsType(t, &guardedTx{}, reader, "snapshot statements bypass per-statement retry")
}

func TestDatabaseRetryPathsUseInjectedWaitSchedule(t *testing.T) {
	for _, wholeTransaction := range []bool{false, true} {
		t.Run(map[bool]string{false: "pool", true: "transaction"}[wholeTransaction], func(t *testing.T) {
			schedule := []time.Duration{17 * time.Millisecond, 31 * time.Millisecond}
			var waits []time.Duration
			wait := func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			calls := 0
			operation := func() error {
				calls++
				if calls < 3 {
					return errors.New("database is locked")
				}
				return nil
			}
			var err error
			if wholeTransaction {
				conn, openErr := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
				require.NoError(t, openErr)
				sqlDB, dbErr := conn.DB()
				require.NoError(t, dbErr)
				t.Cleanup(func() { _ = sqlDB.Close() })
				err = transactionWithPolicy(context.Background(), conn, busyRetryPolicy(schedule, wait), func(*gorm.DB) error { return operation() })
			} else {
				pool := newRetryConnPool(newCountingPool(t, 0, nil))
				pool.backoffs = schedule
				pool.wait = wait
				err = pool.retry(context.Background(), operation)
			}
			require.NoError(t, err)
			require.Equal(t, 3, calls)
			require.Equal(t, schedule, waits)
		})
	}
}
