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
	"github.com/mattn/go-sqlite3"
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

func TestClaimerClaimNextPrefersHigherPriority(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	now := time.Now().UTC()
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		priority:                run.PriorityLowValue,
		outstandingPredecessors: 0,
		createdAt:               now.Add(-3 * time.Minute),
	})
	high := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		priority:                run.PriorityHighValue,
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
	})
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		priority:                run.PriorityNormalValue,
		outstandingPredecessors: 0,
		createdAt:               now.Add(-2 * time.Minute),
	})

	claimer := NewClaimer("node-priority", run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, high.ID, claimed.ID)
	require.Equal(t, run.PriorityHighValue, claimed.Priority)
	require.GreaterOrEqual(t, metrictestutil.CounterValue(t, metrics.TaskPriorityClaimTotal, "high"), float64(1))
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

func TestClaimerClaimNextReturnsNilWhenClaimRaceIsLost(t *testing.T) {
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

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "id = ?", task.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), persisted.Status)
	require.Equal(t, "", persisted.ClaimedBy)
}

func TestWithBusyRetryRetriesBusyErrors(t *testing.T) {
	attempts := 0
	retries := 0
	err := withBusyRetry(context.Background(), []time.Duration{0, 0}, func() error {
		attempts++
		if attempts < 3 {
			return sqlite3.Error{Code: sqlite3.ErrBusy}
		}
		return nil
	}, func(error) {
		retries++
	})

	require.NoError(t, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, 2, retries)
}

func TestWithBusyRetryExhaustsBusyErrors(t *testing.T) {
	attempts := 0
	retries := 0
	err := withBusyRetry(context.Background(), []time.Duration{0, 0}, func() error {
		attempts++
		return sqlite3.Error{Code: sqlite3.ErrLocked}
	}, func(error) {
		retries++
	})

	require.Error(t, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, 2, retries)
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
	priority                int
	jobRunStatus            string
	createdAt               time.Time
	// jobRunID, when non-nil, reuses the given job_run_id instead of creating a
	// fresh one.  The caller is responsible for ensuring the job_run already exists.
	jobRunID *uuid.UUID
}

func seedTaskRun(t *testing.T, db *gorm.DB, in seedTaskRunInput) *models.TaskRun {
	t.Helper()

	if in.createdAt.IsZero() {
		in.createdAt = time.Now().UTC()
	}
	if strings.TrimSpace(in.jobRunStatus) == "" {
		in.jobRunStatus = string(run.StatusRunning)
	}
	if in.priority == 0 {
		in.priority = run.PriorityNormalValue
	}

	var jobRunID uuid.UUID
	if in.jobRunID != nil {
		jobRunID = *in.jobRunID
	} else {
		jobRunID = uuid.New()
		jobID := uuid.New()
		require.NoError(t, db.Create(&models.JobRun{
			ID:        jobRunID,
			JobID:     jobID,
			Status:    in.jobRunStatus,
			Priority:  in.priority,
			StartedAt: in.createdAt,
			CreatedAt: in.createdAt,
			UpdatedAt: in.createdAt,
		}).Error)
	}

	record := &models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                jobRunID,
		TaskID:                  uuid.New(),
		AtomID:                  uuid.New(),
		Engine:                  models.AtomEngineDocker,
		Image:                   "alpine:3.23",
		Command:                 `["echo","ok"]`,
		Status:                  in.status,
		Priority:                in.priority,
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

// seedJobRun creates a job_run row and returns its ID.  Used by tests that need
// to share a job_run across multiple task_run seeds or insert a run_leases row
// for the same run.
func seedJobRun(t *testing.T, db *gorm.DB, status string) uuid.UUID {
	t.Helper()
	if strings.TrimSpace(status) == "" {
		status = string(run.StatusRunning)
	}
	runID := uuid.New()
	jobID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    status,
		Priority:  run.PriorityNormalValue,
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return runID
}

// seedRunLease inserts a run_leases row directly.  leaseExpiresAt controls
// whether the lease is live (future) or expired (past).
func seedRunLease(t *testing.T, db *gorm.DB, runID uuid.UUID, ownerNode string, leaseExpiresAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.RunLease{
		RunID:          runID.String(),
		OwnerNode:      ownerNode,
		AcquiredAt:     now,
		LeaseExpiresAt: leaseExpiresAt,
		Generation:     1,
	}).Error)
}

func ptrTime(v time.Time) *time.Time {
	return &v
}

// --- B0/B1 deferral tests ---

// TestClaimerClaimNextBackwardCompatNoLeases verifies that ClaimNext claims a
// ready task exactly as before when no run_leases rows exist (owner mode off).
// This is the critical backward-compat property.
func TestClaimerClaimNextBackwardCompatNoLeases(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	ready := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, ready.ID, claimed.ID)
}

// TestClaimerClaimNextDefersLiveLease verifies that a ready task whose run has
// a live (non-expired) lease is NOT claimed by ClaimNext — it should be
// dispatched by the owner's loop instead.
func TestClaimerClaimNextDefersLiveLease(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	runID := seedJobRun(t, db, string(run.StatusRunning))
	// Insert a live lease (expires in the future).
	seedRunLease(t, db, runID, "owner-node", now.Add(30*time.Second))

	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
		jobRunID:                &runID,
	})

	claimer := NewClaimer("node-a", run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.Nil(t, claimed, "ClaimNext must not claim tasks owned by a live lease")
}

// TestClaimerClaimNextRecoveryExpiredLease verifies that a ready task whose run
// has an *expired* lease IS claimed by ClaimNext (recovery path: owner is dead).
func TestClaimerClaimNextRecoveryExpiredLease(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	runID := seedJobRun(t, db, string(run.StatusRunning))
	// Insert an expired lease.
	seedRunLease(t, db, runID, "dead-owner", now.Add(-5*time.Second))

	ready := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
		jobRunID:                &runID,
	})

	claimer := NewClaimer("node-a", run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed, "ClaimNext must claim tasks from runs with expired leases")
	require.Equal(t, ready.ID, claimed.ID)
}

// TestClaimerClaimNextOwnerNodeAlsoDefers verifies that even the owner node
// itself does not claim tasks via ClaimNext when a live lease is present.
// The owner's dispatch loop — not ClaimNext — is the correct path for owned runs.
func TestClaimerClaimNextOwnerNodeAlsoDefers(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	runID := seedJobRun(t, db, string(run.StatusRunning))
	const ownerNode = "owner-node"
	// The same node as the claimer holds the live lease.
	seedRunLease(t, db, runID, ownerNode, now.Add(30*time.Second))

	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
		jobRunID:                &runID,
	})

	// Even the owner node must not use ClaimNext for its own owned runs.
	claimer := NewClaimer(ownerNode, run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.Nil(t, claimed, "owner node must use dispatch loop, not ClaimNext, for live-lease runs")
}

// TestClaimerClaimNextMixedLeasesPicksUnowned verifies that ClaimNext skips
// tasks with live leases and picks the first unowned ready task.
func TestClaimerClaimNextMixedLeasesPicksUnowned(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()

	// Run with live lease — ClaimNext must skip.
	ownedRunID := seedJobRun(t, db, string(run.StatusRunning))
	seedRunLease(t, db, ownedRunID, "owner-node", now.Add(30*time.Second))
	_ = seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-2 * time.Minute),
		jobRunID:                &ownedRunID,
	})

	// Unowned run (no run_leases row) — ClaimNext may claim this.
	freeTask := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusPending),
		outstandingPredecessors: 0,
		createdAt:               now.Add(-time.Minute),
	})

	claimer := NewClaimer("node-a", run.NewStore(db), 2*time.Minute)
	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, freeTask.ID, claimed.ID, "must claim the unowned task, not the owned one")
}

// TestClaimerReclaimExpiredSkipsLiveLeasedRun verifies that ReclaimExpired does
// not reset a task whose run has a live lease (the owner handles re-dispatch).
func TestClaimerReclaimExpiredSkipsLiveLeasedRun(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	runID := seedJobRun(t, db, string(run.StatusRunning))
	seedRunLease(t, db, runID, "owner-node", now.Add(30*time.Second))

	// A task with an expired task-claim (worker crashed) but whose run is live-leased.
	owned := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusRunning),
		outstandingPredecessors: 0,
		claimedBy:               "worker-old",
		claimExpiresAt:          ptrTime(now.Add(-time.Minute)),
		claimAttempt:            1,
		createdAt:               now.Add(-2 * time.Minute),
		jobRunID:                &runID,
	})

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "id = ?", owned.ID).Error)
	// The owner's dispatch loop is responsible — ReclaimExpired must not touch this.
	require.Equal(t, string(run.TaskStatusRunning), persisted.Status,
		"ReclaimExpired must not reset tasks in live-leased runs")
	require.Equal(t, "worker-old", persisted.ClaimedBy)
}

// TestClaimerReclaimExpiredReclaimsExpiredLeaseRun verifies that ReclaimExpired
// DOES reset a task whose run has an *expired* run-lease (owner is dead — recovery path).
func TestClaimerReclaimExpiredReclaimsExpiredLeaseRun(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	runID := seedJobRun(t, db, string(run.StatusRunning))
	// Run-level lease is expired (owner crashed).
	seedRunLease(t, db, runID, "dead-owner", now.Add(-5*time.Second))

	expired := seedTaskRun(t, db, seedTaskRunInput{
		status:                  string(run.TaskStatusRunning),
		outstandingPredecessors: 0,
		claimedBy:               "worker-old",
		claimExpiresAt:          ptrTime(now.Add(-time.Minute)),
		claimAttempt:            1,
		createdAt:               now.Add(-2 * time.Minute),
		jobRunID:                &runID,
	})

	claimer := NewClaimer("node-a", run.NewStore(db), time.Minute)
	require.NoError(t, claimer.ReclaimExpired(context.Background()))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "id = ?", expired.ID).Error)
	require.Equal(t, string(run.TaskStatusPending), persisted.Status,
		"ReclaimExpired must reset tasks in runs with expired leases (recovery)")
	require.Equal(t, "", persisted.ClaimedBy)
}
