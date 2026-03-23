package worker

import (
	"encoding/json"
	"testing"
	"time"

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
		Image:     "alpine",
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
