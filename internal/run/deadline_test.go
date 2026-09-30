package run

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestTaskExecutionDeadlineUsesFrozenPolicyAndDurableAnchor(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	job := &models.Job{ID: uuid.New(), Alias: "deadline", TaskTimeout: 2 * time.Second, RunTimeout: 9 * time.Second}
	require.NoError(t, db.Create(job).Error)
	runRecord, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`}
	taskModel := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "work"}
	require.NoError(t, db.Create(atomModel).Error)
	require.NoError(t, db.Create(taskModel).Error)
	require.NoError(t, store.RegisterTask(runRecord.ID, taskModel, atomModel, 0))

	// Mutable job edits after registration must not alter an in-flight run.
	require.NoError(t, db.Model(job).Updates(map[string]any{
		"task_timeout": 30 * time.Second,
		"run_timeout":  40 * time.Second,
	}).Error)
	row, err := loadUniqueTaskRun(db, runRecord.ID, taskModel.ID)
	require.NoError(t, err)
	got, err := store.TaskExecutionDeadlineForRun(context.Background(), runRecord.ID, row.ID)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, got.TaskTimeout)
	require.Equal(t, 9*time.Second, got.RunTimeout)
	require.WithinDuration(t, runRecord.StartedAt, got.RunStarted, time.Millisecond)

	// Rows created before timeout_started_at was added retain their original
	// StartedAt anchor rather than receiving a fresh budget on takeover.
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runRecord.ID).
		Update("timeout_started_at", nil).Error)
	got, err = store.TaskExecutionDeadlineForRun(context.Background(), runRecord.ID, row.ID)
	require.NoError(t, err)
	require.WithinDuration(t, runRecord.StartedAt, got.RunStarted, time.Millisecond)
}

func TestRunTimeoutAnchorRefreshesOnlyWhenTerminalRunReopens(t *testing.T) {
	f := newFanOutFixture(t, nil)
	original := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).
		Update("timeout_started_at", original).Error)

	// Retrying a failed partition while the run is still active leaves the
	// whole-run deadline unchanged.
	producer := f.producerRow(t)
	require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", producer.ID).Updates(map[string]any{
		"status":          string(TaskStatusFailed),
		"completed_at":    time.Now().UTC(),
		"partition_count": 1,
		"partition_index": 0,
		"partition_value": "p0",
	}).Error)
	_, reopened, err := f.store.RetryPartition(context.Background(), f.runID, producer.ID)
	require.NoError(t, err)
	require.False(t, reopened)
	var active models.JobRun
	require.NoError(t, f.db.First(&active, "id = ?", f.runID).Error)
	require.NotNil(t, active.TimeoutStartedAt)
	require.WithinDuration(t, original, *active.TimeoutStartedAt, time.Millisecond)

	// Reopening a terminal run starts a new execution window and therefore a
	// fresh run-timeout budget.
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", f.runID).Updates(map[string]any{
		"status":       string(StatusFailed),
		"completed_at": time.Now().UTC(),
	}).Error)
	beforeRetry := time.Now().UTC()
	_, err = f.store.RetryFromFailure(f.runID)
	require.NoError(t, err)
	var reopenedRun models.JobRun
	require.NoError(t, f.db.First(&reopenedRun, "id = ?", f.runID).Error)
	require.NotNil(t, reopenedRun.TimeoutStartedAt)
	require.False(t, reopenedRun.TimeoutStartedAt.Before(beforeRetry))
}

func TestRunTimeoutTerminalizesAllUnfinishedTasksAndFencesLateResults(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "timeout-fence"}).Error)
	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)

	rows := []models.TaskRun{
		{ID: uuid.New(), JobRunID: runRecord.ID, TaskID: uuid.New(), Status: string(TaskStatusRunning), ClaimedBy: "worker-a", ClaimAttempt: 1},
		{ID: uuid.New(), JobRunID: runRecord.ID, TaskID: uuid.New(), Status: string(TaskStatusRunning), ClaimedBy: "worker-b", ClaimAttempt: 1},
		{ID: uuid.New(), JobRunID: runRecord.ID, TaskID: uuid.New(), Status: string(TaskStatusPending), PartitionRetryPending: true},
	}
	require.NoError(t, db.Create(&rows).Error)

	timeoutErr := NewRunDeadlineError(2 * time.Second)
	finalized, err := store.CompleteIfActive(runRecord.ID, timeoutErr)
	require.NoError(t, err)
	require.True(t, finalized)

	var persistedRun models.JobRun
	require.NoError(t, db.First(&persistedRun, "id = ?", runRecord.ID).Error)
	require.Equal(t, string(StatusFailed), persistedRun.Status)
	require.Contains(t, persistedRun.Error, "run timed out after 2s")

	var persisted []models.TaskRun
	require.NoError(t, db.Where("job_run_id = ?", runRecord.ID).Order("id").Find(&persisted).Error)
	require.Len(t, persisted, 3)
	for _, row := range persisted {
		require.Equal(t, string(TaskStatusFailed), row.Status)
		require.Empty(t, row.ClaimedBy)
		require.Nil(t, row.ClaimExpiresAt)
		require.False(t, row.PartitionRetryPending)
		require.Contains(t, row.Error, "run timed out after 2s")
	}

	require.ErrorIs(t, store.CompleteTaskClaimed(runRecord.ID, rows[0].ID, "success", "worker-a", nil, nil), ErrTaskClaimMismatch)
	require.ErrorIs(t, store.CacheHitTaskClaimed(runRecord.ID, rows[1].ID, CacheHitSource{}, "cached", "worker-b", nil, nil), ErrTaskClaimMismatch)
	require.ErrorIs(t, store.CompleteTaskOwner(runRecord.ID, rows[0].ID, TaskStatusSucceeded, "success", "", "worker-a", nil, nil, 1, 1, nil, nil), ErrRunTerminal)
}

func TestConcurrentRunTimeoutSerializesTerminalTaskWrites(t *testing.T) {
	for _, tc := range []struct {
		name            string
		terminalTask    TaskStatus
		terminalErr     string
		ownerCompletion bool
		complete        func(*Store, models.TaskRun) error
	}{
		{
			name:         "local success",
			terminalTask: TaskStatusSucceeded,
			complete: func(store *Store, taskRun models.TaskRun) error {
				return store.CompleteTask(taskRun.JobRunID, taskRun.ID, "success", nil, nil)
			},
		},
		{
			name:         "local failure",
			terminalTask: TaskStatusFailed,
			terminalErr:  "local failure",
			complete: func(store *Store, taskRun models.TaskRun) error {
				return store.FailTask(taskRun.JobRunID, taskRun.ID, errors.New("local failure"))
			},
		},
		{
			name:         "claimed success",
			terminalTask: TaskStatusSucceeded,
			complete: func(store *Store, taskRun models.TaskRun) error {
				return store.CompleteTaskClaimed(taskRun.JobRunID, taskRun.ID, "success", taskRun.ClaimedBy, nil, nil)
			},
		},
		{
			name:         "claimed cache hit",
			terminalTask: TaskStatusCached,
			complete: func(store *Store, taskRun models.TaskRun) error {
				return store.CacheHitTaskClaimed(taskRun.JobRunID, taskRun.ID, CacheHitSource{}, "cached", taskRun.ClaimedBy, nil, nil)
			},
		},
		{
			name:            "owner completion",
			terminalTask:    TaskStatusSucceeded,
			ownerCompletion: true,
			complete: func(store *Store, taskRun models.TaskRun) error {
				return store.CompleteTaskOwner(taskRun.JobRunID, taskRun.ID, TaskStatusSucceeded, "success", "", taskRun.ClaimedBy, nil, nil, 1, 1, nil, nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", filepath.Join(t.TempDir(), "deadline.db"))
			db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(models.All...))
			store := NewStore(db)

			jobID := uuid.New()
			require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "concurrent-timeout"}).Error)
			runRecord, err := store.Start(jobID, nil)
			require.NoError(t, err)
			taskRun := models.TaskRun{
				ID: uuid.New(), JobRunID: runRecord.ID, TaskID: uuid.New(),
				Status: string(TaskStatusRunning), ClaimedBy: "worker-race", ClaimAttempt: 1,
			}
			require.NoError(t, db.Create(&taskRun).Error)

			reachedWrite := make(chan struct{})
			releaseWrite := make(chan struct{})
			var once sync.Once
			callbackName := "test:block_terminal_task_write"
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Table != "task_runs" {
					return
				}
				updates, ok := tx.Statement.Dest.(map[string]any)
				if !ok || updates["status"] != string(tc.terminalTask) {
					return
				}
				if tc.terminalErr != "" && updates["error"] != tc.terminalErr {
					return
				}
				once.Do(func() {
					close(reachedWrite)
					<-releaseWrite
				})
			}))
			t.Cleanup(func() { _ = db.Callback().Update().Remove(callbackName) })

			completionDone := make(chan error, 1)
			go func() { completionDone <- tc.complete(store, taskRun) }()
			select {
			case <-reachedWrite:
			case <-time.After(2 * time.Second):
				t.Fatal("completion did not reach the terminal TaskRun write")
			}

			timeoutDone := make(chan error, 1)
			go func() {
				_, timeoutErr := store.CompleteIfActive(runRecord.ID, NewRunDeadlineError(time.Second))
				timeoutDone <- timeoutErr
			}()
			select {
			case err := <-timeoutDone:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				close(releaseWrite)
				t.Fatal("run timeout could not commit while terminal task write was paused")
			}
			close(releaseWrite)
			select {
			case err := <-completionDone:
				if tc.ownerCompletion {
					require.ErrorIs(t, err, ErrRunTerminal,
						"owner completion must report the timeout's durable terminal fence")
				} else {
					require.True(t, errors.Is(err, ErrTaskClaimMismatch) || err == nil, "unexpected completion result: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("terminal task writer did not finish after timeout")
			}

			var gotRun models.JobRun
			var gotTask models.TaskRun
			require.NoError(t, db.First(&gotRun, "id = ?", runRecord.ID).Error)
			require.NoError(t, db.First(&gotTask, "id = ?", taskRun.ID).Error)
			require.Equal(t, string(StatusFailed), gotRun.Status)
			require.Equal(t, string(TaskStatusFailed), gotTask.Status,
				"a task write that began before but committed after timeout must not publish a late terminal result")
		})
	}
}
