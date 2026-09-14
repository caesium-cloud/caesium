package run

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestSecretLogCaptureKeepsMarkersRawAndSnapshotExactOnly(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskRunID := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: uuid.New(), Status: string(StatusRunning)}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: uuid.New(), AtomID: uuid.New(),
		Status: string(TaskStatusRunning), Attempt: 1,
	}).Error)
	fence := SecretLogFence{Attempt: 1}
	require.NoError(t, store.PrepareSecretTaskLog(runID, taskRunID, fence))
	collector := NewSecretLogCollector(store, runID, taskRunID, fence, []string{"abc"}, 1<<20)
	raw := "ref=secret://env/QA key=abc normal=AKIAJ83HFKD9SLXMZ7Q2b8Xy1pQ9rT4\n" +
		"##caesium::output {\"token\":\"abc\"}\n"
	markers, err := CaptureSecretTaskLogs(io.NopCloser(strings.NewReader(raw)), collector, 0, 0)
	require.NoError(t, err)
	require.Equal(t, "abc", markers.Output["token"], "redaction must not mutate output protocol parsing")
	require.Contains(t, markers.LogText, "secret://env/QA")
	require.Contains(t, markers.LogText, "AKIAJ83HFKD9SLXMZ7Q2b8Xy1pQ9rT4")
	require.NotContains(t, markers.LogText, "key=abc")
	require.Contains(t, markers.LogText, "key=[REDACTED]")
}

func TestSecretLogWritesAreAttemptAndClaimFenced(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID, taskRunID := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: uuid.New(), Status: string(StatusRunning)}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: uuid.New(), AtomID: uuid.New(),
		Status: string(TaskStatusRunning), Attempt: 2, ClaimedBy: "worker-a", ClaimAttempt: 7,
	}).Error)

	good := SecretLogFence{Attempt: 2, Claim: &TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 7}}
	require.NoError(t, store.PrepareSecretTaskLog(runID, taskRunID, good))
	require.NoError(t, store.SaveSecretTaskLogSnapshot(runID, taskRunID, good, &TaskLogSnapshot{Text: "safe"}))
	require.ErrorIs(t, store.SaveSecretTaskLogSnapshot(runID, taskRunID,
		SecretLogFence{Attempt: 1, Claim: good.Claim}, &TaskLogSnapshot{Text: "stale-attempt"}), ErrTaskClaimMismatch)
	require.ErrorIs(t, store.SaveSecretTaskLogSnapshot(runID, taskRunID,
		SecretLogFence{Attempt: 2, Claim: &TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 6}},
		&TaskLogSnapshot{Text: "stale-claim"}), ErrTaskClaimMismatch)

	state, err := store.TaskLogReadStateForInstance(context.Background(), runID, taskRunID)
	require.NoError(t, err)
	require.True(t, state.Scrubbed)
	require.Equal(t, 2, state.Attempt)
	require.Equal(t, "worker-a", state.ClaimedBy)
	require.Equal(t, 7, state.ClaimAttempt)
	require.Equal(t, "safe", state.Snapshot.Text)

	require.NoError(t, store.SaveCapturedTaskLogSnapshot(runID, taskRunID,
		&TaskLogSnapshot{Text: "generic-stale-overwrite"}))
	state, err = store.TaskLogReadStateForInstance(context.Background(), runID, taskRunID)
	require.NoError(t, err)
	require.Equal(t, "safe", state.Snapshot.Text,
		"the shared executor final-save seam must leave secret logs to the fenced collector")
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", taskRunID).Update("log_scrubbed", false).Error)
	require.NoError(t, store.SaveCapturedTaskLogSnapshot(runID, taskRunID,
		&TaskLogSnapshot{Text: "ordinary-final-save"}))
	state, err = store.TaskLogReadStateForInstance(context.Background(), runID, taskRunID)
	require.NoError(t, err)
	require.Equal(t, "ordinary-final-save", state.Snapshot.Text,
		"the same atomic seam must continue to save non-secret task logs")
}

func TestSecretLogReclaimClearsOnlySanitizedSnapshotAndPreservesClaimCount(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: uuid.New(), Status: string(StatusRunning)}).Error)

	rows := []models.TaskRun{
		{
			ID: uuid.New(), JobRunID: runID, TaskID: uuid.New(), AtomID: uuid.New(),
			Status: string(TaskStatusRunning), Attempt: 1, ClaimedBy: "worker-a", ClaimAttempt: 4,
			LogScrubbed: true, LogText: "old sanitized claim", LogTruncated: true,
		},
		{
			ID: uuid.New(), JobRunID: runID, TaskID: uuid.New(), AtomID: uuid.New(),
			Status: string(TaskStatusRunning), Attempt: 1, ClaimedBy: "worker-a", ClaimAttempt: 4,
			LogText: "ordinary retained claim", LogTruncated: true,
		},
	}
	require.NoError(t, db.Create(&rows).Error)
	require.NoError(t, store.ResetInFlightTasks(runID))

	var scrubbed, ordinary models.TaskRun
	require.NoError(t, db.First(&scrubbed, "id = ?", rows[0].ID).Error)
	require.NoError(t, db.First(&ordinary, "id = ?", rows[1].ID).Error)
	require.Equal(t, "", scrubbed.ClaimedBy)
	require.Equal(t, 4, scrubbed.ClaimAttempt, "returning to pending is not a new claim")
	require.Empty(t, scrubbed.LogText)
	require.False(t, scrubbed.LogTruncated)
	require.Equal(t, "ordinary retained claim", ordinary.LogText)
	require.True(t, ordinary.LogTruncated)
}

func TestSecretLogRoutingIsRegisteredBeforeExecutionAndSurvivesRetryReset(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	jobID, runID := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "secret-log-registration"}).Error)
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: jobID, Status: string(StatusRunning)}).Error)

	spec, err := json.Marshal(container.Spec{Env: map[string]string{
		"CREDENTIAL": "secret://env/CAESIUM_IT_REGISTRY_CREDS",
	}})
	require.NoError(t, err)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Spec: datatypes.JSON(spec)}
	task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "secret"}
	require.NoError(t, db.Create(atom).Error)
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, store.RegisterTask(runID, task, atom, 0))

	var persisted models.TaskRun
	require.NoError(t, db.First(&persisted, "job_run_id = ? AND task_id = ?", runID, task.ID).Error)
	require.True(t, persisted.LogScrubbed,
		"the API routing bit must exist before a worker can publish a runtime ID")
	_, resetByRetry := retryResetColumns()["log_scrubbed"]
	require.False(t, resetByRetry,
		"retry clears the snapshot but retains the frozen spec's secret-log routing")
}
