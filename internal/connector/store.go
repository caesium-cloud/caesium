package connector

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Display statuses. Terminal values are completed, failed, canceled,
// terminated, continued, and timed_out. running and unknown are not terminal.
const (
	DisplayRunning    = "running"
	DisplayCompleted  = "completed"
	DisplayFailed     = "failed"
	DisplayCanceled   = "canceled"
	DisplayTerminated = "terminated"
	DisplayContinued  = "continued"
	DisplayTimedOut   = "timed_out"
	DisplayUnknown    = "unknown"

	AvailabilityAvailable   = "available"
	AvailabilityStale       = "stale"
	AvailabilityUnavailable = "unavailable"
	AvailabilityPartial     = "partial"

	CompletenessComplete = "complete"
	CompletenessPartial  = "partial"

	SourceDirect    = "direct"
	SourceDiscovery = "discovery"

	RelationParent       = "parent"
	RelationChild        = "child"
	RelationContinuation = "continuation"
	RelationDelegation   = "delegation"

	OperationSubmitted = "submitted"
	OperationAccepted  = "accepted"
	OperationCompleted = "completed"
	OperationRejected  = "rejected"
	OperationUnknown   = "unknown"
)

var (
	// ErrNotFound is an unknown identity, operation, or epoch row.
	ErrNotFound = errors.New("connector: not found")
	// ErrStaleGeneration rejects a generation behind the latest snapshot, or
	// the same generation with different evidence.
	ErrStaleGeneration = errors.New("connector: stale observation generation")
	// ErrTerminalRegression rejects a non-terminal observation after a
	// terminal snapshot for the same identity.
	ErrTerminalRegression = errors.New("connector: terminal observation cannot regress")
	// ErrDiscoverySuperseded rejects discovery that is not strictly newer than
	// the latest direct snapshot.
	ErrDiscoverySuperseded = errors.New("connector: discovery cannot overwrite newer direct state")
	// ErrIdempotencyMismatch rejects the same idempotency key when the
	// execution, action, binding version, external update id, or request
	// fingerprint differs from the stored receipt.
	ErrIdempotencyMismatch = errors.New("connector: idempotency key reused with a different request")
	// ErrSnapshotConflict means a concurrent writer won the identity
	// compare-and-swap. RecordSnapshot retries it, then returns this error.
	ErrSnapshotConflict = errors.New("connector: snapshot compare-and-swap conflict")
	// ErrEpochMismatch means the stored fingerprint was not the expected previous value.
	ErrEpochMismatch = errors.New("connector: configuration epoch mismatch")
	// ErrEmptyEpoch rejects an empty next fingerprint.
	ErrEmptyEpoch = errors.New("connector: configuration fingerprint is empty")
	// ErrInvalidTransition rejects an operation move the state machine does not allow.
	ErrInvalidTransition = errors.New("connector: invalid operation transition")
	// ErrOperationTerminal rejects any change to a completed or rejected operation.
	ErrOperationTerminal = errors.New("connector: operation is terminal")
	// ErrPayloadTooLarge rejects metadata, actor JSON, or relation evidence over MaxPageMetadataBytes.
	ErrPayloadTooLarge = errors.New("connector: payload exceeds the page metadata budget")
	// ErrInvalidPayload rejects non-empty bytes that are not JSON.
	ErrInvalidPayload = errors.New("connector: payload is not json")
	// ErrInvalidObservation rejects a display status, availability, completeness, or source the catalog does not store.
	ErrInvalidObservation = errors.New("connector: observation is invalid")
	// ErrInvalidRelation rejects a relation type outside parent, child, continuation, and delegation.
	ErrInvalidRelation = errors.New("connector: invalid relation type")
	// ErrInvalidOperation rejects an incomplete admission or an unknown operation state.
	ErrInvalidOperation = errors.New("connector: operation request is invalid")
	// ErrRetentionLimit rejects a non-positive budget or one above the hard ceiling.
	ErrRetentionLimit = errors.New("connector: retention limit exceeds the hard ceiling")
)

// Observation is one direct read or one discovery read of an execution that
// was already observed directly. ObservedAt is stored only when evidence
// advances. An unchanged read returns the stored time instead.
type Observation struct {
	Execution     ExecutionReference
	Generation    int64
	SourceEventID string
	NativeStatus  string
	DisplayStatus string
	Availability  string
	Completeness  string
	Metadata      []byte
	SourceKind    string
	ObservedAt    time.Time
}

// SnapshotResult reports whether a snapshot row was inserted. ObservedAt is
// the stored time: the caller's time on a write, the previous row's time when
// evidence did not change.
type SnapshotResult struct {
	Wrote      bool
	ObservedAt time.Time
}

// OperationRequest is the receipt admitted before an action is sent.
// Actor is bounded JSON. The request fingerprint is stored; the raw payload
// and secret bytes are not.
type OperationRequest struct {
	ConnectionID       string
	IdempotencyKey     string
	Execution          ExecutionReference
	Action             string
	RequestFingerprint string
	Actor              []byte
	BindingVersion     string
	ExternalUpdateID   string
}

// OperationDecision is the admission result. Created is true only for a new
// row. Conflict is true when a different idempotency key hits the open guard;
// Operation is then the existing receipt.
type OperationDecision struct {
	Operation models.ConnectorOperation
	Created   bool
	Conflict  bool
}

// Store persists connector catalog rows. It does not dial a provider.
type Store struct {
	db    *gorm.DB
	clock func() time.Time
}

// NewStore returns a catalog store. clock, when set, is used only for
// retention cutoffs. Observation times come from the caller.
func NewStore(db *gorm.DB, clock ...func() time.Time) *Store {
	fn := time.Now
	if len(clock) > 0 && clock[0] != nil {
		fn = clock[0]
	}
	return &Store{db: db, clock: fn}
}

// WriteBounds is one connection's configured write budget. A zero field uses
// the matching hard ceiling. A value outside 1..ceiling is rejected. C1 passes
// the connection limits. Callers that omit the argument stay on the ceilings.
type WriteBounds struct {
	MaxMetadataBytes         int
	MaxUnreferencedSnapshots int
}

// RecordSnapshot inserts an identity for a direct read and appends a snapshot
// when evidence advances. Discovery never inserts an identity and writes
// nothing for an unknown one. Unchanged evidence does not write a snapshot and
// does not refresh the stored observation time. A higher generation with the
// same evidence advances the generation watermark only. List and history pages
// are not accepted here; there is no method that inserts from a page of search
// results. bounds, when set, supplies the connection's metadata and snapshot caps.
func (s *Store) RecordSnapshot(ctx context.Context, obs Observation, bounds ...WriteBounds) (SnapshotResult, error) {
	limit, err := resolveWriteBounds(bounds...)
	if err != nil {
		return SnapshotResult{}, err
	}
	prepared, terminal, digest, err := prepareObservation(obs, limit.MaxMetadataBytes)
	if err != nil {
		return SnapshotResult{}, err
	}
	opaque := prepared.Execution.OpaqueID()

	var ident models.ExternalExecution
	err = s.db.WithContext(ctx).Where("opaque_id = ?", opaque).First(&ident).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if prepared.SourceKind == SourceDiscovery {
			return SnapshotResult{}, ErrNotFound
		}
		return s.advanceSnapshot(ctx, prepared, digest, terminal, limit.MaxUnreferencedSnapshots)
	}
	if err != nil {
		return SnapshotResult{}, err
	}
	if decision, stop := decideSnapshot(ident, prepared, digest, terminal); stop {
		return decision.result, decision.err
	}
	return s.advanceSnapshot(ctx, prepared, digest, terminal, limit.MaxUnreferencedSnapshots)
}

// RecordRelation stores typed evidence between two identities that already
// exist. It does not create identities. The same from, to, type, and evidence
// digest returns the original row. A different digest inserts another row.
// Both identities are marked referenced.
func (s *Store) RecordRelation(ctx context.Context, from, to ExecutionReference, relationType string, evidence []byte, bounds ...WriteBounds) (models.ExternalExecutionRelation, error) {
	limit, err := resolveWriteBounds(bounds...)
	if err != nil {
		return models.ExternalExecutionRelation{}, err
	}
	if !validRelationType(relationType) {
		return models.ExternalExecutionRelation{}, ErrInvalidRelation
	}
	if err := boundJSON(evidence, limit.MaxMetadataBytes); err != nil {
		return models.ExternalExecutionRelation{}, err
	}
	fromRef, err := NewExecutionReference(from.ConnectionID, from.Coordinates)
	if err != nil {
		return models.ExternalExecutionRelation{}, err
	}
	toRef, err := NewExecutionReference(to.ConnectionID, to.Coordinates)
	if err != nil {
		return models.ExternalExecutionRelation{}, err
	}
	fromID := fromRef.OpaqueID()
	toID := toRef.OpaqueID()
	digest := digestBytes(evidence)
	row := models.ExternalExecutionRelation{
		ID:             uuid.New(),
		FromOpaqueID:   fromID,
		ToOpaqueID:     toID,
		RelationType:   relationType,
		Evidence:       jsonValue(evidence),
		EvidenceDigest: digest,
		CreatedAt:      time.Now().UTC(),
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := identityExists(tx, fromID); err != nil {
			return err
		}
		if fromID != toID {
			if err := identityExists(tx, toID); err != nil {
				return err
			}
		}
		res := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "from_opaque_id"},
				{Name: "to_opaque_id"},
				{Name: "relation_type"},
				{Name: "evidence_digest"},
			},
			DoNothing: true,
		}).Create(&row)
		if res.Error != nil {
			return res.Error
		}
		// Look up with a zero primary key. First on the inserted struct would
		// add its new id, and ON CONFLICT DO NOTHING leaves that id unstored.
		var stored models.ExternalExecutionRelation
		if err := tx.Where(
			"from_opaque_id = ? AND to_opaque_id = ? AND relation_type = ? AND evidence_digest = ?",
			fromID, toID, relationType, digest,
		).Take(&stored).Error; err != nil {
			return err
		}
		row = stored
		if err := markReferencedTx(tx, fromID); err != nil {
			return err
		}
		if fromID != toID {
			if err := markReferencedTx(tx, toID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return models.ExternalExecutionRelation{}, err
	}
	return row, nil
}

// MarkReferenced flags an identity that a verified relationship or an
// operation depends on. Referenced snapshots are not eligible for eviction.
// Merely recording a snapshot does not call this.
func (s *Store) MarkReferenced(ctx context.Context, opaqueID string) error {
	if strings.TrimSpace(opaqueID) == "" {
		return ErrNotFound
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return markReferencedTx(tx, opaqueID)
	})
}

// AdmitOperation inserts one receipt for a connection and idempotency key.
// The identity must already exist. The same key and the same execution,
// action, binding version, external update id, and fingerprint returns the
// original row. Any of those fields differing writes nothing. A second key
// for the same open connection, execution, and action returns the existing
// id with Conflict set and writes nothing. A new row marks the identity
// referenced. bounds, when set, caps the actor JSON.
func (s *Store) AdmitOperation(ctx context.Context, req OperationRequest, bounds ...WriteBounds) (OperationDecision, error) {
	limit, err := resolveWriteBounds(bounds...)
	if err != nil {
		return OperationDecision{}, err
	}
	ref, err := validateOperationRequest(req, limit.MaxMetadataBytes)
	if err != nil {
		return OperationDecision{}, err
	}
	opaque := ref.OpaqueID()
	openKey := models.OperationOpenKey(req.ConnectionID, opaque, req.Action)
	now := time.Now().UTC()
	op := models.ConnectorOperation{
		ID:                 uuid.New(),
		ConnectionID:       req.ConnectionID,
		IdempotencyKey:     req.IdempotencyKey,
		OpaqueID:           opaque,
		ActionName:         req.Action,
		RequestFingerprint: req.RequestFingerprint,
		Actor:              jsonValue(req.Actor),
		BindingVersion:     req.BindingVersion,
		ExternalUpdateID:   req.ExternalUpdateID,
		State:              OperationSubmitted,
		OpenKey:            &openKey,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	var created bool
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var owner models.ExternalExecution
		if err := tx.Where("opaque_id = ?", opaque).First(&owner).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "connection_id"}, {Name: "idempotency_key"}},
			DoNothing: true,
		}).Create(&op)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var stored models.ConnectorOperation
			if err := tx.Where("connection_id = ? AND idempotency_key = ?", req.ConnectionID, req.IdempotencyKey).First(&stored).Error; err != nil {
				return err
			}
			if !admissionMatches(stored, req, opaque) {
				return ErrIdempotencyMismatch
			}
			op = stored
			return nil
		}
		created = true
		return markReferencedTx(tx, opaque)
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Idempotency is checked first. A replay that missed ON CONFLICT still
			// resolves to the original row. A different key shares only the open guard.
			var stored models.ConnectorOperation
			loadErr := s.db.WithContext(ctx).
				Where("connection_id = ? AND idempotency_key = ?", req.ConnectionID, req.IdempotencyKey).
				First(&stored).Error
			if loadErr == nil {
				if !admissionMatches(stored, req, opaque) {
					return OperationDecision{}, ErrIdempotencyMismatch
				}
				return OperationDecision{Operation: stored, Created: false, Conflict: false}, nil
			}
			if !errors.Is(loadErr, gorm.ErrRecordNotFound) {
				return OperationDecision{}, loadErr
			}
			existing, found, openErr := s.operationByOpenKey(ctx, openKey)
			if openErr != nil {
				return OperationDecision{}, openErr
			}
			if found {
				return OperationDecision{Operation: existing, Created: false, Conflict: true}, nil
			}
		}
		return OperationDecision{}, err
	}
	return OperationDecision{Operation: op, Created: created, Conflict: false}, nil
}

// SetOperationState moves a receipt. submitted and accepted may become
// accepted, completed, rejected, or unknown. unknown may become submitted,
// accepted, completed, or rejected and stays open until a terminal state.
// completed and rejected are terminal: a later change returns an error, does
// not reopen the guard, and does not insert a row. Moving to a terminal state
// sets OpenKey NULL so a future admission can insert a new row.
func (s *Store) SetOperationState(ctx context.Context, operationID uuid.UUID, next string) (models.ConnectorOperation, error) {
	if !validOperationState(next) {
		return models.ConnectorOperation{}, ErrInvalidOperation
	}
	var lastErr error
	for range 4 {
		op, err := s.transitionOperation(ctx, operationID, next)
		if err == nil {
			return op, nil
		}
		if !errors.Is(err, errTransitionRace) {
			return models.ConnectorOperation{}, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errTransitionRace
	}
	return models.ConnectorOperation{}, lastErr
}

// ActivateEpoch compares and swaps the stored configuration fingerprint.
// An empty next value is rejected. On an empty table, previous must be empty.
// A row updates only where the stored fingerprint equals previous. Exactly one
// concurrent caller from the same previous value wins.
func (s *Store) ActivateEpoch(ctx context.Context, previousFingerprint, nextFingerprint string) error {
	if nextFingerprint == "" {
		return ErrEmptyEpoch
	}
	now := time.Now().UTC()
	if previousFingerprint == "" {
		row := models.ConnectorConfiguration{
			ID:          models.ConnectorConfigurationID,
			Fingerprint: nextFingerprint,
			UpdatedAt:   now,
		}
		res := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoNothing: true,
		}).Create(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrEpochMismatch
		}
		return nil
	}
	res := s.db.WithContext(ctx).Model(&models.ConnectorConfiguration{}).
		Where("id = ? AND fingerprint = ?", models.ConnectorConfigurationID, previousFingerprint).
		Updates(map[string]any{
			"fingerprint": nextFingerprint,
			"updated_at":  now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrEpochMismatch
	}
	return nil
}

// ReadCatalogEpoch returns the stored fingerprint. A missing row is an empty
// fingerprint and a nil error. Startup calls this after migration when the
// connector gate is on. It does not compare the fingerprint; that is C1.
func ReadCatalogEpoch(ctx context.Context, db *gorm.DB) (string, error) {
	fingerprint, err := NewStore(db).ActiveEpoch(ctx)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return fingerprint, err
}

// ActiveEpoch returns the stored configuration fingerprint.
func (s *Store) ActiveEpoch(ctx context.Context) (string, error) {
	var row models.ConnectorConfiguration
	err := s.db.WithContext(ctx).Where("id = ?", models.ConnectorConfigurationID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return row.Fingerprint, nil
}

// EnforceRetention deletes unreferenced historical snapshots for connectionID
// older than maxAge, then evicts the oldest of those until at most maxCount
// remain. An identity's latest snapshot is kept even when it is old and
// unreferenced. Identities, relations, operations, the epoch row, and
// snapshots of a referenced identity are kept. Limits above the hard ceilings
// or non-positive limits are rejected.
func (s *Store) EnforceRetention(ctx context.Context, connectionID string, maxAge time.Duration, maxCount int) error {
	if maxAge <= 0 || maxCount <= 0 || maxAge > MaxUnreferencedSnapshotAge || maxCount > MaxUnreferencedSnapshots {
		return ErrRetentionLimit
	}
	cutoff := s.clock().Add(-maxAge)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := deleteUnreferencedOlderThan(tx, connectionID, cutoff); err != nil {
			return err
		}
		return evictHistoricalAbove(tx, connectionID, maxCount)
	})
}

type snapshotDecision struct {
	result    SnapshotResult
	err       error
	watermark bool
}

func prepareObservation(obs Observation, maxMetadata int) (Observation, bool, string, error) {
	ref, err := NewExecutionReference(obs.Execution.ConnectionID, obs.Execution.Coordinates)
	if err != nil {
		return Observation{}, false, "", err
	}
	obs.Execution = ref
	terminal, ok := terminalDisplay(obs.DisplayStatus)
	if !ok || !validAvailability(obs.Availability) || !validCompleteness(obs.Completeness) || !validSourceKind(obs.SourceKind) {
		return Observation{}, false, "", ErrInvalidObservation
	}
	if err := boundJSON(obs.Metadata, maxMetadata); err != nil {
		return Observation{}, false, "", err
	}
	obs.ObservedAt = obs.ObservedAt.UTC()
	return obs, terminal, evidenceDigest(obs, terminal), nil
}

func decideSnapshot(ident models.ExternalExecution, obs Observation, digest string, terminal bool) (snapshotDecision, bool) {
	if ident.LatestGeneration == nil {
		return snapshotDecision{}, false
	}
	latest := *ident.LatestGeneration
	if ident.LatestEvidenceDigest == digest {
		if obs.Generation > latest {
			return snapshotDecision{watermark: true}, false
		}
		observed := time.Time{}
		if ident.LatestObservedAt != nil {
			observed = ident.LatestObservedAt.UTC()
		}
		return snapshotDecision{result: SnapshotResult{Wrote: false, ObservedAt: observed}}, true
	}
	if obs.Generation <= latest {
		if obs.SourceKind == SourceDiscovery && ident.LatestSourceKind == SourceDirect {
			return snapshotDecision{err: ErrDiscoverySuperseded}, true
		}
		return snapshotDecision{err: ErrStaleGeneration}, true
	}
	if !terminal && ident.LatestTerminal {
		return snapshotDecision{err: ErrTerminalRegression}, true
	}
	return snapshotDecision{}, false
}

func (s *Store) advanceSnapshot(ctx context.Context, obs Observation, digest string, terminal bool, maxHistorical int) (SnapshotResult, error) {
	var lastErr error
	for range 8 {
		result, err := s.advanceSnapshotOnce(ctx, obs, digest, terminal, maxHistorical)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, ErrSnapshotConflict) {
			return SnapshotResult{}, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrSnapshotConflict
	}
	return SnapshotResult{}, lastErr
}

func (s *Store) advanceSnapshotOnce(ctx context.Context, obs Observation, digest string, terminal bool, maxHistorical int) (SnapshotResult, error) {
	opaque := obs.Execution.OpaqueID()
	var result SnapshotResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if obs.SourceKind == SourceDirect {
			if err := ensureIdentity(tx, obs.Execution); err != nil {
				return err
			}
		}
		var ident models.ExternalExecution
		if err := tx.Where("opaque_id = ?", opaque).First(&ident).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		decision, stop := decideSnapshot(ident, obs, digest, terminal)
		if decision.err != nil {
			return decision.err
		}
		if decision.watermark {
			res := tx.Model(&models.ExternalExecution{}).
				Where("opaque_id = ? AND cas_version = ?", opaque, ident.CASVersion).
				Updates(map[string]any{
					"cas_version":       ident.CASVersion + 1,
					"latest_generation": obs.Generation,
					"updated_at":        time.Now().UTC(),
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrSnapshotConflict
			}
			observed := time.Time{}
			if ident.LatestObservedAt != nil {
				observed = ident.LatestObservedAt.UTC()
			}
			result = SnapshotResult{Wrote: false, ObservedAt: observed}
			return nil
		}
		if stop {
			result = decision.result
			return nil
		}
		res := tx.Model(&models.ExternalExecution{}).
			Where("opaque_id = ? AND cas_version = ?", opaque, ident.CASVersion).
			Updates(map[string]any{
				"cas_version":            ident.CASVersion + 1,
				"latest_generation":      obs.Generation,
				"latest_evidence_digest": digest,
				"latest_terminal":        terminal,
				"latest_source_kind":     obs.SourceKind,
				"latest_observed_at":     obs.ObservedAt,
				"updated_at":             time.Now().UTC(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrSnapshotConflict
		}
		if err := evictHistoricalAbove(tx, obs.Execution.ConnectionID, maxHistorical); err != nil {
			return err
		}
		snap := models.ExternalExecutionSnapshot{
			ID:             uuid.New(),
			OpaqueID:       opaque,
			ConnectionID:   obs.Execution.ConnectionID,
			Generation:     obs.Generation,
			SourceEventID:  obs.SourceEventID,
			NativeStatus:   obs.NativeStatus,
			DisplayStatus:  obs.DisplayStatus,
			Availability:   obs.Availability,
			Completeness:   obs.Completeness,
			Metadata:       jsonValue(obs.Metadata),
			SourceKind:     obs.SourceKind,
			Terminal:       terminal,
			EvidenceDigest: digest,
			ObservedAt:     obs.ObservedAt,
			CreatedAt:      time.Now().UTC(),
		}
		if err := tx.Create(&snap).Error; err != nil {
			return err
		}
		result = SnapshotResult{Wrote: true, ObservedAt: obs.ObservedAt}
		return nil
	})
	if err != nil {
		return SnapshotResult{}, err
	}
	return result, nil
}

func ensureIdentity(tx *gorm.DB, ref ExecutionReference) error {
	raw, err := json.Marshal(ref.Coordinates)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row := models.ExternalExecution{
		OpaqueID:     ref.OpaqueID(),
		ConnectionID: ref.ConnectionID,
		Coordinates:  datatypes.JSON(raw),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "opaque_id"}},
		DoNothing: true,
	}).Create(&row).Error
}

func admissionMatches(stored models.ConnectorOperation, req OperationRequest, opaque string) bool {
	return stored.RequestFingerprint == req.RequestFingerprint &&
		stored.OpaqueID == opaque &&
		stored.ActionName == req.Action &&
		stored.BindingVersion == req.BindingVersion &&
		stored.ExternalUpdateID == req.ExternalUpdateID
}

func resolveWriteBounds(bounds ...WriteBounds) (WriteBounds, error) {
	out := WriteBounds{
		MaxMetadataBytes:         MaxPageMetadataBytes,
		MaxUnreferencedSnapshots: MaxUnreferencedSnapshots,
	}
	if len(bounds) == 0 || bounds[0] == (WriteBounds{}) {
		return out, nil
	}
	in := bounds[0]
	if in.MaxMetadataBytes != 0 {
		if in.MaxMetadataBytes < 1 || in.MaxMetadataBytes > MaxPageMetadataBytes {
			return WriteBounds{}, ErrRetentionLimit
		}
		out.MaxMetadataBytes = in.MaxMetadataBytes
	}
	if in.MaxUnreferencedSnapshots != 0 {
		if in.MaxUnreferencedSnapshots < 1 || in.MaxUnreferencedSnapshots > MaxUnreferencedSnapshots {
			return WriteBounds{}, ErrRetentionLimit
		}
		out.MaxUnreferencedSnapshots = in.MaxUnreferencedSnapshots
	}
	return out, nil
}

func identityExists(tx *gorm.DB, opaqueID string) error {
	var n int64
	if err := tx.Model(&models.ExternalExecution{}).Where("opaque_id = ?", opaqueID).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func markReferencedTx(tx *gorm.DB, opaqueID string) error {
	res := tx.Model(&models.ExternalExecution{}).Where("opaque_id = ?", opaqueID).Update("referenced", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return identityExists(tx, opaqueID)
}

var errTransitionRace = errors.New("connector: operation transition lost the compare-and-swap")

func (s *Store) transitionOperation(ctx context.Context, operationID uuid.UUID, next string) (models.ConnectorOperation, error) {
	var out models.ConnectorOperation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cur models.ConnectorOperation
		if err := tx.Where("id = ?", operationID).First(&cur).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if cur.State == OperationCompleted || cur.State == OperationRejected {
			return ErrOperationTerminal
		}
		if !operationTransitionAllowed(cur.State, next) {
			return ErrInvalidTransition
		}
		updates := map[string]any{
			"state":      next,
			"updated_at": time.Now().UTC(),
		}
		if next == OperationCompleted || next == OperationRejected {
			updates["open_key"] = gorm.Expr("NULL")
		}
		res := tx.Model(&models.ConnectorOperation{}).
			Where("id = ? AND state = ?", operationID, cur.State).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errTransitionRace
		}
		return tx.Where("id = ?", operationID).First(&out).Error
	})
	if err != nil {
		return models.ConnectorOperation{}, err
	}
	return out, nil
}

func (s *Store) operationByOpenKey(ctx context.Context, openKey string) (models.ConnectorOperation, bool, error) {
	var row models.ConnectorOperation
	err := s.db.WithContext(ctx).Where("open_key = ?", openKey).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.ConnectorOperation{}, false, nil
	}
	if err != nil {
		return models.ConnectorOperation{}, false, err
	}
	return row, true, nil
}

func validateOperationRequest(req OperationRequest, maxMetadata int) (ExecutionReference, error) {
	if strings.TrimSpace(req.ConnectionID) == "" ||
		strings.TrimSpace(req.IdempotencyKey) == "" ||
		strings.TrimSpace(req.Action) == "" ||
		strings.TrimSpace(req.RequestFingerprint) == "" ||
		strings.TrimSpace(req.BindingVersion) == "" ||
		strings.TrimSpace(req.ExternalUpdateID) == "" {
		return ExecutionReference{}, ErrInvalidOperation
	}
	if err := boundJSON(req.Actor, maxMetadata); err != nil {
		return ExecutionReference{}, err
	}
	ref, err := NewExecutionReference(req.Execution.ConnectionID, req.Execution.Coordinates)
	if err != nil {
		return ExecutionReference{}, err
	}
	if ref.ConnectionID != req.ConnectionID {
		return ExecutionReference{}, ErrInvalidOperation
	}
	return ref, nil
}

func operationTransitionAllowed(from, to string) bool {
	switch from {
	case OperationSubmitted, OperationAccepted:
		switch to {
		case OperationAccepted, OperationCompleted, OperationRejected, OperationUnknown:
			return true
		default:
			return false
		}
	case OperationUnknown:
		switch to {
		case OperationSubmitted, OperationAccepted, OperationCompleted, OperationRejected:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func evictHistoricalAbove(tx *gorm.DB, connectionID string, keep int) error {
	rows, err := historicalSnapshots(tx, connectionID)
	if err != nil {
		return err
	}
	if len(rows) <= keep {
		return nil
	}
	sortHistorical(rows)
	drop := rows[:len(rows)-keep]
	ids := make([]string, len(drop))
	for i, row := range drop {
		ids[i] = row.ID
	}
	return deleteSnapshotIDs(tx, ids)
}

func deleteUnreferencedOlderThan(tx *gorm.DB, connectionID string, cutoff time.Time) error {
	// Compare instants in Go. SQLite stores the driver's timestamp text, and a
	// SQL inequality on that text is not the retention rule. The latest
	// snapshot of each identity is not eligible.
	rows, err := historicalSnapshots(tx, connectionID)
	if err != nil {
		return err
	}
	var ids []string
	for _, row := range rows {
		if row.ObservedAt.Before(cutoff) {
			ids = append(ids, row.ID)
		}
	}
	return deleteSnapshotIDs(tx, ids)
}

type agedSnapshot struct {
	ID         string    `gorm:"column:id"`
	ObservedAt time.Time `gorm:"column:observed_at"`
}

func historicalSnapshots(tx *gorm.DB, connectionID string) ([]agedSnapshot, error) {
	var rows []agedSnapshot
	err := tx.Raw(`
		SELECT s.id AS id, s.observed_at AS observed_at
		FROM external_execution_snapshots AS s
		INNER JOIN external_executions AS e ON e.opaque_id = s.opaque_id
		WHERE s.connection_id = ? AND e.referenced = ?
		  AND NOT (e.latest_generation IS NOT NULL AND s.generation = e.latest_generation)
	`, connectionID, false).Scan(&rows).Error
	return rows, err
}

func sortHistorical(rows []agedSnapshot) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ObservedAt.Equal(rows[j].ObservedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].ObservedAt.Before(rows[j].ObservedAt)
	})
}

func deleteSnapshotIDs(tx *gorm.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return tx.Exec(`DELETE FROM external_execution_snapshots WHERE id IN ?`, ids).Error
}

func boundJSON(payload []byte, maxBytes int) error {
	if len(payload) > maxBytes {
		return ErrPayloadTooLarge
	}
	if len(payload) > 0 && !json.Valid(payload) {
		return ErrInvalidPayload
	}
	return nil
}

func jsonValue(payload []byte) datatypes.JSON {
	if len(payload) == 0 {
		return nil
	}
	return datatypes.JSON(append([]byte(nil), payload...))
}

func evidenceDigest(obs Observation, terminal bool) string {
	terminalBit := "0"
	if terminal {
		terminalBit = "1"
	}
	return digestBytes(
		[]byte(obs.SourceEventID),
		[]byte(obs.NativeStatus),
		[]byte(obs.DisplayStatus),
		[]byte(obs.Availability),
		[]byte(obs.Completeness),
		obs.Metadata,
		[]byte(terminalBit),
	)
}

func digestBytes(parts ...[]byte) string {
	hash := sha256.New()
	var prefix [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(prefix[:], uint64(len(part)))
		_, _ = hash.Write(prefix[:])
		_, _ = hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func terminalDisplay(status string) (bool, bool) {
	switch status {
	case DisplayRunning, DisplayUnknown:
		return false, true
	case DisplayCompleted, DisplayFailed, DisplayCanceled, DisplayTerminated, DisplayContinued, DisplayTimedOut:
		return true, true
	default:
		return false, false
	}
}

func validAvailability(value string) bool {
	switch value {
	case AvailabilityAvailable, AvailabilityStale, AvailabilityUnavailable, AvailabilityPartial:
		return true
	default:
		return false
	}
}

func validCompleteness(value string) bool {
	switch value {
	case CompletenessComplete, CompletenessPartial:
		return true
	default:
		return false
	}
}

func validSourceKind(value string) bool {
	switch value {
	case SourceDirect, SourceDiscovery:
		return true
	default:
		return false
	}
}

func validRelationType(value string) bool {
	switch value {
	case RelationParent, RelationChild, RelationContinuation, RelationDelegation:
		return true
	default:
		return false
	}
}

func validOperationState(value string) bool {
	switch value {
	case OperationSubmitted, OperationAccepted, OperationCompleted, OperationRejected, OperationUnknown:
		return true
	default:
		return false
	}
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "duplicated key")
}
