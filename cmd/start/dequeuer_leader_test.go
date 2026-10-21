package start

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDequeuerLeaderCheckIsBackendAware: on the internal dqlite database the
// dequeuer drains only on the leader; on PostgreSQL, which has no dqlite app,
// it carries no leadership gate (as the cron scheduler) instead of failing
// every tick and stranding quota-held runs.
func TestDequeuerLeaderCheckIsBackendAware(t *testing.T) {
	for _, databaseType := range []string{"", "internal", "dqlite", " DQLITE "} {
		require.NotNil(t, dequeuerLeaderCheck(databaseType), "%q", databaseType)
	}
	require.Nil(t, dequeuerLeaderCheck("postgres"))
	require.Nil(t, dequeuerLeaderCheck("Postgres"))
}
