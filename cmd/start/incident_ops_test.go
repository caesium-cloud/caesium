package start

import (
	"context"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestIncidentRetryResumesExactCommittedRevision(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db).WithLeaseStore(run.NewLeaseStore(db))
	jobModel := &models.Job{ID: uuid.New(), Alias: "incident-retry-" + uuid.NewString()[:8]}
	require.NoError(t, db.Create(jobModel).Error)
	runRecord, err := store.Start(jobModel.ID, nil, run.WithStartParams(map[string]string{"region": "us-east"}))
	require.NoError(t, err)
	disposition, err := store.CompleteAtStateRevision(runRecord.ID, context.DeadlineExceeded, 1)
	require.NoError(t, err)
	require.Equal(t, run.CompletionFinalized, disposition)

	type resumed struct {
		jobID    uuid.UUID
		runID    uuid.UUID
		params   map[string]string
		revision int64
	}
	resumedCh := make(chan resumed, 1)
	originalResume := incidentResumeRun
	incidentResumeRun = func(j *models.Job, runID uuid.UUID, params map[string]string, revision int64) {
		resumedCh <- resumed{jobID: j.ID, runID: runID, params: params, revision: revision}
	}
	t.Cleanup(func() { incidentResumeRun = originalResume })

	require.NoError(t, newIncidentActionOps(store).RetryFromFailure(context.Background(), runRecord.ID))
	got := <-resumedCh
	require.Equal(t, jobModel.ID, got.jobID)
	require.Equal(t, runRecord.ID, got.runID)
	require.Equal(t, map[string]string{"region": "us-east"}, got.params)
	require.Equal(t, int64(2), got.revision)

	lease, err := store.LeaseStore().GetLease(context.Background(), runRecord.ID)
	require.NoError(t, err)
	require.Equal(t, got.revision, lease.StateRevision)
	current, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatusRunning, current.Status)
}

func TestIncidentRetryPreflightFailureLeavesRunTerminal(t *testing.T) {
	t.Run("missing job", func(t *testing.T) {
		db := jobdeftestutil.OpenTestDB(t)
		t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
		leases := run.NewLeaseStore(db)
		store := run.NewStore(db).WithLeaseStore(leases)
		runID, missingJobID := uuid.New(), uuid.New()
		now := time.Now().UTC()
		require.NoError(t, db.Create(&models.JobRun{
			ID: runID, JobID: missingJobID, Status: string(run.StatusFailed),
			StartedAt: now, CompletedAt: &now,
		}).Error)
		_, err := leases.AcquireLease(context.Background(), runID, "node-a", time.Minute)
		require.NoError(t, err)

		require.Error(t, newIncidentActionOps(store).RetryFromFailure(context.Background(), runID))
		assertIncidentRetryUnchanged(t, store, runID)
	})

	t.Run("malformed params", func(t *testing.T) {
		db := jobdeftestutil.OpenTestDB(t)
		t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
		leases := run.NewLeaseStore(db)
		store := run.NewStore(db).WithLeaseStore(leases)
		jobModel := &models.Job{ID: uuid.New(), Alias: "incident-bad-params-" + uuid.NewString()[:8]}
		require.NoError(t, db.Create(jobModel).Error)
		runID := uuid.New()
		now := time.Now().UTC()
		require.NoError(t, db.Create(&models.JobRun{
			ID: runID, JobID: jobModel.ID, Status: string(run.StatusFailed),
			Params: datatypes.JSON([]byte("{")), StartedAt: now, CompletedAt: &now,
		}).Error)
		_, err := leases.AcquireLease(context.Background(), runID, "node-a", time.Minute)
		require.NoError(t, err)

		require.Error(t, newIncidentActionOps(store).RetryFromFailure(context.Background(), runID))
		assertIncidentRetryUnchanged(t, store, runID)
	})
}

func assertIncidentRetryUnchanged(t *testing.T, store *run.Store, runID uuid.UUID) {
	t.Helper()
	current, err := store.Get(runID)
	require.NoError(t, err)
	require.Equal(t, run.StatusFailed, current.Status)
	lease, err := store.LeaseStore().GetLease(context.Background(), runID)
	require.NoError(t, err)
	require.Equal(t, int64(1), lease.StateRevision)
}
