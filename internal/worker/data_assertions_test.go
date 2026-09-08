package worker

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// metricsEngine is captureCreateEngine with a caller-chosen terminal result, so
// a scenario can drive the real executeTask path for a FAILED container without
// hand-seeding any row.
type metricsEngine struct {
	logs   string
	result atom.Result
}

func (e *metricsEngine) Get(*atom.EngineGetRequest) (atom.Atom, error) {
	return &fakeMonitorAtom{id: "metrics", result: e.result}, nil
}
func (e *metricsEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) { return nil, nil }
func (e *metricsEngine) Create(*atom.EngineCreateRequest) (atom.Atom, error) {
	return &fakeMonitorAtom{id: "metrics", result: atom.Unknown}, nil
}
func (e *metricsEngine) Wait(*atom.EngineWaitRequest) (atom.Atom, error) {
	return &fakeMonitorAtom{id: "metrics", result: e.result}, nil
}
func (e *metricsEngine) Stop(*atom.EngineStopRequest) error { return nil }
func (e *metricsEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(e.logs)), nil
}

// declareProducedDataset registers the produces declaration the metric samples
// attribute to, keyed the way run.EvaluateDataAssertions reads it: by the
// catalog task's job id and step name.
func declareProducedDataset(t *testing.T, db *gorm.DB, taskID uuid.UUID, dataset string) {
	t.Helper()

	var task models.Task
	require.NoError(t, db.First(&task, "id = ?", taskID).Error)
	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID:        uuid.New(),
		JobID:     task.JobID,
		JobAlias:  "schema-validation-job",
		StepName:  task.Name,
		Name:      dataset,
		Direction: models.DatasetDirectionProduces,
	}).Error)
}

func countDatasetMetrics(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.DatasetMetric{}).Count(&n).Error)
	return n
}

func enableDataAssertionsForTest(t *testing.T) {
	t.Helper()
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })
}

// TestRuntimeExecutorRecordsMetricsOnSuccess is the working path: a succeeding
// container's ##caesium::metrics line lands as a DatasetMetric row.
func TestRuntimeExecutorRecordsMetricsOnSuccess(t *testing.T) {
	enableDataAssertionsForTest(t)

	taskRun, db := seedSchemaValidationTaskRun(t, "")
	declareProducedDataset(t, db, taskRun.TaskID, "warehouse/orders")

	engine := &metricsEngine{
		logs:   "loading\n##caesium::metrics {\"rowCount\":10400312}\ndone\n",
		result: atom.Success,
	}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)
	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status)

	var metrics []models.DatasetMetric
	require.NoError(t, db.Find(&metrics).Error)
	require.Len(t, metrics, 1)
	assert.Equal(t, "warehouse/orders", metrics[0].Name)
	assert.Equal(t, "rowCount", metrics[0].Metric)
	assert.InDelta(t, 10400312, metrics[0].Value, 0.5)
	assert.Equal(t, taskRun.ID, metrics[0].TaskRunID)
}

// TestRuntimeExecutorRecordsNoMetricsForAFailedAttempt pins the ordering fix.
//
// A retry reuses this very row (RetryTaskClaimedInstance keys on taskRun.ID)
// and run.Baseline's cleanliness filter reads the row's FINAL status — so a
// failed attempt that emitted `rowCount: 0` followed by a succeeding retry
// would leave TWO rows both counted as clean samples of the same run, and the
// median would be poisoned by a number no successful run ever produced. The
// local executor cannot do that (its seam runs only when execErr == nil), and
// the two executors must baseline a job identically.
func TestRuntimeExecutorRecordsNoMetricsForAFailedAttempt(t *testing.T) {
	enableDataAssertionsForTest(t)

	taskRun, db := seedSchemaValidationTaskRun(t, "")
	declareProducedDataset(t, db, taskRun.TaskID, "warehouse/orders")

	engine := &metricsEngine{
		logs:   "loading\n##caesium::metrics {\"rowCount\":0}\nERROR: upstream truncated\n",
		result: atom.Failure,
	}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)
	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusFailed), row.Status)
	assert.Zero(t, countDatasetMetrics(t, db),
		"a failed attempt must not move a baseline")
}
