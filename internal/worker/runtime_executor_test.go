package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// TestMonitorTaskReturnsPostWaitAtom locks in the contract that monitorTask
// returns the atom snapshot produced by engine.Wait, not the pre-Wait atom
// it was handed. Regression guard: if monitorTask reverts to discarding the
// Wait result, executeTask would record TaskRun.Result from the pre-execution
// pod state — which the kubernetes engine reports as "unknown" (Pending phase),
// causing every k8s task to fail with the "task X failed with result \"unknown\""
// error users hit on `just k8s-distributed`.
func TestMonitorTaskReturnsPostWaitAtom(t *testing.T) {
	preExec := &fakeMonitorAtom{id: "pod-1", result: atom.Unknown}
	postExec := &fakeMonitorAtom{id: "pod-1", result: atom.Success}

	engine := &fakeMonitorEngine{waitResult: postExec}
	executor := &runtimeExecutor{}
	taskRun := &models.TaskRun{ID: uuid.New(), TaskID: uuid.New(), JobRunID: uuid.New()}

	got, err := executor.monitorTask(context.Background(), taskRun, engine, preExec)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, atom.Success, got.Result(), "monitorTask must return the atom from Wait, not the pre-Wait input")
	require.Zero(t, engine.stopCalls, "monitorTask must not call engine.Stop on success — caller stops the atom after reading logs")
}

func TestMonitorTaskReturnsInputAtomOnWaitError(t *testing.T) {
	preExec := &fakeMonitorAtom{id: "pod-1", result: atom.Unknown}
	engine := &fakeMonitorEngine{waitErr: errors.New("watch closed")}
	executor := &runtimeExecutor{}
	taskRun := &models.TaskRun{ID: uuid.New(), TaskID: uuid.New(), JobRunID: uuid.New()}

	got, err := executor.monitorTask(context.Background(), taskRun, engine, preExec)
	require.Error(t, err)
	require.Same(t, preExec, got, "on Wait error monitorTask should return the input atom so the caller can still call Stop with its ID")
	require.Equal(t, 1, engine.stopCalls, "monitorTask must clean up the atom on Wait error to avoid leaking the underlying container/pod")
}

// TestMonitorTaskStopsAtomOnContextCancel pins that monitorTask cleans up the
// atom when the parent context is cancelled mid-execution. Without this the
// worker can leak running pods/containers when a task is cancelled (e.g. by
// shutdown signal) before engine.Wait observes a terminal phase.
func TestMonitorTaskStopsAtomOnContextCancel(t *testing.T) {
	preExec := &fakeMonitorAtom{id: "pod-1", result: atom.Unknown}
	// Block Wait until ctx is cancelled — simulates a pod that never reaches
	// a terminal phase before the task is cancelled.
	engine := &fakeMonitorEngine{waitBlocks: true}
	executor := &runtimeExecutor{}
	taskRun := &models.TaskRun{ID: uuid.New(), TaskID: uuid.New(), JobRunID: uuid.New()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := executor.monitorTask(ctx, taskRun, engine, preExec)
	require.ErrorIs(t, err, context.Canceled)
	require.Same(t, preExec, got)
	require.Equal(t, 1, engine.stopCalls, "monitorTask must clean up the atom on context cancellation to avoid leaks")
}

type fakeMonitorAtom struct {
	id     string
	result atom.Result
}

func (a *fakeMonitorAtom) ID() string           { return a.id }
func (a *fakeMonitorAtom) State() atom.State    { return atom.Stopped }
func (a *fakeMonitorAtom) Result() atom.Result  { return a.result }
func (a *fakeMonitorAtom) CreatedAt() time.Time { return time.Time{} }
func (a *fakeMonitorAtom) StartedAt() time.Time { return time.Time{} }
func (a *fakeMonitorAtom) StoppedAt() time.Time { return time.Time{} }
func (a *fakeMonitorAtom) Engine() atom.Engine  { return nil }

type fakeMonitorEngine struct {
	waitResult atom.Atom
	waitErr    error
	waitBlocks bool
	stopCalls  int
}

func (e *fakeMonitorEngine) Get(*atom.EngineGetRequest) (atom.Atom, error) { return e.waitResult, nil }
func (e *fakeMonitorEngine) List(*atom.EngineListRequest) ([]atom.Atom, error) {
	return nil, nil
}
func (e *fakeMonitorEngine) Create(*atom.EngineCreateRequest) (atom.Atom, error) {
	return e.waitResult, nil
}
func (e *fakeMonitorEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	if e.waitBlocks {
		<-req.Context.Done()
		return nil, req.Context.Err()
	}
	if e.waitErr != nil {
		return nil, e.waitErr
	}
	return e.waitResult, nil
}
func (e *fakeMonitorEngine) Stop(*atom.EngineStopRequest) error {
	e.stopCalls++
	return nil
}
func (e *fakeMonitorEngine) Logs(*atom.EngineLogsRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func TestLeaseRenewInterval(t *testing.T) {
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "default when ttl disabled", ttl: 0, want: defaultLeaseRenewInterval},
		{name: "half ttl", ttl: 20 * time.Second, want: 10 * time.Second},
		{name: "minimum bound", ttl: 1500 * time.Millisecond, want: minLeaseRenewInterval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaseRenewInterval(tt.ttl)
			if got != tt.want {
				t.Fatalf("leaseRenewInterval(%s)=%s, want %s", tt.ttl, got, tt.want)
			}
		})
	}
}

func TestRunSchemaValidationWarnPersistsViolations(t *testing.T) {
	taskRun, db := seedSchemaValidationTaskRun(t, jobdef.SchemaValidationWarn)

	executor := &runtimeExecutor{store: run.NewStore(db)}
	err := executor.runSchemaValidation(taskRun, map[string]string{"rows_written": "unknown"})
	require.NoError(t, err)

	violations := persistedSchemaViolations(t, db, taskRun.JobRunID, taskRun.TaskID)
	require.NotEmpty(t, violations)
	require.Contains(t, violations[0].Message, "integer")
}

func TestRunSchemaValidationFailReturnsError(t *testing.T) {
	taskRun, db := seedSchemaValidationTaskRun(t, jobdef.SchemaValidationFail)

	executor := &runtimeExecutor{store: run.NewStore(db)}
	err := executor.runSchemaValidation(taskRun, map[string]string{"rows_written": "unknown"})
	require.ErrorContains(t, err, "violates declared schema")

	violations := persistedSchemaViolations(t, db, taskRun.JobRunID, taskRun.TaskID)
	require.NotEmpty(t, violations)
	require.Contains(t, violations[0].Message, "integer")
}

func TestRunSchemaValidationFailReturnsErrorForMissingRequiredOutput(t *testing.T) {
	taskRun, db := seedSchemaValidationTaskRun(t, jobdef.SchemaValidationFail)

	executor := &runtimeExecutor{store: run.NewStore(db)}
	err := executor.runSchemaValidation(taskRun, nil)
	require.ErrorContains(t, err, "violates declared schema")

	violations := persistedSchemaViolations(t, db, taskRun.JobRunID, taskRun.TaskID)
	require.NotEmpty(t, violations)
}

func seedSchemaValidationTaskRun(t *testing.T, schemaValidation string) (*models.TaskRun, *gorm.DB) {
	t.Helper()

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() {
		jobdeftestutil.CloseDB(db)
	})

	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID:        uuid.New(),
		Alias:     "schema-validation-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{
		ID:               uuid.New(),
		Alias:            "schema-validation-job",
		TriggerID:        trigger.ID,
		SchemaValidation: schemaValidation,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, db.Create(job).Error)

	atom := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["echo","test"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atom).Error)

	schemaBytes, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rows_written": map[string]any{"type": "integer"},
		},
		"required": []string{"rows_written"},
	})
	require.NoError(t, err)

	task := &models.Task{
		ID:           uuid.New(),
		JobID:        job.ID,
		AtomID:       atom.ID,
		Name:         "transform",
		OutputSchema: datatypes.JSON(schemaBytes),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, db.Create(task).Error)

	jobRun := &models.JobRun{
		ID:          uuid.New(),
		JobID:       job.ID,
		TriggerID:   trigger.ID,
		TriggerType: string(trigger.Type),
		Status:      string(run.StatusRunning),
		StartedAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, db.Create(jobRun).Error)

	taskRun := &models.TaskRun{
		ID:               uuid.New(),
		JobRunID:         jobRun.ID,
		TaskID:           task.ID,
		AtomID:           atom.ID,
		Engine:           atom.Engine,
		Image:            atom.Image,
		Command:          atom.Command,
		Status:           string(run.TaskStatusRunning),
		Attempt:          1,
		MaxAttempts:      1,
		OutputSchema:     datatypes.JSON(schemaBytes),
		SchemaValidation: schemaValidation,
		CreatedAt:        now,
		UpdatedAt:        now,
		StartedAt:        &now,
	}
	require.NoError(t, db.Create(taskRun).Error)

	return taskRun, db
}

func persistedSchemaViolations(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID) []pkgtask.SchemaViolation {
	t.Helper()

	var taskRun models.TaskRun
	require.NoError(t, db.First(&taskRun, "job_run_id = ? AND task_id = ?", runID, taskID).Error)

	var violations []pkgtask.SchemaViolation
	require.NoError(t, json.Unmarshal(taskRun.SchemaViolations, &violations))
	return violations
}
