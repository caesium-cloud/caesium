package run

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// PredecessorInputs captures the distinct output, cache and descriptor
// projections from one successful execution-input snapshot.
type PredecessorInputs struct {
	OutputsByName     map[string]map[string]string
	Hashes            []string
	DescriptorOutputs map[uuid.UUID]map[string]string
	DescriptorHashes  map[uuid.UUID]string
}

// PredecessorExecutionInputs acquires all predecessor inputs in one transaction.
// An empty graph or absent output is valid; acquisition or decoding failures
// return no partial inputs. Legacy status/trigger readers remain best-effort.
func (s *Store) PredecessorExecutionInputs(ctx context.Context, runID, taskID uuid.UUID) (PredecessorInputs, error) {
	var result PredecessorInputs
	var options []*sql.TxOptions
	if s.db.Dialector.Name() == "postgres" {
		// READ COMMITTED would let the graph/name and output queries observe
		// different commits. SQLite/dqlite already keep one read view per txn.
		options = []*sql.TxOptions{{Isolation: sql.LevelRepeatableRead, ReadOnly: true}}
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		refs, err := s.resolvePredecessorsWithPolicyTx(tx, runID, taskID, true)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return nil
		}
		rows, err := predecessorTaskRunsTx(tx, runID, refs,
			"task_id", "id", "output", "hash", "effective_hash", "status",
			"partition_value", "partition_index", "partition_count")
		if err != nil {
			return fmt.Errorf("load predecessor execution rows: %w", err)
		}
		byTask := groupTaskRunsByTaskID(rows)
		result.OutputsByName = make(map[string]map[string]string, len(refs))
		result.DescriptorOutputs = make(map[uuid.UUID]map[string]string, len(byTask))
		result.DescriptorHashes = make(map[uuid.UUID]string, len(byTask))
		for _, ref := range refs {
			group := byTask[ref.TaskID]
			output, ok, err := predecessorGroupOutputWithPolicy(ref.Name, group, true)
			if err != nil {
				return err
			}
			if ok && len(output) > 0 {
				result.OutputsByName[ref.Name] = output
				result.DescriptorOutputs[ref.TaskID] = output
			}
			successes := make([]models.TaskRun, 0, len(group))
			for i := range group {
				if IsTerminalSuccess(TaskStatus(group[i].Status)) {
					successes = append(successes, group[i])
				}
			}
			if hash := predecessorGroupHash(successes); hash != "" {
				result.DescriptorHashes[ref.TaskID] = hash
			}
		}
		// Cache identity intentionally excludes rows with an empty original hash,
		// even if their effective hash is present; descriptor identity does not.
		cacheRows := make([]models.TaskRun, 0, len(rows))
		for i := range rows {
			if IsTerminalSuccess(TaskStatus(rows[i].Status)) && rows[i].Hash != "" {
				cacheRows = append(cacheRows, rows[i])
			}
		}
		result.Hashes = predecessorHashList(cacheRows)
		if len(result.OutputsByName) == 0 {
			result.OutputsByName = nil
		}
		return ctx.Err()
	}, options...)
	if err != nil {
		return PredecessorInputs{}, err
	}
	if err := ctx.Err(); err != nil {
		return PredecessorInputs{}, err
	}
	return result, nil
}
