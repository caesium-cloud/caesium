package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// postgresReclaimRenewalWait bounds how long the reclaim's read seam waits for
// a concurrent claim renewal to commit. A renewal that is not blocked commits
// in milliseconds; one blocked by the reclaim's row locks never commits inside
// the window, and the reclaim then proceeds.
const postgresReclaimRenewalWait = 2 * time.Second

// renewClaimConcurrently starts a worker's claim renewal on its own pooled
// connection, outside the reclaim's transaction, and waits up to
// postgresReclaimRenewalWait for it to commit. It returns the renewal's result
// channel so the caller can collect the row count once the reclaim commits.
func renewClaimConcurrently(t *testing.T, db *gorm.DB, taskRunID uuid.UUID, claimedBy string) <-chan int64 {
	t.Helper()
	done := make(chan int64, 1)
	go func() {
		res := db.Model(&models.TaskRun{}).
			Where("id = ? AND claimed_by = ?", taskRunID, claimedBy).
			Update("claim_expires_at", time.Now().UTC().Add(time.Hour))
		if res.Error != nil {
			done <- -1
			return
		}
		done <- res.RowsAffected
	}()
	out := make(chan int64, 1)
	select {
	case n := <-done:
		out <- n
	case <-time.After(postgresReclaimRenewalWait):
		go func() { out <- <-done }()
	}
	return out
}

// seedPostgresExpiredClaims seeds a running run with two in-flight rows whose
// worker claims lapsed a minute ago, both stamped by owner generation 1.
func seedPostgresExpiredClaims(t *testing.T, db *gorm.DB, claimedBy string) (uuid.UUID, []models.TaskRun) {
	t.Helper()
	fixture := seedPostgresDeadlineFixture(t, db, "reclaim-"+uuid.NewString()[:8])
	lapsed := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ?", fixture.runID).
		Updates(map[string]any{
			"status":           string(TaskStatusRunning),
			"claimed_by":       claimedBy,
			"claim_expires_at": lapsed,
			"owner_generation": 1,
		}).Error)
	var rows []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ?", fixture.runID).Order("id").Find(&rows).Error)
	require.Len(t, rows, 2)
	return fixture.runID, rows
}

// On PostgreSQL READ COMMITTED a worker's renewal can commit between the
// owner reap's read and its guarded UPDATE. The UPDATE then skips the renewed
// row, so the reap must not report it, nor publish a lease-expiry event for it:
// the in-memory owner requeues every row it is told about and would dispatch a
// task whose attempt is still running.
func TestPostgresReclaimOwnerExpiredClaimsReportsOnlyTheRowsItReset(t *testing.T) {
	db := openDeadlinePostgres(t)
	require.NoError(t, db.AutoMigrate(&models.DatasetMetric{}))
	store := NewStore(db)
	const worker = "postgres-reclaim-worker"
	runID, rows := seedPostgresExpiredClaims(t, db, worker)
	renewed := rows[0]

	var renewal <-chan int64
	store.afterRecoveryRowsRead = func(*gorm.DB) {
		if renewal == nil {
			renewal = renewClaimConcurrently(t, db, renewed.ID, worker)
		}
	}

	reported, err := store.ReclaimOwnerExpiredClaims(runID, 1)
	require.NoError(t, err)
	require.NotNil(t, renewal, "the seam must have run")
	renewedRows := <-renewal
	require.GreaterOrEqual(t, renewedRows, int64(0), "the renewal must not error")

	var pending []uuid.UUID
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", runID, string(TaskStatusPending)).
		Order("id").Pluck("id", &pending).Error)
	reportedIDs := make([]uuid.UUID, 0, len(reported))
	for _, row := range reported {
		reportedIDs = append(reportedIDs, row.ID)
	}
	require.ElementsMatch(t, pending, reportedIDs, "the reap reports exactly the rows it re-pended")

	var expiredEvents int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("run_id = ? AND type = ?", runID, string(event.TypeTaskLeaseExpired)).
		Count(&expiredEvents).Error)
	require.Equal(t, int64(len(pending)), expiredEvents, "one lease-expiry event per row the reap re-pended")

	if renewedRows == 1 {
		row := taskRunFor(t, db, runID, renewed.TaskID)
		require.Equal(t, string(TaskStatusRunning), row.Status, "a renewal that won keeps its attempt")
	}
}
