package job

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestStartNamespaceGuardRefusesAForeignRunningRun (identity-and-access A6):
// a guarded start (a confined manual fire) whose AdmitRun check passes must
// not then adopt a running row persisted outside the caller's authority — the
// interleaving where the idle job moved, was started elsewhere and moved back
// between AdmitRun's commit and the running-run lookup. The row is left
// untouched and Run returns the guard's refusal.
func TestStartNamespaceGuardRefusesAForeignRunningRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := run.NewStore(db)
	model := &models.Job{ID: uuid.New(), Alias: "guard-" + uuid.NewString()[:8], Namespace: "marketing"}
	require.NoError(t, db.Create(model).Error)
	foreign, err := store.Start(model.ID, nil)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", foreign.ID).Update("namespace", "finance").Error)

	refused := errors.New("outside the caller's namespaces")
	var seen []string
	guard := func(namespace string) error {
		seen = append(seen, namespace)
		if namespace != "marketing" {
			return refused
		}
		return nil
	}
	j := New(model, WithRunStoreFactory(func() *run.Store { return store }), WithStartNamespaceGuard(guard))
	require.ErrorIs(t, j.Run(t.Context()), refused)
	require.Equal(t, []string{"marketing", "finance"}, seen, "AdmitRun's check, then the adopted row's")

	var runs []models.JobRun
	require.NoError(t, db.Where("job_id = ?", model.ID).Find(&runs).Error)
	require.Len(t, runs, 1, "nothing new was admitted")
	require.Equal(t, string(run.StatusRunning), runs[0].Status, "the foreign run is left alone")
}
