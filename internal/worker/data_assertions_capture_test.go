package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// data_assertions_capture_test.go is the DISTRIBUTED executor's half of issue
// #437, the mirror of internal/job/data_assertions_capture_test.go: a marker
// stream this executor could not read completely must reach the evaluator as a
// capture, so the verdict is `unavailable` rather than a false `missing` that
// reddens the task under onViolation: fail.

// unreadableLogsEngine is metricsEngine whose Logs call fails outright — the
// engine losing the container's output, which is not evidence about the data.
type unreadableLogsEngine struct {
	metricsEngine
}

func (e *unreadableLogsEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return nil, errors.New("container log stream gone")
}

// declareProducedDatasetWithMinBound registers a produces declaration carrying
// a rowCount min contract in the given dispatch mode.
func declareProducedDatasetWithMinBound(t *testing.T, db *gorm.DB, taskID uuid.UUID, dataset, onViolation string) {
	t.Helper()

	var task models.Task
	require.NoError(t, db.First(&task, "id = ?", taskID).Error)

	min := 1000.0
	spec, err := json.Marshal(jobdef.DatasetAssertions{
		RowCount: &jobdef.AssertionSpec{Min: &min},
	})
	require.NoError(t, err)

	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID:             uuid.New(),
		JobID:          task.JobID,
		JobAlias:       "schema-validation-job",
		StepName:       task.Name,
		Name:           dataset,
		Direction:      models.DatasetDirectionProduces,
		AssertionsJSON: string(spec),
		OnViolation:    onViolation,
	}).Error)
}

// dataViolationsOnRow reads back the verdicts the evaluator persisted, which is
// the same column the REST task read surface serves.
func dataViolationsOnRow(t *testing.T, db *gorm.DB, taskRunID uuid.UUID) []run.DataViolation {
	t.Helper()
	var row models.TaskRun
	require.NoError(t, db.First(&row, "id = ?", taskRunID).Error)
	if len(row.DataViolations) == 0 {
		return nil
	}
	var violations []run.DataViolation
	require.NoError(t, json.Unmarshal(row.DataViolations, &violations))
	return violations
}

// overflowingMetricsLog builds a marker stream that fills
// pkgtask.MaxMetricsBytes before the declared rowCount line, so the
// accumulator drops exactly the metric the contract reads.
func overflowingMetricsLog(dataset string) string {
	var b strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "##caesium::metrics {\"dataset\":%q,\"filler%03d\":1}\n", dataset, i)
	}
	fmt.Fprintf(&b, "##caesium::metrics {\"dataset\":%q,\"rowCount\":5000}\n", dataset)
	return b.String()
}

// TestRuntimeExecutorTruncatedMetricsAreUnavailableNotMissing: the scan
// overflowed its cap, so the absent rowCount says nothing about the data and
// the task must stay green even under onViolation: fail.
func TestRuntimeExecutorTruncatedMetricsAreUnavailableNotMissing(t *testing.T) {
	enableDataAssertionsForTest(t)

	taskRun, db := seedSchemaValidationTaskRun(t, "")
	declareProducedDatasetWithMinBound(t, db, taskRun.TaskID, "warehouse/orders", jobdef.DatasetOnViolationFail)

	engine := &metricsEngine{
		logs:   overflowingMetricsLog("warehouse/orders"),
		result: atom.Success,
	}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)
	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status,
		"a dropped sample is an infrastructure fault; it must not redden the task")

	violations := dataViolationsOnRow(t, db, taskRun.ID)
	require.Len(t, violations, 1)
	assert.Equal(t, run.AssertionUnavailable, violations[0].Assertion)
	assert.Equal(t, run.UnavailableStreamTruncated, violations[0].Reason)
	assert.Equal(t, "rowCount", violations[0].Metric)
	assert.False(t, violations[0].Enforceable())
}

// TestRuntimeExecutorUnreadableLogIsUnavailableNotMissing drives the other seam
// branch: no marker of any kind could be read.
func TestRuntimeExecutorUnreadableLogIsUnavailableNotMissing(t *testing.T) {
	enableDataAssertionsForTest(t)

	taskRun, db := seedSchemaValidationTaskRun(t, "")
	declareProducedDatasetWithMinBound(t, db, taskRun.TaskID, "warehouse/orders", jobdef.DatasetOnViolationFail)

	engine := &unreadableLogsEngine{metricsEngine{result: atom.Success}}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)
	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusSucceeded), row.Status)

	violations := dataViolationsOnRow(t, db, taskRun.ID)
	require.Len(t, violations, 1)
	assert.Equal(t, run.AssertionUnavailable, violations[0].Assertion)
	assert.Equal(t, run.UnavailableLogUnreadable, violations[0].Reason)
}

// TestRuntimeExecutorSilentStepStillFailsMissing is the guard against
// over-reach: a readable, untruncated log carrying no metrics still breaks the
// declared contract.
func TestRuntimeExecutorSilentStepStillFailsMissing(t *testing.T) {
	enableDataAssertionsForTest(t)

	taskRun, db := seedSchemaValidationTaskRun(t, "")
	declareProducedDatasetWithMinBound(t, db, taskRun.TaskID, "warehouse/orders", jobdef.DatasetOnViolationFail)

	engine := &metricsEngine{logs: "loading\ndone\n", result: atom.Success}
	executor := newLogSnapshotExecutor(run.NewStore(db), engine)
	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusFailed), row.Status,
		"a step that stops emitting a declared metric must still fail its contract")

	violations := dataViolationsOnRow(t, db, taskRun.ID)
	require.Len(t, violations, 1)
	assert.Equal(t, run.AssertionMissing, violations[0].Assertion)
	assert.Empty(t, violations[0].Reason)
}
