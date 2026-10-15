package connector

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Store) recordCorrelation(ctx context.Context, version string, source ActivityEvidence, admission run.StartAdmission, at time.Time, bounds ...WriteBounds) (models.ExternalExecutionCorrelation, error) {
	limits, err := resolveWriteBounds(bounds...)
	if err != nil {
		return models.ExternalExecutionCorrelation{}, err
	}
	evidence, err := json.Marshal(source)
	if err != nil {
		return models.ExternalExecutionCorrelation{}, err
	}
	if err := boundJSON(evidence, limits.MaxMetadataBytes); err != nil {
		return models.ExternalExecutionCorrelation{}, err
	}
	row := models.ExternalExecutionCorrelation{ID: uuid.New(), OpaqueID: source.Execution.OpaqueID(), ConnectionID: source.Execution.ConnectionID,
		ActivityID: source.ActivityID, ActivityType: source.ActivityType, RelationType: RelationDelegation, BindingVersion: version,
		AdmissionID: admission.ID, JobID: admission.JobID, IdempotencyKey: admission.IdempotencyKey, Outcome: string(admission.Outcome),
		QueueID: admission.QueueID, RunID: admission.RunID, SourceEventID: source.SourceEventID,
		Evidence: jsonValue(evidence), EvidenceDigest: digestBytes(evidence), VerifiedAt: at, CreatedAt: at, UpdatedAt: at}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
			return ErrStaleGeneration
		}
		var previous ActivityEvidence
		if json.Unmarshal(stored.Evidence, &previous) != nil {
			return ErrUnverifiedCorrelation
		}
		// Describe and history can complete out of order. A delayed read must
		// not erase a newer reported retry attempt, even at the same event ID.
		if source.Attempt < previous.Attempt {
			return ErrStaleGeneration
		}

		if stored.Outcome != row.Outcome && stored.Outcome != string(run.StartOutcomeQueued) {
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
				return ErrSnapshotConflict
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
func (s *Store) Correlations(ctx context.Context, ref ExecutionReference) ([]models.ExternalExecutionCorrelation, error) {
	validated, err := NewExecutionReference(ref.ConnectionID, ref.Coordinates)
	if err != nil {
		return nil, err
	}
	var rows []models.ExternalExecutionCorrelation
	err = s.db.WithContext(ctx).Where("opaque_id = ?", validated.OpaqueID()).Order("activity_id").Limit(MaxPageEntries + 1).Find(&rows).Error
	if err == nil && len(rows) > MaxPageEntries {
		return nil, ErrPayloadTooLarge
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
	err := s.db.WithContext(ctx).Where("job_id = ? AND run_id = ?", jobID, runID).Order("opaque_id, activity_id").Limit(MaxPageEntries + 1).Find(&rows).Error
	if err == nil && len(rows) > MaxPageEntries {
		return nil, ErrPayloadTooLarge
	}
	return rows, err
}
