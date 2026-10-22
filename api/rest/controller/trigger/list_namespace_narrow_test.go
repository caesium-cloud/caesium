package trigger

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestTriggerListNamespaceNarrow: ?namespace= narrows within the caller's
// allowed set. A namespace the caller cannot read is an empty 200 (no
// existence oracle), "*" and an omitted value do not narrow (a malformed value is a 400, driven live by
// TestAuthListNamespaceNarrow), and an unrestricted caller gets the triggers serving a live job in
// that namespace.
func TestTriggerListNamespaceNarrow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := triggerListService
	triggerListService = func(ctx context.Context) tsvc.Trigger { return tsvc.ServiceWithDatabase(ctx, db) }
	t.Cleanup(func() { triggerListService = prev })

	now := time.Now().UTC()
	trigger := func(alias string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Trigger{ID: id, Alias: alias, Type: models.TriggerTypeHTTP, Configuration: `{"path":"/hooks/` + alias + `"}`, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	job := func(trigger uuid.UUID, namespace string) {
		require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: "job-" + uuid.NewString(), Namespace: namespace, TriggerID: trigger, CreatedAt: now, UpdatedAt: now}).Error)
	}
	mkt, fin, shared := trigger("mkt"), trigger("fin"), trigger("shared")
	job(mkt, "marketing")
	job(fin, "finance")
	job(shared, "marketing")
	job(shared, "finance")

	list := func(key, query string) (int, []string) {
		rec := serveCollectionGuarded(t, db, key, "/v1/triggers", "/v1/triggers?order_by=alias"+query, List)
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var body []models.Trigger
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		aliases := []string{}
		for _, entry := range body {
			aliases = append(aliases, entry.Alias)
		}
		return rec.Code, aliases
	}
	confined := guardViewerKey(t, db, `["marketing"]`)
	both := guardViewerKey(t, db, `["marketing","finance"]`)
	cluster := guardViewerKey(t, db, "")

	_, got := list(confined, "&namespace=marketing")
	require.Equal(t, []string{"mkt"}, got)
	code, got := list(confined, "&namespace=finance")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{}, got, "a namespace outside the grants is an empty list, not a 403")
	_, got = list(confined, "&namespace=*")
	require.Equal(t, []string{"mkt"}, got, `"*" does not widen`)
	_, got = list(both, "&namespace=finance")
	require.Equal(t, []string{"fin", "shared"}, got)
	_, got = list(cluster, "&namespace=finance")
	require.Equal(t, []string{"fin", "shared"}, got)
	_, got = list(cluster, "")
	require.Equal(t, []string{"fin", "mkt", "shared"}, got, "absent param is unchanged")
}
