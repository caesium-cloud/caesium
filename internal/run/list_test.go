package run

import (
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// list_test.go pins two things about Store.List directly against a real
// (sqlite-backed) database, ahead of the end-to-end coverage in
// test/run_list_test.go:
//
//   - issue #489: cache_hits/executed_tasks/total_tasks must come from the
//     real task_runs rows on the page, not from the Preload("Tasks") that
//     Scan() silently drops.
//   - issue #499: limit/offset must actually bound and page the query, newest
//     run first.

// listFixture is a job with two catalog tasks (alpha, beta) and helpers to
// create job_runs with task_runs of chosen statuses, so counters have real
// data to reconcile against.
type listFixture struct {
	db         *gorm.DB
	jobID      uuid.UUID
	taskAlpha  uuid.UUID
	taskBeta   uuid.UUID
	atomID     uuid.UUID
	nextOffset int
}

func newListFixture(t *testing.T) *listFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Now().UTC()
	jobID, atomID, taskAlpha, taskBeta := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	require.NoError(t, db.Create(&models.Job{
		ID: jobID, Alias: "list-fixture-" + uuid.NewString()[:8], CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Atom{
		ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23",
		Command: `["echo","ok"]`, CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID: taskAlpha, JobID: jobID, AtomID: atomID, Name: "alpha", CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID: taskBeta, JobID: jobID, AtomID: atomID, Name: "beta", CreatedAt: now, UpdatedAt: now,
	}).Error)

	return &listFixture{db: db, jobID: jobID, taskAlpha: taskAlpha, taskBeta: taskBeta, atomID: atomID}
}

// addRun creates one job_run with strictly increasing created_at (so ordering
// is deterministic regardless of clock resolution) and returns its id.
func (f *listFixture) addRun(t *testing.T, status string) uuid.UUID {
	t.Helper()
	created := time.Now().UTC().Add(time.Duration(f.nextOffset) * time.Second)
	f.nextOffset++
	runID := uuid.New()
	require.NoError(t, f.db.Create(&models.JobRun{
		ID: runID, JobID: f.jobID, Status: status,
		StartedAt: created, CreatedAt: created, UpdatedAt: created,
	}).Error)
	return runID
}

// addTaskRun records one task_run row for runID/taskID with the given status
// and cache-hit flag.
func (f *listFixture) addTaskRun(t *testing.T, runID, taskID uuid.UUID, status string, cacheHit bool) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, f.db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: f.atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`,
		Status: status, CacheHit: cacheHit, Attempt: 1, MaxAttempts: 1,
		CreatedAt: now, UpdatedAt: now,
	}).Error)
}

// TestStoreListPopulatesRealTaskCounters pins issue #489: List must report
// the SAME cache_hits/executed_tasks/total_tasks a caller would compute by
// hand from the real task_runs rows — not zeros from an association Scan()
// never populated.
func TestStoreListPopulatesRealTaskCounters(t *testing.T) {
	f := newListFixture(t)
	store := NewStore(f.db)

	executedRun := f.addRun(t, string(StatusSucceeded))
	f.addTaskRun(t, executedRun, f.taskAlpha, string(TaskStatusSucceeded), false)
	f.addTaskRun(t, executedRun, f.taskBeta, string(TaskStatusSucceeded), false)

	cachedRun := f.addRun(t, string(StatusSucceeded))
	f.addTaskRun(t, cachedRun, f.taskAlpha, string(TaskStatusSucceeded), false)
	f.addTaskRun(t, cachedRun, f.taskBeta, string(TaskStatusCached), true)

	failedRun := f.addRun(t, string(StatusFailed))
	f.addTaskRun(t, failedRun, f.taskAlpha, string(TaskStatusFailed), false)
	f.addTaskRun(t, failedRun, f.taskBeta, string(TaskStatusSucceeded), false)

	runs, total, hasMore, err := store.List(f.jobID, 0, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	assert.False(t, hasMore, "an unbounded fetch has no continuation")
	require.Len(t, runs, 3)

	byID := make(map[uuid.UUID]*JobRun, len(runs))
	for _, r := range runs {
		byID[r.ID] = r
	}

	executed := byID[executedRun]
	require.NotNil(t, executed)
	assert.Equal(t, 0, executed.CacheHits, "no task in this run was a cache hit")
	assert.Equal(t, 2, executed.ExecutedTasks)
	assert.Equal(t, 2, executed.TotalTasks)

	cached := byID[cachedRun]
	require.NotNil(t, cached)
	assert.Equal(t, 1, cached.CacheHits, "beta was a real cache hit, not a measured zero")
	assert.Equal(t, 1, cached.ExecutedTasks)
	assert.Equal(t, 2, cached.TotalTasks)

	failed := byID[failedRun]
	require.NotNil(t, failed)
	assert.Equal(t, 0, failed.CacheHits)
	assert.Equal(t, 2, failed.ExecutedTasks, "a failed task still counts as executed")
	assert.Equal(t, 2, failed.TotalTasks)

	// The list response stays a summary: full task rows are the detail
	// endpoint's surface. Only the derived counters above are list surface.
	assert.Empty(t, cached.Tasks, "List must not inflate the response with full task rows")
}

// TestStoreListOrdersNewestFirstAndPages pins issue #499: limit/offset must
// actually bound the query (not be silently ignored), pages must be
// non-overlapping, and ordering must be newest-first and stable.
func TestStoreListOrdersNewestFirstAndPages(t *testing.T) {
	f := newListFixture(t)
	store := NewStore(f.db)

	var runIDs []uuid.UUID // oldest to newest
	for i := 0; i < 5; i++ {
		runIDs = append(runIDs, f.addRun(t, string(StatusSucceeded)))
	}

	all, total, hasMore, err := store.List(f.jobID, 0, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 5, total)
	assert.False(t, hasMore)
	require.Len(t, all, 5)
	for i, r := range all {
		assert.Equal(t, runIDs[len(runIDs)-1-i], r.ID, "unbounded List must be newest-first")
	}

	seen := map[uuid.UUID]bool{}
	for offset := 0; offset < 5; offset++ {
		page, pageTotal, pageHasMore, err := store.List(f.jobID, 1, offset)
		require.NoError(t, err)
		assert.EqualValues(t, 5, pageTotal)
		assert.Equal(t, offset < 4, pageHasMore, "hasMore must be true until the last single-row page")
		require.Len(t, page, 1)
		assert.False(t, seen[page[0].ID], "page at offset %d overlapped an earlier page", offset)
		seen[page[0].ID] = true
		assert.Equal(t, runIDs[len(runIDs)-1-offset], page[0].ID)
	}
	assert.Len(t, seen, 5, "five single-row pages must cover every run exactly once")

	empty, emptyTotal, emptyHasMore, err := store.List(f.jobID, 1, 5)
	require.NoError(t, err)
	assert.EqualValues(t, 5, emptyTotal)
	assert.False(t, emptyHasMore)
	assert.Empty(t, empty, "offset past the end returns no rows, not an error")
}

// TestStoreListDerivesContinuationFromPageQueryNotStaleCount pins a review
// finding: List's total COUNT and its page SELECT are two separate reads
// with no shared snapshot. A run committed in the window between them can
// shift the newest-first page enough that comparing the page length against
// that (now-stale) total would silently report no more pages — even though
// older history still exists. hasMore must instead come from the page query
// itself (a limit+1 probe), which is correct regardless of what the earlier
// COUNT saw.
//
// listCountToPageHook lets the test insert a run in exactly that window,
// deterministically, without real concurrency: with 5 existing runs and
// limit=5, a naive `len(page) < total` check sees 5 == 5 and reports no more
// pages; the fix must still report hasMore=true because its own limit+1
// fetch — issued AFTER the insert — sees 6 rows.
func TestStoreListDerivesContinuationFromPageQueryNotStaleCount(t *testing.T) {
	f := newListFixture(t)
	store := NewStore(f.db)

	for i := 0; i < 5; i++ {
		f.addRun(t, string(StatusSucceeded))
	}

	var injectedRun uuid.UUID
	t.Cleanup(func() { listCountToPageHook = nil })
	listCountToPageHook = func() {
		// Runs once, after Count (which reads 5) and before the page Scan —
		// simulating a run committed in that exact window.
		injectedRun = f.addRun(t, string(StatusSucceeded))
		listCountToPageHook = nil // fire once; re-listing later must not re-trigger it
	}

	runs, total, hasMore, err := store.List(f.jobID, 5, 0)
	require.NoError(t, err)
	// total reflects the COUNT read before the insert — it is display data,
	// not the truncation signal, so it is allowed to be stale here.
	assert.EqualValues(t, 5, total)
	require.Len(t, runs, 5, "the page must still be exactly limit rows")
	assert.True(t, hasMore,
		"a run committed between Count and the page Scan must still be visible to the page's own limit+1 probe")

	// The newly-inserted run is newest, so it must be ON this page, and the
	// previously-oldest-of-five run must have been pushed off it — proving
	// the page itself (not a stale total) is what the client actually saw.
	seenIDs := make(map[uuid.UUID]bool, len(runs))
	for _, r := range runs {
		seenIDs[r.ID] = true
	}
	require.NotZero(t, injectedRun)
	assert.True(t, seenIDs[injectedRun], "the inserted run is newest-first and must be on this page")
}

// TestPageTaskRunsByJobRunIDExcludesLargePayloadColumns pins a review finding
// on #489's fix: pageTaskRunsByJobRunID must project only the columns
// collapseFanOutGroups/summarizeTasks actually read (id, job_run_id, task_id,
// status, cache_hit) — not the whole TaskRun row. A task_run's persisted log
// snapshot (LogText) can be up to 1 MiB, and a default 100-run page with a
// handful of tasks each would otherwise materialize on that order for every
// list request, solely to throw the rows away after computing three counts.
func TestPageTaskRunsByJobRunIDExcludesLargePayloadColumns(t *testing.T) {
	f := newListFixture(t)
	store := NewStore(f.db)

	runID := f.addRun(t, string(StatusSucceeded))
	bigLog := strings.Repeat("x", 1<<20) // 1 MiB, matching the executor's persisted log-snapshot ceiling.
	now := time.Now().UTC()
	require.NoError(t, f.db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: f.taskAlpha, AtomID: f.atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`,
		Status: string(TaskStatusSucceeded), Attempt: 1, MaxAttempts: 1,
		LogText: bigLog, CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, f.db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: f.taskBeta, AtomID: f.atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","ok"]`,
		Status: string(TaskStatusCached), CacheHit: true, Attempt: 1, MaxAttempts: 1,
		LogText: bigLog, CreatedAt: now, UpdatedAt: now,
	}).Error)

	tasksByRun, err := store.pageTaskRunsByJobRunID([]uuid.UUID{runID})
	require.NoError(t, err)
	require.Len(t, tasksByRun[runID], 2)
	for _, row := range tasksByRun[runID] {
		assert.Empty(t, row.LogText, "the summary-counter query must not load persisted task logs")
	}

	// The narrow projection must still leave the counters correct end to end.
	runs, _, _, err := store.List(f.jobID, 0, 0)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, 1, runs[0].CacheHits)
	assert.Equal(t, 1, runs[0].ExecutedTasks)
	assert.Equal(t, 2, runs[0].TotalTasks)
}
