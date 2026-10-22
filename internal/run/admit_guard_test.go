package run

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestAdmitRunNamespaceGuard (identity-and-access A6): AdmitRun, the native
// triggers' policy-only admission, runs the start guard too, before any
// policy decision, so a confined manual fire's job that moved away is never
// admitted, queued or folded into a running run; a passing guard changes
// nothing (no policy ⇒ not handled).
func TestAdmitRunNamespaceGuard(t *testing.T) {
	db, store := openIdempotencyStore(t)
	refused := errors.New("not in marketing")
	guard := func(namespace string) error {
		if namespace != "marketing" {
			return refused
		}
		return nil
	}
	plain := &models.Job{ID: uuid.New(), Alias: "admit-guard-plain", Namespace: "finance"}
	require.NoError(t, db.Create(plain).Error)
	limited := createConcurrencyJob(t, db, "admit-guard-limited", jobdef.ConcurrencyStrategyQueue, 1)
	require.NoError(t, db.Model(limited).Update("namespace", "finance").Error)
	for _, job := range []*models.Job{plain, limited} {
		admitted, handled, err := store.AdmitRun(job.ID, nil, WithStartNamespaceGuard(guard))
		require.ErrorIs(t, err, refused, job.Alias)
		require.True(t, handled, job.Alias)
		require.Nil(t, admitted, job.Alias)
		require.Zero(t, countJobRuns(t, db, job.ID), job.Alias)
		var queued int64
		require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Count(&queued).Error)
		require.Zero(t, queued, job.Alias)
	}
	require.NoError(t, db.Model(plain).Update("namespace", "marketing").Error)
	admitted, handled, err := store.AdmitRun(plain.ID, nil, WithStartNamespaceGuard(guard))
	require.NoError(t, err)
	require.False(t, handled)
	require.Nil(t, admitted)
}
