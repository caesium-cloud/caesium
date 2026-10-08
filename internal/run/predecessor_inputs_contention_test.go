package run

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Every attempt still uses a real SQL transaction. The first commit refuses
// before committing, after all projections have been built; GORM must roll it
// back before a new acquisition observes the changed fixture.
type predecessorContentionPool struct {
	*sql.DB
	failure       error
	failures      int
	begins        int
	commits       int
	rollbacks     int
	options       []*sql.TxOptions
	afterRollback func()
}

func (p *predecessorContentionPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	if opts != nil {
		copied := *opts
		p.options = append(p.options, &copied)
	} else {
		p.options = append(p.options, nil)
	}
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	p.begins++
	return &predecessorContentionTx{Tx: tx, pool: p}, nil
}

func TestPredecessorExecutionInputsRequestsReadOnlySnapshot(t *testing.T) {
	f := newFanOutFixture(t, nil)
	store, pool := predecessorContentionStore(t, f, nil, 0)
	_, err := store.PredecessorExecutionInputs(t.Context(), f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Len(t, pool.options, 1)
	require.Equal(t, &sql.TxOptions{ReadOnly: true}, pool.options[0], "dqlite/SQLite snapshot must route through the read pool")
	require.Zero(t, pool.Stats().InUse)
}

type predecessorContentionTx struct {
	*sql.Tx
	pool *predecessorContentionPool
}

func (tx *predecessorContentionTx) Commit() error {
	tx.pool.commits++
	if tx.pool.commits <= tx.pool.failures {
		return tx.pool.failure
	}
	return tx.Tx.Commit()
}

func (tx *predecessorContentionTx) Rollback() error {
	err := tx.Tx.Rollback()
	if err == nil {
		tx.pool.rollbacks++
		if tx.pool.afterRollback != nil {
			tx.pool.afterRollback()
		}
	}
	return err
}

func predecessorContentionStore(t *testing.T, f *fanOutFixture, failure error, failures int) (*Store, *predecessorContentionPool) {
	t.Helper()
	sqlDB, err := f.db.DB()
	require.NoError(t, err)
	pool := &predecessorContentionPool{DB: sqlDB, failure: failure, failures: failures}
	gdb := f.db.Session(&gorm.Session{NewDB: true}).WithContext(t.Context())
	gdb.Statement.ConnPool = pool
	return NewStore(gdb), pool
}

func TestPredecessorExecutionInputsContentionReacquiresWithoutLeakingProjection(t *testing.T) {
	for _, change := range []string{"new values", "no edges"} {
		t.Run(change, func(t *testing.T) {
			f := newFanOutFixture(t, nil)
			row := f.producerRow(t)
			require.NoError(t, f.db.Model(row).Updates(map[string]any{
				"output": datatypes.JSON(`{"value":"discard"}`), "hash": "discard-hash",
				"status": string(TaskStatusSucceeded), "partition_count": 1, "partition_value": "discard-partition",
			}).Error)
			store, pool := predecessorContentionStore(t, f, errors.New("checkpoint in progress"), 1)
			pool.afterRollback = func() {
				require.Equal(t, 1, pool.begins, "rollback precedes the next transaction")
				if change == "no edges" {
					require.NoError(t, f.db.Where("to_task_id = ?", f.consumer.ID).Delete(&models.TaskEdge{}).Error)
					return
				}
				require.NoError(t, f.db.Model(row).Updates(map[string]any{
					"output": datatypes.JSON(`{"value":"kept"}`), "hash": "kept-hash",
					"partition_count": 0, "partition_value": "",
				}).Error)
			}
			got, err := store.PredecessorExecutionInputs(t.Context(), f.runID, f.consumer.ID)
			require.NoError(t, err)
			require.Equal(t, 2, pool.begins)
			require.Equal(t, 2, pool.commits)
			require.Equal(t, 1, pool.rollbacks)
			require.Zero(t, pool.Stats().InUse)
			if change == "no edges" {
				require.Equal(t, PredecessorInputs{}, got, "no failed-attempt maps/hashes/counts may survive")
				return
			}
			want, err := f.store.PredecessorExecutionInputs(t.Context(), f.runID, f.consumer.ID)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.Equal(t, map[string]map[string]string{"discover": {"value": "kept"}}, got.OutputsByName)
			require.Equal(t, []string{"kept-hash"}, got.Hashes)
			require.Equal(t, map[string]string{"value": "kept"}, got.DescriptorOutputs[f.producer.ID])
			require.Equal(t, "kept-hash", got.DescriptorHashes[f.producer.ID])
		})
	}
}

func TestPredecessorExecutionInputsContentionRefusalsReturnZero(t *testing.T) {
	for _, scenario := range []string{"permanent", "exhausted", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFanOutFixture(t, nil)
			require.NoError(t, f.db.Model(f.producerRow(t)).Updates(map[string]any{
				"output": datatypes.JSON(`{"value":"uncommitted"}`), "hash": "uncommitted-hash",
				"status": string(TaskStatusSucceeded),
			}).Error)
			failure := errors.New("database is locked")
			if scenario == "permanent" {
				failure = errors.New("permanent input commit failure")
			}
			store, pool := predecessorContentionStore(t, f, failure, len(storeBusyRetryBackoffs)+1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "canceled" {
				pool.afterRollback = cancel
			}
			got, err := store.PredecessorExecutionInputs(ctx, f.runID, f.consumer.ID)
			if scenario == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.Equal(t, PredecessorInputs{}, got)
			attempts := 1
			if scenario == "exhausted" {
				attempts += len(storeBusyRetryBackoffs)
			}
			require.Equal(t, attempts, pool.begins)
			require.Equal(t, attempts, pool.commits)
			require.Equal(t, attempts, pool.rollbacks)
			require.Zero(t, pool.Stats().InUse)
		})
	}
}
