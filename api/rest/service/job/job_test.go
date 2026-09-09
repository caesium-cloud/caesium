package job

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(models.All...))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestCreatePersistsMetadata(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	req := &CreateRequest{
		TriggerID:   uuid.New(),
		Alias:       "job-meta",
		Labels:      map[string]string{"team": "data"},
		Annotations: map[string]string{"owner": "qa"},
	}

	job, err := svc.Create(req)
	require.NoError(t, err)
	require.NotNil(t, job)

	var stored models.Job
	require.NoError(t, db.First(&stored, "id = ?", job.ID).Error)
	require.Equal(t, "data", stored.Labels["team"])
	require.Equal(t, "qa", stored.Annotations["owner"])
}

func TestJSONMapFromStringMapHandlesNil(t *testing.T) {
	val := jsonmap.FromStringMap(nil)
	require.Empty(t, val)
}

func TestSetPausedPausesJob(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	created, err := svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "pause-test"})
	require.NoError(t, err)
	require.False(t, created.Paused)

	updated, err := svc.SetPaused(created.ID, true)
	require.NoError(t, err)
	require.True(t, updated.Paused)

	var stored models.Job
	require.NoError(t, db.First(&stored, "id = ?", created.ID).Error)
	require.True(t, stored.Paused)
}

func TestSetPausedUnpausesJob(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	created, err := svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "unpause-test"})
	require.NoError(t, err)

	_, err = svc.SetPaused(created.ID, true)
	require.NoError(t, err)

	updated, err := svc.SetPaused(created.ID, false)
	require.NoError(t, err)
	require.False(t, updated.Paused)

	var stored models.Job
	require.NoError(t, db.First(&stored, "id = ?", created.ID).Error)
	require.False(t, stored.Paused)
}

func TestSetPausedNotFoundReturnsError(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	_, err := svc.SetPaused(uuid.New(), true)
	require.Error(t, err)
}

// TestQueueAnnotatesClaimStateInsteadOfHidingClaimedRows pins the contract the
// queue view is there for: a row whose dequeuer died mid-drain must stay
// VISIBLE and be marked stale, because it is the row an operator most needs to
// see. A live claim is visible too but not stale, and an unclaimed row is
// plainly pending.
func TestQueueAnnotatesClaimStateInsteadOfHidingClaimedRows(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	job, err := svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "queue-claim-state"})
	require.NoError(t, err)

	now := time.Now().UTC()
	// Stale by the DEFAULT lease (2m) that env.Variables() yields in a unit
	// test, so the classification is not clock-flaky.
	expiredClaim := now.Add(-models.DefaultRunQueueClaimStaleAfter - time.Minute)
	liveClaim := now.Add(-time.Second)

	staleID, claimedID, pendingID, unstampedID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, row := range []models.RunQueue{
		{ID: staleID, JobID: job.ID, Priority: 3, ClaimedBy: "node-a/dead", ClaimedAt: &expiredClaim, CreatedAt: now.Add(-4 * time.Minute)},
		{ID: claimedID, JobID: job.ID, Priority: 2, ClaimedBy: "node-b/live", ClaimedAt: &liveClaim, CreatedAt: now.Add(-3 * time.Minute)},
		{ID: pendingID, JobID: job.ID, Priority: 2, CreatedAt: now.Add(-2 * time.Minute)},
		// A claim with no timestamp is stale by the reaper's own predicate
		// (`claimed_at IS NULL OR claimed_at < cutoff`).
		{ID: unstampedID, JobID: job.ID, Priority: 1, ClaimedBy: "node-c/unstamped", CreatedAt: now.Add(-time.Minute)},
	} {
		require.NoError(t, db.Create(&row).Error)
	}

	items, err := svc.Queue(job.ID)
	require.NoError(t, err)
	require.Len(t, items, 4, "claimed rows must not be filtered out of the queue view")

	byID := map[uuid.UUID]QueueItem{}
	for _, item := range items {
		byID[item.ID] = item
	}

	require.Equal(t, models.RunQueueClaimStateStale, byID[staleID].ClaimState)
	require.True(t, byID[staleID].Stale)
	require.Equal(t, "node-a/dead", byID[staleID].ClaimedBy)
	require.NotNil(t, byID[staleID].ClaimedAt)

	require.Equal(t, models.RunQueueClaimStateClaimed, byID[claimedID].ClaimState)
	require.False(t, byID[claimedID].Stale, "a claim inside its lease is live, not stuck")
	require.Equal(t, "node-b/live", byID[claimedID].ClaimedBy)

	require.Equal(t, models.RunQueueClaimStatePending, byID[pendingID].ClaimState)
	require.False(t, byID[pendingID].Stale)
	require.Empty(t, byID[pendingID].ClaimedBy)
	require.Nil(t, byID[pendingID].ClaimedAt)

	require.Equal(t, models.RunQueueClaimStateStale, byID[unstampedID].ClaimState)
	require.True(t, byID[unstampedID].Stale)

	// Ordering and positions still follow the dequeuer's own drain order.
	require.Equal(t, []uuid.UUID{staleID, claimedID, pendingID, unstampedID},
		[]uuid.UUID{items[0].ID, items[1].ID, items[2].ID, items[3].ID})
	for idx, item := range items {
		require.Equal(t, idx+1, item.Position)
	}
}

func TestQueueOfJobWithNoQueuedRunsIsEmpty(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	job, err := svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "queue-empty"})
	require.NoError(t, err)

	items, err := svc.Queue(job.ID)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestListFiltersByAliases(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	_, err := svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "alpha"})
	require.NoError(t, err)
	_, err = svc.Create(&CreateRequest{TriggerID: uuid.New(), Alias: "beta"})
	require.NoError(t, err)

	jobs, err := svc.List(&ListRequest{Aliases: []string{"beta"}})
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "beta", jobs[0].Alias)
}

func TestDeleteSoftDeletesOwningTrigger(t *testing.T) {
	db := openTestDB(t)
	svc := &jobService{ctx: context.Background(), db: db}

	trigger := &models.Trigger{
		ID:            uuid.New(),
		Type:          models.TriggerTypeEvent,
		Configuration: `{"events":[{"type":"webhook.*"}]}`,
	}
	require.NoError(t, db.Create(trigger).Error)

	created, err := svc.Create(&CreateRequest{TriggerID: trigger.ID, Alias: "delete-trigger"})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(created.ID))

	var stored models.Trigger
	require.ErrorIs(t, db.First(&stored, "id = ?", trigger.ID).Error, gorm.ErrRecordNotFound)
	require.NoError(t, db.Unscoped().First(&stored, "id = ?", trigger.ID).Error)
	require.True(t, stored.DeletedAt.Valid)
}

func TestDeleteMissingJobIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	svc := ServiceWithDatabase(context.Background(), db)

	require.NoError(t, svc.Delete(uuid.New()))
}

func TestDeleteSharedTriggerKeepsUntilLastJob(t *testing.T) {
	db := openTestDB(t)
	svc := ServiceWithDatabase(context.Background(), db)

	trigger := &models.Trigger{
		ID:            uuid.New(),
		Type:          models.TriggerTypeEvent,
		Configuration: `{"events":[{"type":"webhook.*"}]}`,
	}
	require.NoError(t, db.Create(trigger).Error)

	firstJob, err := svc.Create(&CreateRequest{TriggerID: trigger.ID, Alias: "shared-trigger-first"})
	require.NoError(t, err)
	secondJob, err := svc.Create(&CreateRequest{TriggerID: trigger.ID, Alias: "shared-trigger-second"})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(firstJob.ID))

	var storedTrigger models.Trigger
	require.NoError(t, db.First(&storedTrigger, "id = ?", trigger.ID).Error)

	var remaining int64
	require.NoError(t, db.Model(&models.Job{}).Where("trigger_id = ?", trigger.ID).Count(&remaining).Error)
	require.Equal(t, int64(1), remaining)

	var deletedJob models.Job
	require.NoError(t, db.Unscoped().First(&deletedJob, "id = ?", firstJob.ID).Error)
	require.True(t, deletedJob.DeletedAt.Valid)

	require.NoError(t, svc.Delete(secondJob.ID))

	require.ErrorIs(t, db.First(&storedTrigger, "id = ?", trigger.ID).Error, gorm.ErrRecordNotFound)
	require.NoError(t, db.Unscoped().First(&storedTrigger, "id = ?", trigger.ID).Error)
	require.True(t, storedTrigger.DeletedAt.Valid)
}
