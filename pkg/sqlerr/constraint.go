package sqlerr

import (
	"errors"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// IsUniqueConstraint reports whether err is a unique/primary-key constraint
// violation from SQLite/dqlite or a compatible SQL driver message.
func IsUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique ||
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		(strings.Contains(msg, "constraint failed") && strings.Contains(msg, "unique")) ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "duplicate entry")
}
