package run

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/dbretry"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
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

func runStatusOf(t *testing.T, db *gorm.DB, runID uuid.UUID) string {
	t.Helper()
	var row struct{ Status string }
	require.NoError(t, db.Table("job_runs").Select("status").Where("id = ?", runID).Scan(&row).Error)
	return row.Status
}

// TestRecoverCommittedRunReadsBackTheExactRun: a start that committed but
// failed its read-back is recovered by re-reading that run, which the caller
// then launches; nothing was finalized, so no callbacks fall to the caller.
func TestRecoverCommittedRunReadsBackTheExactRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	job := createNamespacedJob(t, db, "committed-transient", "quota-a", nil)
	faultRunReadBacks(t, db, 2, errors.New("database is locked"))

	_, err := store.Start(job.ID, nil)
	committedID, ok := CommittedRunID(err)
	require.True(t, ok, "the start committed: %v", err)

	rec, recoverErr := store.RecoverCommittedRun(t.Context(), err, []time.Duration{0, 0})
	require.NoError(t, recoverErr)
	require.True(t, rec.Committed)
	require.Equal(t, committedID, rec.Run.ID)
	require.Equal(t, StatusRunning, rec.Run.Status)
	require.False(t, rec.Finalized)
	require.False(t, rec.DispatchCallbacks())

	rec, recoverErr = store.RecoverCommittedRun(t.Context(), errors.New("not committed"), nil)
	require.NoError(t, recoverErr)
	require.False(t, rec.Committed)
}

// TestRecoverCommittedRunFinalizesAnUnreadableRun: when the run never reads
// back it is finalized as failed, so it frees its namespace slot (quota-a
// holds one) instead of holding it with nothing executing it; the caller is
// told it owns the completion callbacks, exactly once; and the error carries
// no contention text a retrier could mistake for a busy database.
func TestRecoverCommittedRunFinalizesAnUnreadableRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	job := createNamespacedJob(t, db, "committed-stuck", "quota-a", nil)
	disarm := faultRunReadBacks(t, db, -1, errors.New("database is locked"))

	_, err := store.Start(job.ID, nil)
	committedID, ok := CommittedRunID(err)
	require.True(t, ok)

	rec, recoverErr := store.RecoverCommittedRun(t.Context(), err, []time.Duration{0, 0})
	require.ErrorIs(t, recoverErr, ErrCommittedRunAbandoned)
	require.False(t, dbretry.IsLockContention(recoverErr), "a retrier must not start the request again")
	require.Nil(t, rec.Run)
	require.Equal(t, committedID, rec.RunID)
	require.Equal(t, job.ID, rec.JobID)
	require.True(t, rec.DispatchCallbacks(), "this call finalized it: the caller dispatches callbacks")
	require.Error(t, rec.Cause)
	require.Equal(t, string(StatusFailed), runStatusOf(t, db, committedID))
	require.Zero(t, runningIn(t, db, "quota-a"), "the namespace slot is free")

	again, _ := store.RecoverCommittedRun(t.Context(), err, nil)
	require.False(t, again.Finalized, "a second recovery finalizes nothing, so callbacks go out once")

	disarm()
	other := createNamespacedJob(t, db, "committed-next", "quota-a", nil)
	result, err := store.StartWithResult(t.Context(), other.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, result.Outcome)
}

// TestRecoverCommittedRunNeverDispatchesForAQuarantinedRun: a quarantined run
// is finalized like any other but never owes job callbacks.
func TestRecoverCommittedRunNeverDispatchesForAQuarantinedRun(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createNamespacedJob(t, db, "committed-quarantine", "default", nil)
	runID := seedNamespacedRun(t, db, job.ID, "default", StatusRunning, func(r *models.JobRun) { r.Quarantine = true })
	faultRunReadBacks(t, db, -1, errors.New("database is locked"))

	rec, recoverErr := store.RecoverCommittedRun(t.Context(),
		&RunCommittedError{RunID: runID, JobID: job.ID, Quarantine: true, Err: errors.New("database is locked")}, nil)
	require.ErrorIs(t, recoverErr, ErrCommittedRunAbandoned)
	require.True(t, rec.Finalized)
	require.False(t, rec.DispatchCallbacks())
	require.Equal(t, string(StatusFailed), runStatusOf(t, db, runID))
}
