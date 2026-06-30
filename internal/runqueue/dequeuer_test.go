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
