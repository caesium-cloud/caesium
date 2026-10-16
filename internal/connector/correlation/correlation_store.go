package correlation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runadmission"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store owns the catalog connection injected by the composition root. It stores
// D1's verified local-admission relations; shipped external snapshots and other
// catalog methods remain on connector.Store.
type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) recordCorrelation(ctx context.Context, version string, source ActivityEvidence, admission runadmission.StartAdmission, at time.Time, bounds ...connector.WriteBounds) (models.ExternalExecutionCorrelation, error) {
	limit, err := evidenceLimit(bounds...)
	if err != nil {
		return models.ExternalExecutionCorrelation{}, err
	}
	evidence, err := json.Marshal(source)
	if err != nil {
		return models.ExternalExecutionCorrelation{}, err
	}
	if err := boundJSON(evidence, limit); err != nil {
		return models.ExternalExecutionCorrelation{}, err
	}
	row := models.ExternalExecutionCorrelation{ID: uuid.New(), OpaqueID: source.Execution.OpaqueID(), ConnectionID: source.Execution.ConnectionID,
		ActivityID: source.ActivityID, ActivityType: source.ActivityType, RelationType: connector.RelationDelegation, BindingVersion: version,
		AdmissionID: admission.ID, JobID: admission.JobID, IdempotencyKey: admission.IdempotencyKey, Outcome: string(admission.Outcome),
		QueueID: admission.QueueID, RunID: admission.RunID, SourceEventID: source.SourceEventID,
		Evidence: datatypes.JSON(evidence), EvidenceDigest: digestBytes(evidence), VerifiedAt: at, CreatedAt: at, UpdatedAt: at}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(bounds) > 0 && bounds[0].ExpectedEpoch != "" {
			if err := connector.NewStore(tx).RequireEpoch(ctx, bounds[0].ExpectedEpoch); err != nil {
				return err
			}
		}
		if err := identityExists(tx, row.OpaqueID); err != nil {
			return err
		}
		var stored models.ExternalExecutionCorrelation
		findErr := tx.Where("opaque_id = ? AND activity_id = ?", row.OpaqueID, row.ActivityID).Take(&stored).Error
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			inserted := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "opaque_id"}, {Name: "activity_id"}}, DoNothing: true}).Create(&row)
			if inserted.Error != nil {
				return inserted.Error
			}
			if inserted.RowsAffected == 1 {
				return tx.Model(&models.ExternalExecution{}).Where("opaque_id = ? AND referenced = ?", row.OpaqueID, false).Update("referenced", true).Error
			}
			findErr = tx.Where("opaque_id = ? AND activity_id = ?", row.OpaqueID, row.ActivityID).Take(&stored).Error
		}
		if findErr != nil {
			return findErr
		}
		if stored.AdmissionID != row.AdmissionID || stored.JobID != row.JobID || stored.ActivityType != row.ActivityType || stored.IdempotencyKey != row.IdempotencyKey ||
			stored.QueueID != nil && !sameUUID(stored.QueueID, row.QueueID) || stored.RunID != nil && !sameUUID(stored.RunID, row.RunID) {
			return ErrUnverifiedCorrelation
		}
		if stored.SourceEventID > row.SourceEventID {
			return connector.ErrStaleGeneration
		}
		var previous ActivityEvidence
		if json.Unmarshal(stored.Evidence, &previous) != nil {
			return ErrUnverifiedCorrelation
		}
		// Describe and history can complete out of order. A delayed read must
		// not erase a newer reported retry attempt, even at the same event ID.
		if source.Attempt < previous.Attempt {
			return connector.ErrStaleGeneration
		}

		if stored.Outcome != row.Outcome && stored.Outcome != string(runadmission.StartOutcomeQueued) {
			return ErrUnverifiedCorrelation
		}
		unchanged := stored.EvidenceDigest == row.EvidenceDigest && stored.Outcome == row.Outcome && sameUUID(stored.RunID, row.RunID) && sameUUID(stored.QueueID, row.QueueID) && stored.BindingVersion == row.BindingVersion
		if !unchanged {
			result := tx.Model(&models.ExternalExecutionCorrelation{}).Where("id = ? AND evidence_digest = ? AND outcome = ?", stored.ID, stored.EvidenceDigest, stored.Outcome).
				Updates(map[string]any{"binding_version": row.BindingVersion, "outcome": row.Outcome, "queue_id": row.QueueID, "run_id": row.RunID, "source_event_id": row.SourceEventID,
					"evidence": row.Evidence, "evidence_digest": row.EvidenceDigest, "verified_at": at, "updated_at": at})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return connector.ErrSnapshotConflict
			}
			row.ID = stored.ID
			row.CreatedAt = stored.CreatedAt
		} else {
			row = stored
			return nil
		}
		// Do not issue a write on unchanged polling of an already referenced identity.
		return tx.Model(&models.ExternalExecution{}).Where("opaque_id = ? AND referenced = ?", row.OpaqueID, false).Update("referenced", true).Error
	})
	return row, err
}

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Correlations returns only verified typed local links for one inspected
// execution. It is bounded and creates no identity or snapshot.
func (s *Store) Correlations(ctx context.Context, ref connector.ExecutionReference) ([]models.ExternalExecutionCorrelation, error) {
	validated, err := connector.NewExecutionReference(ref.ConnectionID, ref.Coordinates)
	if err != nil {
		return nil, err
	}
	var rows []models.ExternalExecutionCorrelation
	err = s.db.WithContext(ctx).Where("opaque_id = ?", validated.OpaqueID()).Order("activity_id").Limit(connector.MaxPageEntries + 1).Find(&rows).Error
	if err == nil && len(rows) > connector.MaxPageEntries {
		return nil, connector.ErrPayloadTooLarge
	}
	return rows, err
}

// RunCorrelations is the scope-neutral catalog lookup used after the caller
// authorizes access to this local job/run. External details require separate
// connector authorization at the public surface.
func (s *Store) RunCorrelations(ctx context.Context, jobID, runID uuid.UUID) ([]models.ExternalExecutionCorrelation, error) {
	if jobID == uuid.Nil || runID == uuid.Nil {
		return nil, errors.New("connector: job and run ids are required")
	}
	var rows []models.ExternalExecutionCorrelation
	err := s.db.WithContext(ctx).Where("job_id = ? AND run_id = ?", jobID, runID).Order("opaque_id, activity_id").Limit(connector.MaxPageEntries + 1).Find(&rows).Error
	if err == nil && len(rows) > connector.MaxPageEntries {
		return nil, connector.ErrPayloadTooLarge
	}
	return rows, err
}

// These small repository helpers keep D1 independent of core Store internals.
func evidenceLimit(bounds ...connector.WriteBounds) (int, error) {
	limit := connector.MaxPageMetadataBytes
	if len(bounds) == 0 {
		return limit, nil
	}
	in := bounds[0]
	if in.MaxMetadataBytes != 0 {
		if in.MaxMetadataBytes < 1 || in.MaxMetadataBytes > connector.MaxPageMetadataBytes {
			return 0, connector.ErrRetentionLimit
		}
		limit = in.MaxMetadataBytes
	}
	if in.MaxUnreferencedSnapshots < 0 || in.MaxUnreferencedSnapshots > connector.MaxUnreferencedSnapshots {
		return 0, connector.ErrRetentionLimit
	}
	return limit, nil
}
func boundJSON(raw []byte, limit int) error {
	if len(raw) > limit {
		return connector.ErrPayloadTooLarge
	}
	if len(raw) > 0 && !json.Valid(raw) {
		return connector.ErrInvalidPayload
	}
	return nil
}
func digestBytes(raw []byte) string {
	hash := sha256.New()
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], uint64(len(raw)))
	_, _ = hash.Write(prefix[:])
	_, _ = hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil))
}
func identityExists(tx *gorm.DB, opaque string) error {
	var count int64
	if err := tx.Model(&models.ExternalExecution{}).Where("opaque_id = ?", opaque).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return connector.ErrNotFound
	}
	return nil
}
