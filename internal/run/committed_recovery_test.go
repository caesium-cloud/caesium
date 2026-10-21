package run

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/dbretry"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// faultRunReadBacks fails the first n detached run read-backs (the joined
// job_runs load outside a transaction) with fault; n < 0 fails every one
// until the returned disarm is called. Admission's own reads run inside its
// transaction and are untouched.
func faultRunReadBacks(t *testing.T, db *gorm.DB, n int, fault error) (disarm func()) {
	t.Helper()
	var reads atomic.Int64
	var off atomic.Bool
	const callback = "test:fault_run_read_back"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if off.Load() || tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || transaction {
			return
		}
		if seen := reads.Add(1); n < 0 || seen <= int64(n) {
			_ = tx.AddError(fault)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	return func() { off.Store(true) }
}

// TestRecoverCommittedRunReadsBackTheExactRun: a start that committed but
// failed its read-back is recovered by re-reading that run, which the caller
// then launches.
func TestRecoverCommittedRunReadsBackTheExactRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	job := createNamespacedJob(t, db, "committed-transient", "quota-a", nil)
	faultRunReadBacks(t, db, 2, errors.New("database is locked"))

	_, err := store.Start(job.ID, nil)
	committedID, ok := CommittedRunID(err)
	require.True(t, ok, "the start committed: %v", err)

	adopted, committed, recoverErr := store.RecoverCommittedRun(t.Context(), err, []time.Duration{0, 0})
	require.True(t, committed)
	require.NoError(t, recoverErr)
	require.Equal(t, committedID, adopted.ID)
	require.Equal(t, StatusRunning, adopted.Status)

	_, committed, recoverErr = store.RecoverCommittedRun(t.Context(), errors.New("not committed"), nil)
	require.False(t, committed)
	require.NoError(t, recoverErr)
}

// TestRecoverCommittedRunFinalizesAnUnreadableRun: when the run never reads
// back it is finalized as failed, so it frees its namespace slot (quota-a
// holds one) instead of holding it with nothing executing it, and the error
// carries no contention text a retrier could mistake for a busy database.
func TestRecoverCommittedRunFinalizesAnUnreadableRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	job := createNamespacedJob(t, db, "committed-stuck", "quota-a", nil)
	disarm := faultRunReadBacks(t, db, -1, errors.New("database is locked"))

	_, err := store.Start(job.ID, nil)
	committedID, ok := CommittedRunID(err)
	require.True(t, ok)

	adopted, committed, recoverErr := store.RecoverCommittedRun(t.Context(), err, []time.Duration{0, 0})
	require.True(t, committed)
	require.Nil(t, adopted)
	require.ErrorIs(t, recoverErr, ErrCommittedRunAbandoned)
	require.False(t, dbretry.IsLockContention(recoverErr), "a retrier must not start the request again")

	var row struct{ Status string }
	require.NoError(t, db.Table("job_runs").Select("status").Where("id = ?", committedID).Scan(&row).Error)
	require.Equal(t, string(StatusFailed), row.Status)
	require.Zero(t, runningIn(t, db, "quota-a"), "the namespace slot is free")

	disarm()
	other := createNamespacedJob(t, db, "committed-next", "quota-a", nil)
	result, err := store.StartWithResult(t.Context(), other.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, result.Outcome)
}
