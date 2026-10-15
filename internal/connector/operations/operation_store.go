package operations

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Store owns the injected catalog DB handle for optional operation evidence and
// confirmation transactions. Core admission/epoch policy stays in connector.Store.
// Importing the connector core never initializes or imports this service.
type Store struct {
	db      *gorm.DB
	catalog *connector.Store
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db, catalog: connector.NewStore(db)} }

// ErrOperationChanged means evidence lost a revision/attempt compare-and-swap.
var ErrOperationChanged = errors.New("connector: operation changed concurrently")

func operationTerminal(state string) bool {
	return state == connector.OperationCompleted || state == connector.OperationRejected || state == connector.OperationNotDispatched
}

// Operation returns a durable receipt without making a provider call.
func (s *Store) Operation(ctx context.Context, id uuid.UUID) (models.ConnectorOperation, error) {
	var op models.ConnectorOperation
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&op).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = connector.ErrNotFound
	}
	return op, err
}

// Operations lists bounded receipt metadata for one already known identity.
// It does not reconcile or dispatch any operation.
func (s *Store) Operations(ctx context.Context, execution connector.ExecutionReference, limit int) ([]models.ConnectorOperation, error) {
	if limit < 1 || limit > connector.MaxPageEntries {
		return nil, connector.ErrRetentionLimit
	}
	ref, err := connector.NewExecutionReference(execution.ConnectionID, execution.Coordinates)
	if err != nil {
		return nil, err
	}
	var rows []models.ConnectorOperation
	err = s.db.WithContext(ctx).Where("connection_id = ? AND opaque_id = ?", ref.ConnectionID, ref.OpaqueID()).Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// recordOperationEvidence never overwrites a newer lookup, dispatch attempt or
// terminal result. A stale response returns the current receipt for recovery.
func (s *Store) recordOperationEvidence(ctx context.Context, before models.ConnectorOperation, state string, evidence []byte) (models.ConnectorOperation, error) {
	if operationTerminal(before.State) {
		return before, nil
	}
	updates := map[string]any{"state": state, "provider_evidence": jsonValue(evidence), "revision": before.Revision + 1, "updated_at": time.Now().UTC()}
	if operationTerminal(state) {
		updates["open_key"] = gorm.Expr("NULL")
	}
	res := s.db.WithContext(ctx).Model(&models.ConnectorOperation{}).Where("id = ? AND revision = ? AND attempt = ? AND state = ?", before.ID, before.Revision, before.Attempt, before.State).Updates(updates)
	if res.Error != nil {
		return models.ConnectorOperation{}, res.Error
	}
	op, err := s.Operation(ctx, before.ID)
	if err != nil {
		return op, err
	}
	if res.RowsAffected != 1 {
		return op, ErrOperationChanged
	}
	return op, nil
}

type resendDecision struct {
	At                 time.Time               `json:"at"`
	PrincipalKind      connector.PrincipalKind `json:"principal_kind"`
	StableID           string                  `json:"stable_id"`
	Subject            string                  `json:"subject"`
	Role               string                  `json:"role"`
	Attempt            uint64                  `json:"attempt"`
	ConfigurationEpoch string                  `json:"configuration_epoch"`
}

func (s *Store) confirmOperationResend(ctx context.Context, before models.ConnectorOperation, actor connector.ActorEnvelope, serviceEpoch string, maxBytes int) (models.ConnectorOperation, error) {
	var out models.ConnectorOperation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		active, err := connector.NewStore(tx).ActiveEpoch(ctx)
		if errors.Is(err, connector.ErrNotFound) {
			return connector.ErrEpochMismatch
		}
		if err != nil {
			return err
		}
		if serviceEpoch == "" || active != serviceEpoch {
			return connector.ErrEpochMismatch
		}
		var audit []resendDecision
		if len(before.ResendAudit) > 0 && string(before.ResendAudit) != "null" {
			if err := json.Unmarshal(before.ResendAudit, &audit); err != nil {
				return connector.ErrInvalidOperation
			}
		}
		// Never discard old operator decisions to make room for a new one.
		if len(audit) >= 16 {
			return connector.ErrRetentionLimit
		}
		audit = append(audit, resendDecision{At: time.Now().UTC(), PrincipalKind: actor.PrincipalKind, StableID: actor.StableID, Subject: actor.Subject, Role: actor.Role, Attempt: before.Attempt + 1, ConfigurationEpoch: serviceEpoch})
		raw, err := json.Marshal(audit)
		if err != nil {
			return err
		}
		if err := boundJSON(raw, maxBytes); err != nil {
			return err
		}
		res := tx.Model(&models.ConnectorOperation{}).Where("id = ? AND state = ? AND revision = ? AND attempt = ?", before.ID, connector.OperationUnknown, before.Revision, before.Attempt).Updates(map[string]any{"state": connector.OperationSubmitted, "revision": before.Revision + 1, "attempt": before.Attempt + 1, "resend_audit": jsonValue(raw), "updated_at": time.Now().UTC()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrOperationChanged
		}
		return tx.Where("id = ?", before.ID).First(&out).Error
	})
	return out, err
}

func boundJSON(payload []byte, maxBytes int) error {
	if len(payload) > maxBytes {
		return connector.ErrPayloadTooLarge
	}
	if len(payload) > 0 && !json.Valid(payload) {
		return connector.ErrInvalidPayload
	}
	return nil
}

func jsonValue(payload []byte) datatypes.JSON {
	if len(payload) == 0 {
		return nil
	}
	return datatypes.JSON(append([]byte(nil), payload...))
}
