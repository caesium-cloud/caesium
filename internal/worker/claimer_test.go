package worker

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestClaimerClaimNextClaimsOldestReadyTask(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	now := time.Now().UTC()
	blocked := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 1,
		createdAt:               now.Add(-3 * time.Minute),
	})
	readyOlder := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-2 * time.Minute),
	})
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-1 * time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, readyOlder.ID, claimed.ID)
	require.Equal(t, "node-a", claimed.ClaimedBy)
	require.Equal(t, 1, claimed.ClaimAttempt)
	require.Equal(t, string(run.TaskStatusRunning), claimed.Status)
	require.NotNil(t, claimed.ClaimExpiresAt)
	require.True(t, claimed.ClaimExpiresAt.After(now))

	var persistedBlocked models.TaskRun
	require.NoError(t, db.First(&persistedBlocked, "id = ?", blocked.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), persistedBlocked.Status)
}

func TestClaimerClaimNextSkipsUnexpiredAndReclaimsExpiredLease(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	now := time.Now().UTC()
	reclaimable := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		claimedBy:               "node-old",
		claimExpiresAt:          ptrTime(now.Add(-30 * time.Second)),
		claimAttempt:            2,
		createdAt:               now.Add(-2 * time.Minute),
	})
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		claimedBy:               "node-live",
		claimExpiresAt:          ptrTime(now.Add(10 * time.Minute)),
		claimAttempt:            4,
		createdAt:               now.Add(-3 * time.Minute),
	})

	claimer := NewClaimer("node-new", run.NewStore(db), time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, reclaimable.ID, claimed.ID)
	require.Equal(t, "node-new", claimed.ClaimedBy)
	require.Equal(t, 3, claimed.ClaimAttempt)
	require.Equal(t, string(run.TaskStatusRunning), claimed.Status)
}

func TestClaimerClaimNextReturnsNilWhenNothingReady(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	now := time.Now().UTC()
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusRunning),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.Nil(t, claimed)
}

func TestClaimerReclaimExpiredResetsStaleRunningTasks(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	now := time.Now().UTC()
	expired := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusRunning),
		outstandingPredecessors: 0,
		claimedBy:               "node-old",
		claimExpiresAt:          ptrTime(now.Add(-time.Minute)),
		claimAttempt:            2,
		createdAt:               now.Add(-2 * time.Minute),
	})

	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusRunning),
		outstandingPredecessors: 0,
		claimedBy:               "node-live",
		claimExpiresAt:          ptrTime(now.Add(2 * time.Minute)),
		claimAttempt:            1,
		createdAt:               now.Add(-2 * time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	var stale models.TaskRun
	require.NoError(t, db.First(&stale, "id = ?", expired.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), stale.Status)
	require.Equal(t, "", stale.ClaimedBy)
	require.Nil(t, stale.ClaimExpiresAt)
	require.Equal(t, "", stale.RuntimeID)
}

type seedTaskRunInput struct {
	status                  string
	outstandingPredecessors int
	claimedBy               string
	claimExpiresAt          *time.Time
	claimAttempt            int
	createdAt               time.Time
}

func seedTaskRun(t *testing.T, db *gorm.DB, in seedTaskRunInput) *models.TaskRun {
	t.Helper()

	if in.createdAt.IsZero() {
		in.createdAt = time.Now().UTC()
	}

	record := &models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                uuid.New(),
		TaskID:                  uuid.New(),
		AtomID:                  uuid.New(),
		Engine:                  models.AtomEngineDocker,
		Image:                   "alpine:3.20",
		Command:                 `["echo","ok"]`,
		Status:                  in.status,
		ClaimedBy:               in.claimedBy,
		ClaimExpiresAt:          in.claimExpiresAt,
		ClaimAttempt:            in.claimAttempt,
		OutstandingPredecessors: in.outstandingPredecessors,
		CreatedAt:               in.createdAt,
		UpdatedAt:               in.createdAt,
	}

	require.NoError(t, db.Create(record).Error)
	return record
}

func ptrTime(v time.Time) *time.Time {
	return &v
}
