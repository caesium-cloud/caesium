package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestServerShutdownCompletionPreservesRemoteClaimsAndRetryWork(t *testing.T) {
	for _, cause := range []error{runlife.ErrServerShutdown, fmt.Errorf("owner stopped: %w", runlife.ErrServerShutdown), NewRunCancellationError(runlife.ErrServerShutdown)} {
		t.Run(cause.Error(), func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			store := NewStore(db)
			jobID := uuid.New()
			require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "shutdown-claim-preservation"}).Error)
			jr, err := store.Start(jobID, nil)
			require.NoError(t, err)
			lease := time.Now().UTC().Add(time.Minute)
			rows := []models.TaskRun{
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(TaskStatusRunning), ClaimedBy: "surviving-worker", ClaimExpiresAt: &lease, RuntimeID: "remote-runtime"},
				{ID: uuid.New(), JobRunID: jr.ID, TaskID: uuid.New(), Status: string(TaskStatusPending), PartitionRetryPending: true},
			}
			// CompleteTaskClaimed resolves successor edges from the catalog for
			// these ordinary task rows. Keep that real completion path available.
			atom := models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "hermetic-image", Command: `["true"]`}
			require.NoError(t, db.Create(&atom).Error)
			for idx := range rows {
				rows[idx].AtomID = atom.ID
				require.NoError(t, db.Create(&models.Task{ID: rows[idx].TaskID, JobID: jobID, AtomID: atom.ID, Position: idx}).Error)
			}
			require.NoError(t, db.Create(&rows).Error)
			var beforeRun models.JobRun
			var beforeTasks []models.TaskRun
			var beforeEvents []models.ExecutionEvent
			require.NoError(t, db.First(&beforeRun, "id = ?", jr.ID).Error)
			require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("id").Find(&beforeTasks).Error)
			require.NoError(t, db.Where("run_id = ?", jr.ID).Order("sequence").Find(&beforeEvents).Error)
			finalized, err := store.CompleteIfActive(jr.ID, cause)
			require.NoError(t, err)
			require.False(t, finalized)
			var afterRun models.JobRun
			var afterTasks []models.TaskRun
			var afterEvents []models.ExecutionEvent
			require.NoError(t, db.First(&afterRun, "id = ?", jr.ID).Error)
			require.NoError(t, db.Where("job_run_id = ?", jr.ID).Order("id").Find(&afterTasks).Error)
			require.NoError(t, db.Where("run_id = ?", jr.ID).Order("sequence").Find(&afterEvents).Error)
			require.Equal(t, beforeRun, afterRun)
			require.Equal(t, beforeTasks, afterTasks)
			require.Equal(t, beforeEvents, afterEvents)
			require.NoError(t, store.CompleteTaskClaimed(jr.ID, rows[0].ID, "success", "surviving-worker", nil, nil), "the same worker claim remains valid after shutdown")
			var completed models.TaskRun
			require.NoError(t, db.First(&completed, "id = ?", rows[0].ID).Error)
			require.Equal(t, string(TaskStatusSucceeded), completed.Status)
			require.Equal(t, rows[0].RuntimeID, completed.RuntimeID)
		})
	}
}
