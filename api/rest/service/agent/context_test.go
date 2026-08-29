package agent

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedFannedIncident builds a job/run/task with FOUR fan-out instance TaskRun
// rows sharing (job_run_id, task_id) — a re-keyed group, exactly what F9
// (dynamic-fanout closeout) requires FailingLog to read correctly: it must
// scan every instance in partition_index order and prefer the failed one,
// never a bare .First() that could hand back an unrelated succeeded sibling's
// log while the incident is about a failed partition.
//
// TWO instances fail, at DIFFERENT partition_index values (1 and 3), each with
// its own distinct log, and the higher-index failure is inserted FIRST — so a
// correct implementation must scan by partition_index and stop at the LOWER
// one, not whichever failure happens to sort first some other way (row 0,
// last inserted, or the first "failed" row in an UNORDERED result set).
//
// With only one failure (the test this replaces), FailedOrLastTaskRunForTask
// finds it wherever it sits regardless of ordering, so a dropped ORDER BY is
// invisible — this fixture is what actually distinguishes "prefers the failed
// instance" from "prefers the LOWEST-partition_index failed instance".
//
// Caveat, confirmed by probing this fixture directly: on the sqlite backend
// testutil.OpenTestDB uses, models.TaskRun's own unique index — `partition_index`
// tagged `uniqueIndex:idx_taskrun_jobrun_task` alongside job_run_id/task_id —
// makes SQLite's query planner satisfy this exact (job_run_id, task_id)
// equality WHERE via an index scan that already returns partition_index order,
// REGARDLESS of whether the Go code adds `Order("partition_index ASC")`. So
// removing that clause alone will not fail this test on sqlite; the ORDER BY
// remains correct and necessary (SQL row order is unspecified without one —
// dqlite's replicated storage and any future schema/index change are not
// obligated to preserve this incidental ordering), but this fixture's real,
// falsifiable contribution is pinning FailedOrLastTaskRunForTask's own
// "first failed row wins" contract against a genuinely ambiguous case (two
// failures, not one) rather than proving the SQL clause's presence per se.
func seedFannedIncident(t *testing.T, db *gorm.DB) (*models.Incident, string) {
	t.Helper()
	now := time.Now().UTC()

	jobID := seedJob(t, db, "fanned-job")

	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sh","-c","true"]`, CreatedAt: now, UpdatedAt: now}).Error)

	taskID := uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Name: "process", CreatedAt: now, UpdatedAt: now}).Error)

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)

	const (
		lowerIndexFailedLog  = "boom from partition b"
		higherIndexFailedLog = "boom from partition d"
	)
	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 0, PartitionValue: "a", Status: "succeeded", LogText: "ok from partition a", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 1, PartitionValue: "b", Status: "failed", LogText: lowerIndexFailedLog, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 2, PartitionValue: "c", Status: "succeeded", LogText: "ok from partition c", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: atomID, PartitionIndex: 3, PartitionValue: "d", Status: "failed", LogText: higherIndexFailedLog, CreatedAt: now, UpdatedAt: now},
	}
	// Insert in an order that does NOT match partition_index — the
	// higher-index failure (d, index 3) goes in FIRST, so unordered
	// (insertion-order) reads would find IT before the lower-index failure.
	require.NoError(t, db.Create(&rows[3]).Error)
	require.NoError(t, db.Create(&rows[2]).Error)
	require.NoError(t, db.Create(&rows[0]).Error)
	require.NoError(t, db.Create(&rows[1]).Error)

	inc := seedIncident(t, db, jobID)
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", inc.ID).
		Updates(map[string]any{"run_id": runID, "task_id": taskID}).Error)
	inc.RunID = &runID
	inc.TaskID = &taskID

	return inc, lowerIndexFailedLog
}

// TestFailingLogReadsGroupInPartitionOrderAndPrefersFailed is the F9 coverage
// for api/rest/service/agent/context.go: a fanned task has N TaskRun rows
// under one (run, task), and the incident's failing log must come from the
// LOWEST-partition_index FAILED instance — proving both that the query orders
// by partition_index (two failures at different indices; the lower one must
// win) and that it prefers a failure over a succeeded sibling.
func TestFailingLogReadsGroupInPartitionOrderAndPrefersFailed(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	inc, lowerIndexFailedLog := seedFannedIncident(t, db)

	svc := &Service{ctx: context.Background(), db: db}
	log, ok, err := svc.FailingLog(inc)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, lowerIndexFailedLog, log,
		"FailingLog must return the LOWER-partition_index failed instance's log, not the higher one's or a succeeded sibling's")
}

// TestFailingLogNoFailingRunWithoutRunOrTask pins the guard: an incident with
// no addressable failing run (RunID or TaskID unset) must report ErrNoFailingRun
// rather than querying with a nil pointer.
func TestFailingLogNoFailingRunWithoutRunOrTask(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := seedJob(t, db, "no-failing-run-job")
	inc := seedIncident(t, db, jobID)

	svc := &Service{ctx: context.Background(), db: db}
	_, ok, err := svc.FailingLog(inc)
	require.ErrorIs(t, err, ErrNoFailingRun)
	require.False(t, ok)
}
