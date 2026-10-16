package dbretry

import (
	"errors"
	"strings"
)

// IsLockContention reports whether err is transient SQLite/dqlite contention,
// recognised by message so the check needs no driver import: a write lock,
// a busy database, a checkpoint in progress, or a pooled connection left
// inside a failed transaction. pkg/dqlite.IsContentionError adds the typed
// sqlite3 codes on top and delegates its message matching here, so the
// matched strings live in one place.
func IsLockContention(err error) bool {
	if err == nil {
		return false
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

// IsSerializationConflict reports whether err carries a PostgreSQL
// serialization_failure (SQLSTATE 40001) or deadlock_detected (40P01). The
// server aborted one side of a conflict and re-running the whole transaction
// is the documented remedy; re-running one statement of it is not. The driver
// error (pgconn.PgError) is matched by its SQLState method, so this package
// does not import a Postgres driver.
func IsSerializationConflict(err error) bool {
	pgErr, ok := errors.AsType[interface {
		error
		SQLState() string
	}](err)
	if !ok {
		return false
	}
	switch pgErr.SQLState() {
	case "40001", "40P01":
		return true
	default:
		return false
	}
}
