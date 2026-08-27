package worker

import (
	"context"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// deniedLimiter always rejects, so every claimed task takes the rate-limit
// parking branch.
type deniedLimiter struct{}

func (deniedLimiter) Acquire(context.Context, string, int, int, time.Duration) (bool, error) {
	return false, nil
}

// TestClaimNextParksRateLimitedFanOutInstanceByRow drives the real pull-worker
// path — ClaimNext → acquireRateLimit → RateLimitTask — for a NON-TEMPLATE
// sibling of a fanned group.
//
// ClaimNext parked the task with `claimed.TaskID`, the CATALOG task id. For a
// fanned step that id names all N sibling rows, so RateLimitTask's
// loadTaskRunByIDOrUnique returned ErrAmbiguousTaskRun and ClaimNext bailed out
// with an error — after the claim UPDATE had already flipped the row to
// running. The instance was left running, claimed, with no container and no
// worker that would ever start one: a permanent stall for the whole run, and
// only for partition indexes above zero. The park must be keyed to the row that
// was actually claimed.
func TestClaimNextParksRateLimitedFanOutInstanceByRow(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	runID, taskID, instances := seedRateLimitedFanOutGroup(t, db, 3)

	// Only the LAST partition is claimable, so ClaimNext is forced to pick a
	// non-zero index — the case the catalog-id park breaks.
	require.NoError(t, db.Model(&models.TaskRun{}).
		Where("id IN ?", []uuid.UUID{instances[0], instances[1]}).
		Update("outstanding_predecessors", 1).Error)

	claimer := NewClaimer("node-rl", run.NewStore(db), time.Minute).
		WithRateLimiter(deniedLimiter{})

	claimed, err := claimer.ClaimNext(context.Background())
	require.NoError(t, err,
		"parking a rate-limited fan-out instance must not fail with an ambiguous-task error")
	assert.Nil(t, claimed, "a rate-limited task is parked, not handed to the pool")

	var parked models.TaskRun
	require.NoError(t, db.First(&parked, "id = ?", instances[2]).Error)
	assert.Equal(t, string(run.TaskStatusPending), parked.Status,
		"the claimed instance must be parked back to pending, not left running with no container")
	assert.Empty(t, parked.ClaimedBy, "the claim must be released so the next window can re-claim it")
	assert.Empty(t, parked.RuntimeID)
	require.NotNil(t, parked.RateLimitRetryAfter, "the park must record when the instance may retry")

	// The siblings are untouched: parking is per row, never per group.
	for _, id := range []uuid.UUID{instances[0], instances[1]} {
		var sibling models.TaskRun
		require.NoError(t, db.First(&sibling, "id = ?", id).Error)
		assert.Nil(t, sibling.RateLimitRetryAfter,
			"a sibling that was never claimed must not be parked")
	}

	_ = runID
	_ = taskID
}

// seedRateLimitedFanOutGroup creates a job whose single step declares a rate
// limit and fans out into n sibling task_runs, all ready to claim.
func seedRateLimitedFanOutGroup(t *testing.T, db *gorm.DB, n int) (uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:         jobID,
		Alias:      "rl-fanout-" + uuid.NewString()[:8],
		RateLimits: datatypes.JSON([]byte(`[{"resource":"warehouse","limit":1,"window":"1m"}]`)),
		CreatedAt:  now,
		UpdatedAt:  now,
	}).Error)

	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Atom{
		ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23",
		Command: `["echo","ok"]`, CreatedAt: now, UpdatedAt: now,
	}).Error)

	taskID := uuid.New()
	require.NoError(t, db.Create(&models.Task{
		ID: taskID, JobID: jobID, AtomID: atomID, Name: "shard",
		RateLimitResource: "warehouse", RateLimitUnits: 1,
		CreatedAt: now, UpdatedAt: now,
	}).Error)

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(run.StatusRunning),
		Priority: run.PriorityNormalValue, StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)

	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.New()
		require.NoError(t, db.Create(&models.TaskRun{
			ID: id, JobRunID: runID, TaskID: taskID, AtomID: atomID,
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`,
			Status: string(run.TaskStatusPending), Priority: run.PriorityNormalValue,
			Attempt: 1, MaxAttempts: 1, OutstandingPredecessors: 0,
			PartitionValue: string(rune('a' + i)), PartitionIndex: i, PartitionCount: n,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
			UpdatedAt: now,
		}).Error)
		ids = append(ids, id)
	}
	return runID, taskID, ids
}
