package job

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// faultStartReadBacks fails the first n detached run read-backs (n < 0: all).
func faultStartReadBacks(t *testing.T, db *gorm.DB, n int) {
	t.Helper()
	var reads atomic.Int64
	const callback = "test:fault_start_read_back"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || transaction {
			return
		}
		if seen := reads.Add(1); n < 0 || seen <= int64(n) {
			_ = tx.AddError(errors.New("database is locked"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
}

// TestAdoptCommittedStartExecutesOrFinalizesTheCommittedRun covers the
// trigger path (cron, HTTP, event → job.Run → AdmitRun/Start): a start whose
// admission committed but whose read-back failed used to return the error,
// leaving the run running with nothing executing it — and, because the read
// error reads as contention, retryOnContentionDo would then start the request
// again. Now the exact run is executed once it reads back, or finalized, and
// the error never looks like contention.
func TestAdoptCommittedStartExecutesOrFinalizesTheCommittedRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults int
	}{{"reads back", 2}, {"never reads back", -1}} {
		t.Run(tc.name, func(t *testing.T) {
			withFastRunStartBackoffs(t, 2)
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := run.NewStore(db)
			job := &models.Job{ID: uuid.New(), Alias: "adopt-" + uuid.NewString()[:8]}
			require.NoError(t, db.Create(job).Error)
			faultStartReadBacks(t, db, tc.faults)

			started, err := store.Start(job.ID, nil)
			committedID, ok := run.CommittedRunID(err)
			require.True(t, ok, "the start committed: %v", err)
			require.True(t, dqlite.IsContentionError(err), "the raw error reads as contention")

			adopted, err := adoptCommittedStart(t.Context(), store, started, err)
			var status string
			require.NoError(t, db.Table("job_runs").Select("status").Where("id = ?", committedID).Row().Scan(&status))
			if tc.faults < 0 {
				require.ErrorIs(t, err, run.ErrCommittedRunAbandoned)
				require.False(t, dqlite.IsContentionError(err), "retryOnContentionDo must not start it again")
				require.Nil(t, adopted)
				require.Equal(t, string(run.StatusFailed), status)
			} else {
				require.NoError(t, err)
				require.Equal(t, committedID, adopted.ID)
				require.Equal(t, string(run.StatusRunning), status)
			}
			var runs int64
			require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", job.ID).Count(&runs).Error)
			require.Equal(t, int64(1), runs)
		})
	}
}
