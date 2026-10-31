package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var transactionDriverSequence atomic.Uint64

// Fail at the actual database/sql driver's transaction boundary. Returning a
// non-ErrBadConn error without ending SQLite's transaction reproduces dqlite's
// rejected COMMIT/ROLLBACK; injecting above sql.Tx would miss its pool release.
type transactionFaultState struct {
	mu          sync.Mutex
	nextID      int64
	fail        string
	failures    int
	err         error
	afterCommit bool
	calls       map[string]int
	closed      map[int64]bool
}

func (s *transactionFaultState) attempt(operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[operation]++
	if s.fail == operation && s.failures > 0 {
		s.failures--
		return s.err
	}
	return nil
}

func (s *transactionFaultState) arm(operation string, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail, s.failures = operation, failures
}

func (s *transactionFaultState) count(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[operation]
}

func (s *transactionFaultState) wasClosed(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed[id]
}

type transactionFaultDriver struct{ state *transactionFaultState }

func (d transactionFaultDriver) Open(name string) (driver.Conn, error) {
	raw, err := (&sqlite3.SQLiteDriver{}).Open(name)
	if err != nil {
		return nil, err
	}
	conn := raw.(*sqlite3.SQLiteConn)
	d.state.mu.Lock()
	d.state.nextID++
	id := d.state.nextID
	d.state.mu.Unlock()
	if err := conn.RegisterFunc("physical_connection", func() int64 { return id }, true); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &transactionFaultConn{SQLiteConn: conn, state: d.state, id: id}, nil
}

type transactionFaultConn struct {
	*sqlite3.SQLiteConn
	state *transactionFaultState
	id    int64
}

func (c *transactionFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.state.attempt("begin"); err != nil {
		return nil, err
	}
	tx, err := c.SQLiteConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &transactionFaultTx{Tx: tx, state: c.state}, nil
}

func (c *transactionFaultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.state.attempt("exec"); err != nil {
		return nil, err
	}
	return c.SQLiteConn.ExecContext(ctx, query, args)
}

func (c *transactionFaultConn) Close() error {
	c.state.mu.Lock()
	c.state.closed[c.id] = true
	c.state.mu.Unlock()
	return c.SQLiteConn.Close()
}

type transactionFaultTx struct {
	driver.Tx
	state *transactionFaultState
}

func (tx *transactionFaultTx) Commit() error {
	tx.state.mu.Lock()
	after := tx.state.afterCommit
	tx.state.mu.Unlock()
	err := tx.state.attempt("commit")
	if err != nil && !after {
		return err
	}
	if commitErr := tx.Tx.Commit(); commitErr != nil {
		return commitErr
	}
	return err
}

func (tx *transactionFaultTx) Rollback() error {
	if err := tx.state.attempt("rollback"); err != nil {
		return err
	}
	return tx.Tx.Rollback()
}

func faultSQLite(t *testing.T) (*gorm.DB, *sql.DB, *sql.DB, *transactionFaultState) {
	t.Helper()
	state := &transactionFaultState{err: errors.New("checkpoint in progress"), calls: map[string]int{}, closed: map[int64]bool{}}
	name := fmt.Sprintf("transaction-fault-%d", transactionDriverSequence.Add(1))
	sql.Register(name, transactionFaultDriver{state: state})
	dsn := "file:" + filepath.Join(t.TempDir(), "snapshot.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	writer, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	reader, err := sql.Open(name, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	reader.SetMaxOpenConns(1)
	reader.SetMaxIdleConns(1)
	_, err = writer.ExecContext(t.Context(), "CREATE TABLE recovery_rows (id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	_, err = writer.ExecContext(t.Context(), "INSERT INTO recovery_rows VALUES (1)")
	require.NoError(t, err)
	gdb, err := gorm.Open(sqlite.New(sqlite.Config{Conn: writer}), &gorm.Config{SkipDefaultTransaction: true})
	require.NoError(t, err)
	installConnPool(gdb, newRWSplitConnPool(writer, reader))
	return gdb, writer, reader, state
}

type snapshotObservation struct {
	RowCount     int
	ConnectionID int64
}

func observeSnapshot(t *testing.T, db *gorm.DB) snapshotObservation {
	t.Helper()
	var observed snapshotObservation
	require.NoError(t, db.Raw("SELECT count(*) AS row_count, physical_connection() AS connection_id FROM recovery_rows WHERE id = 2").Scan(&observed).Error)
	return observed
}

func TestFailedTransactionCompletionDiscardsStaleSnapshot(t *testing.T) {
	for _, operation := range []string{"rollback", "commit"} {
		t.Run(operation, func(t *testing.T) {
			gdb, writer, _, state := faultSQLite(t)
			tx := gdb.WithContext(t.Context()).Begin(&sql.TxOptions{ReadOnly: true})
			require.NoError(t, tx.Error)
			before := observeSnapshot(t, tx)
			require.Zero(t, before.RowCount)
			_, err := writer.ExecContext(t.Context(), "INSERT INTO recovery_rows VALUES (2)")
			require.NoError(t, err, "the independent writer commits while the reader holds its old WAL snapshot")
			state.arm(operation, 1)
			if operation == "commit" {
				err = tx.Commit().Error
			} else {
				err = tx.Rollback().Error
			}
			require.ErrorIs(t, err, state.err)
			after := observeSnapshot(t, gdb.WithContext(t.Context()))
			require.Equal(t, 1, after.RowCount, "stale snapshot reused: physical connection before=%d after=%d", before.ConnectionID, after.ConnectionID)
			require.NotEqual(t, before.ConnectionID, after.ConnectionID)
			require.True(t, state.wasClosed(before.ConnectionID), "the exact failed physical connection must be discarded")
			require.Equal(t, 1, state.count(operation), "transaction completion must never be replayed")
		})
	}
}

func TestSuccessfulTransactionCompletionReusesConnection(t *testing.T) {
	for _, operation := range []string{"rollback", "commit"} {
		t.Run(operation, func(t *testing.T) {
			gdb, writer, _, state := faultSQLite(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tx := gdb.WithContext(ctx).Begin(&sql.TxOptions{ReadOnly: true})
			require.NoError(t, tx.Error)
			before := observeSnapshot(t, tx)
			_, err := writer.ExecContext(t.Context(), "INSERT INTO recovery_rows VALUES (2)")
			require.NoError(t, err)
			completion := tx.Statement.ConnPool.(gorm.Tx)
			if operation == "commit" {
				err = completion.Commit()
			} else {
				err = completion.Rollback()
			}
			require.NoError(t, err)
			cancel() // A late cancellation must not touch a returned connection.
			require.ErrorIs(t, completion.Rollback(), sql.ErrTxDone)
			after := observeSnapshot(t, gdb.WithContext(t.Context()))
			require.Equal(t, 1, after.RowCount)
			require.Equal(t, before.ConnectionID, after.ConnectionID)
			require.False(t, state.wasClosed(before.ConnectionID))
			require.Equal(t, 1, state.count(operation))
		})
	}
}

func TestCanceledTransactionReleasesPinWithoutExplicitCompletion(t *testing.T) {
	gdb, writer, reader, _ := faultSQLite(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tx := gdb.WithContext(ctx).Begin(&sql.TxOptions{ReadOnly: true})
	require.NoError(t, tx.Error)
	observeSnapshot(t, tx)
	_, err := writer.ExecContext(t.Context(), "INSERT INTO recovery_rows VALUES (2)")
	require.NoError(t, err)
	require.Equal(t, 1, reader.Stats().InUse)
	cancel()
	require.Eventually(t, func() bool { return reader.Stats().InUse == 0 }, time.Second, time.Millisecond,
		"cancellation must release the separately acquired sql.Conn even without Commit/Rollback")
	require.Equal(t, 1, observeSnapshot(t, gdb.WithContext(t.Context())).RowCount)
}

// A request that ends during a snapshot: cancellation rolls the transaction
// back behind fn, so COMMIT answers sql.ErrTxDone whatever fn returned. That is
// neither contention nor a success, so it surfaces once and is not retried.
func TestCanceledSnapshotSurfacesTxDoneWithoutRetry(t *testing.T) {
	gdb, _, reader, _ := faultSQLite(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0
	err := TransactionOn(ctx, gdb, func(tx *gorm.DB) error {
		calls++
		observeSnapshot(t, tx)
		cancel()
		require.Eventually(t, func() bool { return reader.Stats().InUse == 0 }, time.Second, time.Millisecond,
			"the cancellation rollback completes before fn returns")
		return nil
	}, &sql.TxOptions{ReadOnly: true})

	require.ErrorIs(t, err, sql.ErrTxDone)
	require.Equal(t, 1, calls)
}

func TestUnknownCommitOutcomeIsNotReplayed(t *testing.T) {
	gdb, _, reader, state := faultSQLite(t)
	installConnPool(gdb, newRetryConnPool(reader))
	state.err = errors.New("commit reply unavailable")
	state.afterCommit = true
	state.arm("commit", 1)
	var originalID int64
	err := gdb.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		originalID = observeSnapshot(t, tx).ConnectionID
		return tx.Exec("INSERT INTO recovery_rows VALUES (2)").Error
	})
	require.ErrorIs(t, err, state.err, "preserve uncertainty even when the native commit actually succeeded")
	require.Equal(t, 1, state.count("commit"))
	require.Equal(t, 1, state.count("exec"), "neither the write nor completion may be replayed")
	require.Zero(t, state.count("rollback"), "GORM's deferred second completion must not reach a released connection")
	after := observeSnapshot(t, gdb.WithContext(t.Context()))
	require.Equal(t, 1, after.RowCount, "the committed row must survive the failed reply")
	require.NotEqual(t, originalID, after.ConnectionID)
	require.True(t, state.wasClosed(originalID))
}

func TestRetryPluginRetriesPhysicalBeginWithoutRetryingTransactionStatements(t *testing.T) {
	gdb, _, reader, state := faultSQLite(t)
	installConnPool(gdb, reader)
	require.NoError(t, gdb.Use(retryPlugin{}))
	state.err = errors.New("cannot start a transaction within a transaction")
	state.arm("begin", 1)
	err := gdb.WithContext(t.Context()).Transaction(func(tx *gorm.DB) error {
		require.IsType(t, &guardedTx{}, tx.Statement.ConnPool)
		state.err = errors.New("checkpoint in progress")
		state.arm("exec", 1)
		return tx.Exec("INSERT INTO recovery_rows VALUES (2)").Error
	})
	require.ErrorIs(t, err, state.err)
	require.Equal(t, 2, state.count("begin"))
	require.Equal(t, 1, state.count("exec"), "a failed transaction statement must not be retried individually")
	require.Equal(t, 1, state.count("rollback"))
	require.True(t, state.wasClosed(1), "failed BEGIN must discard only its own connection")
}

func TestCancellationRacingCompletionDoesNotDisposeReplacement(t *testing.T) {
	for _, operation := range []string{"rollback", "commit"} {
		t.Run(operation, func(t *testing.T) {
			gdb, _, reader, state := faultSQLite(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tx := gdb.WithContext(ctx).Begin(&sql.TxOptions{ReadOnly: true})
			require.NoError(t, tx.Error)
			observeSnapshot(t, tx)
			completion := tx.Statement.ConnPool.(gorm.Tx)
			start := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				<-start
				if operation == "commit" {
					finished <- completion.Commit()
				} else {
					finished <- completion.Rollback()
				}
			}()
			close(start)
			cancel()
			select {
			case err := <-finished:
				require.True(t, err == nil || errors.Is(err, sql.ErrTxDone) || errors.Is(err, context.Canceled), "unexpected completion error: %v", err)
			case <-time.After(time.Second):
				t.Fatal("cancellation/completion deadlocked")
			}
			require.Eventually(t, func() bool { return reader.Stats().InUse == 0 }, time.Second, time.Millisecond)
			replacement := observeSnapshot(t, gdb.WithContext(t.Context()))
			require.ErrorIs(t, completion.Rollback(), sql.ErrTxDone)
			require.Equal(t, replacement.ConnectionID, observeSnapshot(t, gdb.WithContext(t.Context())).ConnectionID)
			require.False(t, state.wasClosed(replacement.ConnectionID))
		})
	}
}
