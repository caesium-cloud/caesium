package run

import (
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
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
