package dataset

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A dataset an active hold is on, whose state row does not exist (assertions on,
// freshness scheduling off: nothing creates one), must appear in ?status=held
// with the right total and pagination, merged with held datasets that have a
// state row - not only in the unfiltered feed.
func TestHeldFilterIncludesDeclarationOnlyDatasets(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false"); require.NoError(t, env.Process()) })

	base := time.Now().UTC().Add(-time.Hour)
	declare := func(name string, age time.Duration) {
		at := base.Add(age)
		require.NoError(t, conn.Create(&models.DatasetDeclaration{ID: uuid.New(), JobID: uuid.New(), JobAlias: "j-" + name, StepName: "load",
			Name: name, Direction: models.DatasetDirectionProduces, CreatedAt: at, UpdatedAt: at}).Error)
	}
	hold := func(name string) {
		key := models.DatasetHoldKey("", name)
		require.NoError(t, conn.Create(&models.DatasetHold{ID: uuid.New(), Name: name, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
			Reason: "min", HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: base, CreatedAt: base, UpdatedAt: base}).Error)
	}

	// Two held datasets with no state row, one unheld declaration-only dataset,
	// and one held dataset that has a state row.
	declare("declared-held-a", 1*time.Minute)
	declare("declared-held-b", 2*time.Minute)
	declare("declared-clean", 3*time.Minute)
	hold("declared-held-a")
	hold("declared-held-b")
	declare("observed-held", 4*time.Minute)
	hold("observed-held")
	require.NoError(t, conn.Create(&models.DatasetState{Name: "observed-held", Status: models.DatasetStatusFresh, Watermark: "1",
		CreatedAt: base.Add(5 * time.Minute), UpdatedAt: base.Add(5 * time.Minute)}).Error)

	names := func(res *ListResult) []string {
		out := make([]string, 0, len(res.Datasets))
		for _, state := range res.Datasets {
			require.Equal(t, models.DatasetStatusHeld, state.Status, state.Name)
			require.Contains(t, state.Reason, fmt.Sprintf("dataset_hold:/%s hold=", state.Name))
			out = append(out, state.Name)
		}
		return out
	}

	all, err := svc.List(ListParams{Status: models.DatasetStatusHeld})
	require.NoError(t, err)
	require.EqualValues(t, 3, all.Total)
	require.Equal(t, []string{"observed-held", "declared-held-b", "declared-held-a"}, names(all), "newest first, state-backed and declaration-only merged")

	// Pagination over the merged set: pages are disjoint and cover the total.
	first, err := svc.List(ListParams{Status: models.DatasetStatusHeld, Limit: 2})
	require.NoError(t, err)
	second, err := svc.List(ListParams{Status: models.DatasetStatusHeld, Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.EqualValues(t, 3, first.Total)
	require.EqualValues(t, 3, second.Total)
	require.Equal(t, []string{"observed-held", "declared-held-b"}, names(first))
	require.Equal(t, []string{"declared-held-a"}, names(second))

	// The held declaration-only datasets are not unknown, and `fresh` never
	// lists a held dataset.
	unknown, err := svc.List(ListParams{Status: models.DatasetStatusUnknown})
	require.NoError(t, err)
	require.EqualValues(t, 1, unknown.Total)
	require.Equal(t, "declared-clean", unknown.Datasets[0].Name)
	fresh, err := svc.List(ListParams{Status: models.DatasetStatusFresh})
	require.NoError(t, err)
	require.Empty(t, fresh.Datasets)

	// With the circuit breaker off, no hold counts and nothing is merged.
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
	require.NoError(t, env.Process())
	off, err := svc.List(ListParams{Status: models.DatasetStatusHeld})
	require.NoError(t, err)
	require.Empty(t, off.Datasets)
	require.EqualValues(t, 0, off.Total)
}
