package callback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// openConcurrentTestDBs returns two INDEPENDENT gorm handles onto the same
// file-backed database. Two handles with their own connections is the closest a
// unit test gets to two caesium server processes dispatching against one dqlite
// cluster: nothing in-process serialises them, so an ordinal allocated by a
// read-then-write pair can be raced exactly as it can in production.
//
// The shared in-memory helper (testutil.OpenTestDB) cannot be used: it pins the
// pool to a single connection, which hides every write race by construction.
func openConcurrentTestDBs(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()

	dsn := "file:" + filepath.Join(t.TempDir(), "callbacks.db") +
		"?_busy_timeout=5000&_journal_mode=WAL"

	open := func() *gorm.DB {
		conn, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
		require.NoError(t, err)
		sqlDB, err := conn.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
		return conn
	}

	first := open()
	require.NoError(t, first.AutoMigrate(models.All...))
	return first, open()
}

// attemptBarrier releases every waiter once `size` of them have arrived, or
// after `grace` if one never does (so a failing delivery cannot hang the test).
type attemptBarrier struct {
	mu      sync.Mutex
	arrived int
	size    int
	grace   time.Duration
	release chan struct{}
	once    sync.Once
}

func newAttemptBarrier(size int, grace time.Duration) *attemptBarrier {
	return &attemptBarrier{size: size, grace: grace, release: make(chan struct{})}
}

func (b *attemptBarrier) arrive() {
	b.mu.Lock()
	b.arrived++
	full := b.arrived >= b.size
	b.mu.Unlock()

	if full {
		b.once.Do(func() { close(b.release) })
		return
	}
	select {
	case <-b.release:
	case <-time.After(b.grace):
	}
}

// TestRetryOrdinalsAreAllocatedAtomically is the maintainer's barrier-controlled
// reproduction from PR #460 (finding 3979788103).
//
// Two concurrent RetryFailed calls, on separate handles standing in for two
// server processes, are aligned at the instant each inserts its attempt row.
// While the ordinal was read by a COUNT that committed separately from the
// INSERT, both retries read one prior row and both persisted retry_count=1 —
// [0,1,1] for three actual deliveries, so the run detail page understated the
// delivery history. The ordinal is now produced by the INSERT itself, which
// evaluates its sub-select while holding the write lock, so the second insert
// cannot observe a pre-first-insert count.
func TestRetryOrdinalsAreAllocatedAtomically(t *testing.T) {
	dbA, dbB := openConcurrentTestDBs(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("still down"))
	}))
	defer server.Close()

	jobID, runID := seedCallbackRun(t, dbA, server.URL)

	dispatcherA := NewDispatcher(dbA)
	dispatcherA.WithHTTPClient(server.Client())
	dispatcherB := NewDispatcher(dbB)

	// Attempt 0: the ordinary run-completion dispatch.
	require.Error(t, dispatcherA.Dispatch(context.Background(), jobID, runID, nil))

	// Arm the barrier only now, so the uncontended first attempt does not sit
	// in it waiting for a partner that never comes.
	barrier := newAttemptBarrier(2, 5*time.Second)
	for _, conn := range []*gorm.DB{dbA, dbB} {
		require.NoError(t, conn.Callback().Create().Before("gorm:create").
			Register("test:attempt_barrier", func(tx *gorm.DB) {
				if tx.Statement.Table == "callback_runs" {
					barrier.arrive()
				}
			}))
	}

	var wg sync.WaitGroup
	for _, dispatcher := range []*Dispatcher{dispatcherA, dispatcherB} {
		wg.Add(1)
		go func(d *Dispatcher) {
			defer wg.Done()
			// The receiver is still down, so each retry fails; the assertion is
			// about the ordinal it recorded, not the delivery outcome.
			_ = d.RetryFailed(context.Background(), runID)
		}(dispatcher)
	}
	wg.Wait()

	rows := callbackRunRows(t, dbA, runID)
	require.Len(t, rows, 3, "each delivery records its own attempt row")

	counts := make([]int, 0, len(rows))
	for _, row := range rows {
		counts = append(counts, row.RetryCount)
	}
	require.ElementsMatch(t, []int{0, 1, 2}, counts,
		"concurrent retries must allocate distinct ordinals; got %v", counts)
}

// The postgres branch of the allocation lock cannot be reached from the sqlite
// harness above, so its statement — and the loud failure on a dialect with no
// known guarantee — are pinned directly. A dialect that silently allocated
// ordinals with neither a statement-level write lock nor a row lock would
// reintroduce the race this file exists to prevent.
func TestAttemptOrdinalLockSQLByDialect(t *testing.T) {
	stmt, err := attemptOrdinalLockSQL("postgres")
	require.NoError(t, err)
	require.Equal(t, "SELECT id FROM callbacks WHERE id = ? FOR UPDATE", stmt)

	for _, dialect := range []string{"dqlite", "sqlite", "sqlite3"} {
		stmt, err := attemptOrdinalLockSQL(dialect)
		require.NoError(t, err)
		require.Empty(t, stmt, "%s serialises writers already", dialect)
	}

	_, err = attemptOrdinalLockSQL("mysql")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported dialect")
}
