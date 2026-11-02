package run

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// resultsOf reads back the per-check results the evaluator persisted on one
// task run.
func resultsOf(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) []AssertionResult {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	return decodeAssertionResults(row.DataAssertionResults)
}

func rowCountSample(value float64) MetricsCapture {
	return CapturedMetrics([]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: value}})
}

func evalOne(t *testing.T, db *gorm.DB, store *Store, taskID, taskRunID uuid.UUID, claim *TaskClaim, capture MetricsCapture) {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	require.NoError(t, EvaluateDataAssertionsClaimed(context.Background(), store, row.JobRunID, taskID, taskRunID, claim, capture))
}

func TestEvaluateDataAssertions_RecordsPassesOnACleanTask(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000), Max: ptrOf(5000)}},
		jobdef.DatasetOnViolationFail)

	evalOne(t, db, store, taskID, taskRunID, nil, rowCountSample(1200))

	assert.Nil(t, dataViolationsOf(t, db, taskRunID), "a clean task still records no violation")
	results := resultsOf(t, db, taskRunID)
	require.Len(t, results, 2)
	for _, r := range results {
		assert.Equal(t, AssertionOutcomePassed, r.Outcome)
		assert.Equal(t, "warehouse/orders", r.Dataset)
		require.NotNil(t, r.Observed)
		assert.InDelta(t, 1200, *r.Observed, 0.001)
	}
}

func TestEvaluateDataAssertions_RecordsPassesBesideTheViolationInOneWrite(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{
			RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)},
			Custom:   []jobdef.AssertionSpec{{Metric: "dedup_ratio", Max: ptrOf(0.5)}},
		},
		jobdef.DatasetOnViolationWarn)

	evalOne(t, db, store, taskID, taskRunID, nil, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12},
		{Dataset: "warehouse/orders", Metric: "dedup_ratio", Value: 0.1},
	}))

	require.Len(t, dataViolationsOf(t, db, taskRunID), 1)
	results := resultsOf(t, db, taskRunID)
	require.Len(t, results, 2)
	assert.Equal(t, AssertionOutcomeViolated, resultFor(t, results, "rowCount", AssertionMin).Outcome)
	assert.Equal(t, DispositionWarn, resultFor(t, results, "rowCount", AssertionMin).Disposition)
	assert.True(t, resultFor(t, results, "dedup_ratio", AssertionMax).Clean())
}

// TestEvaluateDataAssertions_WritesNothingForATaskWithoutAContract pins the
// hot-path cost: no assertions declared means no write, so the column stays
// NULL and "no record" cannot be read as "everything passed".
func TestEvaluateDataAssertions_WritesNothingForATaskWithoutAContract(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProduces(t, db, jobID, stepName, "warehouse/orders")

	evalOne(t, db, store, taskID, taskRunID, nil, rowCountSample(12))

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	assert.Empty(t, row.DataAssertionResults)
	assert.Empty(t, row.DataViolations)
}

func TestEvaluateDataAssertions_RecordsNothingWhenTheGateIsOff(t *testing.T) {
	setDataAssertions(t, false)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)

	evalOne(t, db, store, taskID, taskRunID, nil, rowCountSample(1200))
	assert.Nil(t, resultsOf(t, db, taskRunID))
}

func TestEvaluateDataAssertions_RecordsAColdStartDeltaAsSeedingAndALostMetricAsUnavailable(t *testing.T) {
	setDataAssertions(t, true)
	setBaselineMinSamples(t, 5)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{
			RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"},
			NullRate: &jobdef.AssertionSpec{Max: ptrOf(0.1)},
		}, jobdef.DatasetOnViolationFail)
	// Three clean prior samples: a baseline, but below the floor of five.
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		_, _, priorRun, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		seedMetric(t, db, priorRun, "warehouse/orders", "rowCount", 100, base.Add(time.Duration(i)*time.Minute))
	}

	evalOne(t, db, store, taskID, taskRunID, nil, MetricsCapture{
		Samples:   []pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 110}},
		Truncated: true,
	})

	results := resultsOf(t, db, taskRunID)
	require.Len(t, results, 2)
	delta := resultFor(t, results, "rowCount", AssertionDeltaFromBaseline)
	assert.Equal(t, AssertionOutcomePassed, delta.Outcome)
	assert.True(t, delta.Seeding, "a pass against 3 of 5 samples must not be recorded as a clean pass")
	assert.Equal(t, 3, delta.BaselineSamples)
	assert.Equal(t, BaselineScopeNamespace, delta.BaselineScope)
	lost := resultFor(t, results, "nullRate", AssertionUnavailable)
	assert.Equal(t, AssertionOutcomeUnavailable, lost.Outcome)
	assert.Equal(t, UnavailableStreamTruncated, lost.Reason)
}

// TestEvaluateDataAssertionsClaimed_StaleClaimRecordsNoResults is the claim
// fence (issue #438) for the new column: a superseded worker must not stamp its
// results on the row another attempt now owns.
func TestEvaluateDataAssertionsClaimed_StaleClaimRecordsNoResults(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)

	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2})

	evalOne(t, db, store, taskID, taskRunID, &stale, rowCountSample(1200))
	assert.Nil(t, resultsOf(t, db, taskRunID), "the superseded attempt's pass is not recorded")

	live := TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2}
	evalOne(t, db, store, taskID, taskRunID, &live, rowCountSample(1200))
	require.Len(t, resultsOf(t, db, taskRunID), 1, "the live claim still records, so the fence is not a kill switch")
}

func TestAssertionResultsFollowTheRetryAndFailoverResets(t *testing.T) {
	assert.Contains(t, retryResetColumns(), "data_assertion_results")
	assert.Nil(t, retryResetColumns()["data_assertion_results"])
}

// --- the read ---------------------------------------------------------------

// addInstance adds a sibling TaskRun of an existing task in the same run, the
// way fan-out expansion does.
func addInstance(t *testing.T, db *gorm.DB, like uuid.UUID, partition string, index int, mutate func(*models.TaskRun)) uuid.UUID {
	t.Helper()
	var base models.TaskRun
	require.NoError(t, db.Where("id = ?", like).First(&base).Error)
	sibling := models.TaskRun{
		ID: uuid.New(), JobRunID: base.JobRunID, TaskID: base.TaskID, AtomID: base.AtomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: "[]",
		Status: string(TaskStatusSucceeded), PartitionValue: partition, PartitionIndex: index, PartitionCount: 3,
	}
	if mutate != nil {
		mutate(&sibling)
	}
	require.NoError(t, db.Create(&sibling).Error)
	return sibling.ID
}

func setPartition(t *testing.T, db *gorm.DB, id uuid.UUID, partition string, index int) {
	t.Helper()
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", id).
		Updates(map[string]any{"partition_value": partition, "partition_index": index, "partition_count": 3}).Error)
}

func runIDOf(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) (jobID, runID uuid.UUID) {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	var run models.JobRun
	require.NoError(t, db.Where("id = ?", row.JobRunID).First(&run).Error)
	return run.JobID, run.ID
}

// TestRunAssertions_AttributesEachInstanceItsOwnVerdict: a fan-out step has one
// verdict per partition, and a group never borrows one instance's answer.
func TestRunAssertions_AttributesEachInstanceItsOwnVerdict(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, first, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)
	setPartition(t, db, first, "eu", 0)
	second := addInstance(t, db, first, "us", 1, func(r *models.TaskRun) { r.Status = string(TaskStatusRunning) })
	third := addInstance(t, db, first, "ap", 2, func(r *models.TaskRun) { r.Status = string(TaskStatusRunning) })

	evalOne(t, db, store, taskID, first, nil, rowCountSample(1500))
	evalOne(t, db, store, taskID, second, nil, rowCountSample(12))
	evalOne(t, db, store, taskID, third, nil, rowCountSample(1500))

	job, runID := runIDOf(t, db, first)
	got, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 1)
	task := got.Tasks[0]
	assert.Equal(t, taskID, task.TaskID)
	assert.True(t, task.Fanned)
	assert.Equal(t, 3, task.InstanceCount)
	assert.Equal(t, map[string]int{VerdictPassed: 2, VerdictViolated: 1}, task.VerdictCounts,
		"the group's counts say 2 passed and 1 violated; no single verdict stands for all three")

	byID := map[uuid.UUID]AssertionInstance{}
	for _, in := range task.Instances {
		byID[in.TaskRunID] = in
	}
	assert.Equal(t, VerdictPassed, byID[first].Verdict)
	assert.Equal(t, "eu", byID[first].PartitionValue)
	assert.Equal(t, VerdictViolated, byID[second].Verdict)
	assert.Equal(t, "us", byID[second].PartitionValue)
	assert.Equal(t, AssertionOutcomeViolated, byID[second].Results[0].Outcome)
	assert.Equal(t, VerdictPassed, byID[third].Verdict)
}

func TestRunAssertions_RetriedInstanceReportsOnlyItsLatestAttempt(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)

	evalOne(t, db, store, taskID, taskRunID, nil, rowCountSample(12))
	job, runID := runIDOf(t, db, taskRunID)
	first, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	assert.Equal(t, VerdictViolated, first.Tasks[0].Instances[0].Verdict)

	// The retry reset, then a second attempt that passes.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).Updates(retryResetColumns()).Error)
	cleared, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	assert.Empty(t, cleared.Tasks, "a reset attempt carries nothing of the previous one")

	evalOne(t, db, store, taskID, taskRunID, nil, rowCountSample(1500))
	second, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	require.Len(t, second.Tasks[0].Instances, 1)
	assert.Equal(t, VerdictPassed, second.Tasks[0].Instances[0].Verdict)
	assert.Nil(t, dataViolationsOf(t, db, taskRunID), "the first attempt's violation is gone with it")
}

func TestRunAssertions_CacheHitRunsNoAssertionsAndSaysSo(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	// Declares a contract, served from cache: no assertion ran.
	jobID, _, cached, stepName := seedTaskRun(t, db, string(TaskStatusCached), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", cached).Update("cache_hit", true).Error)

	job, runID := runIDOf(t, db, cached)
	got, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 1)
	assert.Equal(t, VerdictNotEvaluated, got.Tasks[0].Instances[0].Verdict)
	assert.True(t, got.Tasks[0].Instances[0].CacheHit)
	assert.Empty(t, got.Tasks[0].Instances[0].Results)

	// A cache hit on a step with no contract has nothing to report.
	noContractJob, _, noContract, _ := seedTaskRun(t, db, string(TaskStatusCached), false)
	job2, run2 := runIDOf(t, db, noContract)
	assert.Equal(t, noContractJob, job2)
	none, err := store.RunAssertions(context.Background(), job2, run2)
	require.NoError(t, err)
	assert.Empty(t, none.Tasks)
}

// TestRunAssertions_OldRowsClaimNothingPassed: a row recorded before results
// existed has violations but no results, and a clean one has neither.
func TestRunAssertions_OldRowsClaimNothingPassed(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, _, old, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)
	require.NoError(t, store.SaveDataViolations(runIDFor(t, db, old), old, []DataViolation{{Dataset: "warehouse/orders", Metric: "rowCount", Assertion: AssertionMin, Message: "m"}}))

	job, runID := runIDOf(t, db, old)
	got, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 1)
	assert.Equal(t, VerdictViolated, got.Tasks[0].Instances[0].Verdict)
	assert.Empty(t, got.Tasks[0].Instances[0].Results, "no per-check results to show, and none invented")

	// A succeeded task with a contract but no record at all is simply absent:
	// it must not be rendered as passed.
	_, _, unrecorded, step2 := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	_ = step2
	job2, run2 := runIDOf(t, db, unrecorded)
	empty, err := store.RunAssertions(context.Background(), job2, run2)
	require.NoError(t, err)
	assert.Empty(t, empty.Tasks)
}

func runIDFor(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) uuid.UUID {
	t.Helper()
	_, runID := runIDOf(t, db, taskRunID)
	return runID
}

func TestRunAssertions_WrongJobIsNotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	_, runID := runIDOf(t, db, taskRunID)
	_, err := store.RunAssertions(context.Background(), uuid.New(), runID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "a run addressed under another job's path is not served")
}

// TestRunAssertions_ConfinedReaderIsNotGivenUnownedBaselineEvidence is the
// tenant rule: a result carrying baseline evidence that was not computed over
// the run's own namespace is served without it to a confined reader, and whole
// to an unconfined one.
func TestRunAssertions_ConfinedReaderIsNotGivenUnownedBaselineEvidence(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	median := 4242.0
	legacy := []AssertionResult{
		{Dataset: "warehouse/orders", Metric: "rowCount", Assertion: AssertionDeltaFromBaseline, Outcome: AssertionOutcomePassed,
			BaselineMedian: &median, BaselineSamples: 9, Bound: ptrOf(2121)},
		{Dataset: "warehouse/orders", Metric: "rowCount", Assertion: AssertionMin, Outcome: AssertionOutcomePassed, Bound: ptrOf(1000)},
	}
	require.NoError(t, store.saveResultsForTest(t, taskRunID, legacy))

	job, runID := runIDOf(t, db, taskRunID)
	whole, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	delta := resultFor(t, whole.Tasks[0].Instances[0].Results, "rowCount", AssertionDeltaFromBaseline)
	require.NotNil(t, delta.BaselineMedian)
	assert.InDelta(t, 4242, *delta.BaselineMedian, 0.001)

	confined := authz.WithAllowedNamespaces(context.Background(), []string{"default"})
	shaped, err := store.RunAssertions(confined, job, runID)
	require.NoError(t, err)
	results := shaped.Tasks[0].Instances[0].Results
	delta = resultFor(t, results, "rowCount", AssertionDeltaFromBaseline)
	assert.Nil(t, delta.BaselineMedian)
	assert.Nil(t, delta.Bound)
	assert.True(t, delta.BoundWithheld)
	assert.Zero(t, delta.BaselineSamples)
	min := resultFor(t, results, "rowCount", AssertionMin)
	require.NotNil(t, min.Bound, "an absolute bound is the manifest's, not another owner's baseline")
	assert.InDelta(t, 1000, *min.Bound, 0.001)
}

func (s *Store) saveResultsForTest(t *testing.T, taskRunID uuid.UUID, results []AssertionResult) error {
	t.Helper()
	_, err := s.saveDataAssertionOutcomeClaimed(context.Background(), taskRunID, nil, nil, results)
	return err
}

func TestRunAssertions_CapsAGroupButCountsEveryInstance(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, first, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}}, jobdef.DatasetOnViolationWarn)
	setPartition(t, db, first, "p-0", 0)
	total := MaxAssertionInstances + 20
	ids := []uuid.UUID{first}
	for i := 1; i < total; i++ {
		ids = append(ids, addInstance(t, db, first, fmt.Sprintf("p-%d", i), i, func(r *models.TaskRun) { r.Status = string(TaskStatusRunning) }))
	}
	// The ONLY breach is the very last partition.
	for i, id := range ids {
		value := 1500.0
		if i == total-1 {
			value = 12
		}
		evalOne(t, db, store, taskID, id, nil, rowCountSample(value))
	}

	job, runID := runIDOf(t, db, first)
	got, err := store.RunAssertions(context.Background(), job, runID)
	require.NoError(t, err)
	task := got.Tasks[0]
	assert.True(t, task.InstancesTruncated)
	assert.Len(t, task.Instances, MaxAssertionInstances)
	assert.Equal(t, total, task.InstanceCount)
	assert.Equal(t, map[string]int{VerdictPassed: total - 1, VerdictViolated: 1}, task.VerdictCounts)
	assert.Equal(t, VerdictViolated, task.Instances[len(task.Instances)-1].Verdict,
		"the one breach survives the cap, and the list is back in partition order")
}

// TestSaveDataAssertionOutcomeClaimed_FenceIsInTheUpdateItself pins the write's
// own claim predicate, which the seam's earlier sample fence otherwise hides:
// a claim reclaimed between that fence and this UPDATE must write nothing.
func TestSaveDataAssertionOutcomeClaimed_FenceIsInTheUpdateItself(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2})

	results := []AssertionResult{{Dataset: "d", Metric: "rowCount", Assertion: AssertionMin, Outcome: AssertionOutcomePassed}}
	violations := []DataViolation{{Dataset: "d", Metric: "rowCount", Assertion: AssertionMin, Message: "m"}}

	written, err := store.saveDataAssertionOutcomeClaimed(context.Background(), taskRunID, &stale, violations, results)
	require.NoError(t, err)
	assert.False(t, written, "a superseded claim writes nothing")
	assert.Nil(t, resultsOf(t, db, taskRunID))
	assert.Nil(t, dataViolationsOf(t, db, taskRunID))

	live := TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2}
	written, err = store.saveDataAssertionOutcomeClaimed(context.Background(), taskRunID, &live, violations, results)
	require.NoError(t, err)
	assert.True(t, written)
	assert.Len(t, resultsOf(t, db, taskRunID), 1)
	assert.Len(t, dataViolationsOf(t, db, taskRunID), 1)

	written, err = store.saveDataAssertionOutcomeClaimed(context.Background(), taskRunID, &stale, nil, nil)
	require.NoError(t, err)
	assert.True(t, written, "nothing to record is not a lost claim")
}
