package dqlite

import (
	"database/sql"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestSQLiteIsConfiguredForMultiThreadedUse guards the fix for the `no such
// vfs: ` flake (docs/exec-plans/active/trust-the-substrate.md, Ledger L12).
//
// go-dqlite's package init puts SQLite into single-thread mode, which disables
// the mutexes that make sqlite3_initialize() safe to call from two goroutines
// at once — and every Go process here also drives SQLite directly through
// mattn/go-sqlite3. threading.go undoes that. If a future import graph opens a
// database before that init runs, sqlite3_config comes back SQLITE_MISUSE and
// the process silently reverts to the unsafe mode; this test is what notices.
func TestSQLiteIsConfiguredForMultiThreadedUse(t *testing.T) {
	require.NoError(t, multiThreadConfigErr,
		"SQLite must be in multi-thread mode: single-thread mode makes concurrent "+
			"go-sqlite3 use (including the first open in a fresh test binary) racy")
}

// TestConcurrentFirstOpensSucceed is the behavioural half: many goroutines
// opening their first SQLite connection at once, which is exactly the shape
// that produced `no such vfs: ` in internal/trigger/event's parallel tests.
func TestConcurrentFirstOpensSucceed(t *testing.T) {
	const workers = 16
	ctx := t.Context()

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			db, err := sql.Open("sqlite3", "file:"+uuid.NewString()+"?mode=memory")
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = db.Close() }()
			errs[i] = db.PingContext(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "worker %d failed to open sqlite", i)
	}
}
