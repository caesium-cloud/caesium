package replay

import (
	"errors"
	"testing"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestReplayNamespaceGuardRunsBeforeResume: an idempotent retry finds the
// replay recorded under its fingerprint; the guard sees that replay's
// namespace and a refusal returns before anything is resumed or dispatched.
func TestReplayNamespaceGuardRunsBeforeResume(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := runstorage.NewStore(db)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	jobID, baselineRunID, replayRunID, triggerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	keyID := uuid.New()
	principal := &iauth.Principal{Kind: iauth.PrincipalAPIKey, KeyID: &keyID}
	fingerprint, err := Fingerprint(jobID, baselineRunID, principal, nil, "guard-key")
	require.NoError(t, err)

	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeHTTP, CreatedAt: now, UpdatedAt: now}).Error)
	// The job is back in marketing; the replay it made stayed in finance.
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "guarded-replay", Namespace: "marketing", TriggerID: triggerID, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&models.JobRun{ID: baselineRunID, JobID: jobID, Namespace: "marketing", TriggerID: triggerID,
		Status: string(runstorage.StatusSucceeded), StartedAt: now, CompletedAt: &now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&models.JobRun{ID: replayRunID, JobID: jobID, Namespace: "finance", Status: string(runstorage.StatusRunning),
		Quarantine: true, ReplayFingerprint: &fingerprint, TriggerType: "replay", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&models.TaskRun{ID: uuid.New(), JobRunID: replayRunID, TaskID: uuid.New(), AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: `["sh","-c","echo replay"]`,
		Status: string(runstorage.TaskStatusPending), Attempt: 1, MaxAttempts: 1, Quarantine: true, ReplaySafe: true,
		CreatedAt: now, UpdatedAt: now}).Error)

	dispatcher := &recordingDispatcher{}
	svc := &Service{ctx: replayTestContext(t), store: store, dispatcher: dispatcher, executionMode: func() string { return "distributed" }}
	refused := errors.New("no runner in finance")
	var seen []string
	_, err = svc.Replay(Request{
		JobID: jobID, BaselineRunID: baselineRunID, IdempotencyKey: "guard-key", Principal: principal,
		NamespaceGuard: func(namespace string) error {
			seen = append(seen, namespace)
			return refused
		},
	})
	require.ErrorIs(t, err, refused)
	require.Equal(t, []string{"finance"}, seen)
	require.Empty(t, dispatcher.calls, "a refused retry must not resume the replay")

	result, err := svc.Replay(Request{
		JobID: jobID, BaselineRunID: baselineRunID, IdempotencyKey: "guard-key", Principal: principal,
		NamespaceGuard: func(string) error { return nil },
	})
	require.NoError(t, err)
	require.True(t, result.Existing)
	require.Equal(t, []uuid.UUID{replayRunID}, dispatcher.calls)
}

// TestReplayNamespaceGuardRunsBeforeMaterialization: a new replay is checked
// against the namespace Prepare read from the job (the one Materialize will
// create the run in), and a refusal writes and dispatches nothing.
func TestReplayNamespaceGuardRunsBeforeMaterialization(t *testing.T) {
	f := newServiceReplayFixture(t)
	f.seedTask(t, true, "success")
	// Authorised while the job was in marketing; it has moved to finance.
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.jobID).Update("namespace", "finance").Error)

	dispatcher := &recordingDispatcher{}
	svc := (&Service{ctx: replayTestContext(t), store: f.store, dispatcher: dispatcher}).WithExecutionMode("distributed")
	refused := errors.New("no runner in finance")
	var seen []string
	_, err := svc.Replay(Request{
		JobID: f.jobID, BaselineRunID: f.runID, Set: map[string]string{"mode": "what-if"},
		IdempotencyKey: "guard-new", Principal: f.principal,
		NamespaceGuard: func(namespace string) error {
			seen = append(seen, namespace)
			return refused
		},
	})
	require.ErrorIs(t, err, refused)
	require.Equal(t, []string{"finance"}, seen)
	require.Empty(t, dispatcher.calls)
	var replays int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("quarantine = ?", true).Count(&replays).Error)
	require.Zero(t, replays, "a refused replay is never materialized")
}
