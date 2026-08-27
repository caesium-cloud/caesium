package reproduce

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func seedDescriptorJobRun(t *testing.T, db *gorm.DB, jobID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "succeeded",
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return runID
}

func seedDescriptorTask(t *testing.T, db *gorm.DB, jobID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	taskID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Task{
		ID:        taskID,
		JobID:     jobID,
		AtomID:    uuid.New(),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return taskID
}

func seedDescriptorTaskRun(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID, partitionValue string, partitionIndex, partitionCount int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.TaskRun{
		ID:                  id,
		JobRunID:            runID,
		TaskID:              taskID,
		AtomID:              uuid.New(),
		Engine:              models.AtomEngineDocker,
		Image:               "alpine:3.23",
		Command:             `["echo"]`,
		Status:              "succeeded",
		Attempt:             1,
		MaxAttempts:         1,
		PartitionValue:      partitionValue,
		PartitionIndex:      partitionIndex,
		PartitionCount:      partitionCount,
		ExecutionDescriptor: datatypes.JSON([]byte(`{"schemaVersion":1}`)),
		StartedAt:           &now,
		CompletedAt:         &now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}).Error)
	return id
}

// TestDescriptor_UnfannedResolves is the baseline: an ordinary task run has no
// partition identity and resolves to its stored descriptor.
func TestDescriptor_UnfannedResolves(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	taskID := seedDescriptorTask(t, db, jobID, "load")
	seedDescriptorTaskRun(t, db, runID, taskID, "", 0, 0)

	svc := NewWithDatabase(context.Background(), db)
	got, err := svc.Descriptor(runID, "load")
	require.NoError(t, err)
	assert.JSONEq(t, `{"schemaVersion":1}`, string(got.Descriptor))
}

// TestDescriptor_SinglePartitionExpansionIsAmbiguous pins the fix for the
// adversarial-review finding that the fan-out guard tested partition_count > 1.
// A fan-out that expanded to exactly one partition still produces a
// per-partition descriptor whose identity includes the partition key, so
// resolving it by bare task name silently hands back a partition-specific
// descriptor from a surface that is documented to refuse fanned tasks — and an
// N=1 group can become N>1 on the next run, making the surface's answer
// non-reproducible.
func TestDescriptor_SinglePartitionExpansionIsAmbiguous(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	taskID := seedDescriptorTask(t, db, jobID, "process")
	seedDescriptorTaskRun(t, db, runID, taskID, "2026-07-01", 0, 1)

	svc := NewWithDatabase(context.Background(), db)

	_, err := svc.Descriptor(runID, "process")
	require.ErrorIs(t, err, ErrFannedTaskAmbiguous)

	_, err = svc.Descriptor(runID, taskID.String())
	require.ErrorIs(t, err, ErrFannedTaskAmbiguous, "the task-UUID path must refuse the same row the name path refuses")
}

// TestDescriptor_MultiPartitionExpansionIsAmbiguous is the already-covered N>1
// case, kept so the N=1 fix cannot regress it.
func TestDescriptor_MultiPartitionExpansionIsAmbiguous(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	taskID := seedDescriptorTask(t, db, jobID, "process")
	seedDescriptorTaskRun(t, db, runID, taskID, "a", 0, 2)
	seedDescriptorTaskRun(t, db, runID, taskID, "b", 1, 2)

	svc := NewWithDatabase(context.Background(), db)
	_, err := svc.Descriptor(runID, "process")
	require.ErrorIs(t, err, ErrFannedTaskAmbiguous)
}

// TestDescriptor_NotFound keeps the not-found contract distinct from the
// fanned-ambiguous one.
func TestDescriptor_NotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	seedDescriptorTask(t, db, jobID, "load")

	svc := NewWithDatabase(context.Background(), db)
	_, err := svc.Descriptor(runID, "load")
	require.ErrorIs(t, err, runstorage.ErrTaskRunNotFound)
}
