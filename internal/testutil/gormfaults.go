package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

var faultSequence atomic.Uint64

// CommitFaults is the pool FailCommits installs. Every attempt is a real
// transaction; the counters tell a retry from a single pass.
type CommitFaults struct {
	*sql.DB
	failure   error
	remaining int

	Begins    int
	Commits   int
	Rollbacks int
	// Options holds each BEGIN's options, in order.
	Options []sql.TxOptions
	// AfterRollback, when set, runs each time a transaction has rolled back:
	// where a concurrent write lands between an attempt and its retry.
	AfterRollback func()
}

// FailCommits returns a handle on db whose next n COMMITs fail with failure
// after the transaction's reads have all succeeded, the way dqlite refuses a
// read-only COMMIT during a checkpoint. It needs a single goroutine.
func FailCommits(tb testing.TB, db *gorm.DB, failure error, n int) (*gorm.DB, *CommitFaults) {
	tb.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		tb.Fatalf("underlying database: %v", err)
	}
	faults := &CommitFaults{DB: sqlDB, failure: failure, remaining: n}
	// WithContext clones the statement, so db keeps its own pool.
	conn := db.Session(&gorm.Session{NewDB: true}).WithContext(tb.Context())
	conn.Statement.ConnPool = faults
	return conn, faults
}

// BeginTx starts a real transaction whose COMMIT consumes the faults.
func (f *CommitFaults) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	if opts != nil {
		f.Options = append(f.Options, *opts)
	}
	tx, err := f.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	f.Begins++
	return &commitFaultTx{Tx: tx, faults: f}, nil
}

type commitFaultTx struct {
	*sql.Tx
	faults *CommitFaults
}

func (tx *commitFaultTx) Commit() error {
	tx.faults.Commits++
	if tx.faults.remaining > 0 {
		tx.faults.remaining--
		return tx.faults.failure
	}
	return tx.Tx.Commit()
}

func (tx *commitFaultTx) Rollback() error {
	err := tx.Tx.Rollback()
	if err == nil {
		tx.faults.Rollbacks++
		if tx.faults.AfterRollback != nil {
			tx.faults.AfterRollback()
		}
	}
	return err
}

// FailJobRunReads fails the next n job_runs queries with transient contention.
// Each installation owns a unique callback and removes it when its test ends.
func FailJobRunReads(tb testing.TB, db *gorm.DB, n int) {
	tb.Helper()
	name := fmt.Sprintf("test:fail_job_run_reads:%s:%d", tb.Name(), faultSequence.Add(1))
	remaining := n
	if err := db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if remaining <= 0 {
			return
		}
		table := tx.Statement.Table
		if table == "" && tx.Statement.Schema != nil {
			table = tx.Statement.Schema.Table
		}
		if table != "job_runs" {
			return
		}
		remaining--
		_ = tx.AddError(errors.New("database is locked"))
	}); err != nil {
		tb.Fatalf("register query callback: %v", err)
	}
	tb.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
}
