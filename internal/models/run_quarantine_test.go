package models_test

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestQuarantineColumnsDefaultFalse(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Now().UTC()
	runID := uuid.New()
	taskID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     uuid.New(),
		Status:    "running",
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID:        uuid.New(),
		JobRunID:  runID,
		TaskID:    taskID,
		AtomID:    uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["true"]`,
		Status:    "pending",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.ExecutionEvent{
		Type:      "run_started",
		RunID:     &runID,
		TaskID:    &taskID,
		CreatedAt: now,
	}).Error)

	var jobRun models.JobRun
	require.NoError(t, db.First(&jobRun, "id = ?", runID).Error)
	require.False(t, jobRun.Quarantine)
	require.Nil(t, jobRun.ReplayFingerprint)

	var taskRun models.TaskRun
	require.NoError(t, db.First(&taskRun, "job_run_id = ? AND task_id = ?", runID, taskID).Error)
	require.False(t, taskRun.Quarantine)

	var evt models.ExecutionEvent
	require.NoError(t, db.First(&evt, "run_id = ? AND task_id = ?", runID, taskID).Error)
	require.False(t, evt.Quarantine)
}
