package incident

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestIncidentListNamespaceNarrow: ?namespace= narrows the feed (and its
// total) within the caller's allowed set; a namespace the caller cannot read
// is an empty 200, "*" and an omitted value do not narrow (a malformed value is a 400, driven live by
// TestAuthListNamespaceNarrow).
func TestIncidentListNamespaceNarrow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := incidentService
	incidentService = func(ctx context.Context) *svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { incidentService = prev })

	now := time.Now().UTC()
	incident := func(namespace string, age time.Duration) {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Incident{ID: id, Namespace: namespace, JobID: uuid.New(), Class: "image_pull", Status: models.IncidentStatus("open"),
			DedupeKey: id.String(), OpenedAt: now.Add(-age), CreatedAt: now, UpdatedAt: now}).Error)
	}
	incident("marketing", time.Minute)
	incident("finance", 2*time.Minute)
	incident("finance", 3*time.Minute)

	ctrl := New(nil)
	total := func(key, query string) (int, int64) {
		rec := serveCollectionGuarded(t, db, key, "/v1/incidents", "/v1/incidents"+query, ctrl.List)
		if rec.Code != http.StatusOK {
			return rec.Code, 0
		}
		var body struct {
			Incidents []json.RawMessage `json:"incidents"`
			Total     int64             `json:"total"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.EqualValues(t, len(body.Incidents), body.Total)
		return rec.Code, body.Total
	}
	confined := guardViewerKey(t, db, `["marketing"]`)
	cluster := guardViewerKey(t, db, "")

	_, n := total(confined, "?namespace=marketing")
	require.EqualValues(t, 1, n)
	code, n := total(confined, "?namespace=finance")
	require.Equal(t, http.StatusOK, code)
	require.EqualValues(t, 0, n, "a namespace outside the grants is an empty list, not a 403")
	_, n = total(confined, "?namespace=*")
	require.EqualValues(t, 1, n, `"*" does not widen`)
	_, n = total(cluster, "?namespace=finance")
	require.EqualValues(t, 2, n)
	_, n = total(cluster, "")
	require.EqualValues(t, 3, n, "absent param is unchanged")
}
