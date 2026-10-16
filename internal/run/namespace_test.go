package run

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStartStampsNamespaceAtAdmission(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := &models.Job{ID: uuid.New(), Alias: "namespace-admission", Namespace: "marketing"}
	require.NoError(t, db.Create(job).Error)
	ordinary, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.Equal(t, "marketing", ordinary.Namespace)
	require.NoError(t, db.Model(job).Update("namespace", "finance").Error)
	backfill, err := store.StartForBackfill(job.ID, uuid.New(), nil)
	require.NoError(t, err)
	require.Equal(t, "finance", backfill.Namespace)
	historical, err := store.Get(ordinary.ID)
	require.NoError(t, err)
	require.Equal(t, "marketing", historical.Namespace)
	var persisted models.JobRun
	require.NoError(t, db.First(&persisted, "id = ?", backfill.ID).Error)
	require.Equal(t, "finance", persisted.Namespace)
}

// TestStartNamespaceGuardRunsInsideAdmission: the guard sees the namespace
// admission freezes from the job row (not a value the caller looked up
// earlier), and its refusal aborts the start before anything is written, on
// the plain and the concurrency-policy admission paths alike.
func TestStartNamespaceGuardRunsInsideAdmission(t *testing.T) {
	db, store := openIdempotencyStore(t)
	plain := &models.Job{ID: uuid.New(), Alias: "guard-plain", Namespace: "marketing"}
	require.NoError(t, db.Create(plain).Error)
	limited := createConcurrencyJob(t, db, "guard-limited", jobdef.ConcurrencyStrategyQueue, 1)
	refused := errors.New("not in finance")

	for _, job := range []*models.Job{plain, limited} {
		// Authorised against marketing, then the job moved to finance.
		require.NoError(t, db.Model(job).Update("namespace", "finance").Error)
		var seen []string
		guard := func(namespace string) error {
			seen = append(seen, namespace)
			if namespace != "marketing" {
				return refused
			}
			return nil
		}
		_, err := store.StartWithResult(t.Context(), job.ID, nil,
			WithStartNamespaceGuard(guard), WithStartIdempotencyKey("guard-"+job.Alias))
		require.ErrorIs(t, err, refused, job.Alias)
		require.Equal(t, []string{"finance"}, seen, job.Alias)
		require.Zero(t, countJobRuns(t, db, job.ID), "%s: a refused start writes no run", job.Alias)
		var recorded int64
		require.NoError(t, db.Model(&models.RunStartIdempotency{}).Where("job_id = ?", job.ID).Count(&recorded).Error)
		require.Zero(t, recorded, "%s: a refused start records no idempotent outcome", job.Alias)
		var queued int64
		require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Count(&queued).Error)
		require.Zero(t, queued, job.Alias)

		allowed, err := store.StartWithResult(t.Context(), job.ID, nil,
			WithStartNamespaceGuard(func(string) error { return nil }))
		require.NoError(t, err, job.Alias)
		require.Equal(t, StartOutcomeCreated, allowed.Outcome, job.Alias)
		require.Equal(t, "finance", allowed.Run.Namespace, job.Alias)
	}
}

// TestConcurrencyAdmissionStampsNamespace: a job with a concurrency policy is
// admitted through the conditional INSERT, which must persist the owning
// job's namespace, not the column default. Route authorisation (A3) and
// incidents both read the run's persisted namespace.
func TestConcurrencyAdmissionStampsNamespace(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "namespace-concurrency", jobdef.ConcurrencyStrategyFail, 2)
	require.NoError(t, db.Model(job).Update("namespace", "marketing").Error)

	result, err := store.StartWithResult(t.Context(), job.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeCreated, result.Outcome)
	require.Equal(t, "marketing", result.Run.Namespace)
	var persisted models.JobRun
	require.NoError(t, db.First(&persisted, "id = ?", result.Run.ID).Error)
	require.Equal(t, "marketing", persisted.Namespace)
}
