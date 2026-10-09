package connector

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// ErrIdempotencyMismatch rejects the same idempotency key with a different
	// request fingerprint.
	ErrIdempotencyMismatch = errors.New("connector: idempotency key reused with a different request fingerprint")
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

// RecordSnapshot inserts an identity for a direct read and appends a snapshot
// when evidence advances. Discovery never inserts an identity and writes
// nothing for an unknown one. Unchanged evidence does not write and does not
// refresh the stored observation time. List and history pages are not accepted
// here; there is no method that inserts from a page of search results.
func (s *Store) RecordSnapshot(ctx context.Context, obs Observation) (SnapshotResult, error) {
	prepared, terminal, digest, err := prepareObservation(obs)
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
		return s.advanceSnapshot(ctx, prepared, digest, terminal)
	}
	if err != nil {
		return SnapshotResult{}, err
	}
	if decision, stop := decideSnapshot(ident, prepared, digest, terminal); stop {
		return decision.result, decision.err
	}
	return s.advanceSnapshot(ctx, prepared, digest, terminal)
}

// RecordRelation stores typed evidence between two identities that already
// exist. It does not create identities. The same from, to, type, and evidence
// digest returns the original row. A different digest inserts another row.
// Both identities are marked referenced.
func (s *Store) RecordRelation(ctx context.Context, from, to ExecutionReference, relationType string, evidence []byte) (models.ExternalExecutionRelation, error) {
	if !validRelationType(relationType) {
		return models.ExternalExecutionRelation{}, ErrInvalidRelation
	}
	if err := boundJSON(evidence); err != nil {
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
// The identity must already exist. The same key and fingerprint returns the
// original row. A different fingerprint writes nothing. A second key for the
// same open connection, execution, and action returns the existing id with
// Conflict set and writes nothing. A new row marks the identity referenced.
func (s *Store) AdmitOperation(ctx context.Context, req OperationRequest) (OperationDecision, error) {
	ref, err := validateOperationRequest(req)
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
		if err := identityExists(tx, opaque); err != nil {
			return err
		}
		var owner models.ExternalExecution
		if err := tx.Select("connection_id").Where("opaque_id = ?", opaque).First(&owner).Error; err != nil {
			return err
		}
		if owner.ConnectionID != req.ConnectionID {
			return ErrInvalidOperation
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
			if stored.RequestFingerprint != req.RequestFingerprint {
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
				if stored.RequestFingerprint != req.RequestFingerprint {
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

// EnforceRetention deletes unreferenced snapshots for connectionID older than
// maxAge, then evicts the oldest unreferenced snapshots until that count is
// below maxCount. Identities, relations, operations, the epoch row, and
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
		return evictUnreferencedBelow(tx, connectionID, maxCount)
	})
}

type snapshotDecision struct {
	result SnapshotResult
	err    error
}

func prepareObservation(obs Observation) (Observation, bool, string, error) {
	ref, err := NewExecutionReference(obs.Execution.ConnectionID, obs.Execution.Coordinates)
	if err != nil {
		return Observation{}, false, "", err
	}
	obs.Execution = ref
	terminal, ok := terminalDisplay(obs.DisplayStatus)
	if !ok || !validAvailability(obs.Availability) || !validCompleteness(obs.Completeness) || !validSourceKind(obs.SourceKind) {
		return Observation{}, false, "", ErrInvalidObservation
	}
	if err := boundJSON(obs.Metadata); err != nil {
		return Observation{}, false, "", err
	}
	obs.ObservedAt = obs.ObservedAt.UTC()
	return obs, terminal, evidenceDigest(obs, terminal), nil
}

func decideSnapshot(ident models.ExternalExecution, obs Observation, digest string, terminal bool) (snapshotDecision, bool) {
	if ident.LatestGeneration == nil {
		return snapshotDecision{}, false
	}
	if ident.LatestEvidenceDigest == digest {
		observed := time.Time{}
		if ident.LatestObservedAt != nil {
			observed = ident.LatestObservedAt.UTC()
		}
		return snapshotDecision{result: SnapshotResult{Wrote: false, ObservedAt: observed}}, true
	}
	latest := *ident.LatestGeneration
	if !terminal && ident.LatestTerminal {
		return snapshotDecision{err: ErrTerminalRegression}, true
	}
	if obs.SourceKind == SourceDiscovery && ident.LatestSourceKind == SourceDirect && obs.Generation <= latest {
		return snapshotDecision{err: ErrDiscoverySuperseded}, true
	}
	if obs.Generation < latest || obs.Generation == latest {
		return snapshotDecision{err: ErrStaleGeneration}, true
	}
	return snapshotDecision{}, false
}

func (s *Store) advanceSnapshot(ctx context.Context, obs Observation, digest string, terminal bool) (SnapshotResult, error) {
	opaque := obs.Execution.OpaqueID()
	var result SnapshotResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if obs.SourceKind == SourceDirect {
			if err := ensureIdentity(tx, obs.Execution); err != nil {
				return err
			}
		}
		for range 8 {
			var ident models.ExternalExecution
			if err := tx.Where("opaque_id = ?", opaque).First(&ident).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
			if decision, stop := decideSnapshot(ident, obs, digest, terminal); stop {
				result = decision.result
				return decision.err
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
				continue
			}
			if err := evictUnreferencedBelow(tx, obs.Execution.ConnectionID, MaxUnreferencedSnapshots); err != nil {
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
		}
		return errors.New("connector: snapshot advance lost the compare-and-swap")
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

func validateOperationRequest(req OperationRequest) (ExecutionReference, error) {
	if strings.TrimSpace(req.ConnectionID) == "" ||
		strings.TrimSpace(req.IdempotencyKey) == "" ||
		strings.TrimSpace(req.Action) == "" ||
		strings.TrimSpace(req.RequestFingerprint) == "" ||
		strings.TrimSpace(req.BindingVersion) == "" ||
		strings.TrimSpace(req.ExternalUpdateID) == "" {
		return ExecutionReference{}, ErrInvalidOperation
	}
	if err := boundJSON(req.Actor); err != nil {
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

func evictUnreferencedBelow(tx *gorm.DB, connectionID string, maxCount int) error {
	for {
		count, err := countUnreferenced(tx, connectionID)
		if err != nil {
			return err
		}
		if count < int64(maxCount) {
			return nil
		}
		id, ok, err := oldestUnreferenced(tx, connectionID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("connector: unreferenced snapshot count did not match a row")
		}
		res := tx.Exec(`DELETE FROM external_execution_snapshots WHERE id = ?`, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("connector: retention eviction made no progress")
		}
	}
}

func deleteUnreferencedOlderThan(tx *gorm.DB, connectionID string, cutoff time.Time) error {
	// Compare instants in Go. SQLite stores the driver's timestamp text, and a
	// SQL inequality on that text is not the retention rule.
	type aged struct {
		ID         string    `gorm:"column:id"`
		ObservedAt time.Time `gorm:"column:observed_at"`
	}
	var rows []aged
	err := tx.Raw(`
		SELECT s.id AS id, s.observed_at AS observed_at
		FROM external_execution_snapshots AS s
		INNER JOIN external_executions AS e ON e.opaque_id = s.opaque_id
		WHERE s.connection_id = ? AND e.referenced = ?
	`, connectionID, false).Scan(&rows).Error
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !row.ObservedAt.Before(cutoff) {
			continue
		}
		if err := tx.Exec(`DELETE FROM external_execution_snapshots WHERE id = ?`, row.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

func countUnreferenced(tx *gorm.DB, connectionID string) (int64, error) {
	var row struct {
		N int64 `gorm:"column:n"`
	}
	err := tx.Raw(`
		SELECT COUNT(*) AS n
		FROM external_execution_snapshots AS s
		INNER JOIN external_executions AS e ON e.opaque_id = s.opaque_id
		WHERE s.connection_id = ? AND e.referenced = ?
	`, connectionID, false).Scan(&row).Error
	return row.N, err
}

func oldestUnreferenced(tx *gorm.DB, connectionID string) (string, bool, error) {
	type aged struct {
		ID         string    `gorm:"column:id"`
		ObservedAt time.Time `gorm:"column:observed_at"`
	}
	var rows []aged
	err := tx.Raw(`
		SELECT s.id AS id, s.observed_at AS observed_at
		FROM external_execution_snapshots AS s
		INNER JOIN external_executions AS e ON e.opaque_id = s.opaque_id
		WHERE s.connection_id = ? AND e.referenced = ?
	`, connectionID, false).Scan(&rows).Error
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	oldest := rows[0]
	for _, row := range rows[1:] {
		if row.ObservedAt.Before(oldest.ObservedAt) || (row.ObservedAt.Equal(oldest.ObservedAt) && row.ID < oldest.ID) {
			oldest = row
		}
	}
	if oldest.ID == "" {
		return "", false, nil
	}
	return oldest.ID, true, nil
}

func boundJSON(payload []byte) error {
	if len(payload) > MaxPageMetadataBytes {
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
		[]byte(obs.SourceKind),
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
