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

// TestTriggerListCollectionGuard serves the real GET /v1/triggers handler as
// confined principals: a trigger is listed only when it serves a live job and
// the caller may read every namespace it serves (the GET /v1/triggers/:id
// rule), so a trigger shared with another namespace's job, an idle trigger
// and one serving only a deleted job stay cluster-only (B4).
func TestTriggerListCollectionGuard(t *testing.T) {
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
	job := func(trigger uuid.UUID, namespace string) *models.Job {
		j := &models.Job{ID: uuid.New(), Alias: "job-" + uuid.NewString(), Namespace: namespace, TriggerID: trigger, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, db.Create(j).Error)
		return j
	}
	own, shared, fin, deleted := trigger("own"), trigger("shared"), trigger("fin"), trigger("deleted")
	trigger("idle")
	job(own, "marketing")
	job(shared, "marketing")
	job(shared, "finance")
	job(fin, "finance")
	require.NoError(t, db.Delete(job(deleted, "marketing")).Error)

	list := func(key string) []string {
		rec := serveCollectionGuarded(t, db, key, "/v1/triggers", "/v1/triggers?order_by=alias", List)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body []models.Trigger
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		aliases := make([]string, 0, len(body))
		for _, entry := range body {
			aliases = append(aliases, entry.Alias)
		}
		return aliases
	}
	require.Equal(t, []string{"own"}, list(guardViewerKey(t, db, `["marketing"]`)))
	require.Equal(t, []string{"fin", "own", "shared"}, list(guardViewerKey(t, db, `["marketing","finance"]`)))
	require.Empty(t, list(guardViewerKey(t, db, `["default"]`)))
	require.Equal(t, []string{"deleted", "fin", "idle", "own", "shared"}, list(guardViewerKey(t, db, "")), "an unrestricted principal lists every trigger")
}
