package run

import (
	"encoding/json"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// HasUnresolvedPredecessorImage reports uncertainty from any predecessor
// instance, including a fanned group. Callers propagate it only in transitive
// chain mode. Reading errors must bypass cache rather than imply verification.
func (s *Store) HasUnresolvedPredecessorImage(runID, taskID uuid.UUID) (bool, error) {
	refs, err := s.resolvePredecessorsTx(s.db, runID, taskID)
	if err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	var rows []models.TaskRun
	if err := s.db.Select("hash_input_blob", "cache_enabled", "cache_pin_digests", "resolved_image_digest").Where("job_run_id = ? AND task_id IN ? AND status IN ?", runID, predecessorTaskIDs(refs), []string{string(TaskStatusSucceeded), string(TaskStatusCached), string(TaskStatusFailed)}).Find(&rows).Error; err != nil {
		return false, err
	}
	for i := range rows {
		row := &rows[i]
		if row.CacheEnabled && row.CachePinDigests && row.ResolvedImageDigest == "" {
			return true, nil
		}
		if len(row.HashInputBlob) == 0 {
			continue
		}
		var blob cache.HashInputBlob
		if err := json.Unmarshal(row.HashInputBlob, &blob); err != nil {
			return false, fmt.Errorf("decode predecessor image identity: %w", err)
		}
		if blob.UnresolvedImageIdentity != "" {
			return true, nil
		}
	}
	return false, nil
}
