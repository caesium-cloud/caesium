package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// data_assertions_capture_test.go is the LOCAL executor's half of issue #437:
// the seam must tell the evaluator not only what the step emitted but how
// completely its ##caesium::metrics stream was read, so a lost observation is
// recorded as `unavailable` instead of reddening a run declared
// onViolation: fail.
//
// These scenarios drive the real local run loop (New(...).Run) against a fake
// engine whose log stream is truncating, unreadable or simply silent — never
// run.EvaluateDataAssertions directly, because the wiring is exactly what
// broke.

// captureFixture is one job with one step that declares a produced dataset
// under a min-bound contract, wired to the local executor.
type captureFixture struct {
	db     *gorm.DB
	store  *run.Store
	engine *fakeEngine
	jobID  uuid.UUID
	taskID uuid.UUID
	opts   []JobOption
}

func newCaptureFixture(t *testing.T, onViolation string) *captureFixture {
	t.Helper()

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	f := &captureFixture{
		db:     db,
		store:  run.NewStore(db),
		engine: newFakeEngine(),
		jobID:  uuid.New(),
		taskID: uuid.New(),
	}

	atomID := uuid.New()
	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{ID: f.taskID, JobID: f.jobID, AtomID: atomID, Name: "load"},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{atomID: fakeModelAtom(atomID)}}
	persistGraph(t, db, taskSvc.tasks, nil)

	spec, err := json.Marshal(jobdefschema.DatasetAssertions{
		RowCount: &jobdefschema.AssertionSpec{Min: ptrTo(float64(1000))},
	})
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID:             uuid.New(),
		JobID:          f.jobID,
		JobAlias:       "capture-job",
		StepName:       "load",
		Name:           "warehouse/orders",
		Direction:      models.DatasetDirectionProduces,
		AssertionsJSON: string(spec),
		OnViolation:    onViolation,
	}).Error)

	f.opts = withTestDeps(f.store, env.Environment{
		MaxParallelTasks:      1,
		TaskFailurePolicy:     taskFailurePolicyHalt,
		ExecutionMode:         executionModeLocal,
		DataAssertionsEnabled: true,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, f.engine)

	return f
}

// violations runs the job and returns the verdicts recorded on its task row.
func (f *captureFixture) run(t *testing.T) ([]run.DataViolation, error) {
	t.Helper()
	err := New(&models.Job{ID: f.jobID}, f.opts...).Run(context.Background())
	snapshot := latestRunSnapshot(t, f.store, f.jobID)
	taskRun := taskRunByID(snapshot, f.taskID)
	require.NotNil(t, taskRun)
	return taskRun.DataViolations, err
}

func ptrTo[T any](v T) *T { return &v }

// overflowingMetricsLog builds a marker stream that overflows
// pkgtask.MaxMetricsBytes BEFORE it reaches the declared rowCount, which the
// accumulator therefore drops. It is the real shape of the bug: the step DID
// emit its declared metric, and the executor never saw it.
func overflowingMetricsLog(dataset string) string {
	var b strings.Builder
	// Each entry costs len(dataset)+len(metric)+32 bytes against the cap, so a
	// few hundred filler metrics overflow 16 KiB with room to spare.
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "##caesium::metrics {\"dataset\":%q,\"filler%03d\":1}\n", dataset, i)
	}
	fmt.Fprintf(&b, "##caesium::metrics {\"dataset\":%q,\"rowCount\":5000}\n", dataset)
	return b.String()
}

// TestOverflowingMetricsLogTruncatesTheScan is a self-test for the fixture
// above: if the accumulator ever stopped truncating on this input, every
// scenario below would silently stop testing anything.
func TestOverflowingMetricsLogTruncatesTheScan(t *testing.T) {
	markers, err := pkgtask.ParseMarkers(strings.NewReader(overflowingMetricsLog("warehouse/orders")))
	require.NoError(t, err)
	require.True(t, markers.MetricsTruncated, "the fixture must overflow pkgtask.MaxMetricsBytes")

	for _, sample := range markers.Metrics {
		require.NotEqual(t, "rowCount", sample.Metric,
			"the declared metric must be one of the samples the cap dropped")
	}
}

// TestLocalExecutorTruncatedMetricsAreUnavailableNotMissing is the headline
// regression: onViolation: fail plus a truncated scan used to fail the run for
// an infrastructure reason.
func TestLocalExecutorTruncatedMetricsAreUnavailableNotMissing(t *testing.T) {
	f := newCaptureFixture(t, jobdefschema.DatasetOnViolationFail)
	f.engine.logsByName[f.taskID.String()] = overflowingMetricsLog("warehouse/orders")

	violations, err := f.run(t)
	require.NoError(t, err, "a dropped sample is an infrastructure fault, not a contract breach")

	require.Len(t, violations, 1)
	assert.Equal(t, run.AssertionUnavailable, violations[0].Assertion)
	assert.Equal(t, run.UnavailableStreamTruncated, violations[0].Reason)
	assert.Equal(t, "rowCount", violations[0].Metric)
	assert.False(t, violations[0].Enforceable())

	snapshot := latestRunSnapshot(t, f.store, f.jobID)
	assert.Equal(t, run.TaskStatusSucceeded, taskStatusByID(snapshot)[f.taskID])
}

// TestLocalExecutorUnreadableLogIsUnavailableNotMissing drives the other seam
// branch: the engine cannot hand the executor the container's output at all.
func TestLocalExecutorUnreadableLogIsUnavailableNotMissing(t *testing.T) {
	f := newCaptureFixture(t, jobdefschema.DatasetOnViolationFail)
	f.engine.logsErrByName[f.taskID.String()] = errors.New("container log stream gone")

	violations, err := f.run(t)
	require.NoError(t, err)

	require.Len(t, violations, 1)
	assert.Equal(t, run.AssertionUnavailable, violations[0].Assertion)
	assert.Equal(t, run.UnavailableLogUnreadable, violations[0].Reason)

	snapshot := latestRunSnapshot(t, f.store, f.jobID)
	assert.Equal(t, run.TaskStatusSucceeded, taskStatusByID(snapshot)[f.taskID])
}

// TestLocalExecutorSilentStepStillFailsMissing is the guard against over-reach:
// a readable, untruncated log that simply carries no metrics still breaks the
// declared contract, exactly as B1 intended.
func TestLocalExecutorSilentStepStillFailsMissing(t *testing.T) {
	f := newCaptureFixture(t, jobdefschema.DatasetOnViolationFail)
	f.engine.logsByName[f.taskID.String()] = "loading\ndone\n"

	violations, err := f.run(t)
	require.Error(t, err, "a step that stops emitting a declared metric must still fail its contract")
	assert.Contains(t, err.Error(), "rowCount")

	require.Len(t, violations, 1)
	assert.Equal(t, run.AssertionMissing, violations[0].Assertion)
	assert.Empty(t, violations[0].Reason)

	snapshot := latestRunSnapshot(t, f.store, f.jobID)
	assert.Equal(t, run.TaskStatusFailed, taskStatusByID(snapshot)[f.taskID])
}
