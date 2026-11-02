package dataset

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Rows sharing one updated_at must page deterministically: every row exactly
// once, in (updated_at DESC, namespace, name) order, whatever the insert order.
// Cutting a page by updated_at alone and merging by identity used to repeat one
// tied row and never return the other.
func TestListPagesTiedTimestampsWithoutSkippingOrRepeating(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false"); require.NoError(t, env.Process()) })

	tied := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	// Inserted in reverse identity order on purpose.
	for _, name := range []string{"z", "m", "a"} {
		require.NoError(t, conn.Create(&models.DatasetState{ID: uuid.New(), Name: name, Status: models.DatasetStatusStale, Watermark: "1", CreatedAt: tied, UpdatedAt: tied}).Error)
		key := models.DatasetHoldKey("", name)
		require.NoError(t, conn.Create(&models.DatasetHold{ID: uuid.New(), Name: name, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
			Reason: "min", HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: tied, CreatedAt: tied, UpdatedAt: tied}).Error)
	}
	// Declaration-only datasets tied with the state-backed ones, with a NULL and
	// an empty namespace for the same dataset identity space.
	for _, name := range []string{"y", "b"} {
		require.NoError(t, conn.Create(&models.DatasetDeclaration{ID: uuid.New(), JobID: uuid.New(), JobAlias: "j-" + name, StepName: "load",
			Name: name, Direction: models.DatasetDirectionProduces, CreatedAt: tied, UpdatedAt: tied}).Error)
		key := models.DatasetHoldKey("", name)
		require.NoError(t, conn.Create(&models.DatasetHold{ID: uuid.New(), Name: name, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
			Reason: "min", HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: tied, CreatedAt: tied, UpdatedAt: tied}).Error)
	}

	page := func(status string, limit, offset int) []string {
		res, err := svc.List(ListParams{Status: status, Limit: limit, Offset: offset})
		require.NoError(t, err)
		out := make([]string, 0, len(res.Datasets))
		for _, state := range res.Datasets {
			out = append(out, state.Name)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		status string
		want   []string
	}{
		// Merged path (state-backed + declaration-only), held filter and unfiltered.
		{"held merged", models.DatasetStatusHeld, []string{"a", "b", "m", "y", "z"}},
		{"unfiltered merged", "", []string{"a", "b", "m", "y", "z"}},
		// Single-source path: persisted status only, no merge.
		{"observed only", models.DatasetStatusStale, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == nil {
				// Every stale row is held, so the effective status is held and
				// this filter is empty - proves the held rows do not leak here.
				require.Empty(t, page(tc.status, 50, 0))
				return
			}
			require.Equal(t, tc.want, page(tc.status, 50, 0))
			var paged []string
			for offset := 0; offset < len(tc.want); offset++ {
				got := page(tc.status, 1, offset)
				require.Len(t, got, 1, "offset %d", offset)
				paged = append(paged, got...)
			}
			require.Equal(t, tc.want, paged, "limit=1 paging must return every row exactly once, in order")
			require.Equal(t, tc.want[1:3], page(tc.status, 2, 1))
		})
	}
}

// The single-source path (no declaration merge) cuts its page in SQL too.
func TestObservedOnlyListPagesTiedTimestamps(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	tied := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, name := range []string{"z", "m", "a"} {
		require.NoError(t, conn.Create(&models.DatasetState{ID: uuid.New(), Name: name, Status: models.DatasetStatusStale, Watermark: "1", CreatedAt: tied, UpdatedAt: tied}).Error)
	}
	var paged []string
	for offset := 0; offset < 3; offset++ {
		res, err := svc.List(ListParams{Status: models.DatasetStatusStale, Limit: 1, Offset: offset})
		require.NoError(t, err)
		require.EqualValues(t, 3, res.Total)
		require.Len(t, res.Datasets, 1)
		paged = append(paged, res.Datasets[0].Name)
	}
	require.Equal(t, []string{"a", "m", "z"}, paged)
}
