package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvaluateDataAssertions_ViolatingSampleIsExcludedFromTheNextBaseline is
// the baseline-hygiene regression: under the default `warn` disposition a
// violating run still succeeds, so without the Violated flag its rejected value
// would enter the very baseline that is supposed to catch the next one — three
// warn anomalies in a row would move the median far enough to silence the
// assertion during exactly the incident it exists to report.
func TestEvaluateDataAssertions_ViolatingSampleIsExcludedFromTheNextBaseline(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationWarn)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, []pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 10},
		// An unasserted metric on the same dataset is not collateral damage:
		// only the metric an enforced violation named is flagged.
		{Dataset: "warehouse/orders", Metric: "dedup_ratio", Value: 0.01},
	}))

	rows := metricRows(t, db)
	require.Len(t, rows, 2)
	byMetric := make(map[string]models.DatasetMetric, len(rows))
	for _, row := range rows {
		byMetric[row.Metric] = row
	}
	assert.True(t, byMetric["rowCount"].Violated, "the rejected value is flagged")
	assert.False(t, byMetric["dedup_ratio"].Violated, "an unasserted metric is untouched")

	// The task run succeeded and is not quarantined, so the flag is the only
	// thing keeping the rejected value out of the next run's baseline.
	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Samples, "a rejected value must never become the normal it is next compared against")

	stats, err = Baseline(context.Background(), db, "", "warehouse/orders", "dedup_ratio", 20, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Samples, "an unrejected sample is still history")
}

// TestEvaluateDataAssertions_SeedingSampleStaysCleanHistory is the other side
// of that rule: a cold-start verdict was measured against a baseline too short
// to trust, so the value was never really judged and must remain history.
func TestEvaluateDataAssertions_SeedingSampleStaysCleanHistory(t *testing.T) {
	setDataAssertions(t, true)
	setBaselineMinSamples(t, 5)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, priorRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, priorRunID, "warehouse/orders", "rowCount", 10000, time.Now().Add(-time.Hour))

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	require.NoError(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, []pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 10},
	}))

	for _, row := range metricRows(t, db) {
		assert.False(t, row.Violated, "a seeding verdict does not reject its sample")
	}
	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, stats.Samples, "both the prior sample and the seeding one are history")
}

// TestEvaluateDataAssertions_RetryDiscardsThePriorAttemptsSamples drives the
// REAL retry path rather than the reset map: an onViolation: fail verdict IS an
// attempt failure, the executors retry the same TaskRun row, and without
// clearing the prior attempt's dataset_metrics rows the rejected value would
// come back as clean history through the very retry it caused — one logical run
// contributing two samples, one of them the anomaly.
func TestEvaluateDataAssertions_RetryDiscardsThePriorAttemptsSamples(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	// Attempt 1: the container succeeded but the data is bad, so the seam fails
	// the attempt — after having recorded the sample.
	require.Error(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, []pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 3},
	}))
	require.Len(t, metricRows(t, db), 1)

	// The executors' retry path, unchanged: same row, next attempt.
	require.NoError(t, store.RetryTaskInstance(taskRun.JobRunID, taskRunID, 2))
	assert.Empty(t, metricRows(t, db), "the rejected attempt's samples go with its columns")
	assert.Nil(t, dataViolationsOf(t, db, taskRunID), "and so do its verdicts")

	// Attempt 2 passes.
	require.NoError(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, []pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 10400},
	}))

	rows := metricRows(t, db)
	require.Len(t, rows, 1, "one logical run contributes exactly one sample")
	assert.InDelta(t, 10400, rows[0].Value, 0.001)

	// The row lands succeeded, as it does once the retried attempt completes:
	// the baseline must then see the clean attempt only.
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).
		Update("status", string(TaskStatusSucceeded)).Error)
	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, stats.Samples)
	assert.InDelta(t, 10400, stats.Median, 0.001, "the rejected 3 must not drag the median")
}

// TestEvaluateDataAssertions_PersistsTheDeclarationNamespace pins that a sample
// is written under the SAME namespace the baseline read queries it by. Both are
// empty in v1, so a divergence would only surface the day namespaces are
// populated — as every namespaced deltaFromBaseline silently reading zero
// samples and never firing again.
func TestEvaluateDataAssertions_PersistsTheDeclarationNamespace(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	namespace := "warehouse"
	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID:        uuid.New(),
		JobID:     jobID,
		JobAlias:  "alias",
		StepName:  stepName,
		Namespace: &namespace,
		Name:      "orders",
		Direction: models.DatasetDirectionProduces,
	}).Error)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	require.NoError(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, []pkgtask.DatasetMetricSample{
		{Metric: "rowCount", Value: 42},
	}))

	rows := metricRows(t, db)
	require.Len(t, rows, 1)
	assert.Equal(t, namespace, rows[0].Namespace)
	assert.Equal(t, "orders", rows[0].Name)

	stats, err := Baseline(context.Background(), db, namespace, "orders", "rowCount", 20, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Samples, "the write and the read must agree on the namespace")
}

// TestBaselineMetricsOnlyCoversDeltaAssertions pins that no baseline query is
// issued for a bound that cannot use one: min/max enforce from run one and the
// freshness assertion measures lag from the evaluation instant.
func TestBaselineMetricsOnlyCoversDeltaAssertions(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{
		RowCount:  &jobdef.AssertionSpec{Min: ptrOf(1), DeltaFromBaseline: "50%"},
		NullRate:  &jobdef.AssertionSpec{Max: ptrOf(0.02)},
		Freshness: &jobdef.FreshnessAssertion{Watermark: "max_event_time", MaxLag: "26h"},
		Custom:    []jobdef.AssertionSpec{{Metric: "dedup_ratio", Max: ptrOf(0.05)}},
	}
	assert.Equal(t, []string{"rowCount"}, BaselineMetrics(assertions))
	assert.Nil(t, BaselineMetrics(nil))

	// AssertionMetrics still reports everything the contract READS — that is
	// the "missing metric is a violation" set, a different question.
	assert.Equal(t, []string{"dedup_ratio", "max_event_time", "nullRate", "rowCount"}, AssertionMetrics(assertions))
}

// TestEvaluateDataAssertions_DedupesOneSamplePerMetric pins that a metric
// emitted twice for the same dataset — once with an explicit selector, once
// relying on the sole-declared-dataset default — is ONE row, matching the one
// verdict the evaluator computes from it. Two rows would give a single logical
// run two baseline samples for one metric, one of them never judged.
func TestEvaluateDataAssertions_DedupesOneSamplePerMetric(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProduces(t, db, jobID, stepName, "warehouse/orders")

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	require.NoError(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, []pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 5},
		{Metric: "rowCount", Value: 7},
	}))

	rows := metricRows(t, db)
	require.Len(t, rows, 1, "one (dataset, metric) is one sample")
	assert.InDelta(t, 7, rows[0].Value, 0.001, "last write wins, as it does for the evaluated value")
}

// TestReclaimOwnerExpiredClaims_ClearsTheLostAttemptsSamples is the failover
// twin of the retry test: a worker that dies between the seam's insert and its
// completion report leaves samples on a row another worker then re-runs, so the
// reclaim must take them with it or the baseline reads two sets of samples as
// clean history of one logical run.
func TestReclaimOwnerExpiredClaims_ClearsTheLostAttemptsSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).Updates(map[string]any{
		"claimed_by":       "worker-a",
		"claim_expires_at": time.Now().UTC().Add(-time.Minute),
		"owner_generation": 1,
	}).Error)
	seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", 3, time.Now().Add(-time.Second))

	reset, err := store.ReclaimOwnerExpiredClaims(taskRun.JobRunID, 1)
	require.NoError(t, err)
	require.Len(t, reset, 1)
	assert.Empty(t, metricRows(t, db), "the lost attempt's samples are reset with its columns")
}

// TestResetInFlightTasks_ClearsTheLostAttemptsSamples covers the other failover
// reset — owner takeover and run resumption after a restart.
func TestResetInFlightTasks_ClearsTheLostAttemptsSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", 3, time.Now().Add(-time.Second))

	require.NoError(t, store.ResetInFlightTasks(taskRun.JobRunID))

	var reset models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&reset).Error)
	assert.Equal(t, string(TaskStatusPending), reset.Status, "the row is re-pended exactly as before")
	assert.Equal(t, "", reset.ClaimedBy)
	assert.Empty(t, metricRows(t, db), "and its samples go with it")
}
