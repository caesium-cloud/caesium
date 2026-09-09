package runqueue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestDequeuerLeaderGatedPriorityDrain(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	job := createQueuedJob(t, db)
	now := time.Now().UTC()
	lowID := uuid.New()
	highID := uuid.New()
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        lowID,
		JobID:     job.ID,
		Priority:  runstorage.PriorityLowValue,
		CreatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        highID,
		JobID:     job.ID,
		Priority:  runstorage.PriorityHighValue,
		CreatedAt: now.Add(time.Second),
	}).Error)

	store := runstorage.NewStore(db)
	var launchedRunID uuid.UUID
	var launchedJobID uuid.UUID
	dequeuer := NewDequeuer(Config{
		DB:     db,
		Store:  store,
		NodeID: "node-a",
		LeaderCheck: func(context.Context) (bool, error) {
			return true, nil
		},
		LaunchRun: func(_ context.Context, jobModel *models.Job, started *runstorage.JobRun) {
			launchedJobID = jobModel.ID
			launchedRunID = started.ID
		},
	})
	require.NoError(t, dequeuer.DrainOnce(context.Background()))

	var runs []models.JobRun
	require.NoError(t, db.Find(&runs, "job_id = ?", job.ID).Error)
	require.Len(t, runs, 1)
	require.Equal(t, runstorage.PriorityHighValue, runs[0].Priority)
	require.Equal(t, job.ID, launchedJobID)
	require.Equal(t, runs[0].ID, launchedRunID)

	var remaining []models.RunQueue
	require.NoError(t, db.Find(&remaining, "job_id = ?", job.ID).Error)
	require.Len(t, remaining, 1)
	require.Equal(t, lowID, remaining[0].ID)

	follower := NewDequeuer(Config{
		DB:     db,
		Store:  store,
		NodeID: "node-b",
		LeaderCheck: func(context.Context) (bool, error) {
			return false, nil
		},
	})
	require.NoError(t, follower.DrainOnce(context.Background()))

	var afterFollower int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Count(&afterFollower).Error)
	require.Equal(t, int64(1), afterFollower)
}

// TestReclaimStaleClaimsMatchesTheViewsClaimState is the contract that lets the
// queue view annotate rows honestly: the reaper must release EXACTLY the rows
// models.RunQueue.ClaimState calls stale, and leave the rest alone. If the two
// predicates ever drift, an operator sees `stale` on a row nobody will reclaim,
// or a live row silently vanishing.
func TestReclaimStaleClaimsMatchesTheViewsClaimState(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	job := createQueuedJob(t, db)
	now := time.Now().UTC()
	const lease = time.Minute
	expired := now.Add(-2 * lease)
	live := now.Add(-time.Second)

	staleID, unstampedID, liveID, pendingID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, row := range []models.RunQueue{
		{ID: staleID, JobID: job.ID, Priority: runstorage.PriorityNormalValue, ClaimedBy: "node-a/dead", ClaimedAt: &expired, CreatedAt: now},
		{ID: unstampedID, JobID: job.ID, Priority: runstorage.PriorityNormalValue, ClaimedBy: "node-b/unstamped", CreatedAt: now},
		{ID: liveID, JobID: job.ID, Priority: runstorage.PriorityNormalValue, ClaimedBy: "node-c/live", ClaimedAt: &live, CreatedAt: now},
		{ID: pendingID, JobID: job.ID, Priority: runstorage.PriorityNormalValue, CreatedAt: now},
	} {
		require.NoError(t, db.Create(&row).Error)
	}

	cutoff := models.RunQueueStaleCutoff(now, lease)
	wantState := map[uuid.UUID]string{
		staleID:     models.RunQueueClaimStateStale,
		unstampedID: models.RunQueueClaimStateStale,
		liveID:      models.RunQueueClaimStateClaimed,
		pendingID:   models.RunQueueClaimStatePending,
	}
	var before []models.RunQueue
	require.NoError(t, db.Find(&before, "job_id = ?", job.ID).Error)
	require.Len(t, before, 4)
	for _, row := range before {
		require.Equal(t, wantState[row.ID], row.ClaimState(cutoff))
	}

	d := NewDequeuer(Config{DB: db, StaleClaimThreshold: lease})
	require.NoError(t, d.reclaimStaleClaims(context.Background()))

	var after []models.RunQueue
	require.NoError(t, db.Find(&after, "job_id = ?", job.ID).Error)
	released := map[uuid.UUID]bool{}
	for _, row := range after {
		released[row.ID] = row.ClaimedBy == "" && row.ClaimedAt == nil
	}
	require.True(t, released[staleID], "an expired claim must be released")
	require.True(t, released[unstampedID], "a claim with no timestamp must be released")
	require.False(t, released[liveID], "a claim inside its lease must be left alone")
	require.True(t, released[pendingID], "an unclaimed row is already released")
}

func TestNewDequeuerDefaultsToTheSharedClaimLease(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	require.Equal(t, models.DefaultRunQueueClaimStaleAfter,
		NewDequeuer(Config{DB: db}).staleClaimThreshold,
		"the reaper's default lease must be the one the queue view classifies against")
}

func createQueuedJob(t *testing.T, db *gorm.DB) *models.Job {
	t.Helper()
	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "queue-trigger",
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *","timezone":"UTC"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, db.Create(trigger).Error)
	raw, err := json.Marshal(&jobdef.Concurrency{
		MaxRuns:  1,
		Strategy: jobdef.ConcurrencyStrategyQueue,
	})
	require.NoError(t, err)
	job := &models.Job{
		ID:          uuid.New(),
		Alias:       "queue-job",
		TriggerID:   trigger.ID,
		Concurrency: datatypes.JSON(raw),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, db.Create(job).Error)
	return job
}
