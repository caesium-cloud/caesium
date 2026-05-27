package db

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"gorm.io/gorm"
)

// BusyRetryBackoffs is the shared retry schedule for transient dqlite/SQLite
// contention. Total max wait ~2.27s across 8 retries: the budget must outlast
// worst-case transient write-lock windows, which under burst catalog contention
// exceed a few hundred ms. It is the single source of truth for the autocommit
// pool retry here and the per-transaction busy-retry helpers in internal/run and
// internal/backfill, so the layers compose predictably and cannot drift.
var BusyRetryBackoffs = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
	320 * time.Millisecond,
	640 * time.Millisecond,
	1000 * time.Millisecond,
}

// Transaction runs fn inside a single transaction against the default
// connection, retrying the whole transaction on transient dqlite contention.
//
// Use this for multi-statement units that must commit atomically. Statements
// issued inside a transaction bypass the connection pool's per-statement retry
// (re-running one statement of a partially-applied transaction would be
// unsafe), so the entire BEGIN..COMMIT is retried here on the shared
// BusyRetryBackoffs budget. A rolled-back transaction leaves no state, so
// re-running fn from the top is safe.
func Transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return transaction(ctx, Connection(), fn)
}

func transaction(ctx context.Context, conn *gorm.DB, fn func(tx *gorm.DB) error) error {
	conn = conn.WithContext(ctx)
	var err error
	for attempt := 0; ; attempt++ {
		err = conn.Transaction(fn)
		if err == nil || !dqlite.IsContentionError(err) {
			return err
		}
		if attempt >= len(BusyRetryBackoffs) {
			return err
		}
		metrics.DBBusyRetriesTotal.Inc()
		if sleepErr := sleepRetry(ctx, BusyRetryBackoffs[attempt]); sleepErr != nil {
			// Context cancelled/timed out during backoff: surface that, not the
			// contention error, so callers see context.Canceled/DeadlineExceeded
			// rather than a misleading DB failure.
			return sleepErr
		}
	}
}

// retryConnPool is a gorm.ConnPool decorator that transparently retries a
// single autocommit statement when it fails with a transient contention error
// (e.g. "database is locked", "checkpoint in progress").
//
// SAFETY: this only ever wraps the connection POOL, never a transaction. GORM
// runs an explicit transaction (db.Transaction(fn) / db.Begin()) by calling
// BeginTx and then issuing every statement against the returned *sql.Tx. This
// decorator's BeginTx delegates to the underlying *sql.DB and returns the raw
// *sql.Tx unwrapped, so statements executed inside a transaction bypass this
// decorator entirely and are NEVER retried individually — re-running one
// statement against a transaction the DB may have already rolled back would
// corrupt state. Whole-transaction retry remains owned by the existing
// withStoreBusyRetry / withBusyRetry closures, which re-run the entire
// BEGIN..COMMIT unit.
//
// Because the catalog/dqlite connections are opened with
// SkipDefaultTransaction=true, top-level single-row Create/Update/Delete also
// execute as one autocommit ExecContext through this decorator (a single
// INSERT/UPDATE/DELETE is atomic in SQLite/dqlite regardless of an enclosing
// BEGIN/COMMIT), so they are retried safely too.
type retryConnPool struct {
	pool     gorm.ConnPool
	backoffs []time.Duration
}

// Compile-time assertions that the decorator satisfies the interfaces GORM
// type-asserts on the connection pool.
var (
	_ gorm.ConnPool       = (*retryConnPool)(nil)
	_ gorm.TxBeginner     = (*retryConnPool)(nil)
	_ gorm.GetDBConnector = (*retryConnPool)(nil)
)

func newRetryConnPool(pool gorm.ConnPool) *retryConnPool {
	return &retryConnPool{pool: pool, backoffs: BusyRetryBackoffs}
}

// retry runs fn, retrying on transient contention with bounded backoff+jitter.
// Non-contention errors (including sql.ErrNoRows / gorm.ErrRecordNotFound,
// which are not contention) return immediately.
func (p *retryConnPool) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !dqlite.IsContentionError(err) {
			return err
		}
		if attempt >= len(p.backoffs) {
			return err
		}

		metrics.DBBusyRetriesTotal.Inc()
		if sleepErr := sleepRetry(ctx, p.backoffs[attempt]); sleepErr != nil {
			return err
		}
	}
}

func (p *retryConnPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	// Prepared statements are not used on the contention-prone hot paths;
	// delegate without retry to avoid double-preparing on a retry.
	return p.pool.PrepareContext(ctx, query)
}

func (p *retryConnPool) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	var (
		res sql.Result
		err error
	)
	err = p.retry(ctx, func() error {
		res, err = p.pool.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

func (p *retryConnPool) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	var (
		rows *sql.Rows
		err  error
	)
	err = p.retry(ctx, func() error {
		rows, err = p.pool.QueryContext(ctx, query, args...)
		return err
	})
	return rows, err
}

func (p *retryConnPool) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	// database/sql defers all errors from QueryRowContext to (*sql.Row).Scan,
	// and *sql.Row has no exported constructor, so the decorator cannot inspect
	// the result or re-run across the caller's Scan boundary. We therefore
	// delegate plainly rather than probe-then-call (the previous approach issued
	// two round-trips on every call and still left the real call unretried).
	//
	// This is an acceptable gap: GORM only routes through QueryRowContext for
	// the explicit `.Row()` API, whose sole users in this codebase are the
	// schema migrator's sqlite_master / PRAGMA reads in pkg/dqlite/migrator.go.
	// Those run at startup against low-contention metadata, before the server
	// takes traffic. Every hot-path read goes through QueryContext (First/Take/
	// Find/Scan all use it), which IS retried above.
	return p.pool.QueryRowContext(ctx, query, args...)
}

// BeginTx returns the raw *sql.Tx from the underlying pool (unwrapped) so that
// statements executed inside the transaction bypass this decorator — see the
// type doc for why in-tx statements must never be individually retried.
//
// The BEGIN itself, however, IS retried on transient contention. dqlite can
// hand out a pooled connection left with a still-active transaction after a
// checkpoint blip interrupted a prior rollback; the next BEGIN on it then fails
// with "cannot start a transaction within a transaction", which previously
// surfaced as a 500 (e.g. the job-apply path). Retrying BEGIN is safe precisely
// because no transaction statements have run yet, so re-running it cannot
// double-apply anything. On the poisoned-connection case we first issue a
// best-effort ROLLBACK to clear the leftover BEGIN so the retry can draw a
// usable connection. This makes every gorm transaction path — including bare
// db.Transaction(fn) call sites with no closure-level retry — recover from a
// poisoned/contended BEGIN uniformly.
func (p *retryConnPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	beginner, ok := p.pool.(gorm.TxBeginner)
	if !ok {
		return nil, gorm.ErrInvalidTransaction
	}
	var tx *sql.Tx
	err := p.retry(ctx, func() error {
		var beginErr error
		tx, beginErr = beginner.BeginTx(ctx, opts)
		if beginErr != nil && dqlite.IsConnPoolPoisonedError(beginErr) {
			// Best-effort: clear the leftover BEGIN on the poisoned pooled
			// connection. Harmless ("no transaction is active") on a clean one.
			// Caveat: database/sql hands out idle connections FIFO, so with
			// several pooled connections this ROLLBACK may land on a clean
			// connection rather than the poisoned one just returned. That is
			// acceptable — the poisoned connection clears once it reaches the
			// front of the idle queue on a later attempt/request, and meanwhile
			// the retry below draws a different (likely clean) connection for the
			// next BEGIN.
			_, _ = p.pool.ExecContext(ctx, "ROLLBACK")
		}
		return beginErr
	})
	return tx, err
}

// GetDBConn lets gorm's db.DB() reach the underlying *sql.DB for pool
// configuration (SetMaxOpenConns, etc.).
func (p *retryConnPool) GetDBConn() (*sql.DB, error) {
	if sqlDB, ok := p.pool.(*sql.DB); ok {
		return sqlDB, nil
	}
	if connector, ok := p.pool.(gorm.GetDBConnector); ok {
		return connector.GetDBConn()
	}
	return nil, gorm.ErrInvalidDB
}

// Ping is consulted by gorm.Open via an interface assertion on the pool.
func (p *retryConnPool) Ping() error {
	if pinger, ok := p.pool.(interface{ Ping() error }); ok {
		return pinger.Ping()
	}
	return nil
}

func sleepRetry(ctx context.Context, base time.Duration) error {
	timer := time.NewTimer(jitterRetryBackoff(base))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitterRetryBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	maxJitter := int64(base / 5)
	if maxJitter <= 0 {
		return base
	}
	return base - time.Duration(rand.Int64N(maxJitter+1))
}

// retryPlugin is a gorm.Plugin that wraps the connection pool with
// retryConnPool so every autocommit statement issued through the *gorm.DB is
// covered by contention retry, regardless of which call site issues it.
type retryPlugin struct{}

func (retryPlugin) Name() string { return "caesium:busy_retry" }

func (retryPlugin) Initialize(db *gorm.DB) error {
	if db.ConnPool == nil {
		return nil
	}
	if _, already := db.ConnPool.(*retryConnPool); already {
		return nil
	}
	db.ConnPool = newRetryConnPool(db.ConnPool)
	return nil
}
