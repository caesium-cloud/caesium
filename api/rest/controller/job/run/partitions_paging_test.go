package run

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// partitions_paging_test.go drives the real ListPartitions / RetryPartition
// handlers against a live database, through echo, and asserts on the JSON the
// CLI and UI actually parse. The pre-existing tests in partitions_test.go call
// the projection helpers directly, which is why a silently truncating page and
// a 200-that-never-runs both shipped green.

type partitionsEnvelope struct {
	Partitions   []partitionRow `json:"partitions"`
	Total        int            `json:"total"`
	Limit        int            `json:"limit"`
	Offset       int            `json:"offset"`
	NextOffset   *int           `json:"next_offset"`
	StatusCounts map[string]int `json:"status_counts"`
}

// TestListPartitionsPagesA250InstanceGroup pins the pagination contract.
//
// The default limit silently truncated at 100 with nothing in the envelope
// saying so: a 250-partition group looked like a 100-partition group, and a
// client had no key to page with. `total`, `limit`, `offset` and `next_offset`
// are the four keys the CLI and UI page on, so they are asserted here as a
// contract, not an implementation detail.
func TestListPartitionsPagesA250InstanceGroup(t *testing.T) {
	f := newPartitionsFixture(t, 250)

	first := f.list(t, "")
	assert.Len(t, first.Partitions, 100, "the default page is 100 instances")
	assert.Equal(t, 250, first.Total, "total counts the whole group, not the page")
	assert.Equal(t, 100, first.Limit)
	assert.Equal(t, 0, first.Offset)
	require.NotNil(t, first.NextOffset, "a truncated page MUST tell the client where to continue")
	assert.Equal(t, 100, *first.NextOffset)
	assert.Equal(t, 0, first.Partitions[0].Index)

	second := f.list(t, "offset=100")
	require.Len(t, second.Partitions, 100)
	assert.Equal(t, 100, second.Offset)
	require.NotNil(t, second.NextOffset)
	assert.Equal(t, 200, *second.NextOffset)
	assert.Equal(t, 100, second.Partitions[0].Index)

	last := f.list(t, "offset=200")
	require.Len(t, last.Partitions, 50)
	assert.Nil(t, last.NextOffset, "the final page must report next_offset null, not 250")
	assert.Equal(t, 249, last.Partitions[49].Index)
}

// TestListPartitionsHonoursExplicitLimitAndRejectsOversize pins the documented
// maximum. An out-of-range limit previously fell back to 100 — quietly giving
// the caller a different page size than the one they asked for.
func TestListPartitionsHonoursExplicitLimitAndRejectsOversize(t *testing.T) {
	f := newPartitionsFixture(t, 250)

	page := f.list(t, "limit=250")
	assert.Len(t, page.Partitions, 250)
	assert.Equal(t, 250, page.Limit)
	assert.Nil(t, page.NextOffset)

	rec, err := f.call(t, "limit=5000")
	require.Error(t, err, "an oversize limit must be rejected, not silently clamped")
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusBadRequest, httpErr.Code)
	_ = rec
}

// TestListPartitionsStatusFilterPagesWithinTheFilteredSet pins that pagination
// metadata describes the FILTERED result set while status_counts still
// describes the whole group — the UI shows "3 of 250 failed" while paging the
// three.
func TestListPartitionsStatusFilterPagesWithinTheFilteredSet(t *testing.T) {
	f := newPartitionsFixture(t, 250)
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND partition_index < ?", f.runID, f.taskID, 3).
		Update("status", "failed").Error)

	page := f.list(t, "status=failed")
	assert.Len(t, page.Partitions, 3)
	assert.Equal(t, 3, page.Total, "total is the filtered count the client is paging")
	assert.Nil(t, page.NextOffset)
	assert.Equal(t, 3, page.StatusCounts["failed"])
	assert.Equal(t, 247, page.StatusCounts["pending"],
		"status_counts stays group-wide so the UI can show the filtered slice in context")
}

// TestListPartitionsKeyedLookupReturnsOneInstance pins the keyed read. Retrying
// a partition by VALUE (the identity a user actually knows — `region=eu-west-1`)
// must not require walking pages until the value appears.
func TestListPartitionsKeyedLookupReturnsOneInstance(t *testing.T) {
	f := newPartitionsFixture(t, 250)

	page := f.list(t, "partition=p-137")
	require.Len(t, page.Partitions, 1, "a keyed lookup returns exactly the named instance")
	assert.Equal(t, "p-137", page.Partitions[0].Value)
	assert.Equal(t, 137, page.Partitions[0].Index)
	assert.Equal(t, 1, page.Total)
	assert.Nil(t, page.NextOffset)

	missing := f.list(t, "partition=p-99999")
	assert.Empty(t, missing.Partitions)
	assert.Equal(t, 0, missing.Total)
}

// --- Review finding 9: a finished local run must resume the reset instance --

// TestRetryPartitionKicksOffResumeWhenRunIsTerminal pins the local-lane
// resume. The original (*job).Run has already returned for a finished run, so
// the handler must start a new job.New(...).Run or the reset instance stays
// pending forever with the run re-opened. The kickoff is a seam; that it is
// invoked with this job and run is what the handler owns. That Run actually
// executes only the pending instance is pinned in internal/job.
func TestRetryPartitionKicksOffResumeWhenRunIsTerminal(t *testing.T) {
	f := newPartitionsFixture(t, 4)
	f.failPartition(t, 1)
	f.finishRun(t, string(runstorage.StatusFailed))

	var (
		kickedJob  uuid.UUID
		kickedRun  uuid.UUID
		kickCount  int
		kickedJobM *models.Job
	)
	partitionKickoff = func(j *models.Job, runID uuid.UUID, _ map[string]string) {
		kickCount++
		kickedJobM = j
		if j != nil {
			kickedJob = j.ID
		}
		kickedRun = runID
	}

	require.NoError(t, f.retry(t, 1))

	var row models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ? AND partition_index = ?",
		f.runID, f.taskID, 1).First(&row).Error)
	assert.Equal(t, "pending", row.Status)
	assert.Equal(t, 1, kickCount, "a terminal run must start a new engine against the reopened run")
	require.NotNil(t, kickedJobM)
	assert.Equal(t, f.jobID, kickedJob)
	assert.Equal(t, f.runID, kickedRun)
}

// TestRetryPartitionDoesNotKickOffWhileRunIsStillInFlight pins the other
// half: if the run is still running, the in-process loop (or the dispatcher)
// is already looking at pending rows. A second Run() would race it.
func TestRetryPartitionDoesNotKickOffWhileRunIsStillInFlight(t *testing.T) {
	f := newPartitionsFixture(t, 4)
	f.failPartition(t, 1)

	kickCount := 0
	partitionKickoff = func(*models.Job, uuid.UUID, map[string]string) { kickCount++ }

	require.NoError(t, f.retry(t, 1))

	var row models.TaskRun
	require.NoError(t, f.db.Where("job_run_id = ? AND task_id = ? AND partition_index = ?",
		f.runID, f.taskID, 1).First(&row).Error)
	assert.Equal(t, "pending", row.Status)
	assert.Equal(t, 0, kickCount,
		"a live run already has a driver; kicking off another Run() would race it")
}

// TestRetryPartitionStillRejectsSucceededInstance keeps the FAILED-only
// guard after the mode gate is gone: retrying a success would discard a
// result downstream already consumed.
func TestRetryPartitionStillRejectsSucceededInstance(t *testing.T) {
	f := newPartitionsFixture(t, 4)
	f.finishRun(t, string(runstorage.StatusSucceeded))
	now := time.Now().UTC()
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND partition_index = ?", f.runID, f.taskID, 1).
		Updates(map[string]any{
			"status": "succeeded", "result": "success",
			"started_at": now.Add(-time.Minute), "completed_at": now,
		}).Error)

	kickCount := 0
	partitionKickoff = func(*models.Job, uuid.UUID, map[string]string) { kickCount++ }

	err := f.retry(t, 1)
	require.Error(t, err)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusConflict, httpErr.Code)
	assert.Contains(t, fmt.Sprint(httpErr.Message), "only a failed partition can be retried")
	assert.Equal(t, 0, kickCount, "a refused retry must not start an engine")
}

func TestPartitionRetryNeedsKickoff(t *testing.T) {
	assert.True(t, partitionRetryNeedsKickoff(runstorage.StatusFailed))
	assert.True(t, partitionRetryNeedsKickoff(runstorage.StatusSucceeded))
	assert.False(t, partitionRetryNeedsKickoff(runstorage.StatusRunning))
	assert.False(t, partitionRetryNeedsKickoff(runstorage.StatusCancelled))
}

// --- fixture --------------------------------------------------------------

type partitionsFixture struct {
	db     *gorm.DB
	jobID  uuid.UUID
	runID  uuid.UUID
	taskID uuid.UUID
}

func newPartitionsFixture(t *testing.T, instances int) *partitionsFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	usePartitionTestDB(t, db)

	now := time.Now().UTC()
	jobID, runID, taskID, atomID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	require.NoError(t, db.Create(&models.Job{
		ID: jobID, Alias: "fanout-http-" + uuid.NewString()[:8], CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Atom{
		ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23",
		Command: `["echo","ok"]`, CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID: taskID, JobID: jobID, AtomID: atomID, Name: "shard", CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(runstorage.StatusRunning),
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)

	rows := make([]models.TaskRun, 0, instances)
	for i := 0; i < instances; i++ {
		rows = append(rows, models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID,
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`,
			Status: "pending", Attempt: 1, MaxAttempts: 1,
			PartitionValue: fmt.Sprintf("p-%d", i), PartitionIndex: i, PartitionCount: instances,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	require.NoError(t, db.CreateInBatches(rows, 100).Error)

	return &partitionsFixture{db: db, jobID: jobID, runID: runID, taskID: taskID}
}

func (f *partitionsFixture) failPartition(t *testing.T, index int) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND partition_index = ?", f.runID, f.taskID, index).
		Updates(map[string]any{
			"status": "failed", "result": "failure", "error": "boom",
			"started_at": now.Add(-time.Minute), "completed_at": now,
		}).Error)
}

func (f *partitionsFixture) finishRun(t *testing.T, status string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).
		Updates(map[string]any{"status": status, "completed_at": now}).Error)
}

func (f *partitionsFixture) call(t *testing.T, query string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	target := fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions", f.jobID, f.runID, f.taskID)
	if query != "" {
		target += "?" + query
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{
		{Name: "id", Value: f.jobID.String()},
		{Name: "run_id", Value: f.runID.String()},
		{Name: "task_id", Value: f.taskID.String()},
	})
	return rec, ListPartitions(c)
}

func (f *partitionsFixture) list(t *testing.T, query string) partitionsEnvelope {
	t.Helper()
	rec, err := f.call(t, query)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	var out partitionsEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (f *partitionsFixture) retry(t *testing.T, index int) error {
	t.Helper()
	target := fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry",
		f.jobID, f.runID, f.taskID, index)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{
		{Name: "id", Value: f.jobID.String()},
		{Name: "run_id", Value: f.runID.String()},
		{Name: "task_id", Value: f.taskID.String()},
		{Name: "index", Value: fmt.Sprint(index)},
	})
	return RetryPartition(c)
}

// usePartitionTestDB points the handler's process-wide dependencies
// (runstorage.Default, jsvc.Service, runsvc.New) at a scratch database for the
// duration of one test, so the HANDLER is what runs — not a helper function
// standing in for it.
func usePartitionTestDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	store := runstorage.NewStore(db)
	origDB, origJob, origRun, origRetry, origKickoff :=
		partitionDB, partitionGetJob, partitionGetRun, partitionRetryInstance, partitionKickoff

	partitionDB = func() *gorm.DB { return db }
	partitionGetJob = func(_ context.Context, jobID uuid.UUID) (*models.Job, error) {
		var j models.Job
		if err := db.First(&j, "id = ?", jobID).Error; err != nil {
			return nil, err
		}
		return &j, nil
	}
	partitionGetRun = func(_ context.Context, runID uuid.UUID) (*runstorage.JobRun, error) {
		var r models.JobRun
		if err := db.First(&r, "id = ?", runID).Error; err != nil {
			return nil, err
		}
		return &runstorage.JobRun{ID: r.ID, JobID: r.JobID, Status: runstorage.Status(r.Status)}, nil
	}
	partitionRetryInstance = func(ctx context.Context, runID, taskRunID uuid.UUID) (*runstorage.TaskRun, error) {
		return store.RetryPartition(ctx, runID, taskRunID)
	}
	partitionKickoff = func(*models.Job, uuid.UUID, map[string]string) {}

	t.Cleanup(func() {
		partitionDB, partitionGetJob, partitionGetRun, partitionRetryInstance, partitionKickoff =
			origDB, origJob, origRun, origRetry, origKickoff
	})
}
