package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedFailedRun creates a terminal (failed) run for a job so retry paths have a
// run to reset.
func seedFailedRun(t *testing.T, db *gorm.DB, jobID uuid.UUID) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:          runID,
		JobID:       jobID,
		Status:      string(StatusFailed),
		StartedAt:   now.Add(-time.Minute),
		CompletedAt: &now,
		CreatedAt:   now.Add(-time.Minute),
		UpdatedAt:   now,
	}).Error)
	return runID
}

func seedRunningRun(t *testing.T, db *gorm.DB, jobID uuid.UUID) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    string(StatusRunning),
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return runID
}

// Safety valve 1: an agent retry is refused while the job is paused; a manual
// retry (human decision) is not.
func TestRetryFromFailureAdmittedRefusesPausedJob(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	job := createConcurrencyJob(t, db, "paused-retry", jobdef.ConcurrencyStrategyQueue, 1)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", job.ID).Update("paused", true).Error)
	runID := seedFailedRun(t, db, job.ID)

	_, err := store.RetryFromFailureAdmitted(runID)
	require.ErrorIs(t, err, ErrJobPaused)

	// The run must remain terminal — the refused retry did not flip it.
	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", runID).Error)
	require.Equal(t, string(StatusFailed), row.Status)

	// The manual/human entry point ignores the pause and retries.
	r, err := store.RetryFromFailure(runID)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.NoError(t, db.First(&row, "id = ?", runID).Error)
	require.Equal(t, string(StatusRunning), row.Status)
}

// Safety valve 2: an agent retry re-admits under queue semantics. On a job at
// max concurrency it is refused (never replace-cancels the live run), even when
// the declared strategy is replace.
func TestRetryFromFailureAdmittedRefusesWhenNoSlot(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	// Declared strategy is replace — an agent retry must NOT honor it.
	job := createConcurrencyJob(t, db, "full-retry", jobdef.ConcurrencyStrategyReplace, 1)
	liveRun := seedRunningRun(t, db, job.ID) // occupies the single slot
	failedRun := seedFailedRun(t, db, job.ID)

	_, err := store.RetryFromFailureAdmitted(failedRun)
	require.ErrorIs(t, err, ErrMaxConcurrentRunsReached)

	// The live run is untouched (no replace-cancel) and the failed run stays failed.
	var live models.JobRun
	require.NoError(t, db.First(&live, "id = ?", liveRun).Error)
	require.Equal(t, string(StatusRunning), live.Status)
	var failed models.JobRun
	require.NoError(t, db.First(&failed, "id = ?", failedRun).Error)
	require.Equal(t, string(StatusFailed), failed.Status)
}

// With a free slot the admit-aware retry succeeds.
func TestRetryFromFailureAdmittedAdmitsWhenSlotFree(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	job := createConcurrencyJob(t, db, "free-retry", jobdef.ConcurrencyStrategyQueue, 1)
	failedRun := seedFailedRun(t, db, job.ID)

	r, err := store.RetryFromFailureAdmitted(failedRun)
	require.NoError(t, err)
	require.NotNil(t, r)

	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", failedRun).Error)
	require.Equal(t, string(StatusRunning), row.Status)
}

// A job with no concurrency policy admits the agent retry unconditionally.
func TestRetryFromFailureAdmittedNoPolicy(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "nopolicy-retry-trigger",
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *","timezone":"UTC"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "nopolicy-retry", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	// Even with an active run, no policy means no slot cap.
	seedRunningRun(t, db, job.ID)
	failedRun := seedFailedRun(t, db, job.ID)

	r, err := store.RetryFromFailureAdmitted(failedRun)
	require.NoError(t, err)
	require.NotNil(t, r)
	var row models.JobRun
	require.NoError(t, db.First(&row, "id = ?", failedRun).Error)
	require.Equal(t, string(StatusRunning), row.Status)
}
