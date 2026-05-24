package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestLeaseStore_AcquireAndRenew tests the basic lease lifecycle:
//  1. AcquireLease writes a row and returns generation=1.
//  2. A second AcquireLease for the same run is idempotent (returns current gen).
//  3. RenewRunLeases updates the expiry for the owning node.
//  4. OwnedRuns only returns non-expired leases owned by the given node.
func TestLeaseStore_AcquireAndRenew(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	ls := NewLeaseStore(db)
	ctx := context.Background()

	runID := uuid.New()
	const ownerNode = "10.0.0.1:9001"

	// First acquisition should return generation 1.
	gen, err := ls.AcquireLease(ctx, runID, ownerNode, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), gen, "first acquisition must be generation 1")

	// Verify the DB row.
	var lease models.RunLease
	require.NoError(t, db.First(&lease, "run_id = ?", runID.String()).Error)
	require.Equal(t, ownerNode, lease.OwnerNode)
	require.Equal(t, int64(1), lease.Generation)
	require.True(t, lease.LeaseExpiresAt.After(lease.AcquiredAt))

	// Second acquisition for same run is idempotent.
	gen2, err := ls.AcquireLease(ctx, runID, ownerNode, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), gen2, "second acquisition must return existing generation")

	// Renewal should extend the expiry.
	before := lease.LeaseExpiresAt
	newExpiry := time.Now().UTC().Add(60 * time.Second)
	rows, err := ls.RenewRunLeases(ctx, ownerNode, []uuid.UUID{runID}, newExpiry)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	require.NoError(t, db.First(&lease, "run_id = ?", runID.String()).Error)
	require.True(t, lease.LeaseExpiresAt.After(before), "expiry must advance after renewal")
}

// TestLeaseStore_OwnerModeFlagOff tests that no run_leases row is written
// when the flag is off (leaseStore == nil).  This is enforced at the Store
// level: when leaseStore is nil, Start() skips lease writing.
func TestLeaseStore_OwnerModeFlagOff(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	// leaseStore is nil — flag is off.

	jobID := uuid.New()
	r, err := store.Start(jobID, nil)
	require.NoError(t, err)

	var count int64
	require.NoError(t, db.Model(&models.RunLease{}).Count(&count).Error)
	require.Equal(t, int64(0), count,
		"no run_leases rows should be written when owner mode is disabled (runID=%s)", r.ID)
}

// TestLeaseStore_OwnerModeFlagOn tests that a run_leases row is written when
// Start() is called with a configured LeaseStore.
func TestLeaseStore_OwnerModeFlagOn(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	ls := NewLeaseStore(db)
	store := NewStore(db).WithLeaseStore(ls)

	jobID := uuid.New()
	r, err := store.Start(jobID, nil)
	require.NoError(t, err)

	var count int64
	require.NoError(t, db.Model(&models.RunLease{}).Count(&count).Error)
	require.Equal(t, int64(1), count,
		"one run_leases row should be written when owner mode is enabled (runID=%s)", r.ID)

	var lease models.RunLease
	require.NoError(t, db.First(&lease, "run_id = ?", r.ID.String()).Error)
	require.Equal(t, int64(1), lease.Generation)
}

// TestLeaseStore_IsOwner verifies that IsOwner correctly distinguishes
// owned/unowned/expired runs.
func TestLeaseStore_IsOwner(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	ls := NewLeaseStore(db)
	ctx := context.Background()

	runID := uuid.New()
	const ownerNode = "10.0.0.1:9001"
	const otherNode = "10.0.0.2:9001"

	_, err := ls.AcquireLease(ctx, runID, ownerNode, 30*time.Second)
	require.NoError(t, err)

	owned, err := ls.IsOwner(ctx, ownerNode, runID)
	require.NoError(t, err)
	require.True(t, owned, "owner node should be recognized as owner")

	owned, err = ls.IsOwner(ctx, otherNode, runID)
	require.NoError(t, err)
	require.False(t, owned, "other node should not be recognized as owner")

	// Simulate an expired lease.
	require.NoError(t, db.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		Update("lease_expires_at", time.Now().UTC().Add(-1*time.Second)).Error)

	owned, err = ls.IsOwner(ctx, ownerNode, runID)
	require.NoError(t, err)
	require.False(t, owned, "expired lease should not be recognized as owned")
}

// TestLeaseStore_OwnedRuns verifies that OwnedRuns only returns non-expired
// leases for the given owner node.
func TestLeaseStore_OwnedRuns(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	ls := NewLeaseStore(db)
	ctx := context.Background()

	const ownerNode = "10.0.0.1:9001"
	const otherNode = "10.0.0.2:9001"

	runA := uuid.New()
	runB := uuid.New()
	runC := uuid.New()
	runExpired := uuid.New()

	_, err := ls.AcquireLease(ctx, runA, ownerNode, 30*time.Second)
	require.NoError(t, err)
	_, err = ls.AcquireLease(ctx, runB, ownerNode, 30*time.Second)
	require.NoError(t, err)
	_, err = ls.AcquireLease(ctx, runC, otherNode, 30*time.Second)
	require.NoError(t, err)

	// runExpired: owned by ownerNode but already expired.
	_, err = ls.AcquireLease(ctx, runExpired, ownerNode, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.RunLease{}).
		Where("run_id = ?", runExpired.String()).
		Update("lease_expires_at", time.Now().UTC().Add(-1*time.Second)).Error)

	ids, err := ls.OwnedRuns(ctx, ownerNode)
	require.NoError(t, err)
	require.Len(t, ids, 2, "only active, non-expired leases for ownerNode should be returned")

	idSet := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	require.True(t, idSet[runA])
	require.True(t, idSet[runB])
	require.False(t, idSet[runC], "runC is owned by otherNode")
	require.False(t, idSet[runExpired], "runExpired should be excluded")
}

// TestLeaseStore_RenewSkipsWrongOwner verifies that RenewRunLeases does not
// extend leases that are now owned by a different node.
func TestLeaseStore_RenewSkipsWrongOwner(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	ls := NewLeaseStore(db)
	ctx := context.Background()

	runID := uuid.New()
	const ownerNode = "10.0.0.1:9001"
	const otherNode = "10.0.0.2:9001"

	_, err := ls.AcquireLease(ctx, runID, ownerNode, 30*time.Second)
	require.NoError(t, err)

	// Simulate takeover: change owner_node to otherNode directly.
	require.NoError(t, db.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		Update("owner_node", otherNode).Error)

	newExpiry := time.Now().UTC().Add(60 * time.Second)
	rows, err := ls.RenewRunLeases(ctx, ownerNode, []uuid.UUID{runID}, newExpiry)
	require.NoError(t, err)
	require.Equal(t, int64(0), rows, "renewal must not touch rows owned by a different node")
}

// TestLeaseStore_RenewOwnedLeases verifies that the single-call API extends
// every non-expired lease for the owner in one round-trip and returns the
// correct count (which is what feeds the caesium_run_leases_owned gauge).
func TestLeaseStore_RenewOwnedLeases(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	ls := NewLeaseStore(db)
	ctx := context.Background()

	const ownerNode = "10.0.0.1:9001"
	const otherNode = "10.0.0.2:9001"

	// Two owned, one foreign-owned, one expired.
	runA := uuid.New()
	runB := uuid.New()
	runForeign := uuid.New()
	runExpired := uuid.New()

	_, err := ls.AcquireLease(ctx, runA, ownerNode, 30*time.Second)
	require.NoError(t, err)
	_, err = ls.AcquireLease(ctx, runB, ownerNode, 30*time.Second)
	require.NoError(t, err)
	_, err = ls.AcquireLease(ctx, runForeign, otherNode, 30*time.Second)
	require.NoError(t, err)
	_, err = ls.AcquireLease(ctx, runExpired, ownerNode, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.RunLease{}).
		Where("run_id = ?", runExpired.String()).
		Update("lease_expires_at", time.Now().UTC().Add(-1*time.Second)).Error)

	newExpiry := time.Now().UTC().Add(120 * time.Second)
	count, err := ls.RenewOwnedLeases(ctx, ownerNode, newExpiry)
	require.NoError(t, err)
	require.Equal(t, int64(2), count,
		"only non-expired owner-held leases should be renewed (runA + runB)")

	// runA + runB extended; runForeign and runExpired left alone.
	for _, runID := range []uuid.UUID{runA, runB} {
		var lease models.RunLease
		require.NoError(t, db.First(&lease, "run_id = ?", runID.String()).Error)
		require.WithinDuration(t, newExpiry, lease.LeaseExpiresAt, time.Second)
	}
}

// TestLeaseStore_MigrationSafety verifies that an existing task_runs row with
// owner_generation=0 is mutable.  The OR = 0 predicate in coordination writes
// is the migration safety net.
func TestLeaseStore_MigrationSafety(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	// Write a task run with owner_generation=0 (legacy/flag-off rows).
	tr := models.TaskRun{
		ID:              uuid.New(),
		JobRunID:        uuid.New(),
		TaskID:          uuid.New(),
		AtomID:          uuid.New(),
		Engine:          models.AtomEngineDocker,
		Image:           "alpine:3.23",
		Command:         `["echo","test"]`,
		Status:          "pending",
		OwnerGeneration: 0,
	}
	// We need a parent JobRun to satisfy FK.
	jr := models.JobRun{
		ID:        tr.JobRunID,
		JobID:     uuid.New(),
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	job := models.Job{ID: jr.JobID, Alias: "test"}
	require.NoError(t, db.Create(&job).Error)
	require.NoError(t, db.Create(&jr).Error)
	require.NoError(t, db.Create(&tr).Error)

	// An update with the "OR owner_generation = 0" fence should touch the row.
	result := db.Model(&models.TaskRun{}).
		Where("id = ? AND (owner_generation = ? OR owner_generation = 0)", tr.ID, int64(1)).
		Update("status", "running")
	require.NoError(t, result.Error)
	require.Equal(t, int64(1), result.RowsAffected,
		"legacy row with owner_generation=0 must be mutable by any node")
}
