package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrReadBudget          = errors.New("connector: read budget exhausted")
	ErrBusy                = errors.New("connector: concurrent request budget exhausted")
	ErrInvalidReference    = errors.New("connector: invalid execution reference")
	ErrGenerationExhausted = errors.New("connector: observation generation exhausted")
)

// ActivateConfiguration joins a matching epoch or atomically changes it after
// checking every previously used connection identity. Removed identities are
// retained, so removing and later readding a connection cannot repoint its id.
// No provider call, secret resolution, or endpoint dial occurs here.
func (s *Store) ActivateConfiguration(ctx context.Context, cfg *Config, previous, fingerprint string) error {
	var err error
	for range 8 {
		err = s.activateConfigurationOnce(ctx, cfg, previous, fingerprint)
		if err == nil {
			return nil
		}
		actual, readErr := s.ActiveEpoch(ctx)
		if readErr != nil || actual != fingerprint {
			return err
		}
	}
	return err
}

func (s *Store) activateConfigurationOnce(ctx context.Context, cfg *Config, previous, fingerprint string) error {
	if cfg == nil || fingerprint == "" {
		return ErrEmptyEpoch
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.ConnectorConfiguration
		err := tx.Where("id = ?", models.ConnectorConfigurationID).Take(&row).Error
		fresh := errors.Is(err, gorm.ErrRecordNotFound)
		if fresh && previous != "" {
			return ErrEpochMismatch
		}
		if err != nil && !fresh {
			return err
		}
		if !fresh && row.Fingerprint != fingerprint && (previous == "" || previous != row.Fingerprint) {
			return ErrEpochMismatch
		}
		identities := map[string]ConnectionIdentity{}
		if len(row.Identities) > 0 && json.Unmarshal(row.Identities, &identities) != nil {
			return ErrInvalidReference
		}
		// Upgrades cannot claim a baseline for existing references from an unknown
		// old fingerprint. A matching epoch can safely establish its own baseline.
		if !fresh && len(row.Identities) == 0 && row.Fingerprint != fingerprint {
			var count int64
			if err := tx.Model(&models.ExternalExecution{}).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return ErrEpochMismatch
			}
		}
		changed := fresh
		for _, conn := range cfg.Connections {
			next := conn.Identity()
			if old, ok := identities[conn.ID]; ok {
				if err := old.RefuseRepoint(next); err != nil {
					return err
				}
			} else {
				identities[conn.ID] = next
				changed = true
			}
		}
		if !fresh && row.Fingerprint == fingerprint && !changed {
			return nil
		}
		raw, err := json.Marshal(identities)
		if err != nil {
			return err
		}
		if fresh {
			row = models.ConnectorConfiguration{ID: models.ConnectorConfigurationID, Fingerprint: fingerprint, Identities: datatypes.JSON(raw), UpdatedAt: time.Now().UTC()}
			res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&row)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrEpochMismatch
			}
			return nil
		}
		res := tx.Model(&models.ConnectorConfiguration{}).Where("id = ? AND fingerprint = ?", row.ID, row.Fingerprint).Updates(map[string]any{"fingerprint": fingerprint, "identities": datatypes.JSON(raw), "updated_at": time.Now().UTC()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrEpochMismatch
		}
		return nil
	})
}

// RequireEpoch is deliberately a database read on every request and RPC.
// A healthy process with a stale file must stop connector work immediately.
func (s *Store) RequireEpoch(ctx context.Context, expected string) error {
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	actual, err := s.ActiveEpoch(call)
	if err != nil {
		return err
	}
	if expected == "" || actual != expected {
		return ErrEpochMismatch
	}
	return nil
}

// ReferenceID is a bounded stateless handle; discovery never inserts identities.
// It is validated by the provider after decoding. The path's connection is
// checked independently, so a handle cannot cross an authorization boundary.
func ReferenceID(ref ExecutionReference) string {
	raw, _ := json.Marshal(ref)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func ParseReferenceID(connection, id string) (ExecutionReference, error) {
	if len(id) > 8192 {
		return ExecutionReference{}, ErrInvalidReference
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return ExecutionReference{}, ErrInvalidReference
	}
	var ref ExecutionReference
	if json.Unmarshal(raw, &ref) != nil || ref.ConnectionID != connection {
		return ExecutionReference{}, ErrInvalidReference
	}
	ref, err = NewExecutionReference(ref.ConnectionID, ref.Coordinates)
	if err != nil {
		return ExecutionReference{}, ErrInvalidReference
	}
	return ref, nil
}

// NextObservationGeneration reads a request's fence from the shared catalog
// before its provider call. It uses no process clock and allocates no write.
// Concurrent reads of one snapshot intentionally share a generation: the first
// changed result wins, and a conflicting late result must refresh its fence.
func (s *Store) NextObservationGeneration(ctx context.Context, ref ExecutionReference) (int64, error) {
	var ident models.ExternalExecution
	err := s.db.WithContext(ctx).Where("opaque_id = ?", ref.OpaqueID()).Take(&ident).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if ident.LatestGeneration == nil {
		return 1, nil
	}
	if *ident.LatestGeneration == math.MaxInt64 {
		return 0, ErrGenerationExhausted
	}
	return *ident.LatestGeneration + 1, nil
}

// RecordDirect fences the epoch and request generation in the same transaction
// as the snapshot. Polling identical evidence never updates a watermark, time,
// or catalog row. Source timestamps are excluded from Metadata by the caller.
func (s *Store) RecordDirect(ctx context.Context, epoch string, obs Observation, limits Limits) (SnapshotResult, error) {
	var result SnapshotResult
	prepared, _, digest, err := prepareObservation(obs, limits.MaxPageMetadataBytes)
	if err != nil {
		return result, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOperationEpoch(tx, epoch); err != nil {
			return err
		}
		var ident models.ExternalExecution
		err := tx.Where("opaque_id = ?", prepared.Execution.OpaqueID()).Take(&ident).Error
		if err == nil && ident.LatestEvidenceDigest == digest && ident.LatestObservedAt != nil {
			result = SnapshotResult{ObservedAt: *ident.LatestObservedAt}
			return enforceDirectRetention(tx, prepared.Execution.ConnectionID, time.Now().Add(-limits.UnreferencedSnapshotMaxAge), limits.MaxUnreferencedSnapshots)
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		result, err = NewStore(tx).RecordSnapshot(ctx, prepared, WriteBounds{MaxMetadataBytes: limits.MaxPageMetadataBytes, MaxUnreferencedSnapshots: limits.MaxUnreferencedSnapshots})
		if err != nil {
			return err
		}
		if result.Wrote {
			return enforceDirectRetention(tx, prepared.Execution.ConnectionID, time.Now().Add(-limits.UnreferencedSnapshotMaxAge), limits.MaxUnreferencedSnapshots)
		}
		return nil
	})
	return result, err
}

// Snapshot returns the latest retained observation. Expired unreferenced detail
// cannot become an outage cache merely because its identity still exists.
func (s *Store) Snapshot(ctx context.Context, ref ExecutionReference, limits Limits) (models.ExternalExecutionSnapshot, error) {
	var snap models.ExternalExecutionSnapshot
	err := s.db.WithContext(ctx).Table("external_execution_snapshots AS s").Select("s.*").Joins("JOIN external_executions AS e ON e.opaque_id = s.opaque_id").Where("s.opaque_id = ? AND (e.referenced = ? OR s.observed_at >= ?)", ref.OpaqueID(), true, time.Now().Add(-limits.UnreferencedSnapshotMaxAge)).Order("s.generation DESC").Take(&snap).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return snap, ErrNotFound
	}
	return snap, err
}

func enforceDirectRetention(tx *gorm.DB, connection string, cutoff time.Time, max int) error {
	var rows []models.ExternalExecutionSnapshot
	if err := tx.Table("external_execution_snapshots AS s").Select("s.*").Joins("JOIN external_executions AS e ON e.opaque_id = s.opaque_id").Where("s.connection_id = ? AND e.referenced = ?", connection, false).Order("s.observed_at DESC, s.id DESC").Find(&rows).Error; err != nil {
		return err
	}
	var ids []string
	for i, row := range rows {
		if i >= max || row.ObservedAt.Before(cutoff) {
			ids = append(ids, row.ID.String())
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// Keep identity watermarks and the evidence digest after expiry. Polling
	// unchanged source evidence must not recreate an expired snapshot.
	return tx.Where("id IN ?", ids).Delete(&models.ExternalExecutionSnapshot{}).Error
}

type flight struct {
	done chan struct{}
	raw  []byte
	err  error
}
type readWindow struct {
	start time.Time
	count int
}

// ReadGuard bounds retained principal buckets and concurrent request keys. It
// coalesces only in-flight identical reads; it is not an unbounded detail cache.
type ReadGuard struct {
	mu         sync.Mutex
	flights    map[string]*flight
	principals map[string]readWindow
	slots      chan struct{}
	limits     Limits
	store      *Store
	epoch      string
}

func NewReadGuard(store *Store, epoch string, limits Limits) *ReadGuard {
	return &ReadGuard{flights: map[string]*flight{}, principals: map[string]readWindow{}, slots: make(chan struct{}, limits.MaxInFlightRPCs), limits: limits, store: store, epoch: epoch}
}
func (g *ReadGuard) Allow(ctx context.Context, principal string) error {
	if err := g.store.RequireEpoch(ctx, g.epoch); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	w := g.principals[principal]
	if now.Sub(w.start) >= time.Minute {
		w = readWindow{start: now}
	}
	if w.count >= g.limits.ReadsPerMinute {
		return ErrReadBudget
	}
	if _, ok := g.principals[principal]; !ok && len(g.principals) >= 4096 {
		for key, v := range g.principals {
			if now.Sub(v.start) >= time.Minute {
				delete(g.principals, key)
			}
		}
		if len(g.principals) >= 4096 {
			return ErrReadBudget
		}
	}
	w.count++
	g.principals[principal] = w
	return nil
}
func (g *ReadGuard) Do(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := g.store.RequireEpoch(ctx, g.epoch); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if f, ok := g.flights[key]; ok {
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			if err := g.store.RequireEpoch(ctx, g.epoch); err != nil {
				return nil, err
			}
			return f.raw, f.err
		}
	}
	select {
	case g.slots <- struct{}{}:
	default:
		g.mu.Unlock()
		return nil, ErrBusy
	}
	f := &flight{done: make(chan struct{})}
	g.flights[key] = f
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.flights, key); close(f.done); g.mu.Unlock(); <-g.slots }()
	call, cancel := context.WithTimeout(ctx, g.limits.RPCDeadline)
	defer cancel()
	if err := g.store.RequireEpoch(call, g.epoch); err != nil {
		f.err = err
		return nil, err
	}
	f.raw, f.err = fn(call)
	if err := g.store.RequireEpoch(ctx, g.epoch); err != nil {
		f.raw = nil
		f.err = err
	}
	return f.raw, f.err
}

// RecordObservedRelation retains genuine provider evidence only when the source
// was directly inspected. The target identity comes from that evidence; this
// does not manufacture a snapshot or local JobRun for the target.
func (s *Store) RecordObservedRelation(ctx context.Context, epoch string, from, to ExecutionReference, kind string, evidence []byte, limits Limits) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireOperationEpoch(tx, epoch); err != nil {
			return err
		}
		if err := identityExists(tx, from.OpaqueID()); err != nil {
			return err
		}

		var existing models.ExternalExecutionRelation
		err := tx.Where("from_opaque_id = ? AND to_opaque_id = ? AND relation_type = ? AND evidence_digest = ?", from.OpaqueID(), to.OpaqueID(), kind, digestBytes(evidence)).Take(&existing).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := ensureIdentity(tx, to); err != nil {
			return err
		}
		_, err = NewStore(tx).RecordRelation(ctx, from, to, kind, evidence, WriteBounds{ExpectedEpoch: epoch, MaxMetadataBytes: limits.MaxPageMetadataBytes, MaxUnreferencedSnapshots: limits.MaxUnreferencedSnapshots})
		return err
	})
}
