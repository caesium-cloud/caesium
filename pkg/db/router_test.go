package db

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRouterSingleShardAliasesCatalog(t *testing.T) {
	catalog := openTestDB(t, "catalog")
	router, err := NewRouter(catalog, nil, nil)
	require.NoError(t, err)

	require.Equal(t, 1, router.ShardCount())
	require.Same(t, catalog, router.Catalog())
	require.Same(t, catalog, router.Cold())

	runID := uuid.New()
	hot, shard, err := router.HotShardForRun(runID)
	require.NoError(t, err)
	require.Equal(t, 0, shard)
	require.Same(t, catalog, hot)

	routed, role, routedShard, err := router.RouteTable("task_runs", runID)
	require.NoError(t, err)
	require.Equal(t, DatabaseRoleHot, role)
	require.Equal(t, 0, routedShard)
	require.Same(t, catalog, routed)

	routed, role, routedShard, err = router.RouteTable("jobs", uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, DatabaseRoleCatalog, role)
	require.Equal(t, -1, routedShard)
	require.Same(t, catalog, routed)
}

func TestRouterRoutesCatalogHotAndCold(t *testing.T) {
	catalog := openTestDB(t, "catalog")
	cold := openTestDB(t, "cold")
	hot := make([]*gorm.DB, 4)
	for idx := range hot {
		hot[idx] = openTestDB(t, fmt.Sprintf("hot-%d", idx))
	}
	router, err := NewRouter(catalog, hot, cold)
	require.NoError(t, err)

	require.Same(t, catalog, router.Catalog())
	require.Same(t, cold, router.Cold())

	runID := uuid.MustParse("2dc7d34e-3b32-4877-9578-c34b33e8f499")
	expectedShard := router.ShardForRunID(runID)
	routed, role, shard, err := router.RouteTable("execution_events", runID)
	require.NoError(t, err)
	require.Equal(t, DatabaseRoleHot, role)
	require.Equal(t, expectedShard, shard)
	require.Same(t, hot[expectedShard], routed)

	routed, role, shard, err = router.RouteTable("atoms", uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, DatabaseRoleCatalog, role)
	require.Equal(t, -1, shard)
	require.Same(t, catalog, routed)

	routed, shard, err = router.Database(DatabaseRoleCold, uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, -1, shard)
	require.Same(t, cold, routed)
}

func TestRouterHotRouteRequiresRunID(t *testing.T) {
	catalog := openTestDB(t, "catalog")
	router, err := NewRouter(catalog, []*gorm.DB{catalog, openTestDB(t, "hot")}, nil)
	require.NoError(t, err)

	_, _, _, err = router.RouteTable("task_runs", uuid.Nil)
	require.Error(t, err)
}

func TestRouterDistributesRunIDsAcrossShards(t *testing.T) {
	catalog := openTestDB(t, "catalog")
	hot := make([]*gorm.DB, 8)
	for idx := range hot {
		hot[idx] = openTestDB(t, fmt.Sprintf("hot-%d", idx))
	}
	router, err := NewRouter(catalog, hot, nil)
	require.NoError(t, err)

	counts := make([]int, router.ShardCount())
	for idx := 0; idx < 50; idx++ {
		runID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("run-%02d", idx)))
		counts[router.ShardForRunID(runID)]++
	}

	used := 0
	maxCount := 0
	for _, count := range counts {
		if count > 0 {
			used++
		}
		if count > maxCount {
			maxCount = count
		}
	}
	require.GreaterOrEqual(t, used, 6)
	require.LessOrEqual(t, maxCount, 14)
}

func openTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()

	conn, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	return conn
}
