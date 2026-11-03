package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// stampOutcome gives a row the outcome a finished attempt would have recorded:
// one violation and one result.
func stampOutcome(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) {
	t.Helper()
	store := NewStore(db)
	ok, err := store.saveDataAssertionOutcomeClaimed(t.Context(), taskRunID, nil,
		[]DataViolation{{Dataset: "d", Metric: "rowCount", Assertion: AssertionMin, Message: "m"}},
		[]AssertionResult{{Dataset: "d", Metric: "rowCount", Assertion: AssertionMin, Outcome: AssertionOutcomeViolated}})
	require.NoError(t, err)
	require.True(t, ok)
}

func outcomeColumns(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) (violations, results int) {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	return len(row.DataViolations), len(row.DataAssertionResults)
}

// TestEveryReexecutionResetClearsTheAssertionOutcome pins the one shared list:
// all four re-execution resets (the retries, ResetInFlightTasks,
// ReclaimOwnerExpiredClaims and the worker's Claimer.ReclaimExpired) start from
// TaskResourceResetColumns, so a column named there cannot be forgotten by one.
func TestEveryReexecutionResetClearsTheAssertionOutcome(t *testing.T) {
	for _, columns := range []map[string]any{TaskResourceResetColumns(), retryResetColumns()} {
		for _, column := range []string{"data_violations", "data_assertion_results"} {
			value, ok := columns[column]
			assert.True(t, ok, column)
			assert.Nil(t, value, column)
		}
	}
}

// TestReclaimOwnerExpiredClaims_ClearsTheLostAttemptsAssertionOutcome: the
// re-executed attempt is judged alone. Without this, a clean re-run would sit
// beside the violations (and results) of the attempt it replaced.
func TestReclaimOwnerExpiredClaims_ClearsTheLostAttemptsAssertionOutcome(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).Updates(map[string]any{
		"claimed_by": "worker-a", "claim_expires_at": time.Now().UTC().Add(-time.Minute), "owner_generation": 1,
	}).Error)
	stampOutcome(t, db, taskRunID)

	// A terminal row with an expired claim keeps its outcome: it completed.
	_, _, succeededID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	var succeeded models.TaskRun
	require.NoError(t, db.Where("id = ?", succeededID).First(&succeeded).Error)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", succeededID).Updates(map[string]any{
		"claimed_by": "worker-a", "claim_expires_at": time.Now().UTC().Add(-time.Minute), "owner_generation": 1,
	}).Error)
	stampOutcome(t, db, succeededID)

	reset, err := store.ReclaimOwnerExpiredClaims(taskRun.JobRunID, 1)
	require.NoError(t, err)
	require.Len(t, reset, 1)
	violations, results := outcomeColumns(t, db, taskRunID)
	assert.Zero(t, violations)
	assert.Zero(t, results)

	_, err = store.ReclaimOwnerExpiredClaims(succeeded.JobRunID, 1)
	require.NoError(t, err)
	violations, results = outcomeColumns(t, db, succeededID)
	assert.NotZero(t, violations, "a row that completed keeps its violations")
	assert.NotZero(t, results, "and its results")
}

// TestResetInFlightTasks_ClearsTheLostAttemptsAssertionOutcome is the same rule
// for owner takeover and run resumption, with a succeeded sibling that must
// keep its outcome.
func TestResetInFlightTasks_ClearsTheLostAttemptsAssertionOutcome(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	stampOutcome(t, db, taskRunID)

	sibling := models.TaskRun{
		ID: uuid.New(), JobRunID: taskRun.JobRunID, TaskID: taskRun.TaskID, AtomID: taskRun.AtomID,
		Engine: taskRun.Engine, Image: taskRun.Image, Command: "[]",
		Status: string(TaskStatusSucceeded), PartitionIndex: 1,
	}
	require.NoError(t, db.Create(&sibling).Error)
	stampOutcome(t, db, sibling.ID)

	require.NoError(t, store.ResetInFlightTasks(taskRun.JobRunID))

	violations, results := outcomeColumns(t, db, taskRunID)
	assert.Zero(t, violations)
	assert.Zero(t, results)
	violations, results = outcomeColumns(t, db, sibling.ID)
	assert.NotZero(t, violations, "a completed sibling keeps its violations")
	assert.NotZero(t, results, "and its results")
}
