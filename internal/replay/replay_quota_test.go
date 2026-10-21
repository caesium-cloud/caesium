package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestReplayConsumesTheNamespaceQuota: a quarantined replay that will execute
// is real compute — at its namespace's cap it is refused with nothing written;
// an all-cached replay takes no slot; once a slot frees the replay runs.
func TestReplayConsumesTheNamespaceQuota(t *testing.T) {
	f := newReplayFixture(t)
	f.seedTask(t, seedTaskConfig{name: "safe", replaySafe: true, result: "success"})

	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  default:\n    quotas:\n      maxConcurrentRuns: 1\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)

	occupant := &models.Job{ID: uuid.New(), Alias: "replay-quota-occupant"}
	require.NoError(t, f.db.Create(occupant).Error)
	occupantRun := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, f.db.Create(&models.JobRun{
		ID: occupantRun, JobID: occupant.ID, Namespace: models.DefaultNamespace, Status: string(run.StatusRunning),
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)
	before := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace)
	countReplays := func() int64 {
		var n int64
		require.NoError(t, f.db.Model(&models.JobRun{}).Where("job_id = ? AND quarantine = ?", f.jobID, true).Count(&n).Error)
		return n
	}

	_, err = New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.ErrorIs(t, err, run.ErrNamespaceQuota)
	require.Zero(t, countReplays(), "a refused replay writes nothing")
	require.Equal(t, before, metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, models.DefaultNamespace),
		"the replay service counts a deferral once the refusal stands, not the constructor")

	cached, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err, "an all-cached replay executes nothing and takes no slot")
	require.Equal(t, run.StatusSucceeded, cached.Run.Status)

	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", occupantRun).Update("status", string(run.StatusSucceeded)).Error)
	executed, err := New(f.store, &recordingDispatcher{}).Replay(context.Background(), Request{
		BaselineRunID: f.runID,
		Set:           map[string]string{"mode": "what-if"},
	})
	require.NoError(t, err)
	require.Equal(t, run.StatusRunning, executed.Run.Status)
}
