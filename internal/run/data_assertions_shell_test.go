package run

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setBaselineMinSamples pins the cold-start floor for one test. Like
// setDataAssertions it re-processes the environment on set and on cleanup,
// because env.Variables() serves a cached parse.
func setBaselineMinSamples(t *testing.T, n int) {
	t.Helper()
	t.Setenv("CAESIUM_BASELINE_MIN_SAMPLES", strconv.Itoa(n))
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })
	require.Equal(t, n, BaselineMinSamples())
}

// declareProducesWithAssertions writes a produced-dataset registry row carrying
// a declared assertion contract — the same row internal/freshness's
// BuildDeclarations writes on `job apply`, which is where the evaluator reads
// its spec from (never by re-parsing the jobdef).
func declareProducesWithAssertions(
	t *testing.T,
	db *gorm.DB,
	jobID uuid.UUID,
	stepName, name string,
	assertions jobdef.DatasetAssertions,
	onViolation string,
) {
	t.Helper()
	spec, err := json.Marshal(assertions)
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID:             uuid.New(),
		JobID:          jobID,
		JobAlias:       "alias",
		StepName:       stepName,
		Name:           name,
		Direction:      models.DatasetDirectionProduces,
		AssertionsJSON: string(spec),
		OnViolation:    onViolation,
	}).Error)
}

// dataViolationsOf reads back the verdicts the evaluator persisted on one task
// run — the same column the REST task read surface exposes.
func dataViolationsOf(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) []DataViolation {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)
	if len(row.DataViolations) == 0 {
		return nil
	}
	var violations []DataViolation
	require.NoError(t, json.Unmarshal(row.DataViolations, &violations))
	return violations
}

func TestEvaluateDataAssertions_WarnRecordsTheViolationAndPublishesTheEvent(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	bus := event.New()
	store.SetBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	published, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeDataViolationRecorded}})
	require.NoError(t, err)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationWarn)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err = EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12},
	}))
	require.NoError(t, err, "warn mode records the violation without failing the task")

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, "warehouse/orders", violations[0].Dataset)
	assert.Equal(t, "rowCount", violations[0].Metric)
	assert.Equal(t, AssertionMin, violations[0].Assertion)
	require.NotNil(t, violations[0].Observed)
	assert.InDelta(t, 12, *violations[0].Observed, 0.001)

	// The sample is still recorded — Plan 3's backtest replays the bad history
	// and `caesium why` needs the value the breaker rejected — but it is
	// FLAGGED, so it can never become the baseline it just broke.
	rows := metricRows(t, db)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Violated)

	select {
	case evt := <-published:
		assert.Equal(t, event.TypeDataViolationRecorded, evt.Type)
		assert.Equal(t, taskID, evt.TaskID)
		assert.Contains(t, string(evt.Payload), `"violations":1`)
		assert.Contains(t, string(evt.Payload), "warehouse/orders")
	case <-time.After(5 * time.Second):
		t.Fatal("expected a data_violation_recorded event for a warn-mode violation")
	}
}

func TestEvaluateDataAssertions_FailEscalatesNamingTheContract(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{Custom: []jobdef.AssertionSpec{{Metric: "dedup_ratio", Max: ptrOf(0.05)}}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "dedup_ratio", Value: 0.31},
	}))
	require.Error(t, err, "fail mode escalates exactly like schemaValidation: fail")
	assert.Contains(t, err.Error(), "warehouse/orders")
	assert.Contains(t, err.Error(), "dedup_ratio")
	assert.Contains(t, err.Error(), AssertionMax)

	// The violation is persisted even though the task is about to go red — the
	// evidence must outlive the failure message.
	require.Len(t, dataViolationsOf(t, db, taskRunID), 1)
}

func TestEvaluateDataAssertions_MissingDeclaredMetricIsAViolation(t *testing.T) {
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

	// A step that emits NOTHING at all must not silently pass its contract:
	// this is the regression the "declared but never emitted" rule exists for,
	// and the reason the seam no longer short-circuits on zero samples.
	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, MetricsCapture{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rowCount")

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionMissing, violations[0].Assertion)
	assert.Nil(t, violations[0].Observed)
}

func TestEvaluateDataAssertions_ColdStartDeltaIsSeedingAndNeverFails(t *testing.T) {
	setDataAssertions(t, true)
	setBaselineMinSamples(t, 5)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	// Two clean prior samples: real history, but below the cold-start floor.
	for i := 0; i < 2; i++ {
		_, _, priorRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		seedMetric(t, db, priorRunID, "warehouse/orders", "rowCount", 10000, time.Now().Add(-time.Duration(i+1)*time.Hour))
	}

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 10},
	}))
	require.NoError(t, err, "a seeding verdict never escalates, whatever onViolation says")

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionDeltaFromBaseline, violations[0].Assertion)
	assert.True(t, violations[0].Seeding)
	assert.False(t, violations[0].Enforceable())
	assert.Equal(t, 2, violations[0].BaselineSamples)
}

func TestEvaluateDataAssertions_SeededDeltaEnforcesAndExcludesItsOwnSample(t *testing.T) {
	setDataAssertions(t, true)
	setBaselineMinSamples(t, 5)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	for i := 0; i < 5; i++ {
		_, _, priorRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		seedMetric(t, db, priorRunID, "warehouse/orders", "rowCount", 10000, time.Now().Add(-time.Duration(i+1)*time.Hour))
	}

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 10},
	}))
	require.Error(t, err, "past the cold-start floor a delta breach is a red run")

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.False(t, violations[0].Seeding)

	// The self-exclusion property, asserted rather than assumed. The row is
	// seeded `succeeded` deliberately: that is the ONE state in which an
	// inclusive query would bite, since cleanSampleQuery keeps only succeeded
	// rows (at the real seam every executor path is still `running` — the
	// worker calls this before reportCompletion). Even so the baseline holds
	// exactly the five PRIOR samples, not six with a median dragged toward its
	// own value, because the read happens before the insert.
	assert.Equal(t, 5, violations[0].BaselineSamples)
	require.NotNil(t, violations[0].BaselineMedian)
	assert.InDelta(t, 10000, *violations[0].BaselineMedian, 0.001)

	// And the sample IS recorded, so the NEXT run's baseline sees it.
	require.Len(t, metricRows(t, db), 6)
}

func TestEvaluateDataAssertions_HoldRecordsAsWarnUntilStreamC(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationHold)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12},
	}))
	require.NoError(t, err, "hold lets the task succeed; the breaker itself is Stream C")
	require.Len(t, dataViolationsOf(t, db, taskRunID), 1)
}

func TestEvaluateDataAssertions_SatisfiedContractRecordsNothing(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000), DeltaFromBaseline: "50%"}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 10400312},
	}))
	require.NoError(t, err)
	assert.Nil(t, dataViolationsOf(t, db, taskRunID), "a passing contract writes no verdicts")
	assert.Len(t, metricRows(t, db), 1)
}

func TestEvaluateDataAssertions_QuarantinedRunIsNeverEvaluated(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), true)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12},
	}))
	require.NoError(t, err, "a what-if must never trip an assertion")
	assert.Nil(t, dataViolationsOf(t, db, taskRunID))
	assert.Empty(t, metricRows(t, db))
}

// TestEvaluateDataAssertions_RetryClearsPreviousVerdicts pins that the data
// verdicts follow the same reset contract as schema violations: an in-run retry
// must not re-execute a task still carrying the previous attempt's violations.
func TestEvaluateDataAssertions_RetryClearsPreviousVerdicts(t *testing.T) {
	assert.Contains(t, retryResetColumns(), "data_violations")
	assert.Nil(t, retryResetColumns()["data_violations"])
}
