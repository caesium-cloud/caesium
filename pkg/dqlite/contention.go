package dqlite

import (
	"errors"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// IsContentionError reports whether err is a transient dqlite/SQLite contention
// or connection-state error worth retrying.
//
// This is the single source of truth for contention classification across the
// codebase. The bounded busy-retry helpers (internal/run, internal/worker,
// internal/backfill) and the global connection-pool retry in pkg/db all
// delegate here so the matched error strings live in exactly one place.
//
// Two distinct classes are recognised:
//
//   - Direct contention (`database is locked`, `checkpoint in progress`, etc.):
//     a simple retry is likely to find the database in a non-busy state. dqlite
//     surfaces a WAL checkpoint in progress as a discrete "checkpoint in
//     progress" error rather than SQLITE_BUSY, so it must be matched by string.
//   - Connection-state poisoning (`cannot start a transaction within a
//     transaction`): a previous transaction on the same pooled connection
//     failed mid-flight (typically because the implicit ROLLBACK after a
//     `checkpoint in progress` itself failed) and left the connection with a
//     still-active SQLite transaction handle. The next caller's BEGIN on that
//     connection then fails. With multiple pooled connections, retries are
//     likely to draw a clean one.
func IsContentionError(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		if sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked {
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "database schema is locked") ||
		strings.Contains(msg, "database is busy") ||
		strings.Contains(msg, "checkpoint in progress") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked") ||
		strings.Contains(msg, "cannot start a transaction within a transaction")
}

// IsConnPoolPoisonedError matches the narrow case where a pooled connection has
// been left with a stale active transaction. Distinct from IsContentionError,
// which also covers transient busy/locked errors that don't require active
// recovery (an explicit ROLLBACK on the pool to clear the leftover BEGIN).
func IsConnPoolPoisonedError(err error) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if strings.Contains(strings.ToLower(err.Error()), "cannot start a transaction within a transaction") {
			return true
		}
	}
	return false
}
