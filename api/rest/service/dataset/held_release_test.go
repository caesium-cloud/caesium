package dataset

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedPersistedHeld(t *testing.T, conn *gorm.DB, name string) models.DatasetHold {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, conn.Create(&models.DatasetState{ID: uuid.New(), Name: name, Status: models.DatasetStatusHeld,
		Reason: "dataset_hold:/" + name + " hold=x", Watermark: "1", CreatedAt: now, UpdatedAt: now}).Error)
	key := models.DatasetHoldKey("", name)
	hold := models.DatasetHold{ID: uuid.New(), Name: name, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
		Reason: "min", HeldByJobID: uuid.New(), OccurrenceCount: 1, OpenedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, conn.Create(&hold).Error)
	return hold
}

func setAssertions(t *testing.T, on bool) {
	t.Helper()
	value := "false"
	if on {
		value = "true"
	}
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", value)
	require.NoError(t, env.Process())
	t.Cleanup(func() { t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false"); require.NoError(t, env.Process()) })
}

// The exact stranding sequence: `held` persisted, freshness scheduling off (so
// no evaluator ever runs again), the hold released manually. The status must not
// outlive the hold in the list, the detail or the ?status=held filter.
func TestHeldStatusDoesNotOutliveAManualReleaseWithoutFreshness(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	setAssertions(t, true)
	t.Setenv("CAESIUM_FRESHNESS_ENABLED", "false")
	require.NoError(t, env.Process())

	hold := seedPersistedHeld(t, conn, "orders")
	held, err := svc.List(ListParams{Status: models.DatasetStatusHeld})
	require.NoError(t, err)
	require.EqualValues(t, 1, held.Total)

	_, err = runstore.NewStore(conn).ReleaseHold(context.Background(), hold.ID, runstore.ReleaseHoldParams{ReleasedBy: "op", Note: "ack"})
	require.NoError(t, err)

	held, err = svc.List(ListParams{Status: models.DatasetStatusHeld})
	require.NoError(t, err)
	require.Empty(t, held.Datasets)
	require.EqualValues(t, 0, held.Total)
	all, err := svc.List(ListParams{})
	require.NoError(t, err)
	require.Len(t, all.Datasets, 1)
	require.Equal(t, models.DatasetStatusUnknown, all.Datasets[0].Status)
	require.Empty(t, all.Datasets[0].Reason)
	require.Nil(t, all.Datasets[0].Hold)
	unknown, err := svc.List(ListParams{Status: models.DatasetStatusUnknown})
	require.NoError(t, err)
	require.EqualValues(t, 1, unknown.Total)
	for _, include := range []bool{true, false} {
		detail, err := svc.GetWithOptions("", "orders", GetOptions{IncludeHold: include})
		require.NoError(t, err)
		require.Equal(t, models.DatasetStatusUnknown, detail.State.Status, "include_hold=%v", include)
		require.Empty(t, detail.State.Reason)
		require.Empty(t, detail.HoldStatus)
	}
}

// With the breaker switched off while a hold is still active, nothing evaluates
// or releases it; a persisted `held` must read as unknown, not held forever.
func TestStrandedHeldReadsAsUnknownWithTheBreakerOff(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	setAssertions(t, false)
	seedPersistedHeld(t, conn, "orders")

	held, err := svc.List(ListParams{Status: models.DatasetStatusHeld})
	require.NoError(t, err)
	require.Empty(t, held.Datasets)
	require.EqualValues(t, 0, held.Total)
	unknown, err := svc.List(ListParams{Status: models.DatasetStatusUnknown})
	require.NoError(t, err)
	require.EqualValues(t, 1, unknown.Total)
	require.Equal(t, models.DatasetStatusUnknown, unknown.Datasets[0].Status)
	require.Empty(t, unknown.Datasets[0].Reason)
	all, err := svc.List(ListParams{})
	require.NoError(t, err)
	require.Equal(t, models.DatasetStatusUnknown, all.Datasets[0].Status)
	detail, err := svc.Get("", "orders")
	require.NoError(t, err)
	require.Equal(t, models.DatasetStatusUnknown, detail.State.Status)
	require.Empty(t, detail.State.Reason)
}
