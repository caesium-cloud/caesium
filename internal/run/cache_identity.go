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
	return s.hasUnresolvedPredecessorImage(runID, taskID, make(map[uuid.UUID]bool))
}

func (s *Store) hasUnresolvedPredecessorImage(runID, taskID uuid.UUID, visited map[uuid.UUID]bool) (bool, error) {
	if visited[taskID] {
		return false, nil
	}
	visited[taskID] = true
	refs, err := s.resolvePredecessorsTx(s.db, runID, taskID)
	if err != nil {
		return false, err
	}
	if len(refs) == 0 {
		return false, nil
	}
	var rows []models.TaskRun
	if err := s.db.Select("task_id", "status", "hash_input_blob", "cache_chain", "cache_enabled", "cache_pin_digests", "resolved_image_digest").Where("job_run_id = ? AND task_id IN ? AND status IN ?", runID, predecessorTaskIDs(refs), []string{string(TaskStatusSucceeded), string(TaskStatusCached), string(TaskStatusFailed), string(TaskStatusSkipped)}).Find(&rows).Error; err != nil {
		return false, err
	}
	for i := range rows {
		row := &rows[i]
		// A skipped step did not execute its own image. Its absent identity must
		// neither prove its ancestors verified nor invent uncertainty of its own.
		if row.Status != string(TaskStatusSkipped) && row.CacheEnabled && row.CachePinDigests && row.ResolvedImageDigest == "" {
			return true, nil
		}
		if len(row.HashInputBlob) != 0 {
			var blob cache.HashInputBlob
			if err := json.Unmarshal(row.HashInputBlob, &blob); err != nil {
				return false, fmt.Errorf("decode predecessor image identity: %w", err)
			}
			if blob.UnresolvedImageIdentity != "" {
				return true, nil
			}
		}
		// Skips have no computed hash. An identity-write failure can also leave a
		// failed step without its blob. Look through those gaps, stopping at the
		// same explicit values boundary an executed intermediary would honor.
		if row.CacheChain != cache.ChainValues && (row.Status == string(TaskStatusSkipped) || (row.Status == string(TaskStatusFailed) && len(row.HashInputBlob) == 0)) {
			unknown, err := s.hasUnresolvedPredecessorImage(runID, row.TaskID, visited)
			if unknown || err != nil {
				return unknown, err
			}
		}
	}
	return false, nil
}
