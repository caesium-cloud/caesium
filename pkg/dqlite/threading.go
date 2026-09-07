package dqlite

import (
	"github.com/caesium-cloud/caesium/pkg/log"
	godqlite "github.com/canonical/go-dqlite/v3"
)

// init puts SQLite into multi-thread mode for every process that links dqlite.
//
// go-dqlite's own package init switches SQLite to SINGLE-THREAD mode
// process-wide (github.com/canonical/go-dqlite/v3/config.go: `bindings.
// ConfigSingleThread()`, unless GO_DQLITE_MULTITHREAD=1), because the dqlite
// engine is itself single-threaded and the mutexes would only cost it. That
// mode disables *every* SQLite mutex, including the static one that guards the
// library's one-time `sqlite3_initialize()`.
//
// Caesium also drives SQLite directly, from Go, on many goroutines at once:
// gorm's sqlite dialector over mattn/go-sqlite3 backs the `sqlite` database
// engine and every in-memory database the unit tests open. With the mutexes
// off, two goroutines opening their first connection in a fresh process race
// that one-time initialization, and the loser observes an unpopulated VFS list
// — reported as `no such vfs: ` with an EMPTY name, which is exactly what
// SQLite prints when the default VFS lookup finds nothing.
//
// That is the `internal/trigger/event` unit-test flake tracked in
// docs/exec-plans/active/trust-the-substrate.md (Ledger L12): the two router
// tests are the package's first `t.Parallel()` openers, so they are the ones
// that collide. Reproduced locally at roughly 1 in 40 FRESH test processes
// (`-race`, 300 runs); `-count=N` inside one process never shows it, because
// only the first open is at risk.
//
// go-dqlite documents this remedy for exactly this situation: "If your Go
// process also uses SQLite directly (e.g. using the github.com/mattn/go-sqlite3
// bindings) you might need to switch to Multi-thread mode in order to be
// thread-safe." It has to run before any SQLite API call, which this does:
// imported packages initialize first, so go-dqlite has already set single-thread
// mode by the time this runs, and nothing in Caesium has opened a database yet.
//
// A failure here is logged rather than fatal: it means some other package
// initialized SQLite ahead of us (SQLITE_MISUSE), and the process is still
// usable — just back on go-dqlite's default threading mode.
func init() {
	multiThreadConfigErr = godqlite.ConfigMultiThread()
	if multiThreadConfigErr != nil {
		log.Warn("dqlite: could not switch SQLite to multi-thread mode; "+
			"concurrent use of the sqlite driver may be unsafe", "error", multiThreadConfigErr)
	}
}

// multiThreadConfigErr records what the switch above returned. It is what
// TestSQLiteIsConfiguredForMultiThreadedUse asserts on: sqlite3_config refuses
// with SQLITE_MISUSE once the library has been initialized, so a non-nil value
// means some package opened a database before this init ran and the process is
// back on go-dqlite's single-thread default.
var multiThreadConfigErr error
