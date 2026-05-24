package run

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LeaseStore manages run_leases rows for Phase 2 run-owner coordination.
// All operations are safe to call when owner mode is disabled — they
// simply become no-ops.
type LeaseStore struct {
	db *gorm.DB
}

// NewLeaseStore constructs a LeaseStore backed by the given connection.
func NewLeaseStore(db *gorm.DB) *LeaseStore {
	return &LeaseStore{db: db}
}

// AcquireLease writes a run_leases row for the given run, recording the
// owning node and expiry.  If a row already exists (e.g., from a previous
// attempt), it is left unchanged — the initial write is treated as
// idempotent: whoever wrote it first is the owner.
//
// Returns the generation written on success (always 1 for a fresh lease).
func (ls *LeaseStore) AcquireLease(ctx context.Context, runID uuid.UUID, ownerNode string, ttl time.Duration) (int64, error) {
	if ls == nil || ls.db == nil {
		return 0, nil
	}

	now := time.Now().UTC()
	lease := &models.RunLease{
		RunID:          runID.String(),
		OwnerNode:      ownerNode,
		AcquiredAt:     now,
		LeaseExpiresAt: now.Add(ttl),
		Generation:     1,
	}

	// INSERT OR IGNORE — if the row already exists, leave it alone.
	result := ls.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(lease)
	if result.Error != nil {
		return 0, result.Error
	}

	if result.RowsAffected == 0 {
		// Row already existed; read back the current generation so callers know.
		var existing models.RunLease
		if err := ls.db.WithContext(ctx).First(&existing, "run_id = ?", runID.String()).Error; err != nil {
			return 0, err
		}
		return existing.Generation, nil
	}

	return lease.Generation, nil
}

// RenewRunLeases performs a single batched UPDATE extending lease_expires_at
// for every run in runIDs that is still owned by ownerNode.  The WHERE
// clause on owner_node is the safety net that prevents renewing a lease that
// was taken over by another node between the decision and the write.
//
// Returns the number of rows actually updated.
func (ls *LeaseStore) RenewRunLeases(ctx context.Context, ownerNode string, runIDs []uuid.UUID, newExpiresAt time.Time) (int64, error) {
	if ls == nil || ls.db == nil || len(runIDs) == 0 {
		return 0, nil
	}

	ids := make([]string, len(runIDs))
	for i, id := range runIDs {
		ids[i] = id.String()
	}

	result := ls.db.WithContext(ctx).
		Model(&models.RunLease{}).
		Where("owner_node = ? AND run_id IN ?", ownerNode, ids).
		Update("lease_expires_at", newExpiresAt)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// RenewOwnedLeases extends lease_expires_at in a single UPDATE for every
// non-expired lease owned by ownerNode — no upstream SELECT required. Returns
// the number of rows actually renewed (which is also the count of currently
// owned, non-expired leases).
//
// Use this on the per-node renewal ticker; it replaces an OwnedRuns +
// RenewRunLeases pair with one round-trip.
func (ls *LeaseStore) RenewOwnedLeases(ctx context.Context, ownerNode string, newExpiresAt time.Time) (int64, error) {
	if ls == nil || ls.db == nil {
		return 0, nil
	}
	result := ls.db.WithContext(ctx).
		Model(&models.RunLease{}).
		Where("owner_node = ? AND lease_expires_at > ?", ownerNode, time.Now().UTC()).
		Update("lease_expires_at", newExpiresAt)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// OwnedRuns returns the IDs of all runs currently owned by ownerNode whose
// leases have not yet expired.  Used by the renewal ticker to build its batch.
func (ls *LeaseStore) OwnedRuns(ctx context.Context, ownerNode string) ([]uuid.UUID, error) {
	if ls == nil || ls.db == nil {
		return nil, nil
	}

	var leases []models.RunLease
	if err := ls.db.WithContext(ctx).
		Select("run_id").
		Where("owner_node = ? AND lease_expires_at > ?", ownerNode, time.Now().UTC()).
		Find(&leases).Error; err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, 0, len(leases))
	for _, l := range leases {
		id, err := uuid.Parse(l.RunID)
		if err != nil {
			log.Warn("run_leases: unparseable run_id", "run_id", l.RunID, "error", err)
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// OwnedRunsWithGenerations returns a map of run IDs to lease generations for
// every non-expired lease owned by ownerNode, in a single query. Used by the
// dispatch loop to avoid an N+1 GetLease pattern on every tick.
func (ls *LeaseStore) OwnedRunsWithGenerations(ctx context.Context, ownerNode string) (map[uuid.UUID]int64, error) {
	if ls == nil || ls.db == nil {
		return nil, nil
	}

	var leases []models.RunLease
	if err := ls.db.WithContext(ctx).
		Select("run_id", "generation").
		Where("owner_node = ? AND lease_expires_at > ?", ownerNode, time.Now().UTC()).
		Find(&leases).Error; err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]int64, len(leases))
	for _, l := range leases {
		id, err := uuid.Parse(l.RunID)
		if err != nil {
			log.Warn("run_leases: unparseable run_id", "run_id", l.RunID, "error", err)
			continue
		}
		out[id] = l.Generation
	}
	return out, nil
}

// IsOwner returns true if ownerNode currently holds a valid (non-expired) lease
// on runID.  Used to validate requests before acting as owner.
func (ls *LeaseStore) IsOwner(ctx context.Context, ownerNode string, runID uuid.UUID) (bool, error) {
	if ls == nil || ls.db == nil {
		return false, nil
	}

	var count int64
	err := ls.db.WithContext(ctx).
		Model(&models.RunLease{}).
		Where("run_id = ? AND owner_node = ? AND lease_expires_at > ?",
			runID.String(), ownerNode, time.Now().UTC()).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetLease returns the current lease record for runID, if any.
func (ls *LeaseStore) GetLease(ctx context.Context, runID uuid.UUID) (*models.RunLease, error) {
	if ls == nil || ls.db == nil {
		return nil, nil
	}

	var lease models.RunLease
	if err := ls.db.WithContext(ctx).First(&lease, "run_id = ?", runID.String()).Error; err != nil {
		return nil, err
	}
	return &lease, nil
}
