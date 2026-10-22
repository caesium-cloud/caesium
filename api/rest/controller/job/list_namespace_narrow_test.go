package job

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestJobListNamespaceNarrow: ?namespace= narrows GET /v1/jobs within the
// caller's allowed set. A namespace outside the grants is an empty 200 (no
// existence oracle), "*" and an omitted value do not narrow, and the legacy "" namespace answers to "default"
// (a malformed value is a 400, driven live by TestAuthListNamespaceNarrow).
func TestJobListNamespaceNarrow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := jobListService
	jobListService = func(ctx context.Context) jsvc.Job { return jsvc.ServiceWithDatabase(ctx, db) }
	t.Cleanup(func() { jobListService = prev })

	now := time.Now().UTC()
	for alias, namespace := range map[string]string{"nar-mkt": "marketing", "nar-fin": "finance", "nar-def": "default"} {
		require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
	}
	legacy := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: legacy, Alias: "nar-legacy", Namespace: "x", TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", legacy).Update("namespace", "").Error)

	list := func(key, query string) (int, []string) {
		rec := serveCollectionGuarded(t, db, key, "/v1/jobs", "/v1/jobs?order_by=alias"+query, List)
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var body []struct {
			Alias string `json:"alias"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		aliases := []string{}
		for _, entry := range body {
			aliases = append(aliases, entry.Alias)
		}
		return rec.Code, aliases
	}
	confined := guardViewerKey(t, db, `["marketing","default"]`)
	cluster := guardViewerKey(t, db, "")

	_, got := list(confined, "&namespace=marketing")
	require.Equal(t, []string{"nar-mkt"}, got)
	code, got := list(confined, "&namespace=finance")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{}, got, "a namespace outside the grants is an empty list, not a 403")
	_, got = list(confined, "&namespace=*")
	require.Equal(t, []string{"nar-def", "nar-legacy", "nar-mkt"}, got, `"*" does not widen`)
	_, got = list(confined, "&namespace=default")
	require.Equal(t, []string{"nar-def", "nar-legacy"}, got)
	_, got = list(cluster, "&namespace=finance")
	require.Equal(t, []string{"nar-fin"}, got)
	_, got = list(cluster, "")
	require.Len(t, got, 4, "absent param is unchanged")
}
