package run

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestQueueOverflowRefusedWhenConsumerDisabled(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-disabled", jobdef.ConcurrencyStrategyQueue, 1)
	t.Cleanup(env.SetRunQueueConsumerEnabled(false))

	first, err := store.Start(job.ID, nil, WithStartParams(map[string]string{"token": "holder"}))
	require.NoError(t, err)
	require.Equal(t, "holder", first.Params["token"])

	_, err = store.Start(job.ID, nil, WithStartParams(map[string]string{"token": "stranded"}))
	require.ErrorIs(t, err, ErrRunQueueConsumerDisabled)
	require.NotErrorIs(t, err, ErrRunQueued)
	require.NotErrorIs(t, err, ErrMaxConcurrentRunsReached)

	var queued int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Count(&queued).Error)
	require.Zero(t, queued)
	var runs int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", job.ID).Count(&runs).Error)
	require.Equal(t, int64(1), runs)

	now := time.Now().UTC()
	queuedID := uuid.New()
	raw, err := json.Marshal(map[string]string{"token": "already-queued"})
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        queuedID,
		JobID:     job.ID,
		Params:    raw,
		Priority:  PriorityNormalValue,
		ClaimedBy: "node-a/promote",
		ClaimedAt: &now,
		CreatedAt: now,
	}).Error)
	_, err = store.StartQueuedRun(t.Context(), &models.RunQueue{
		ID: queuedID, JobID: job.ID, Params: raw, Priority: PriorityNormalValue, ClaimedBy: "node-a/promote",
	})
	require.ErrorIs(t, err, ErrMaxConcurrentRunsReached)
	require.NotErrorIs(t, err, ErrRunQueueConsumerDisabled)

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", first.ID).Update("status", string(StatusSucceeded)).Error)
	promoted, err := store.StartQueuedRun(t.Context(), &models.RunQueue{
		ID: queuedID, JobID: job.ID, Params: raw, Priority: PriorityNormalValue, ClaimedBy: "node-a/promote",
	})
	require.NoError(t, err)
	require.Equal(t, "already-queued", promoted.Params["token"])

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", promoted.ID).Update("status", string(StatusSucceeded)).Error)
	again, err := store.Start(job.ID, nil, WithStartParams(map[string]string{"token": "after-slot"}))
	require.NoError(t, err)
	require.Equal(t, "after-slot", again.Params["token"])
}

func TestQueueOverflowRefusalReplaysRecordedIdempotentStart(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-disabled-idem", jobdef.ConcurrencyStrategyQueue, 1)
	t.Cleanup(env.SetRunQueueConsumerEnabled(false))

	params := map[string]string{"token": "holder"}
	first, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartParams(params), WithStartIdempotencyKey("same"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, first.Outcome)
	require.NotNil(t, first.Run)

	again, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartParams(params), WithStartIdempotencyKey("same"))
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.NotNil(t, again.Run)
	require.Equal(t, first.Run.ID, again.Run.ID)

	_, err = store.StartWithResult(t.Context(), job.ID, nil, WithStartParams(map[string]string{"token": "other"}), WithStartIdempotencyKey("same"))
	require.ErrorIs(t, err, ErrIdempotencyKeyReused)
	require.NotErrorIs(t, err, ErrRunQueueConsumerDisabled)

	_, err = store.StartWithResult(t.Context(), job.ID, nil, WithStartParams(map[string]string{"token": "new"}), WithStartIdempotencyKey("other-key"))
	require.ErrorIs(t, err, ErrRunQueueConsumerDisabled)

	var runs int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", job.ID).Count(&runs).Error)
	require.Equal(t, int64(1), runs)
}

func TestQueueOverflowParksParamsWhenConsumerEnabled(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-enabled", jobdef.ConcurrencyStrategyQueue, 1)

	_, err := store.Start(job.ID, nil, WithStartParams(map[string]string{"token": "holder"}))
	require.NoError(t, err)
	queued, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartParams(map[string]string{"token": "kept"}))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, queued.Outcome)
	require.NotNil(t, queued.QueueID)

	var row models.RunQueue
	require.NoError(t, db.First(&row, "id = ?", *queued.QueueID).Error)
	require.Contains(t, string(row.Params), `"token":"kept"`)
}
