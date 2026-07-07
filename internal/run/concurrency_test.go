package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestStartAdmissionConditionalInsertRowsAffected(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "admit-rows", jobdef.ConcurrencyStrategyFail, 1)

	first, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := store.Start(job.ID, nil)
	require.ErrorIs(t, err, ErrMaxConcurrentRunsReached)
	require.Nil(t, second)

	var count int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", job.ID).Count(&count).Error)
	require.Equal(t, int64(1), count, "failed conditional insert must affect zero rows and create no run")

	active, err := store.CountActive(job.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), active)
}

func TestCancelRunTransitionsRunTasksAndLease(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	now := time.Now().UTC()
	job := createConcurrencyJob(t, db, "cancel-run", jobdef.ConcurrencyStrategyReplace, 1)
	runID := uuid.New()
	taskPending := uuid.New()
	taskRunning := uuid.New()
	taskSucceeded := uuid.New()
	atomID := uuid.New()
	require.NoError(t, db.Create(&models.Atom{
		ID:        atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["true"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	for name, taskID := range map[string]uuid.UUID{
		"pending":   taskPending,
		"running":   taskRunning,
		"succeeded": taskSucceeded,
	} {
		require.NoError(t, db.Create(&models.Task{
			ID:        taskID,
			JobID:     job.ID,
			AtomID:    atomID,
			Name:      name,
			CreatedAt: now,
			UpdatedAt: now,
		}).Error)
	}
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     job.ID,
		Status:    string(StatusRunning),
		StartedAt: now.Add(-time.Minute),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now.Add(-time.Minute),
	}).Error)
	for _, row := range []models.TaskRun{
		{ID: uuid.New(), JobRunID: runID, TaskID: taskPending, AtomID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, Status: string(TaskStatusPending), CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskRunning, AtomID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, Status: string(TaskStatusRunning), ClaimedBy: "node-a", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobRunID: runID, TaskID: taskSucceeded, AtomID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`, Status: string(TaskStatusSucceeded), CreatedAt: now, UpdatedAt: now},
	} {
		require.NoError(t, db.Create(&row).Error)
	}
	require.NoError(t, db.Create(&models.RunLease{
		RunID:          runID.String(),
		OwnerNode:      "node-a",
		AcquiredAt:     now,
		LeaseExpiresAt: now.Add(time.Minute),
		Generation:     1,
	}).Error)

	require.NoError(t, store.CancelRun(context.Background(), runID))

	var runRow models.JobRun
	require.NoError(t, db.First(&runRow, "id = ?", runID).Error)
	require.Equal(t, string(StatusCancelled), runRow.Status)
	require.NotNil(t, runRow.CompletedAt)

	var taskRows []models.TaskRun
	require.NoError(t, db.Order("task_id").Find(&taskRows, "job_run_id = ?", runID).Error)
	statuses := map[uuid.UUID]string{}
	for _, row := range taskRows {
		statuses[row.TaskID] = row.Status
		if row.TaskID == taskRunning {
			require.Empty(t, row.ClaimedBy)
			require.Nil(t, row.ClaimExpiresAt)
		}
	}
	require.Equal(t, string(TaskStatusCancelled), statuses[taskPending])
	require.Equal(t, string(TaskStatusCancelled), statuses[taskRunning])
	require.Equal(t, string(TaskStatusSucceeded), statuses[taskSucceeded])

	var leases int64
	require.NoError(t, db.Model(&models.RunLease{}).Where("run_id = ?", runID.String()).Count(&leases).Error)
	require.Zero(t, leases)
}

func TestDequeueNextRunClaimsOneRowByPriority(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-claim", jobdef.ConcurrencyStrategyQueue, 1)
	now := time.Now().UTC()
	lowID := uuid.New()
	highID := uuid.New()
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        lowID,
		JobID:     job.ID,
		Priority:  PriorityLowValue,
		CreatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        highID,
		JobID:     job.ID,
		Priority:  PriorityHighValue,
		CreatedAt: now.Add(time.Second),
	}).Error)

	first, err := store.DequeueNextRun(context.Background(), job.ID, "claim-a")
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, highID, first.ID)

	second, err := store.DequeueNextRun(context.Background(), job.ID, "claim-b")
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, lowID, second.ID)

	third, err := store.DequeueNextRun(context.Background(), job.ID, "claim-c")
	require.NoError(t, err)
	require.Nil(t, third)
}

func TestEnqueueRunEvictsOldestWhenDepthExceeded(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-depth", jobdef.ConcurrencyStrategyQueue, 1)
	oldestID := uuid.New()
	keptID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        oldestID,
		JobID:     job.ID,
		Priority:  PriorityNormalValue,
		CreatedAt: now.Add(-2 * time.Minute),
	}).Error)
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        keptID,
		JobID:     job.ID,
		Priority:  PriorityNormalValue,
		CreatedAt: now.Add(-time.Minute),
	}).Error)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.enqueueRunTx(tx, job.ID, datatypes.JSON(`{"kind":"new"}`), PriorityNormalValue, 2)
	}))

	var rows []models.RunQueue
	require.NoError(t, db.Order("created_at ASC").Find(&rows, "job_id = ?", job.ID).Error)
	require.Len(t, rows, 2)
	require.Equal(t, keptID, rows[0].ID)
	require.NotEqual(t, oldestID, rows[1].ID)
}

func TestCancelQueuedRunDeletesOnlyUnclaimedRows(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-cancel", jobdef.ConcurrencyStrategyQueue, 1)
	unclaimedID := uuid.New()
	claimedID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        unclaimedID,
		JobID:     job.ID,
		Priority:  PriorityNormalValue,
		CreatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.RunQueue{
		ID:        claimedID,
		JobID:     job.ID,
		Priority:  PriorityNormalValue,
		ClaimedBy: "node-a/race",
		ClaimedAt: &now,
		CreatedAt: now,
	}).Error)

	require.NoError(t, store.CancelQueuedRun(context.Background(), job.ID, unclaimedID))
	var count int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("id = ?", unclaimedID).Count(&count).Error)
	require.Zero(t, count)

	err := store.CancelQueuedRun(context.Background(), job.ID, claimedID)
	require.ErrorIs(t, err, ErrQueuedRunUnavailable)
	require.NoError(t, db.Model(&models.RunQueue{}).Where("id = ? AND claimed_by = ?", claimedID, "node-a/race").Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func createConcurrencyJob(t *testing.T, db *gorm.DB, alias, strategy string, maxRuns int) *models.Job {
	t.Helper()
	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         alias + "-trigger",
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *","timezone":"UTC"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, db.Create(trigger).Error)
	raw, err := json.Marshal(&jobdef.Concurrency{MaxRuns: maxRuns, Strategy: strategy})
	require.NoError(t, err)
	job := &models.Job{
		ID:          uuid.New(),
		Alias:       alias,
		TriggerID:   trigger.ID,
		Concurrency: datatypes.JSON(raw),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, db.Create(job).Error)
	return job
}

func TestStartSkipReturnsSentinel(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "skip-sentinel", jobdef.ConcurrencyStrategySkip, 1)
	_, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	_, err = store.Start(job.ID, nil)
	require.True(t, errors.Is(err, ErrRunSkipped))
}
