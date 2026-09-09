package dataset

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDatasetHoldFieldsAreFlagGatedAndPreserveFreshness(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	now := time.Now().UTC()
	state := models.DatasetState{Namespace: "", Name: "warehouse/orders", Status: models.DatasetStatusFresh, Watermark: "2026-09-08", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, conn.Create(&state).Error)
	hold := models.DatasetHold{ID: uuid.New(), Name: state.Name, Status: models.DatasetHoldStatusActive, HeldByJobID: uuid.New(), OpenedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, conn.Create(&hold).Error)

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false"); require.NoError(t, env.Process()) })
	list, err := svc.List(ListParams{Status: models.DatasetStatusFresh})
	require.NoError(t, err)
	require.Len(t, list.Datasets, 1)
	require.Equal(t, models.DatasetStatusFresh, list.Datasets[0].Status)
	require.Equal(t, hold.ID, list.Datasets[0].Hold.ID)
	detail, err := svc.Get("", state.Name)
	require.NoError(t, err)
	require.Equal(t, "active", detail.HoldStatus)

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
	require.NoError(t, env.Process())
	// An off deployment must never query holds, including one whose catalog
	// predates this feature. Dropping the table makes a stray query fail.
	require.NoError(t, conn.Migrator().DropTable(&models.DatasetHold{}))
	list, err = svc.List(ListParams{})
	require.NoError(t, err)
	require.Len(t, list.Datasets, 1)
	require.Nil(t, list.Datasets[0].Hold)
	detail, err = svc.Get("", state.Name)
	require.NoError(t, err)
	for _, result := range []any{list, detail} {
		body, err := json.Marshal(result)
		require.NoError(t, err)
		require.NotContains(t, string(body), `"hold"`)
		require.NotContains(t, string(body), `"hold_status"`)
	}
}

func TestHoldsFiltersExactIdentityAndPaginate(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	now := time.Now().UTC()
	for i, row := range []models.DatasetHold{
		{Namespace: "", Name: "warehouse/orders", Status: "active"},
		{Namespace: "warehouse", Name: "orders", Status: "active"},
		{Namespace: "other", Name: "warehouse/orders", Status: "active"},
		{Namespace: "", Name: "warehouse/orders", Status: "released"},
	} {
		row.ID, row.HeldByJobID = uuid.New(), uuid.New()
		row.OpenedAt = now.Add(time.Duration(i) * time.Second)
		row.CreatedAt, row.UpdatedAt = row.OpenedAt, row.OpenedAt
		require.NoError(t, conn.Create(&row).Error)
	}
	namespace := ""
	result, err := svc.Holds(HoldsParams{Namespace: &namespace, Name: "warehouse/orders"})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Total)
	require.Len(t, result.Holds, 1)
	require.Empty(t, result.Holds[0].Namespace)
	require.Equal(t, "warehouse/orders", result.Holds[0].Name)
	result, err = svc.Holds(HoldsParams{Status: "all", Limit: 1, Offset: 1})
	require.NoError(t, err)
	require.EqualValues(t, 4, result.Total)
	require.Len(t, result.Holds, 1)
	require.Equal(t, "other", result.Holds[0].Namespace)
	_, err = svc.Holds(HoldsParams{Status: "bogus"})
	require.ErrorIs(t, err, ErrHoldStatus)
}
