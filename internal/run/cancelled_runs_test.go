package run

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// CancelledRunIDs is the local cancel registry's reconciliation read
// (internal/job/cancel_registry.go). Its contract is narrow on purpose and both
// halves matter: it must find a `cancelled` run, and it must NOT report any
// other terminal status — a sweep that treated `succeeded`/`failed` as "stop
// this run" would cancel the engine's own completion path, and `skipped` runs
// never have an engine at all.
func TestCancelledRunIDsSelectsOnlyCancelledRuns(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)

	byStatus := make(map[Status]uuid.UUID, 5)
	for _, status := range []Status{StatusRunning, StatusSucceeded, StatusFailed, StatusCancelled, StatusSkipped} {
		runRecord, err := store.Start(uuid.New(), nil)
		require.NoError(t, err)
		require.NoError(t, db.Model(&models.JobRun{}).
			Where("id = ?", runRecord.ID).
			Update("status", string(status)).Error)
		byStatus[status] = runRecord.ID
	}

	all := make([]uuid.UUID, 0, len(byStatus))
	for _, id := range byStatus {
		all = append(all, id)
	}

	cancelled, err := store.CancelledRunIDs(context.Background(), all)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{byStatus[StatusCancelled]}, cancelled,
		"only a cancelled run may be reconciled: every other status either has no engine or is written BY the engine that is still finishing")
}

func TestCancelledRunIDsIsBatchedAndTolerantOfUnknownIDs(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)

	first, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	second, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.JobRun{}).
		Where("id IN ?", []uuid.UUID{first.ID, second.ID}).
		Update("status", string(StatusCancelled)).Error)

	// A run id the registry still holds but whose row is gone (archived, or a
	// run that never got a row) must not turn the whole sweep into an error.
	ids := []uuid.UUID{first.ID, uuid.New(), second.ID}
	cancelled, err := store.CancelledRunIDs(context.Background(), ids)
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{first.ID, second.ID}, cancelled,
		"the sweep asks about its whole registered set in one statement, not one Get per run")

	empty, err := store.CancelledRunIDs(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, empty, "an idle node must not issue a statement at all")
}
