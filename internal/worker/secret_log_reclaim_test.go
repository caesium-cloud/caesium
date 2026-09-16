package worker

import (
	"context"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

func TestSecretLogsWorkerReclaimClearsSnapshotBeforeRedispatch(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	now := time.Now().UTC()
	expired := seedTaskRun(t, db, seedTaskRunInput{
		status: string(run.TaskStatusRunning), claimedBy: "worker-old",
		claimExpiresAt: new(now.Add(-time.Minute)), claimAttempt: 3,
		createdAt: now.Add(-2 * time.Minute),
	})
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", expired.ID).Updates(map[string]any{
		"log_scrubbed": true, "log_text": "old sanitized snapshot", "log_truncated": true,
	}).Error)

	claimer := NewClaimer("worker-new", run.NewStore(db), time.Minute)
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	var reclaimed models.TaskRun
	require.NoError(t, db.First(&reclaimed, "id = ?", expired.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), reclaimed.Status)
	require.Empty(t, reclaimed.ClaimedBy)
	require.Equal(t, 3, reclaimed.ClaimAttempt, "reclaim itself must not count as a claim")
	require.Empty(t, reclaimed.LogText)
	require.False(t, reclaimed.LogTruncated)
}
