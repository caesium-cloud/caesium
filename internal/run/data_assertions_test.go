package run

import (
	"context"
	"strconv"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setDataAssertions flips the master gate for one test. env.Variables() serves a
// cached parse, so the environment is re-processed on set and on cleanup.
func setDataAssertions(t *testing.T, enabled bool) {
	t.Helper()
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", strconv.FormatBool(enabled))
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })
	require.Equal(t, enabled, DataAssertionsEnabled(), "the gate must be %v for this test to prove anything", enabled)
}

func declareProduces(t *testing.T, db *gorm.DB, jobID uuid.UUID, stepName string, names ...string) {
	t.Helper()
	for _, name := range names {
		require.NoError(t, db.Create(&models.DatasetDeclaration{
			ID:        uuid.New(),
			JobID:     jobID,
			JobAlias:  "alias",
			StepName:  stepName,
			Name:      name,
			Direction: models.DatasetDirectionProduces,
		}).Error)
	}
}

func metricRows(t *testing.T, db *gorm.DB) []models.DatasetMetric {
	t.Helper()
	var rows []models.DatasetMetric
	require.NoError(t, db.Order("metric ASC").Find(&rows).Error)
	return rows
}

func TestEvaluateDataAssertions_PersistsSamplesAgainstTheInstanceRow(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProduces(t, db, jobID, stepName, "warehouse/orders")

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(context.Background(), store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 42},
		// An undeclared METRIC is still recorded: free baseline history for an
		// assertion added later.
		{Dataset: "warehouse/orders", Metric: "dedup_ratio", Value: 0.01},
	}))
	require.NoError(t, err)

	rows := metricRows(t, db)
	require.Len(t, rows, 2)
	assert.Equal(t, "dedup_ratio", rows[0].Metric)
	assert.Equal(t, "rowCount", rows[1].Metric)
	for _, row := range rows {
		assert.Equal(t, taskRunID, row.TaskRunID, "samples attribute to THIS instance, not the catalog task")
		assert.Equal(t, "warehouse/orders", row.Name)
		assert.Equal(t, "", row.Namespace)
		assert.False(t, row.CreatedAt.IsZero())
	}
}

// TestEvaluateDataAssertions_UnfannedPathResolvesByCatalogTask covers the local
// executor's unfanned call site, which passes uuid.Nil for the instance id.
func TestEvaluateDataAssertions_UnfannedPathResolvesByCatalogTask(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProduces(t, db, jobID, stepName, "warehouse/orders")
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, EvaluateDataAssertions(context.Background(), store, taskRun.JobRunID, taskID, uuid.Nil,
		CapturedMetrics([]pkgtask.DatasetMetricSample{{Metric: "rowCount", Value: 7}})))

	rows := metricRows(t, db)
	require.Len(t, rows, 1)
	assert.Equal(t, taskRunID, rows[0].TaskRunID)
	assert.Equal(t, "warehouse/orders", rows[0].Name, "an omitted selector resolves to the sole declared dataset")
}

func TestEvaluateDataAssertions_AmbiguousSelectorDropsTheSample(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProduces(t, db, jobID, stepName, "warehouse/orders", "warehouse/customers")
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, EvaluateDataAssertions(context.Background(), store, taskRun.JobRunID, taskID, taskRunID,
		CapturedMetrics([]pkgtask.DatasetMetricSample{
			{Metric: "rowCount", Value: 7},
			{Dataset: "warehouse/orders", Metric: "rowCount", Value: 9},
		})))

	rows := metricRows(t, db)
	require.Len(t, rows, 1, "the ambiguous sample is dropped, the explicit one is kept")
	assert.Equal(t, "warehouse/orders", rows[0].Name)
	assert.InDelta(t, 9, rows[0].Value, 0.001)
}

func TestEvaluateDataAssertions_QuarantinedRunRecordsNothing(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), true)
	declareProduces(t, db, jobID, stepName, "warehouse/orders")
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, EvaluateDataAssertions(context.Background(), store, taskRun.JobRunID, taskID, taskRunID,
		CapturedMetrics([]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 7}})))

	assert.Empty(t, metricRows(t, db), "a what-if must never move a baseline")
}

func TestEvaluateDataAssertions_InertWhenFlagOff(t *testing.T) {
	setDataAssertions(t, false)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProduces(t, db, jobID, stepName, "warehouse/orders")
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, EvaluateDataAssertions(context.Background(), store, taskRun.JobRunID, taskID, taskRunID,
		CapturedMetrics([]pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 7}})))

	assert.Empty(t, metricRows(t, db), "off means no metrics persistence at all")
}

func TestEvaluateDataAssertions_NilStoreAndNoSamplesAreNoOps(t *testing.T) {
	setDataAssertions(t, true)
	require.NoError(t, EvaluateDataAssertions(context.Background(), nil, uuid.New(), uuid.New(), uuid.New(),
		CapturedMetrics([]pkgtask.DatasetMetricSample{{Metric: "rowCount", Value: 1}})))
	require.NoError(t, EvaluateDataAssertions(context.Background(), nil, uuid.New(), uuid.New(), uuid.New(), MetricsCapture{}))
}

func TestDataAssertionsCanceledContextStopsInitialAcquisition(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, taskID, rowID, name := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProduces(t, db, jobID, name, "orders")
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", rowID).Error)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, EvaluateDataAssertions(ctx, store, row.JobRunID, taskID, rowID, CapturedMetrics([]pkgtask.DatasetMetricSample{{Dataset: "orders", Metric: "rowCount", Value: 12}})))
	require.Empty(t, metricRows(t, db))
	require.Empty(t, dataViolationsOf(t, db, rowID))
	written, err := store.saveDataViolationsClaimed(ctx, row.JobRunID, rowID, nil, []DataViolation{{Dataset: "orders", Assertion: AssertionMin}})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, written)
}

func TestDataViolationWriteCanceledAfterLookupLeavesOldEvidence(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	_, _, rowID, _ := seedTaskRun(t, db, string(TaskStatusRunning), false)
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", rowID).Error)
	old := []DataViolation{{Dataset: "old", Assertion: AssertionMin}}
	require.NoError(t, store.SaveDataViolations(row.JobRunID, rowID, old))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:cancel_violation_update", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*models.TaskRun); ok {
			cancel()
		}
	}))
	written, err := store.saveDataViolationsClaimed(ctx, row.JobRunID, rowID, nil, []DataViolation{{Dataset: "new", Assertion: AssertionMin}})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, written)
	require.NoError(t, db.Callback().Query().Remove("test:cancel_violation_update"))
	require.Equal(t, old, dataViolationsOf(t, db, rowID))
}

func TestDataAssertionsCancellationDuringHoldFailsClosed(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, taskID, rowID, name := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, name, "orders", "auto")
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", rowID).Error)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempted := false
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:cancel_hold", func(tx *gorm.DB) {
		if tx.Statement.Table == "dataset_holds" {
			attempted = true
			cancel()
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove("test:cancel_hold") })
	err := EvaluateDataAssertions(ctx, store, row.JobRunID, taskID, rowID, CapturedMetrics([]pkgtask.DatasetMetricSample{{Dataset: "orders", Metric: "rowCount", Value: 12}}))
	require.True(t, attempted)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "hold could not be opened")
	require.Empty(t, activeHolds(t, db, "orders"))
	require.Empty(t, dataViolationsOf(t, db, rowID), "best-effort violation persistence can be lost after cancellation")
	require.Len(t, metricRows(t, db), 1, "the sample committed before cancellation remains")
}
