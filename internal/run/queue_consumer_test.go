package run

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

// TestQueueOverflowRefusalReplaysRecordedIdempotentStart covers only the
// up-front lookup. The post-admission re-read is
// TestQueueOverflowRefusalRechecksKeyAfterLookupMiss.
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

// TestQueueOverflowRefusalRechecksKeyAfterLookupMiss is the window the up-front
// lookup cannot see. The enricher runs after that lookup misses and before
// admission, and commits the concurrent winner there. A disabled-queue refusal
// inserts nothing, so deleting the post-admission re-read fails this test.
func TestQueueOverflowRefusalRechecksKeyAfterLookupMiss(t *testing.T) {
	t.Run("matching request replays the winner", func(t *testing.T) {
		assertQueueDisabledKeyRace(t, "queue-race-match", nil, startRequestHash(nil, ""), false)
	})
	t.Run("different request is a reused key", func(t *testing.T) {
		assertQueueDisabledKeyRace(t, "queue-race-reuse", map[string]string{"token": "caller"}, startRequestHash(map[string]string{"token": "winner"}, ""), true)
	})
}

func assertQueueDisabledKeyRace(t *testing.T, alias string, callerParams map[string]string, recordedHash string, wantReuse bool) {
	t.Helper()
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, alias, jobdef.ConcurrencyStrategyQueue, 1)
	t.Cleanup(env.SetRunQueueConsumerEnabled(false))

	var (
		winnerID uuid.UUID
		raced    bool
	)
	SetStartParamsEnricher(func(_ context.Context, conn *gorm.DB, jobID uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		// The winner's own start runs this enricher too; only race once.
		if raced {
			return params, nil
		}
		raced = true
		winner, err := store.StartWithResult(context.Background(), jobID, nil)
		require.NoError(t, err)
		winnerID = winner.Run.ID
		require.NoError(t, conn.Create(&models.RunStartIdempotency{
			ID:             uuid.New(),
			JobID:          jobID,
			IdempotencyKey: "raced",
			RequestHash:    recordedHash,
			Outcome:        string(StartOutcomeCreated),
			RunID:          &winnerID,
		}).Error)
		return params, nil
	})
	t.Cleanup(func() { SetStartParamsEnricher(nil) })

	got, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartParams(callerParams), WithStartIdempotencyKey("raced"))
	require.True(t, raced)
	if wantReuse {
		require.ErrorIs(t, err, ErrIdempotencyKeyReused)
		require.NotErrorIs(t, err, ErrRunQueueConsumerDisabled)
	} else {
		require.NoError(t, err)
		require.True(t, got.Replayed)
		require.NotNil(t, got.Run)
		require.Equal(t, winnerID, got.Run.ID)
	}
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID))
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
