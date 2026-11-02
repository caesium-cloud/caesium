package dataset

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A persisted `held` is only believed while an active hold exists. With the
// breaker ON but no active hold (a write that raced the release), list, detail
// and both filters report unknown, with consistent totals and pagination.
func TestPersistedHeldWithoutAnActiveHoldReadsAsUnknown(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	setAssertions(t, true)

	at := time.Now().UTC().Add(-time.Minute)
	persist := func(name, status string) {
		require.NoError(t, conn.Create(&models.DatasetState{ID: uuid.New(), Name: name, Status: status,
			Reason: "dataset_hold:/" + name + " hold=gone", Watermark: "1", CreatedAt: at, UpdatedAt: at}).Error)
	}
	persist("stale-a", models.DatasetStatusHeld)
	persist("stale-b", models.DatasetStatusHeld)
	persist("plain-unknown", models.DatasetStatusUnknown)
	persist("fresh-one", models.DatasetStatusFresh)
	// One genuinely held dataset.
	persist("live", models.DatasetStatusHeld)
	key := models.DatasetHoldKey("", "live")
	require.NoError(t, conn.Create(&models.DatasetHold{ID: uuid.New(), Name: "live", Status: models.DatasetHoldStatusActive, ActiveKey: &key,
		Reason: "min", HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: at, CreatedAt: at, UpdatedAt: at}).Error)

	names := func(res *ListResult) []string {
		out := make([]string, 0, len(res.Datasets))
		for _, state := range res.Datasets {
			out = append(out, state.Name)
		}
		return out
	}

	held, err := svc.List(ListParams{Status: models.DatasetStatusHeld})
	require.NoError(t, err)
	require.EqualValues(t, 1, held.Total)
	require.Equal(t, []string{"live"}, names(held))
	require.Equal(t, models.DatasetStatusHeld, held.Datasets[0].Status)

	unknown, err := svc.List(ListParams{Status: models.DatasetStatusUnknown})
	require.NoError(t, err)
	require.EqualValues(t, 3, unknown.Total)
	require.Equal(t, []string{"plain-unknown", "stale-a", "stale-b"}, names(unknown))
	for _, state := range unknown.Datasets {
		require.Equal(t, models.DatasetStatusUnknown, state.Status, state.Name)
		if state.Name != "plain-unknown" { // its persisted reason is untouched
			require.Empty(t, state.Reason, state.Name)
		}
	}
	var paged []string
	for offset := 0; offset < 3; offset++ {
		page, err := svc.List(ListParams{Status: models.DatasetStatusUnknown, Limit: 1, Offset: offset})
		require.NoError(t, err)
		require.EqualValues(t, 3, page.Total)
		paged = append(paged, names(page)...)
	}
	require.Equal(t, names(unknown), paged)

	all, err := svc.List(ListParams{})
	require.NoError(t, err)
	byName := map[string]string{}
	for _, state := range all.Datasets {
		byName[state.Name] = state.Status
	}
	require.Equal(t, map[string]string{
		"stale-a": models.DatasetStatusUnknown, "stale-b": models.DatasetStatusUnknown, "plain-unknown": models.DatasetStatusUnknown,
		"fresh-one": models.DatasetStatusFresh, "live": models.DatasetStatusHeld,
	}, byName)

	detail, err := svc.Get("", "stale-a")
	require.NoError(t, err)
	require.Equal(t, models.DatasetStatusUnknown, detail.State.Status)
	require.Empty(t, detail.State.Reason)
	live, err := svc.Get("", "live")
	require.NoError(t, err)
	require.Equal(t, models.DatasetStatusHeld, live.State.Status)
}
