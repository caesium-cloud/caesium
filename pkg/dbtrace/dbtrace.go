package dbtrace

import (
	"sync"
	"time"
)

const (
	maxRecentStatements = 16
	maxSQLLength        = 4096
)

// Statement describes a recently executed database statement. SQL is the
// rendered statement emitted by GORM, including bound values where GORM can
// render them for logging.
type Statement struct {
	At       time.Time `json:"at"`
	Duration string    `json:"duration"`
	Rows     int64     `json:"rows"`
	SQL      string    `json:"sql"`
	Error    string    `json:"error,omitempty"`
}

var recent = struct {
	sync.Mutex
	next  int
	count int
	buf   [maxRecentStatements]Statement
}{}

// Record saves a statement in a small process-local ring buffer for nearby
// diagnostic logs such as dqlite protocol warnings.
func Record(sql string, rows int64, duration time.Duration, err error) {
	stmt := Statement{
		At:       time.Now().UTC(),
		Duration: duration.String(),
		Rows:     rows,
		SQL:      truncateSQL(sql),
	}
	if err != nil {
		stmt.Error = err.Error()
	}

	recent.Lock()
	defer recent.Unlock()

	recent.buf[recent.next] = stmt
	recent.next = (recent.next + 1) % len(recent.buf)
	if recent.count < len(recent.buf) {
		recent.count++
	}
}

// Recent returns up to limit recent statements ordered oldest to newest.
func Recent(limit int) []Statement {
	recent.Lock()
	defer recent.Unlock()

	if limit <= 0 || limit > recent.count {
		limit = recent.count
	}
	out := make([]Statement, 0, limit)
	start := recent.next - recent.count
	if start < 0 {
		start += len(recent.buf)
	}
	start += recent.count - limit
	for i := 0; i < limit; i++ {
		idx := (start + i) % len(recent.buf)
		out = append(out, recent.buf[idx])
	}
	return out
}

// Reset clears the trace buffer. It is intended for tests.
func Reset() {
	recent.Lock()
	defer recent.Unlock()

	recent.next = 0
	recent.count = 0
	for idx := range recent.buf {
		recent.buf[idx] = Statement{}
	}
}

func truncateSQL(sql string) string {
	if len(sql) <= maxSQLLength {
		return sql
	}
	return sql[:maxSQLLength] + "...(truncated)"
}
