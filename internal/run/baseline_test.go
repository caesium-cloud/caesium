package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedTaskRun creates a job/task/run/task-run chain and returns the task run id
// the metric rows hang off, so the baseline's cleanliness join has real rows to
// filter on.
func seedTaskRun(t *testing.T, db *gorm.DB, status string, quarantine bool) (jobID, taskID, taskRunID uuid.UUID, stepName string) {
	t.Helper()

	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron}).Error)

	jobID = uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "job-" + jobID.String()[:8], TriggerID: triggerID}).Error)

	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23"}).Error)

	stepName = "load"
	taskID = uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: atomID, Name: stepName}).Error)

	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, TriggerID: triggerID, Status: status, StartedAt: time.Now().UTC(),
	}).Error)

	taskRunID = uuid.New()
	require.NoError(t, db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: taskID, AtomID: atomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: "[]",
		Status: status, Quarantine: quarantine,
	}).Error)

	return jobID, taskID, taskRunID, stepName
}

func seedMetric(t *testing.T, db *gorm.DB, taskRunID uuid.UUID, name, metric string, value float64, at time.Time) {
	t.Helper()
	require.NoError(t, db.Create(&models.DatasetMetric{
		ID:        uuid.New(),
		TaskRunID: taskRunID,
		Name:      name,
		Metric:    metric,
		Value:     value,
		CreatedAt: at,
	}).Error)
}

func TestBaseline_MedianAndPercentiles(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	base := time.Now().UTC().Add(-24 * time.Hour)
	for i, v := range []float64{100, 200, 300, 400, 500} {
		_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", v, base.Add(time.Duration(i)*time.Minute))
	}

	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 5, stats.Samples)
	assert.InDelta(t, 300, stats.Median, 0.001)
	assert.InDelta(t, 140, stats.P10, 0.001)
	assert.InDelta(t, 460, stats.P90, 0.001)
	// Values come back oldest-first, which is the order a sparkline renders.
	assert.Equal(t, []float64{100, 200, 300, 400, 500}, stats.Values)
}

// TestBaseline_AsOfExcludesLaterSamples is the shape contract Plan 3's
// assertion backtest depends on: the window must contain only samples that
// PRECEDE the cut, so a replayed verdict cannot see its own future.
func TestBaseline_AsOfExcludesLaterSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	base := time.Now().UTC().Add(-24 * time.Hour)
	for i, v := range []float64{10, 20, 30} {
		_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", v, base.Add(time.Duration(i)*time.Hour))
	}
	// A far-future outlier that must NOT enter a cut taken before it.
	_, _, futureRun, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, futureRun, "warehouse/orders", "rowCount", 100000, base.Add(10*time.Hour))

	cut := base.Add(5 * time.Hour)
	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, cut)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Samples)
	assert.InDelta(t, 20, stats.Median, 0.001)
	assert.Equal(t, cut, stats.AsOf)

	// Without the cut, the outlier moves the baseline — proving the filter is
	// the thing doing the work, not an empty table.
	all, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 4, all.Samples)
	assert.InDelta(t, 25, all.Median, 0.001)
}

func TestBaseline_ExcludesQuarantinedAndFailedSamples(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	at := time.Now().UTC().Add(-time.Hour)

	_, _, cleanRun, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, cleanRun, "warehouse/orders", "rowCount", 100, at)

	_, _, quarantinedRun, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), true)
	seedMetric(t, db, quarantinedRun, "warehouse/orders", "rowCount", 999999, at)

	_, _, failedRun, _ := seedTaskRun(t, db, string(TaskStatusFailed), false)
	seedMetric(t, db, failedRun, "warehouse/orders", "rowCount", 1, at)

	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Samples, "only the clean, succeeded sample counts")
	assert.InDelta(t, 100, stats.Median, 0.001)
}

func TestBaseline_WindowKeepsMostRecent(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := range 10 {
		_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", float64(i), base.Add(time.Duration(i)*time.Minute))
	}

	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 3, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Samples)
	assert.Equal(t, []float64{7, 8, 9}, stats.Values)
}

func TestBaseline_NoSamplesIsNotAnError(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	stats, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Samples)
	assert.Empty(t, stats.Values)
}

func TestPruneDatasetMetrics_RemovesOnlyExpiredRows(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
	seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", 1, time.Now().UTC().Add(-100*24*time.Hour))
	seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", 2, time.Now().UTC().Add(-time.Hour))

	removed, err := PruneDatasetMetrics(context.Background(), db, 90*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed)

	var remaining int64
	require.NoError(t, db.Model(&models.DatasetMetric{}).Count(&remaining).Error)
	assert.EqualValues(t, 1, remaining)

	// A non-positive retention means "keep forever".
	removed, err = PruneDatasetMetrics(context.Background(), db, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 0, removed)
}
