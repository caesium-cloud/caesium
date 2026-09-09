package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// claimTaskRun stamps a claim onto a seeded TaskRun row the way the worker's
// claimer does: a holder and WHICH of that holder's claims this is
// (claim_attempt is `claim_attempt + 1` in the claimer's single-statement
// UPDATE), plus the running status the row carries for the whole attempt.
func claimTaskRun(t *testing.T, db *gorm.DB, taskRunID uuid.UUID, claim TaskClaim) {
	t.Helper()
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).Updates(map[string]any{
		"claimed_by":       claim.ClaimedBy,
		"claim_attempt":    claim.ClaimAttempt,
		"claim_expires_at": time.Now().UTC().Add(time.Minute),
		"status":           string(TaskStatusRunning),
	}).Error)
}

// sampleRow builds one DatasetMetric the way persistDatasetMetrics does, so the
// store-level fence tests exercise the real row shape.
func sampleRow(taskRunID uuid.UUID, value float64) models.DatasetMetric {
	return models.DatasetMetric{
		ID:        uuid.New(),
		TaskRunID: taskRunID,
		Name:      "warehouse/orders",
		Metric:    "rowCount",
		Value:     value,
	}
}

// fencedInsert drives insertDatasetMetricsFencedTx through a real transaction,
// exactly as releaseHoldsForCleanRun's no-hold branch does.
func fencedInsert(t *testing.T, db *gorm.DB, taskRunID uuid.UUID, claim *TaskClaim, rows []models.DatasetMetric) bool {
	t.Helper()
	inserted := false
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		inserted, err = insertDatasetMetricsFencedTx(tx, taskRunID, claim, rows)
		return err
	}))
	return inserted
}

// TestInsertDatasetMetricsFenced_MatchingClaimInserts is the happy path: the
// worker still holds the exact claim it took at dispatch, so its samples are
// history.
func TestInsertDatasetMetricsFenced_MatchingClaimInserts(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	claim := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, claim)

	require.True(t, fencedInsert(t, db, taskRunID, &claim, []models.DatasetMetric{sampleRow(taskRunID, 100)}))

	rows := metricRows(t, db)
	require.Len(t, rows, 1)
	assert.InDelta(t, 100, rows[0].Value, 0.001)
}

// TestInsertDatasetMetricsFenced_ReclaimedClaimDropsTheSamples is the fence
// doing its job: the row was reclaimed and belongs to another worker now, so
// the superseded attempt writes NOTHING — not a flagged row, not a row at all.
func TestInsertDatasetMetricsFenced_ReclaimedClaimDropsTheSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2})

	require.False(t, fencedInsert(t, db, taskRunID, &stale, []models.DatasetMetric{sampleRow(taskRunID, 100)}))
	assert.Empty(t, metricRows(t, db), "a superseded worker must not append to a row it no longer owns")
}

// TestInsertDatasetMetricsFenced_UnclaimedRowDropsTheSamples covers the window
// between the reset and the next claim: ResetInFlightTasks and
// ReclaimOwnerExpiredClaims both clear claimed_by, so a row that is re-pending
// and not yet re-claimed matches nobody's claim.
func TestInsertDatasetMetricsFenced_UnclaimedRowDropsTheSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).Updates(map[string]any{
		"claimed_by":       "",
		"claim_expires_at": nil,
		"status":           string(TaskStatusPending),
	}).Error)

	require.False(t, fencedInsert(t, db, taskRunID, &stale, []models.DatasetMetric{sampleRow(taskRunID, 100)}))
	assert.Empty(t, metricRows(t, db))
}

// TestInsertDatasetMetricsFenced_SameWorkerNewClaimDropsTheSamples is the ABA
// case, and the reason the fence carries claim_attempt rather than the holder's
// name alone: a single-worker lane can lose a lease and re-claim the SAME row
// with the SAME worker id. Fencing on claimed_by only would let that worker's
// superseded attempt write onto its own new attempt.
func TestInsertDatasetMetricsFenced_SameWorkerNewClaimDropsTheSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 2})

	require.False(t, fencedInsert(t, db, taskRunID, &stale, []models.DatasetMetric{sampleRow(taskRunID, 100)}))
	assert.Empty(t, metricRows(t, db))
}

// TestInsertDatasetMetricsFenced_MissingRowDropsTheSamples pins that a row
// pruned or deleted out from under an in-flight attempt reads as a mismatch and
// not as an error: the samples have nothing to hang off, and the task is not
// the thing that broke.
func TestInsertDatasetMetricsFenced_MissingRowDropsTheSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	require.False(t, fencedInsert(t, db, uuid.New(), &stale, []models.DatasetMetric{sampleRow(uuid.New(), 100)}))
	assert.Empty(t, metricRows(t, db))
}

// TestInsertDatasetMetricsFenced_NilClaimIsTheUnfencedLocalPath pins that the
// LOCAL executor (internal/job, enforceClaim=false) is untouched. It holds no
// claim, cannot be superseded, and must still record its samples on a row whose
// claimed_by is empty — the state every locally executed TaskRun is in.
func TestInsertDatasetMetricsFenced_NilClaimIsTheUnfencedLocalPath(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	// Deliberately claimed by somebody else: an unfenced insert does not look.
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 7})

	require.True(t, fencedInsert(t, db, taskRunID, nil, []models.DatasetMetric{sampleRow(taskRunID, 100)}))
	require.Len(t, metricRows(t, db), 1)

	// And the exported unfenced entry point behaves identically.
	require.NoError(t, InsertDatasetMetrics(context.Background(), db, []models.DatasetMetric{sampleRow(taskRunID, 200)}))
	assert.Len(t, metricRows(t, db), 2)
}

// TestEvaluateDataAssertionsClaimed_LateInsertAfterReclaimLeavesOneSampleSet
// reproduces the exact distributed race issue #438 names, end to end through
// the real seam and the real reset path:
//
//	worker A claims the row and starts executing
//	A's lease expires; ResetInFlightTasks re-pends the row and clears its samples
//	worker B claims the row, re-executes, and its seam records ITS sample
//	A finally reaches its own post-task seam and tries to record too
//
// Before the fence, A's late insert landed and the (dataset, metric) baseline
// carried TWO samples for one logical run — one of them from an attempt whose
// completion was itself claim-rejected and which exists in no other table.
func TestEvaluateDataAssertionsClaimed_LateInsertAfterReclaimLeavesOneSampleSet(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1)}},
		jobdef.DatasetOnViolationWarn)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	runID := row.JobRunID

	// Worker A claims and starts executing.
	claimA := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, claimA)

	// A's lease expires while its container is still finishing: the owner
	// takeover re-pends every in-flight row of the run and clears the samples
	// of the attempt that is about to run again.
	require.NoError(t, store.ResetInFlightTasks(runID))
	require.Empty(t, metricRows(t, db))

	// Worker B claims the same row and re-executes it to completion.
	claimB := TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2}
	claimTaskRun(t, db, taskRunID, claimB)
	require.NoError(t, EvaluateDataAssertionsClaimed(store, runID, taskID, taskRunID, &claimB,
		[]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 200}}))
	require.Len(t, metricRows(t, db), 1)

	// A finally reaches its post-task seam. Its container really did run and
	// really did emit a sample — it just does not own this row any more.
	before := metricstestutil.CounterValue(t, metrics.DatasetMetricsDroppedTotal, datasetMetricDropStaleClaim)
	require.NoError(t, EvaluateDataAssertionsClaimed(store, runID, taskID, taskRunID, &claimA,
		[]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 100}}))
	assert.Equal(t, before+1, metricstestutil.CounterValue(t, metrics.DatasetMetricsDroppedTotal, datasetMetricDropStaleClaim),
		"a dropped sample set is counted, not silent")

	rows := metricRows(t, db)
	require.Len(t, rows, 1, "one logical run contributes exactly one sample set")
	assert.InDelta(t, 200, rows[0].Value, 0.001, "and it is the sample of the worker that owns the row")

	// The row lands succeeded once B's completion is accepted: the baseline the
	// next run is judged against must then hold exactly B's observation.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).
		Update("status", string(TaskStatusSucceeded)).Error)
	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Samples)
	assert.InDelta(t, 200, stats.Median, 0.001, "the superseded attempt must not move the baseline")
}

// TestEvaluateDataAssertionsClaimed_HoldReleaseIsFencedToo pins that the fence
// covers the WHOLE post-task transaction, not just the INSERT. The samples are
// the evidence the contract passed; releasing a hold on evidence whose sample
// was refused would separate "the dataset recovered" from "here is the proof",
// which is exactly what persistDatasetMetrics' one transaction exists to
// prevent. Worker B's re-execution produces both.
func TestEvaluateDataAssertionsClaimed_HoldReleaseIsFencedToo(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	// A breach of this job opens the hold (the local, unfenced path — this is
	// the setup, not the subject).
	jobID, breachTaskID, breachTaskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)
	require.NoError(t, emitSamples(t, store, db, breachTaskID, breachTaskRunID,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}))
	require.Len(t, activeHolds(t, db, "warehouse/orders"), 1)

	// The next run of the same job is clean, and would release the hold. Worker
	// A claimed it, lost the lease, and worker B owns the row now.
	cleanRunID, cleanTaskID, cleanTaskRunID, _ := seedTaskRunForJob(t, db, jobID, stepName, string(TaskStatusRunning))
	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, cleanTaskRunID, stale)
	current := TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2}
	claimTaskRun(t, db, cleanTaskRunID, current)

	clean := []pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 5000}}

	// A's late seam: no sample, and therefore no release either.
	require.NoError(t, EvaluateDataAssertionsClaimed(store, cleanRunID, cleanTaskID, cleanTaskRunID, &stale, clean))
	assert.Len(t, metricRowsFor(t, db, cleanTaskRunID), 0, "the superseded attempt's evidence is dropped")
	assert.Len(t, activeHolds(t, db, "warehouse/orders"), 1,
		"and the hold it would have released stays held: the release travels with the evidence")

	// B's seam, on the same row, releases it — so the fence is what stopped the
	// write above, not a missing precondition.
	require.NoError(t, EvaluateDataAssertionsClaimed(store, cleanRunID, cleanTaskID, cleanTaskRunID, &current, clean))
	assert.Len(t, metricRowsFor(t, db, cleanTaskRunID), 1)
	assert.Empty(t, activeHolds(t, db, "warehouse/orders"))
}

// metricRowsFor reads the samples hanging off ONE task run, which the hold
// scenario needs because the breaching run left a sample of its own.
func metricRowsFor(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) []models.DatasetMetric {
	t.Helper()
	var rows []models.DatasetMetric
	require.NoError(t, db.Where("task_run_id = ?", taskRunID).Find(&rows).Error)
	return rows
}

// TestRetryTaskClaimedInstanceKeepsTheClaimTheFenceChecks is the fence's
// compatibility guard with the IN-WORKER retry, and the regression it would
// otherwise invite: RetryTaskClaimedInstance deliberately does NOT release the
// claim (the worker is about to launch the next container itself), and
// retryResetColumns does not touch claim_attempt. If either ever changed, every
// retried attempt in distributed mode would silently drop its samples — a green
// run recording nothing, which is precisely the failure mode this feature must
// not have.
func TestRetryTaskClaimedInstanceKeepsTheClaimTheFenceChecks(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1)}},
		jobdef.DatasetOnViolationWarn)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	claim := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 3}
	claimTaskRun(t, db, taskRunID, claim)

	require.NoError(t, store.RetryTaskClaimedInstance(row.JobRunID, taskRunID, 2, claim.ClaimedBy))

	// The worker still holds the claim it took at dispatch, so attempt 2's
	// samples are accepted.
	require.NoError(t, EvaluateDataAssertionsClaimed(store, row.JobRunID, taskID, taskRunID, &claim,
		[]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 42}}))
	rows := metricRows(t, db)
	require.Len(t, rows, 1, "an in-worker retry keeps the claim, so the fence must accept the next attempt")
	assert.InDelta(t, 42, rows[0].Value, 0.001)
}

// TestMetricFenceLockSQL_PerDialect pins the dialect split the fence's
// check-then-insert depends on: Postgres runs real concurrent writers at READ
// COMMITTED and needs the row lock, dqlite and SQLite serialize writers already
// and cannot parse FOR UPDATE, and an unknown backend is an error rather than a
// silently missing guard.
func TestMetricFenceLockSQL_PerDialect(t *testing.T) {
	stmt, err := metricFenceLockSQL("postgres")
	require.NoError(t, err)
	assert.Contains(t, stmt, "FOR UPDATE")

	for _, dialect := range []string{"dqlite", "sqlite", "sqlite3"} {
		stmt, err := metricFenceLockSQL(dialect)
		require.NoError(t, err)
		assert.Empty(t, stmt, "%s serializes writers already", dialect)
	}

	_, err = metricFenceLockSQL("cockroach")
	assert.Error(t, err, "an unrecognised dialect must surface the missing guard")
}

// TestEvaluateDataAssertionsClaimed_StaleClaimOpensNoHoldAndWritesNoViolations
// is the maintainer's P1 regression (review of #453). The metric fence stopped
// the superseded worker's SAMPLE, but the seam ran dispatch anyway: the stale
// attempt still opened a DatasetHold and wrote DataViolations onto the
// REPLACEMENT attempt's row. A hold opened on evidence the fence had just
// refused can block every downstream consumer after the worker that owns the
// row has already recovered the dataset — the breaker firing on data nobody
// accepted.
//
// The reviewer's exact scenario: the row is claimed by worker-b#2, worker-a#1
// submits rowCount=12 against `min: 1000` / `onViolation: hold`.
func TestEvaluateDataAssertionsClaimed_StaleClaimOpensNoHoldAndWritesNoViolations(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)

	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2})

	breaching := []pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}}
	require.NoError(t, EvaluateDataAssertionsClaimed(store, row.JobRunID, taskID, taskRunID, &stale, breaching))

	assert.Empty(t, metricRows(t, db), "the superseded worker's sample is refused")
	assert.Empty(t, activeHolds(t, db, "warehouse/orders"),
		"and it must not break the circuit either: the hold would outlive the attempt that was rejected")
	assert.Nil(t, dataViolationsOf(t, db, taskRunID),
		"nor write its verdict onto the replacement attempt's row")
}

// TestEvaluateDataAssertionsClaimed_StaleClaimWithNoSamplesOpensNoHold is the
// same rule on the path that emits NOTHING. A `missing` verdict needs no
// sample, so the metric insert the fence guards never happens and the sample
// write cannot be what stops it — the dispatch writes have to be fenced in
// their own right.
func TestEvaluateDataAssertionsClaimed_StaleClaimWithNoSamplesOpensNoHold(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)

	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2})

	// No samples at all: the declared rowCount assertion is `missing`.
	require.NoError(t, EvaluateDataAssertionsClaimed(store, row.JobRunID, taskID, taskRunID, &stale, nil))

	assert.Empty(t, activeHolds(t, db, "warehouse/orders"))
	assert.Nil(t, dataViolationsOf(t, db, taskRunID))
}

// TestEvaluateDataAssertionsClaimed_LiveClaimStillOpensTheHold is the other
// half, and the one that would catch a fence turned into a kill switch: the
// SAME breaching sample, submitted by the worker that actually holds the row,
// still records its sample, opens the hold and persists the violation.
func TestEvaluateDataAssertionsClaimed_LiveClaimStillOpensTheHold(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)

	live := TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2}
	claimTaskRun(t, db, taskRunID, live)

	breaching := []pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}}
	require.NoError(t, EvaluateDataAssertionsClaimed(store, row.JobRunID, taskID, taskRunID, &live, breaching))

	require.Len(t, metricRows(t, db), 1, "the holder's sample is history")
	require.Len(t, activeHolds(t, db, "warehouse/orders"), 1, "and the circuit still breaks")
	assert.NotNil(t, dataViolationsOf(t, db, taskRunID), "and the verdict is on the row")
}

// TestEvaluateDataAssertions_LocalPathStillOpensTheHold pins that the local
// executor (internal/job, enforceClaim=false) is untouched by any of this: it
// holds no claim, so every dispatch write must go through unfenced.
func TestEvaluateDataAssertions_LocalPathStillOpensTheHold(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)

	require.NoError(t, EvaluateDataAssertions(store, row.JobRunID, taskID, taskRunID,
		[]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}}))

	require.Len(t, metricRows(t, db), 1)
	require.Len(t, activeHolds(t, db, "warehouse/orders"), 1)
	assert.NotNil(t, dataViolationsOf(t, db, taskRunID))
}
