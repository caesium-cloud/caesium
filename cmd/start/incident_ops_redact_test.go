package start

import (
	"context"
	"errors"
	"testing"
	"time"

	contractenforce "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/freshness"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRedactPatchErrorHidesOtherNamespaces (identity-and-access A8): a
// validation error naming a job in another namespace is replaced before it
// reaches the incident's action timeline; one naming only the incident
// namespace's jobs (or the patched job) is kept verbatim.
func TestRedactPatchErrorHidesOtherNamespaces(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	now := time.Now().UTC()
	for alias, namespace := range map[string]string{"mkt-a": "marketing", "mkt-b": "marketing", "fin-secret": "finance"} {
		require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
	}

	foreign := &freshness.GraphError{Err: freshness.ErrDatasetMultipleProducers, Dataset: "fin-dataset", Jobs: []string{"mkt-a", "fin-secret"}}
	err := redactPatchError(ctx, db, "marketing", "mkt-a", foreign)
	require.ErrorIs(t, err, errPatchConflictsElsewhere)
	require.NotContains(t, err.Error(), "fin-secret")
	require.NotContains(t, err.Error(), "fin-dataset")

	own := &freshness.GraphError{Err: freshness.ErrDatasetMultipleProducers, Dataset: "d", Jobs: []string{"mkt-a", "mkt-b", "new-self"}}
	require.Equal(t, own, redactPatchError(ctx, db, "marketing", "new-self", own))

	cycle := &internaljobdef.TriggerChainCycleError{Path: []string{"mkt-a", "fin-secret", "mkt-a"}}
	require.ErrorIs(t, redactPatchError(ctx, db, "marketing", "mkt-a", cycle), errPatchConflictsElsewhere)

	plain := errors.New("step image is required")
	require.Equal(t, plain, redactPatchError(ctx, db, "marketing", "mkt-a", plain))

	visible := patchContractAudience(ctx, "marketing", "mkt-a")(db)
	require.True(t, visible(contractenforce.ContractFinding{Producer: "mkt-a", Consumer: "mkt-b"}))
	require.False(t, visible(contractenforce.ContractFinding{Producer: "mkt-a", Consumer: "fin-secret"}))
	require.False(t, visible(contractenforce.ContractFinding{Producer: "mkt-a", Consumer: ""}))
}
