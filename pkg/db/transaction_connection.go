package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"

	"gorm.io/gorm"
)

// guardedTx retains its exact physical connection until transaction completion
// has either succeeded or discarded it. database/sql alone can pool a connection
// after a non-ErrBadConn COMMIT/ROLLBACK error, leaving its SQLite snapshot open.
// Embedding the raw Tx keeps every statement outside the pool's retry decorator.
type guardedTx struct {
	*sql.Tx
	conn *sql.Conn
	mu   sync.Mutex
	done bool
	stop func() bool
}

var _ gorm.Tx = (*guardedTx)(nil)

func beginGuardedTransaction(ctx context.Context, pool *sql.DB, opts *sql.TxOptions) (gorm.ConnPool, error) {
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		// BEGIN may have found an already-active transaction, or its reply
		// may be uncertain. Discard this connection, never a random pool member.
		discardTransactionConnection(conn)
		return nil, err
	}
	guard := &guardedTx{Tx: tx, conn: conn}
	guard.mu.Lock()
	// database/sql may roll back on cancellation without a caller completing
	// this wrapper. Release the separately acquired pin in that case as well.
	guard.stop = context.AfterFunc(ctx, func() { _ = guard.Rollback() })
	guard.mu.Unlock()
	return guard, nil
}

func (tx *guardedTx) Commit() error { return tx.finish(tx.Tx.Commit) }

func (tx *guardedTx) Rollback() error { return tx.finish(tx.Tx.Rollback) }

func (tx *guardedTx) finish(complete func() error) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return sql.ErrTxDone
	}
	tx.done = true
	tx.stop()
	err := complete()
	if err != nil {
		// This includes ErrTxDone when database/sql's cancellation rollback
		// won the race. Its outcome is unknown here, so never reuse the pin.
		discardTransactionConnection(tx.conn)
	} else {
		_ = tx.conn.Close()
	}
	return err // Preserve the original result; never replay completion.
}

func discardTransactionConnection(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
