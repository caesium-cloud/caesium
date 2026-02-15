package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestClaimerClaimNextClaimsOldestReadyTask(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
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
	require.GreaterOrEqual(t, metrictestutil.CounterValue(t, metrics.WorkerClaimsTotal, "node-a"), float64(1))

	var persistedBlocked models.TaskRun
	require.NoError(t, db.First(&persistedBlocked, "id = ?", blocked.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), persistedBlocked.Status)
}

func TestClaimerClaimNextSkipsUnexpiredAndReclaimsExpiredLease(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
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
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
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
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
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
	require.GreaterOrEqual(t, metrictestutil.CounterValue(t, metrics.WorkerLeaseExpirationsTotal, "node-a"), float64(1))
}

func TestClaimerClaimNextRecordsClaimContentionMetric(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	task := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               time.Now().UTC().Add(-time.Minute),
	})

	triggerSQL := fmt.Sprintf(`
CREATE TRIGGER force_claim_conflict
BEFORE UPDATE OF status,claimed_by,claim_expires_at,claim_attempt ON task_runs
FOR EACH ROW
WHEN OLD.id = '%s' AND NEW.status = 'running'
BEGIN
	SELECT RAISE(IGNORE);
END;
`, task.ID.String())
	require.NoError(t, db.Exec(triggerSQL).Error)

	claimer := NewClaimer("node-racer", run.NewStore(db), time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.Nil(t, claimed)
	require.GreaterOrEqual(t, metrictestutil.CounterValue(t, metrics.WorkerClaimContentionTotal, "node-racer"), float64(1))
}

func TestClaimerClaimNextRespectsNodeSelector(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	now := time.Now().UTC()
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		nodeSelector:            map[string]string{"disk": "hdd"},
		createdAt:               now.Add(-2 * time.Minute),
	})
	match := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		nodeSelector:            map[string]string{"disk": "ssd"},
		createdAt:               now.Add(-time.Minute),
	})

	claimer := NewClaimer("node-ssd", run.NewStore(db), time.Minute, map[string]string{"disk": "ssd"})
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, match.ID, claimed.ID)
}

func TestClaimerClaimNextReturnsNilWhenNoSelectorMatches(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		nodeSelector:            map[string]string{"zone": "us-west-2"},
		createdAt:               time.Now().UTC().Add(-time.Minute),
	})

	claimer := NewClaimer("node-east", run.NewStore(db), time.Minute, map[string]string{"zone": "us-east-1"})
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.Nil(t, claimed)
}

func TestClaimerClaimNextIgnoresNonRunningJobRuns(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	task := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		jobRunStatus:            string(run.StatusFailed),
		createdAt:               time.Now().UTC().Add(-time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.Nil(t, claimed)

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "id = ?", task.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), persisted.Status)
	require.Equal(t, "", persisted.ClaimedBy)
}

func TestClaimerReclaimExpiredIgnoresNonRunningJobRuns(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	now := time.Now().UTC()
	expired := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusRunning),
		outstandingPredecessors: 0,
		claimedBy:               "node-old",
		claimExpiresAt:          ptrTime(now.Add(-time.Minute)),
		claimAttempt:            2,
		jobRunStatus:            string(run.StatusFailed),
		createdAt:               now.Add(-2 * time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "id = ?", expired.ID).Error)
	require.Equal(t, string(run.TaskStatusRunning), persisted.Status)
	require.Equal(t, "node-old", persisted.ClaimedBy)
	require.NotNil(t, persisted.ClaimExpiresAt)
}

type seedTaskRunInput struct {
	status                  string
	outstandingPredecessors int
	claimedBy               string
	nodeSelector            map[string]string
	claimExpiresAt          *time.Time
	claimAttempt            int
	jobRunStatus            string
	createdAt               time.Time
}

func seedTaskRun(t *testing.T, db *gorm.DB, in seedTaskRunInput) *models.TaskRun {
	t.Helper()

	if in.createdAt.IsZero() {
		in.createdAt = time.Now().UTC()
	}
	if strings.TrimSpace(in.jobRunStatus) == "" {
		in.jobRunStatus = string(run.StatusRunning)
	}

	jobRunID := uuid.New()
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        jobRunID,
		JobID:     jobID,
		Status:    in.jobRunStatus,
		StartedAt: in.createdAt,
		CreatedAt: in.createdAt,
		UpdatedAt: in.createdAt,
	}).Error)

	record := &models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                jobRunID,
		TaskID:                  uuid.New(),
		AtomID:                  uuid.New(),
		Engine:                  models.AtomEngineDocker,
		Image:                   "alpine:3.20",
		Command:                 `["echo","ok"]`,
		Status:                  in.status,
		ClaimedBy:               in.claimedBy,
		NodeSelector:            jsonmap.FromStringMap(in.nodeSelector),
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
