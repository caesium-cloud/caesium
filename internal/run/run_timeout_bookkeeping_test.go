package run

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func timeoutCounter(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var metric dto.Metric
	value, err := vec.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	require.NoError(t, value.(prometheus.Metric).Write(&metric))
	return metric.GetCounter().GetValue()
}

func timeoutDurationCount(t *testing.T, jobID uuid.UUID) uint64 {
	t.Helper()
	var metric dto.Metric
	value, err := metrics.TaskRunDurationSeconds.GetMetricWithLabelValues(jobID.String(), "docker", "failed")
	require.NoError(t, err)
	require.NoError(t, value.(prometheus.Metric).Write(&metric))
	return metric.GetHistogram().GetSampleCount()
}

func TestRunTimeoutBookkeepingIsAtomicAndPerConcreteTask(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "commit"
		if retry {
			name = "busy retry"
		}
		t.Run(name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			jobID, taskID := uuid.New(), uuid.New()
			require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "timeout-bookkeeping"}).Error)
			jr, err := store.Start(jobID, nil)
			require.NoError(t, err)
			past := time.Now().UTC().Add(-time.Second)
			rows := []models.TaskRun{
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(TaskStatusSucceeded), TerminalSequence: 8, Result: "success", Output: []byte(`{"kept":"success"}`)},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Engine: models.AtomEngineDocker, Status: string(TaskStatusSkipped), TerminalSequence: 9, Error: "original skip"},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(TaskStatusRunning), PartitionCount: 4, PartitionIndex: 1, PartitionValue: "running", StartedAt: &past, ClaimedBy: "worker", Output: []byte(`{"kept":"partial"}`)},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(TaskStatusPending), PartitionCount: 4, PartitionIndex: 2, PartitionValue: "pending"},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(TaskStatusPending), PartitionCount: 4, PartitionIndex: 3, PartitionValue: "retry waiting", PartitionRetryPending: true},
			}
			require.NoError(t, db.Create(&rows).Error)
			beforeTasks := timeoutCounter(t, metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "failed")
			beforeWrites := timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryTaskRunStatus)
			beforeEvents := timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryEventInsert)
			beforeStatements := timeoutCounter(t, metrics.DBStatementsTotal, metrics.DBWriteCategoryTaskRunStatus)
			beforeDuration := timeoutDurationCount(t, jobID)
			failedOnce := false
			if retry {
				callback := "test:timeout_bookkeeping_retry"
				require.NoError(t, db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
					if failedOnce || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "execution_events" {
						return
					}
					record, ok := tx.Statement.Dest.(*models.ExecutionEvent)
					if ok && record.Type == string(event.TypeTaskFailed) {
						failedOnce = true
						tx.AddError(errors.New("database is locked"))
					}
				}))
				t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
			}
			finalized, err := store.CompleteIfActive(jr.ID, NewRunDeadlineError(time.Second))
			require.NoError(t, err)
			require.True(t, finalized)
			if retry {
				require.True(t, failedOnce, "the failed event insert must exercise whole-transaction retry")
			}
			var persisted []models.TaskRun
			require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("terminal_sequence ASC").Find(&persisted).Error)
			require.Len(t, persisted, 5)
			require.Equal(t, string(TaskStatusSucceeded), persisted[0].Status)
			require.JSONEq(t, `{"kept":"success"}`, string(persisted[0].Output))
			require.Equal(t, "original skip", persisted[1].Error)
			transitioned := make(map[uuid.UUID]models.TaskRun)
			for i, row := range persisted[2:] {
				require.Equal(t, int64(10+i), row.TerminalSequence)
				require.Equal(t, string(TaskStatusFailed), row.Status)
				require.Contains(t, row.Error, "run timed out after 1s")
				require.Empty(t, row.ClaimedBy)
				require.False(t, row.PartitionRetryPending)
				transitioned[row.ID] = row
			}
			require.JSONEq(t, `{"kept":"partial"}`, string(transitioned[rows[2].ID].Output))
			var recorded []models.ExecutionEvent
			require.NoError(t, db.Where("run_id = ? AND type = ?", jr.ID, string(event.TypeTaskFailed)).Order("sequence ASC").Find(&recorded).Error)
			require.Len(t, recorded, 3)
			seen := make(map[uuid.UUID]bool)
			for _, evt := range recorded {
				var task TaskRun
				require.NoError(t, json.Unmarshal(evt.Payload, &task))
				require.Equal(t, taskID, *evt.TaskID)
				require.Equal(t, TaskStatusFailed, task.Status)
				require.Equal(t, transitioned[task.ID].PartitionValue, task.PartitionValue)
				require.False(t, seen[task.ID], "each concrete instance has exactly one failure event")
				seen[task.ID] = true
			}
			require.Equal(t, beforeTasks+3, timeoutCounter(t, metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "failed"))
			require.Equal(t, beforeDuration+1, timeoutDurationCount(t, jobID), "only the task that started contributes a duration")
			require.Equal(t, beforeWrites+3, timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryTaskRunStatus))
			require.Equal(t, beforeStatements+3, timeoutCounter(t, metrics.DBStatementsTotal, metrics.DBWriteCategoryTaskRunStatus))
			require.Equal(t, beforeEvents+5, timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryEventInsert))
			finalized, err = store.CompleteIfActive(jr.ID, NewRunDeadlineError(time.Second))
			require.NoError(t, err)
			require.False(t, finalized)
			require.Equal(t, beforeDuration+1, timeoutDurationCount(t, jobID))
			require.Equal(t, beforeTasks+3, timeoutCounter(t, metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "failed"))
			require.Equal(t, beforeWrites+3, timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryTaskRunStatus))
			var count int64
			require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ? AND type = ?", jr.ID, string(event.TypeTaskFailed)).Count(&count).Error)
			require.Equal(t, int64(3), count)
		})
	}
}

func TestRunTimeoutEventFailureRollsBackTaskAndRunBookkeeping(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, taskID := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "timeout-rollback"}).Error)
	jr, err := store.Start(jobID, nil)
	require.NoError(t, err)
	row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(TaskStatusRunning), ClaimedBy: "worker", Output: []byte(`{"kept":"partial"}`)}
	require.NoError(t, db.Create(&row).Error)
	beforeWrites := timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryTaskRunStatus)
	writeErr := errors.New("cannot persist timeout evidence")
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:timeout_event_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "execution_events" {
			tx.AddError(writeErr)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove("test:timeout_event_failure") })
	finalized, err := store.CompleteIfActive(jr.ID, NewRunDeadlineError(time.Second))
	require.ErrorIs(t, err, writeErr)
	require.False(t, finalized)
	var gotRun models.JobRun
	var gotTask models.TaskRun
	require.NoError(t, db.First(&gotRun, "id = ?", jr.ID).Error)
	require.NoError(t, db.First(&gotTask, "id = ?", row.ID).Error)
	require.Equal(t, string(StatusRunning), gotRun.Status)
	require.Equal(t, string(TaskStatusRunning), gotTask.Status)
	require.Equal(t, "worker", gotTask.ClaimedBy)
	require.Zero(t, gotTask.TerminalSequence)
	require.JSONEq(t, `{"kept":"partial"}`, string(gotTask.Output))
	require.Zero(t, timeoutCounter(t, metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "failed"))
	require.Equal(t, beforeWrites, timeoutCounter(t, metrics.DBWritesTotal, metrics.DBWriteCategoryTaskRunStatus))
	var count int64
	require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ? AND type IN ?", jr.ID, []string{string(event.TypeTaskFailed), string(event.TypeRunFailed)}).Count(&count).Error)
	require.Zero(t, count)
}

func TestRunTimeoutKeepsQuarantinedTaskMetricsIsolated(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, taskID := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "timeout-quarantine"}).Error)
	jr, err := store.Start(jobID, nil)
	require.NoError(t, err)
	row := models.TaskRun{ID: uuid.New(), JobRunID: jr.ID, TaskID: taskID, Engine: models.AtomEngineDocker, Status: string(TaskStatusRunning), Quarantine: true}
	require.NoError(t, db.Create(&row).Error)
	finalized, err := store.CompleteIfActive(jr.ID, NewRunDeadlineError(time.Second))
	require.NoError(t, err)
	require.True(t, finalized)
	require.Zero(t, timeoutCounter(t, metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "failed"))
	var failed models.ExecutionEvent
	require.NoError(t, db.Where("run_id = ? AND type = ?", jr.ID, string(event.TypeTaskFailed)).First(&failed).Error)
	require.True(t, failed.Quarantine)
}
