package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestDispatchLoop_ReclaimsOwnExpiredLeaseOnlyWhenUnheld wires #712 into the
// sweep the in-memory owner actually runs. Peer takeover still moves another
// node's expired lease. This node's expired lease moves only when the
// OwnerManager is not tracking the run; a run it still holds keeps its
// generation, which is what a stalled renewal must not disturb.
func TestDispatchLoop_ReclaimsOwnExpiredLeaseOnlyWhenUnheld(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	const nodeID = "10.0.0.1:9001"
	const peerID = "10.0.0.2:9001"

	heldRun, _ := seedPendingTaskRun(t, store)
	unheld := uuid.New()
	foreign := uuid.New()
	fresh := uuid.New()

	mgr := run.NewOwnerManager(store, run.CheckpointConfig{})
	require.NoError(t, mgr.Adopt(heldRun, 1))
	require.True(t, mgr.Owns(heldRun))

	for _, runID := range []uuid.UUID{heldRun, unheld, fresh} {
		_, err := ls.AcquireLease(ctx, runID, nodeID, time.Minute)
		require.NoError(t, err)
	}
	_, err := ls.AcquireLease(ctx, foreign, peerID, time.Minute)
	require.NoError(t, err)
	for _, runID := range []uuid.UUID{heldRun, unheld, foreign} {
		require.NoError(t, db.Model(&models.RunLease{}).
			Where("run_id = ?", runID.String()).
			Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error)
	}

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:       nodeID,
		LeaseTTL:     30 * time.Second,
		LeaseStore:   &testOwnerReader{ls},
		Store:        store,
		OwnerManager: mgr,
		Peers: PeerListerFunc(func(context.Context) ([]string, error) {
			return nil, errors.New("stop after the lease sweep")
		}),
	})
	loop.tick(ctx)

	assertLease := func(runID uuid.UUID, owner string, generation int64, live bool) {
		t.Helper()
		lease, getErr := ls.GetLease(ctx, runID)
		require.NoError(t, getErr)
		require.Equal(t, owner, lease.OwnerNode)
		require.Equal(t, generation, lease.Generation)
		if live {
			require.True(t, lease.LeaseExpiresAt.After(time.Now().UTC()))
			return
		}
		require.True(t, lease.LeaseExpiresAt.Before(time.Now().UTC()))
	}
	assertLease(heldRun, nodeID, 1, false)
	assertLease(unheld, nodeID, 2, true)
	assertLease(foreign, nodeID, 2, true)
	assertLease(fresh, nodeID, 1, true)
	require.False(t, mgr.Owns(unheld), "the sweep stops before recovery when peers cannot be listed")
}
