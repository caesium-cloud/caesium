package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestResourceOutcomeInstanceIdentityRetryAndClaimFences(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, taskID := uuid.New(), uuid.New()
	run, err := store.Start(jobID, nil)
	require.NoError(t, err)
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: run.ID, TaskID: taskID, AtomID: uuid.New(), Engine: models.AtomEngineDocker, Status: "running", RuntimeID: "first", Attempt: 1, MaxAttempts: 2, PartitionIndex: 0, PartitionCount: 2, PartitionValue: "a"},
		{ID: uuid.New(), JobRunID: run.ID, TaskID: taskID, AtomID: uuid.New(), Engine: models.AtomEngineDocker, Status: "running", RuntimeID: "second", Attempt: 1, MaxAttempts: 2, PartitionIndex: 1, PartitionCount: 2, PartitionValue: "b"},
	}
	require.NoError(t, db.Create(&rows).Error)
	out := TaskResourceOutcome{ExitCode: new(137), RuntimeID: "first", Attempt: 1, ResourceSummary: atom.ResourceSummary{PeakMemoryBytes: new(int64(64)), CPUSeconds: new(1.5), OOMKilled: true, StatsSource: "oom_inferred"}}
	require.ErrorIs(t, store.SetTaskResourceOutcome(run.ID, taskID, out), ErrAmbiguousTaskRun)
	require.NoError(t, store.SetTaskResourceOutcome(run.ID, rows[0].ID, out))
	require.NoError(t, store.SetTaskResourceOutcome(run.ID, rows[0].ID, out), "duplicate completion is idempotent")
	other := out
	other.RuntimeID = "second"
	other.PeakMemoryBytes = new(int64(16))
	other.OOMKilled = false
	other.StatsSource = "sampled"
	require.NoError(t, store.SetTaskResourceOutcome(run.ID, rows[1].ID, other))
	var first, second models.TaskRun
	require.NoError(t, db.First(&first, "id = ?", rows[0].ID).Error)
	require.NoError(t, db.First(&second, "id = ?", rows[1].ID).Error)
	require.Equal(t, int64(64), *first.PeakMemoryBytes)
	require.Equal(t, int64(16), *second.PeakMemoryBytes)
	require.False(t, second.OOMKilled)
	labels := []string{jobID.String(), taskID.String(), string(models.AtomEngineDocker)}
	require.Equal(t, 1.0, metrictestutil.CounterValue(t, metrics.TaskOOMKillsTotal, labels...))
	require.Equal(t, uint64(2), metrictestutil.HistogramSampleCount(t, metrics.TaskMemoryPeakBytes, labels...))
	require.Equal(t, 3.0, metrictestutil.CounterValue(t, metrics.TaskCPUSecondsTotal, labels...))
	view := convertRunTaskModel(&first)
	require.Equal(t, first.PeakMemoryBytes, view.PeakMemoryBytes)
	require.True(t, view.OOMKilled)

	// Retry clears old observations; a delayed prior runtime cannot restore them.
	require.NoError(t, store.RetryTask(run.ID, rows[0].ID, 2))
	require.NoError(t, store.StartTask(run.ID, rows[0].ID, "replacement"))
	require.NoError(t, store.SetTaskResourceOutcome(run.ID, rows[0].ID, out))
	require.NoError(t, db.First(&first, "id = ?", rows[0].ID).Error)
	require.Nil(t, first.PeakMemoryBytes)
	require.Nil(t, first.CPUSeconds)
	require.False(t, first.OOMKilled)
	require.Empty(t, first.StatsSource)
	require.Nil(t, first.ExitCode)

	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", rows[0].ID).Updates(map[string]any{"claimed_by": "new-worker", "claim_attempt": 2}).Error)
	out.RuntimeID = "replacement"
	out.Attempt = 2
	out.ClaimedBy = "old-worker"
	out.ClaimAttempt = 1
	require.NoError(t, store.SetTaskResourceOutcome(run.ID, rows[0].ID, out))
	require.NoError(t, db.First(&first, "id = ?", rows[0].ID).Error)
	require.Empty(t, first.StatsSource)
	out.ClaimedBy = "new-worker"
	out.ClaimAttempt = 2
	require.NoError(t, store.SetTaskResourceOutcome(run.ID, rows[0].ID, out))
	require.NoError(t, db.First(&first, "id = ?", rows[0].ID).Error)
	require.True(t, first.OOMKilled)
}

func TestResourceOutcomeGateOffDoesNotWrite(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "false")
	require.NoError(t, env.Process())
	// Even absent identities/database are inert with the gate off.
	require.NoError(t, (&Store{}).SetTaskResourceOutcome(uuid.New(), uuid.New(), TaskResourceOutcome{}))
}

func TestResourceOutcomeQuarantinePersistsWithoutProductionMetrics(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	for _, taskQuarantine := range []bool{false, true} {
		for _, runQuarantine := range []bool{false, true} {
			t.Run(fmt.Sprintf("task=%t/run=%t", taskQuarantine, runQuarantine), func(t *testing.T) {
				db := testutil.OpenTestDB(t)
				t.Cleanup(func() { testutil.CloseDB(db) })
				store := NewStore(db)
				jr, err := store.Start(uuid.New(), nil)
				require.NoError(t, err)
				require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", jr.ID).Update("quarantine", runQuarantine).Error)
				row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), AtomID: uuid.New(), Engine: models.AtomEngineDocker, Status: "running", RuntimeID: "runtime", Attempt: 1, Quarantine: taskQuarantine}
				require.NoError(t, db.Create(&row).Error)
				outcome := TaskResourceOutcome{RuntimeID: row.RuntimeID, Attempt: 1, ExitCode: new(137), ResourceSummary: atom.ResourceSummary{PeakMemoryBytes: new(int64(64)), CPUSeconds: new(2.0), OOMKilled: true, StatsSource: "oom_inferred"}}
				for range 2 {
					require.NoError(t, store.SetTaskResourceOutcome(jr.ID, row.ID, outcome))
				}
				var persisted models.TaskRun
				require.NoError(t, db.First(&persisted, "id = ?", row.ID).Error)
				require.True(t, persisted.OOMKilled)
				require.Equal(t, int64(64), *persisted.PeakMemoryBytes)
				require.Equal(t, 2.0, *persisted.CPUSeconds)
				require.Equal(t, 137, *persisted.ExitCode)
				require.Equal(t, "oom_inferred", persisted.StatsSource)
				labels := []string{jr.JobID.String(), row.TaskID.String(), string(row.Engine)}
				count := uint64(1)
				if taskQuarantine || runQuarantine {
					count = 0
				}
				require.Equal(t, float64(count), metrictestutil.CounterValue(t, metrics.TaskOOMKillsTotal, labels...))
				require.Equal(t, count, metrictestutil.HistogramSampleCount(t, metrics.TaskMemoryPeakBytes, labels...))
				require.Equal(t, float64(count)*2, metrictestutil.CounterValue(t, metrics.TaskCPUSecondsTotal, labels...))
			})
		}
	}
}

func TestResourceOutcomeReclaimClearsEvidenceAndAdmitsReplacement(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	require.NoError(t, env.Process())
	for _, owner := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", owner), func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			jr, err := store.Start(uuid.New(), nil)
			require.NoError(t, err)
			row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), AtomID: uuid.New(), Engine: models.AtomEngineDocker, Status: "running", RuntimeID: "lost", Attempt: 2, MaxAttempts: 3, ClaimedBy: "old-worker", ClaimAttempt: 1, ClaimExpiresAt: new(time.Now().Add(-time.Minute)), OwnerGeneration: 3}
			require.NoError(t, db.Create(&row).Error)
			outcome := TaskResourceOutcome{RuntimeID: row.RuntimeID, Attempt: row.Attempt, ClaimedBy: row.ClaimedBy, ClaimAttempt: row.ClaimAttempt, ExitCode: new(137), ResourceSummary: atom.ResourceSummary{PeakMemoryBytes: new(int64(64)), CPUSeconds: new(2.0), OOMKilled: true, StatsSource: "oom_inferred"}}
			require.NoError(t, store.SetTaskResourceOutcome(jr.ID, row.ID, outcome))
			// A crash after this write must not carry either observations or a
			// previous runtime's applied sizing into the new container.
			require.NoError(t, db.Model(&row).Updates(map[string]any{"applied_resources": `{"memoryBytes":64}`, "escalation_level": 2}).Error)
			var completed models.TaskRun
			require.NoError(t, db.First(&completed, "id = ?", row.ID).Error)
			completed.ID, completed.TaskID = uuid.New(), uuid.New()
			completed.Status = "succeeded"
			require.NoError(t, db.Create(&completed).Error)
			if owner {
				stale, err := store.ReclaimOwnerExpiredClaims(jr.ID, 2)
				require.NoError(t, err)
				require.Empty(t, stale)
				var unchanged models.TaskRun
				require.NoError(t, db.First(&unchanged, "id = ?", row.ID).Error)
				require.True(t, unchanged.OOMKilled, "stale owner must not clear a newer owner's evidence")
				reset, err := store.ReclaimOwnerExpiredClaims(jr.ID, 3)
				require.NoError(t, err)
				require.Len(t, reset, 1)
			} else {
				require.NoError(t, store.ResetInFlightTasks(jr.ID))
			}
			var reset models.TaskRun
			require.NoError(t, db.First(&reset, "id = ?", row.ID).Error)
			require.Equal(t, "pending", reset.Status)
			require.Equal(t, row.Attempt, reset.Attempt, "failover does not consume retry policy attempts")
			require.Nil(t, reset.ExitCode)
			require.Nil(t, reset.PeakMemoryBytes)
			require.Nil(t, reset.CPUSeconds)
			require.Empty(t, reset.StatsSource)
			require.False(t, reset.OOMKilled)
			require.Empty(t, reset.AppliedResources)
			require.Zero(t, reset.EscalationLevel)
			var preserved models.TaskRun
			require.NoError(t, db.First(&preserved, "id = ?", completed.ID).Error)
			require.Equal(t, "succeeded", preserved.Status)
			require.True(t, preserved.OOMKilled, "a completed sibling's observations survive reclaim")

			require.NoError(t, store.ClaimTaskForDispatch(jr.ID, row.ID, "new-worker", 3, time.Minute, true))
			require.NoError(t, store.StartTaskClaimed(jr.ID, row.ID, "replacement", "new-worker"))
			require.NoError(t, store.SetTaskResourceOutcome(jr.ID, row.ID, outcome))
			reset = models.TaskRun{}
			require.NoError(t, db.First(&reset, "id = ?", row.ID).Error)
			require.Empty(t, reset.StatsSource, "late lost-runtime completion is fenced")
			next := TaskResourceOutcome{RuntimeID: "replacement", Attempt: reset.Attempt, ClaimedBy: reset.ClaimedBy, ClaimAttempt: reset.ClaimAttempt, ExitCode: new(0), ResourceSummary: atom.ResourceSummary{PeakMemoryBytes: new(int64(16)), CPUSeconds: new(0.5), StatsSource: "sampled"}}
			require.NoError(t, store.SetTaskResourceOutcome(jr.ID, row.ID, next))
			reset = models.TaskRun{}
			require.NoError(t, db.First(&reset, "id = ?", row.ID).Error)
			require.Equal(t, "sampled", reset.StatsSource)
			require.Equal(t, int64(16), *reset.PeakMemoryBytes)
			require.Equal(t, 0, *reset.ExitCode)
			require.False(t, reset.OOMKilled)
		})
	}
}
