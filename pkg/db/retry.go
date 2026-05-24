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

// retryBusyBackoffs schedules retries for transient dqlite/SQLite contention on
// single autocommit statements. Total max wait ~310ms across 5 retries, the
// same schedule the per-transaction busy-retry helpers use so the layers
// compose predictably under load.
var retryBusyBackoffs = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
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
	return &retryConnPool{pool: pool, backoffs: retryBusyBackoffs}
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

// BeginTx delegates to the underlying pool and returns the raw transaction so
// that statements executed inside the transaction bypass this decorator. See
// the type doc for the safety rationale.
func (p *retryConnPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	if beginner, ok := p.pool.(gorm.TxBeginner); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, gorm.ErrInvalidTransaction
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
