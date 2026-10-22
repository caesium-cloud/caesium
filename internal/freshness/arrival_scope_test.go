package freshness

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestArrivalScopeFollowsProducers (identity-and-access A6): a scoped arrival
// advances only datasets whose every producer the scope accepts; a
// producerless source is cluster-only and never advanced by a scoped arrival;
// an unscoped arrival advances every matching source, exactly as before.
func TestArrivalScopeFollowsProducers(t *testing.T) {
	ctx := context.Background()
	const eventType = "a6.arrival"
	setup := func(t *testing.T) *ArrivalObserver {
		t.Helper()
		db := openRegistryDB(t)
		for _, name := range []string{"mkt.data", "fin.data", "ext.data", "shared.data"} {
			seedArrivalSource(t, db, name, eventType, "")
		}
		producer := func(alias, namespace string, datasets ...string) {
			jobID := seedFreshnessJob(t, db, alias)
			require.NoError(t, db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", namespace).Error)
			for _, name := range datasets {
				decl := produceDecl(jobID, name, "1h", "")
				decl.JobAlias = alias
				seedDeclarations(t, db, decl)
			}
		}
		producer("mkt-producer", "marketing", "mkt.data", "shared.data")
		producer("fin-producer", "finance", "fin.data", "shared.data")
		return NewArrivalObserver(db)
	}
	advanced := func(res ArrivalResult) []string {
		var names []string
		for _, adv := range res.Advances {
			names = append(names, adv.Dataset)
		}
		slices.Sort(names)
		return names
	}
	evt := func() *models.IngestedEvent {
		return &models.IngestedEvent{ID: uuid.New(), Type: eventType, Data: datatypes.JSON(`{}`), CreatedAt: time.Now().UTC()}
	}
	marketing := func(_, namespace string) bool { return namespace == "marketing" }

	t.Run("scoped", func(t *testing.T) {
		res, err := setup(t).Observe(ctx, evt(), WithArrivalScope(marketing))
		require.NoError(t, err)
		require.Equal(t, []string{"mkt.data"}, advanced(res),
			"finance's dataset, a dataset with a finance co-producer and a producerless source stay put")
	})
	t.Run("alias scoped", func(t *testing.T) {
		res, err := setup(t).Observe(ctx, evt(), WithArrivalScope(func(alias, _ string) bool { return alias == "fin-producer" }))
		require.NoError(t, err)
		require.Equal(t, []string{"fin.data"}, advanced(res))
	})
	t.Run("unscoped", func(t *testing.T) {
		res, err := setup(t).Observe(ctx, evt())
		require.NoError(t, err)
		require.Equal(t, []string{"ext.data", "fin.data", "mkt.data", "shared.data"}, advanced(res))
	})
}
