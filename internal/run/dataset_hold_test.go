package run

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
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

// declareProducesHold writes a produced declaration with a min-bound contract
// under onViolation: hold and the given release policy.
func declareProducesHold(t *testing.T, db *gorm.DB, jobID uuid.UUID, stepName, name, release string) {
	t.Helper()
	spec, err := json.Marshal(jobdef.DatasetAssertions{
		RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)},
	})
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID:             uuid.New(),
		JobID:          jobID,
		JobAlias:       "alias",
		StepName:       stepName,
		Name:           name,
		Direction:      models.DatasetDirectionProduces,
		AssertionsJSON: string(spec),
		OnViolation:    jobdef.DatasetOnViolationHold,
		Release:        release,
	}).Error)
}

// seedTaskRunForJob adds a SECOND run of an existing job, reusing that job's
// catalog task so the evaluator resolves the same declaration — which is what
// "the same job ran again" means to the registry.
func seedTaskRunForJob(t *testing.T, db *gorm.DB, jobID uuid.UUID, stepName, status string) (runID, taskID, taskRunID uuid.UUID, step string) {
	t.Helper()

	var task models.Task
	require.NoError(t, db.Where("job_id = ? AND name = ?", jobID, stepName).First(&task).Error)

	var job models.Job
	require.NoError(t, db.Where("id = ?", jobID).First(&job).Error)

	runID = uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, TriggerID: job.TriggerID, Status: status, StartedAt: time.Now().UTC(),
	}).Error)

	taskRunID = uuid.New()
	require.NoError(t, db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: task.ID, AtomID: task.AtomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: "[]",
		Status: status,
	}).Error)
	return runID, task.ID, taskRunID, stepName
}

// activeHolds reads the live holds for a dataset.
func activeHolds(t *testing.T, db *gorm.DB, name string) []models.DatasetHold {
	t.Helper()
	var rows []models.DatasetHold
	require.NoError(t, db.Where("name = ? AND status = ?", name, models.DatasetHoldStatusActive).
		Find(&rows).Error)
	return rows
}

// emitSamples drives the real post-task seam for one task run.
func emitSamples(t *testing.T, store *Store, db *gorm.DB, taskID, taskRunID uuid.UUID, samples ...pkgtask.DatasetMetricSample) error {
	t.Helper()
	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)
	return EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, samples)
}

// TestHoldOpensOnceAndAppendsOccurrences is the alert-once contract, driven
// through the real evaluator seam rather than the store primitive: two separate
// breaching runs of the same job produce ONE active hold with occurrence count
// 2, and exactly ONE dataset_held event. It also pins that the breaching task
// SUCCEEDS — the hold disposition never reddens a run.
func TestHoldOpensOnceAndAppendsOccurrences(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	bus := event.New()
	store.SetBus(bus)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	held, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeDatasetHeld}})
	require.NoError(t, err)

	before := metricstestutil.CounterValue(t, metrics.DatasetHoldsTotal, AssertionMin)
	beforeVerdicts := metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultHold)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	require.NoError(t, emitSamples(t, store, db, taskID, taskRunID,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}),
		"onViolation: hold must not fail the task — the work is done, it is the DATA that is wrong")

	holds := activeHolds(t, db, "warehouse/orders")
	require.Len(t, holds, 1)
	assert.Equal(t, 1, holds[0].OccurrenceCount)
	assert.Equal(t, AssertionMin, holds[0].Reason)
	assert.Equal(t, jobID, holds[0].HeldByJobID)
	assert.Equal(t, stepName, holds[0].HeldByStepName)
	require.NotNil(t, holds[0].ActiveKey)
	assert.Equal(t, "/warehouse/orders", *holds[0].ActiveKey)

	// The hold carries the whole verdict, baseline snapshot included, because
	// Stream F's bundle serves it without a second read.
	var recorded []DataViolation
	require.NoError(t, json.Unmarshal(holds[0].Violations, &recorded))
	require.Len(t, recorded, 1)
	assert.Equal(t, AssertionMin, recorded[0].Assertion)
	require.NotNil(t, recorded[0].Observed)
	assert.InDelta(t, 12, *recorded[0].Observed, 0.001)
	require.NotNil(t, recorded[0].Bound)
	assert.InDelta(t, 1000, *recorded[0].Bound, 0.001)

	// A SECOND breach, on a second run of the same job.
	_, taskID2, taskRunID2, _ := seedTaskRunForJob(t, db, jobID, stepName, string(TaskStatusRunning))
	require.NoError(t, emitSamples(t, store, db, taskID2, taskRunID2,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 9}))

	holds = activeHolds(t, db, "warehouse/orders")
	require.Len(t, holds, 1, "a repeat breach must append to the one active hold, never open a twin")
	assert.Equal(t, 2, holds[0].OccurrenceCount)

	assert.Equal(t, before+1, metricstestutil.CounterValue(t, metrics.DatasetHoldsTotal, AssertionMin),
		"caesium_dataset_holds_total counts holds OPENED; an occurrence is not a new hold")
	assert.Equal(t, beforeVerdicts+2, metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultHold),
		"both verdicts are counted as hold dispositions")
	assert.Equal(t, float64(1), metricstestutil.GaugeValue(t, metrics.DatasetHoldsActive))

	// Exactly one page.
	select {
	case evt := <-held:
		var payload DatasetHoldEvent
		require.NoError(t, json.Unmarshal(evt.Payload, &payload))
		assert.Equal(t, holds[0].ID, payload.HoldID)
		assert.Equal(t, "warehouse/orders", payload.Dataset)
		assert.Equal(t, models.DatasetHoldStatusActive, payload.Status)
		assert.Equal(t, AssertionMin, payload.Reason)
		require.Len(t, payload.Violations, 1)
		require.NotNil(t, payload.Violations[0].Observed)
		assert.InDelta(t, 12, *payload.Violations[0].Observed, 0.001)
	case <-time.After(2 * time.Second):
		t.Fatal("expected exactly one dataset_held event")
	}
	select {
	case evt := <-held:
		t.Fatalf("a repeat breach must not re-alert; got a second dataset_held: %s", string(evt.Payload))
	case <-time.After(200 * time.Millisecond):
	}

	// It is persisted too, not merely published: /v1/events and `caesium why`
	// read the store, not the bus.
	var stored int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).
		Where("type = ?", string(event.TypeDatasetHeld)).Count(&stored).Error)
	assert.Equal(t, int64(1), stored)
}

// TestCleanRunReleasesAutoHoldOnly covers both halves of the release policy:
// `release: auto` (the default) clears on the holder's next clean run, and
// `release: manual` stays held — which is what makes A3's field mean something.
func TestCleanRunReleasesAutoHoldOnly(t *testing.T) {
	for _, tc := range []struct {
		name            string
		release         string
		expectReleased  bool
		expectedReason  string
		expectedEvents  int
		remainingActive int
	}{
		{"auto releases", jobdef.DatasetReleaseAuto, true, models.DatasetHoldReleaseCleanRun, 1, 0},
		{"manual stays held", jobdef.DatasetReleaseManual, false, "", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDataAssertions(t, true)
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			bus := event.New()
			store.SetBus(bus)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			released, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeDatasetReleased}})
			require.NoError(t, err)

			jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
			declareProducesHold(t, db, jobID, stepName, "warehouse/orders", tc.release)

			require.NoError(t, emitSamples(t, store, db, taskID, taskRunID,
				pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}))
			require.Len(t, activeHolds(t, db, "warehouse/orders"), 1)

			// A clean run of the SAME job.
			_, cleanTaskID, cleanTaskRunID, _ := seedTaskRunForJob(t, db, jobID, stepName, string(TaskStatusRunning))
			require.NoError(t, emitSamples(t, store, db, cleanTaskID, cleanTaskRunID,
				pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 5000}))

			assert.Len(t, activeHolds(t, db, "warehouse/orders"), tc.remainingActive)

			if tc.expectReleased {
				var hold models.DatasetHold
				require.NoError(t, db.Where("name = ?", "warehouse/orders").First(&hold).Error)
				assert.Equal(t, models.DatasetHoldStatusReleased, hold.Status)
				assert.Equal(t, tc.expectedReason, hold.ReleaseReason)
				assert.Equal(t, "system", hold.ReleasedBy)
				assert.Nil(t, hold.ActiveKey, "a released hold frees the active key for a future break")
				require.NotNil(t, hold.ReleasedAt)
				require.NotNil(t, hold.ReleaseRunID)
				select {
				case evt := <-released:
					var payload DatasetHoldEvent
					require.NoError(t, json.Unmarshal(evt.Payload, &payload))
					assert.Equal(t, models.DatasetHoldReleaseCleanRun, payload.ReleaseReason)
				case <-time.After(2 * time.Second):
					t.Fatal("expected a dataset_released event")
				}
				return
			}

			select {
			case evt := <-released:
				t.Fatalf("release: manual must survive a clean run; got %s", string(evt.Payload))
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

// TestCleanRunOfAnotherProducerDoesNotRelease answers plan Open Question 6:
// only the HOLDER's clean run releases. A second job that also writes the
// dataset has produced its own slice, not the one that broke.
func TestCleanRunOfAnotherProducerDoesNotRelease(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)
	require.NoError(t, emitSamples(t, store, db, taskID, taskRunID,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}))
	require.Len(t, activeHolds(t, db, "warehouse/orders"), 1)

	// A DIFFERENT job producing the same name, running clean.
	otherJobID, otherTaskID, otherTaskRunID, otherStep := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, otherJobID, otherStep, "warehouse/orders", jobdef.DatasetReleaseAuto)
	require.NoError(t, emitSamples(t, store, db, otherTaskID, otherTaskRunID,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 5000}))

	assert.Len(t, activeHolds(t, db, "warehouse/orders"), 1,
		"a clean run of a different producer must not clear another producer's hold")
}

// TestManualAckReleasesHoldAndRefusesTwice drives the human-ack path at the
// store level: an authenticated principal is recorded, and a second ack on the
// same hold is a conflict rather than a silent second release.
func TestManualAckReleasesHoldAndRefusesTwice(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseManual)
	require.NoError(t, emitSamples(t, store, db, taskID, taskRunID,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}))
	holds := activeHolds(t, db, "warehouse/orders")
	require.Len(t, holds, 1)

	ctx := context.Background()
	released, err := store.ReleaseHold(ctx, holds[0].ID, ReleaseHoldParams{
		ReleasedBy: "user:ada",
		Note:       "backfilled the source",
		Tolerances: map[string]string{"rowCount": "24h"},
	})
	require.NoError(t, err)
	assert.Equal(t, models.DatasetHoldStatusReleased, released.Status)
	assert.Equal(t, models.DatasetHoldReleaseManualAck, released.ReleaseReason)
	assert.Equal(t, "user:ada", released.ReleasedBy)
	assert.Contains(t, string(released.Tolerances), "24h")
	assert.Empty(t, activeHolds(t, db, "warehouse/orders"))
	assert.Equal(t, float64(0), metricstestutil.GaugeValue(t, metrics.DatasetHoldsActive))

	_, err = store.ReleaseHold(ctx, holds[0].ID, ReleaseHoldParams{ReleasedBy: "user:grace"})
	require.ErrorIs(t, err, ErrDatasetHoldNotActive)

	_, err = store.ReleaseHold(ctx, holds[0].ID, ReleaseHoldParams{ReleasedBy: "   "})
	require.Error(t, err, "ReleasedBy must always be an authenticated principal")
}

// TestCleanSampleQueryExcludesSamplesTakenWhileHeld pins the design's third
// "clean" predicate, and its one exemption: the releasing run's own samples.
func TestCleanSampleQueryExcludesSamplesTakenWhileHeld(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	base := time.Now().UTC().Add(-24 * time.Hour)

	// One honest sample from before the hold.
	_, _, beforeRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, beforeRunID, "warehouse/orders", "rowCount", 100, base)

	// One taken while the dataset was held.
	_, _, duringRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, duringRunID, "warehouse/orders", "rowCount", 999, base.Add(2*time.Hour))

	// And one from the run that RELEASED the hold, which is exempt.
	_, _, releaseTaskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, releaseTaskRunID, "warehouse/orders", "rowCount", 110, base.Add(3*time.Hour))
	var releasingTaskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", releaseTaskRunID).First(&releasingTaskRun).Error)

	openedAt := base.Add(time.Hour)
	releasedAt := base.Add(4 * time.Hour)
	releaseRunID := releasingTaskRun.JobRunID
	require.NoError(t, db.Create(&models.DatasetHold{
		ID:              uuid.New(),
		Name:            "warehouse/orders",
		Status:          models.DatasetHoldStatusReleased,
		OccurrenceCount: 1,
		OpenedAt:        openedAt,
		ReleasedAt:      &releasedAt,
		ReleaseReason:   models.DatasetHoldReleaseCleanRun,
		ReleaseRunID:    &releaseRunID,
		CreatedAt:       openedAt,
		UpdatedAt:       releasedAt,
	}).Error)

	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, 2, stats.Samples,
		"the sample observed mid-hold is excluded; the releasing run's own sample is not")
	assert.NotContains(t, stats.Values, float64(999))
	assert.Contains(t, stats.Values, float64(110))
}
