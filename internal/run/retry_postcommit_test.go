package run

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestWholeRetryPostCommitReadFailurePreservesIdentityAndResets(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		for _, quarantine := range []bool{false, true} {
			name := "manual"
			if admitted {
				name = "agent"
			}
			if quarantine {
				name += "_quarantine"
			}
			t.Run(name, func(t *testing.T) {
				db := testutil.OpenTestDB(t)
				t.Cleanup(func() { testutil.CloseDB(db) })
				store := NewStore(db)
				j := createConcurrencyJob(t, db, "retry-read-"+uuid.NewString(), jobdef.ConcurrencyStrategyQueue, 1)
				runID := seedFailedRun(t, db, j.ID)
				params := `{"input":"durable","zero":"0"}`
				require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Updates(map[string]any{"params": params, "quarantine": quarantine}).Error)
				a := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "frozen:1", Command: `["false"]`}
				require.NoError(t, db.Create(a).Error)
				var taskIDs []uuid.UUID
				for _, status := range []TaskStatus{TaskStatusFailed, TaskStatusSucceeded} {
					task := &models.Task{ID: uuid.New(), JobID: j.ID, AtomID: a.ID, Name: string(status)}
					require.NoError(t, db.Create(task).Error)
					taskIDs = append(taskIDs, task.ID)
					require.NoError(t, db.Create(&models.TaskRun{ID: uuid.New(), JobRunID: runID, TaskID: task.ID, AtomID: a.ID, Status: string(status), Engine: a.Engine, Image: a.Image, Command: a.Command}).Error)
				}
				fault := errors.New("post-commit retry read unavailable")
				reads := 0
				const callback = "test:retry_postcommit_read"
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
					// Allow the load used by run_retried inside the transaction. Only the
					// joined refresh after commit fails, so this is not a rollback case.
					_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
					if tx.Statement.Table == "job_runs" && len(tx.Statement.Joins) > 0 && !transaction {
						reads++
						_ = tx.AddError(fault)
					}
				}))
				t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
				retry := store.RetryFromFailure
				if admitted {
					retry = store.RetryFromFailureAdmitted
				}
				got, err := retry(runID)
				require.Nil(t, got)
				require.ErrorIs(t, err, fault)
				var committed *RunCommittedError
				require.ErrorAs(t, err, &committed)
				require.Equal(t, runID, committed.RunID)
				require.Equal(t, j.ID, committed.JobID)
				require.Equal(t, 1, reads)
				require.NoError(t, db.Callback().Query().Remove(callback))
				row, err := store.Get(runID)
				require.NoError(t, err)
				require.Equal(t, StatusRunning, row.Status)
				require.Equal(t, map[string]string{"input": "durable", "zero": "0"}, row.Params)
				require.Equal(t, quarantine, row.Quarantine)
				statuses := map[uuid.UUID]TaskStatus{}
				for _, task := range row.Tasks {
					statuses[task.TaskID] = task.Status
				}
				require.Equal(t, TaskStatusPending, statuses[taskIDs[0]])
				require.Equal(t, TaskStatusSucceeded, statuses[taskIDs[1]])
				var events int64
				require.NoError(t, db.Model(&models.ExecutionEvent{}).Where("run_id = ? AND type = ?", runID, "run_retried").Count(&events).Error)
				require.EqualValues(t, 1, events)
				_, active := store.startedRuns[runID]
				require.Equal(t, !quarantine, active)
			})
		}
	}
}

func TestWholeRetryPreCommitFailureHasNoCommittedIdentity(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	j := createConcurrencyJob(t, db, "retry-refused", jobdef.ConcurrencyStrategyQueue, 1)
	runID := seedFailedRun(t, db, j.ID)
	require.NoError(t, db.Model(j).Update("paused", true).Error)
	_, err := store.RetryFromFailureAdmitted(runID)
	require.ErrorIs(t, err, ErrJobPaused)
	_, committed := CommittedRunID(err)
	require.False(t, committed)
	row, err := store.Get(runID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, row.Status)
}
