package testutil

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

var faultSequence atomic.Uint64

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
		tx.AddError(errors.New("database is locked"))
	}); err != nil {
		tb.Fatalf("register query callback: %v", err)
	}
	tb.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
}
