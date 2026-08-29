package run

import (
	"context"
	"errors"
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

// retiredRunLeaseOwner is a reserved, non-routable owner identity. A terminal
// cancellation keeps its lease row at this owner so StateRevision remains a
// durable callback fence, while renewal and takeover queries ignore it.
const retiredRunLeaseOwner = "__caesium_terminal__"

// ErrOwnerStateChanged fences an owner operation whose cached state revision no
// longer matches the durable run lease. It is retryable: the owner must rebuild
// at the current revision before applying the operation again.
var ErrOwnerStateChanged = errors.New("run owner state changed")

// LeaseVersion is the complete identity of an owner's mutable run state.
// Generation changes on ownership transfer; StateRevision changes when a retry
// rewrites task rows without transferring ownership.
type LeaseVersion struct {
	Generation    int64
	StateRevision int64
}

// NewLeaseStore constructs a LeaseStore backed by the given connection.
func NewLeaseStore(db *gorm.DB) *LeaseStore {
	return &LeaseStore{db: db}
}

// retireRunLeaseTx preserves the terminal execution's StateRevision while
// making the lease impossible to renew or take over. The retained revision is
// the durable callback fence for waiters that were already running when a
// cancellation committed: only the waiter for the cancelled execution may
// publish its callback; older retry epochs remain superseded.
func retireRunLeaseTx(tx *gorm.DB, runID uuid.UUID, retiredAt time.Time) error {
	if tx == nil || runID == uuid.Nil {
		return nil
	}
	return tx.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		Updates(map[string]interface{}{
			"owner_node":       retiredRunLeaseOwner,
			"lease_expires_at": retiredAt,
		}).Error
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
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	now := time.Now().UTC()
	lease := &models.RunLease{
		RunID:          runID.String(),
		OwnerNode:      ownerNode,
		AcquiredAt:     now,
		LeaseExpiresAt: now.Add(ttl),
		Generation:     1,
		StateRevision:  1,
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

// OwnedRunsWithVersions returns the full owner-state version for every live
// lease held by ownerNode. The dispatch loop uses both fields so a retry served
// by any replica invalidates the actual owner's process-local cache.
func (ls *LeaseStore) OwnedRunsWithVersions(ctx context.Context, ownerNode string) (map[uuid.UUID]LeaseVersion, error) {
	if ls == nil || ls.db == nil {
		return nil, nil
	}

	var leases []models.RunLease
	if err := ls.db.WithContext(ctx).
		Select("run_id", "generation", "state_revision").
		Where("owner_node = ? AND lease_expires_at > ?", ownerNode, time.Now().UTC()).
		Find(&leases).Error; err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]LeaseVersion, len(leases))
	for _, l := range leases {
		id, err := uuid.Parse(l.RunID)
		if err != nil {
			log.Warn("run_leases: unparseable run_id", "run_id", l.RunID, "error", err)
			continue
		}
		out[id] = LeaseVersion{Generation: l.Generation, StateRevision: l.StateRevision}
	}
	return out, nil
}

// ValidateVersion confirms that generation+revision still name the durable
// lease state. Revision zero is the legacy/test compatibility mode and skips
// this additional fence; production owner mode always uses a positive value.
func (ls *LeaseStore) ValidateVersion(ctx context.Context, runID uuid.UUID, version LeaseVersion) error {
	if ls == nil || ls.db == nil || version.StateRevision <= 0 {
		return nil
	}
	return validateRunLeaseVersionTx(ls.db.WithContext(ctx), runID, version)
}

func validateRunLeaseVersionTx(tx *gorm.DB, runID uuid.UUID, version LeaseVersion) error {
	if tx == nil || version.StateRevision <= 0 {
		return nil
	}
	// A plain SELECT is a TOCTOU check at PostgreSQL READ COMMITTED: a retry can
	// bump the revision after the read and before this transaction's owner write.
	// This conditional no-op UPDATE takes the lease row's write lock and holds it
	// through the caller's transaction. Therefore a retry revision bump is
	// serialized entirely before (mismatch) or after (its reset wins last).
	res := tx.Model(&models.RunLease{}).
		Where("run_id = ? AND generation = ? AND state_revision = ? AND lease_expires_at > ?",
			runID.String(), version.Generation, version.StateRevision, time.Now().UTC()).
		UpdateColumn("state_revision", gorm.Expr("state_revision"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrOwnerStateChanged
	}
	return nil
}

func (s *Store) validateOwnerVersionTx(tx *gorm.DB, runID uuid.UUID, version LeaseVersion) error {
	if s == nil || s.leaseStore == nil {
		return nil
	}
	if version.StateRevision <= 0 {
		return ErrOwnerStateChanged
	}
	return validateRunLeaseVersionTx(tx, runID, version)
}

// bumpRunStateRevisionTx durably invalidates every owner's cached RunState in
// the same transaction that rewrites retry rows. A missing lease means owner
// mode is disabled and is intentionally a no-op.
func bumpRunStateRevisionTx(tx *gorm.DB, runID uuid.UUID) (int64, error) {
	if tx == nil {
		return 0, nil
	}
	result := tx.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		UpdateColumn("state_revision", gorm.Expr("state_revision + 1"))
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 0 {
		return 0, nil
	}
	var lease models.RunLease
	if err := tx.Select("state_revision").First(&lease, "run_id = ?", runID.String()).Error; err != nil {
		return 0, err
	}
	return lease.StateRevision, nil
}

// lockRunLeaseTx takes the per-run lease row write lock without changing its
// value. A missing row is valid when owner mode is disabled. Callers that also
// mutate job/task/checkpoint rows must call this before touching those rows.
func lockRunLeaseTx(tx *gorm.DB, runID uuid.UUID) error {
	if tx == nil {
		return nil
	}
	return tx.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		UpdateColumn("state_revision", gorm.Expr("state_revision")).Error
}

// lockRunStateRevisionTx serializes the durable job waiter with retry,
// cancellation, and lease takeover. Generation and expiry deliberately do not
// participate: neither changes the logical TaskRun execution the waiter
// observed. The no-op UPDATE takes the lease row's write lock before the
// JobRun lock used by completion, matching the lease-first order of retry and
// cancellation.
func lockRunStateRevisionTx(tx *gorm.DB, runID uuid.UUID, stateRevision int64) (CompletionDisposition, error) {
	if tx == nil || stateRevision <= 0 {
		return CompletionSuperseded, ErrOwnerStateChanged
	}
	if err := tx.Model(&models.RunLease{}).
		Where("run_id = ?", runID.String()).
		UpdateColumn("state_revision", gorm.Expr("state_revision")).Error; err != nil {
		return CompletionSuperseded, err
	}

	var lease models.RunLease
	if err := tx.Select("state_revision").First(&lease, "run_id = ?", runID.String()).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CompletionLeaseMissing, nil
		}
		return CompletionSuperseded, err
	}
	if lease.StateRevision != stateRevision {
		return CompletionSuperseded, nil
	}
	return CompletionFinalized, nil
}

// lockRunStateForRetryTx serializes retries for one live owner across replicas.
// The terminal guards on the retry's row updates remain the final arbiter, but
// taking this row lock before reading them prevents two replicas from both
// planning events/resets from the same terminal snapshot.
func lockRunStateForRetryTx(tx *gorm.DB, runID uuid.UUID) error {
	if tx == nil {
		return nil
	}
	if err := lockRunLeaseTx(tx, runID); err != nil {
		return err
	}
	// Owner-disabled runs have no lease row. Lock their job_run instead so two
	// replicas still cannot both pass terminal validation and emit duplicate
	// retry effects. With owner mode this follows the shared lease-first order.
	return tx.Model(&models.JobRun{}).
		Where("id = ?", runID).
		UpdateColumn("updated_at", gorm.Expr("updated_at")).Error
}

// AcquireExpiredLeases takes over every run lease whose holder let it expire
// (lease_expires_at <= now), reassigning it to newOwner with an incremented
// generation and a fresh expiry — in a single atomic UPDATE.  The expiry
// predicate is the compare-and-swap: if two nodes sweep concurrently, the first
// commit moves those rows out of the expired set, so the second updates nothing
// (no double takeover).  Returns the number of leases taken over; the caller's
// dispatch loop then sees them via OwnedRunsWithGenerations and recovers their
// in-memory state from the latest checkpoint + terminal tail.
//
// This is the run-owner failover mechanism: in in-memory mode the DB's
// per-task predecessor counters are stale (the owner advanced the DAG in
// memory), so ClaimNext recovery does not apply — a peer must take ownership
// and replay.
func (ls *LeaseStore) AcquireExpiredLeases(ctx context.Context, newOwner string, ttl time.Duration) (int64, error) {
	if ls == nil || ls.db == nil {
		return 0, nil
	}
	now := time.Now().UTC()
	result := ls.db.WithContext(ctx).
		Model(&models.RunLease{}).
		Where("lease_expires_at <= ? AND owner_node <> ? AND owner_node <> ?", now, newOwner, retiredRunLeaseOwner).
		Updates(map[string]interface{}{
			"owner_node":       newOwner,
			"acquired_at":      now,
			"lease_expires_at": now.Add(ttl),
			"generation":       gorm.Expr("generation + 1"),
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
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
