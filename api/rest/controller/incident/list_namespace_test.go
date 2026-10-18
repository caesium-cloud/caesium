package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestIncidentListCollectionGuard serves the real GET /v1/incidents handler as
// a confined principal: only its namespace's incidents, a total counting only
// those, and never the frozen agent allowlist (which names downstream jobs in
// any namespace); an unrestricted principal is unchanged (B4).
func TestIncidentListCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := incidentService
	incidentService = func(ctx context.Context) *svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { incidentService = prev })

	now := time.Now().UTC()
	incident := func(namespace string, age time.Duration) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Incident{ID: id, Namespace: namespace, JobID: uuid.New(), Class: "image_pull", Status: models.IncidentStatus("open"),
			DedupeKey: id.String(), AllowedJobs: datatypes.JSON(`["finance-downstream-secret-alias"]`), OpenedAt: now.Add(-age), CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	mkt := incident("marketing", time.Minute)
	fin := incident("finance", 2*time.Minute)

	ctrl := New(nil)
	type listBody struct {
		Incidents []struct {
			ID          uuid.UUID       `json:"id"`
			AllowedJobs json.RawMessage `json:"allowed_jobs"`
		} `json:"incidents"`
		Total int64 `json:"total"`
	}
	list := func(key string) (listBody, string) {
		rec := serveCollectionGuarded(t, db, key, "/v1/incidents", "/v1/incidents", ctrl.List)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body listBody
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body, rec.Body.String()
	}

	confined, raw := list(guardViewerKey(t, db, `["marketing"]`))
	require.EqualValues(t, 1, confined.Total)
	require.Len(t, confined.Incidents, 1)
	require.Equal(t, mkt, confined.Incidents[0].ID)
	require.Empty(t, confined.Incidents[0].AllowedJobs)
	require.False(t, strings.Contains(raw, "finance-downstream-secret-alias"), "the allowlist never reaches a confined caller: %s", raw)
	require.False(t, strings.Contains(raw, fin.String()))

	cluster, raw := list(guardViewerKey(t, db, ""))
	require.EqualValues(t, 2, cluster.Total)
	require.Contains(t, raw, "finance-downstream-secret-alias", "an unrestricted principal still gets the allowlist")
}
