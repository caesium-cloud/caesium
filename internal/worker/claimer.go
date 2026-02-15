package worker

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/mattn/go-sqlite3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const defaultLeaseTTL = 5 * time.Minute

type Claimer struct {
	nodeID     string
	nodeLabels map[string]string
	store      *run.Store
	leaseTTL   time.Duration
}

func NewClaimer(nodeID string, store *run.Store, leaseTTL time.Duration, nodeLabels ...map[string]string) *Claimer {
	if store == nil {
		panic("worker claimer requires run store")
	}
	if strings.TrimSpace(nodeID) == "" {
		nodeID = "unknown-node"
	}
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}

	labels := map[string]string{}
	if len(nodeLabels) > 0 {
		labels = maps.Clone(nodeLabels[0])
		if labels == nil {
			labels = map[string]string{}
		}
	}

	return &Claimer{
		nodeID:     nodeID,
		nodeLabels: labels,
		store:      store,
		leaseTTL:   leaseTTL,
	}
}

// ClaimNext claims one ready task, or returns nil when no tasks are available.
func (c *Claimer) ClaimNext(ctx context.Context) (*models.TaskRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	leaseExpiry := now.Add(c.leaseTTL)
	var claimed *models.TaskRun

	err := c.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var candidates []models.TaskRun
		err := tx.
			Where(
				"status = ? AND outstanding_predecessors = ? AND (claimed_by = '' OR claim_expires_at IS NULL OR claim_expires_at < ?)",
				string(run.TaskStatusPending),
				0,
				now,
			).
			Order("created_at ASC").
			Limit(64).
			Find(&candidates).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}

		for _, candidate := range candidates {
			if !matchesNodeSelector(candidate.NodeSelector, c.nodeLabels) {
				continue
			}

			result := tx.Model(&models.TaskRun{}).
				Where(
					"id = ? AND status = ? AND outstanding_predecessors = ? AND (claimed_by = '' OR claim_expires_at IS NULL OR claim_expires_at < ?)",
					candidate.ID,
					string(run.TaskStatusPending),
					0,
					now,
				).
				Updates(map[string]interface{}{
					"claimed_by":       c.nodeID,
					"claim_expires_at": leaseExpiry,
					"claim_attempt":    candidate.ClaimAttempt + 1,
					"status":           string(run.TaskStatusRunning),
				})
			if result.Error != nil {
				if isClaimContentionErr(result.Error) {
					metrics.WorkerClaimContentionTotal.WithLabelValues(c.nodeID).Inc()
				}
				return result.Error
			}
			if result.RowsAffected == 0 {
				// Another node won the race.
				metrics.WorkerClaimContentionTotal.WithLabelValues(c.nodeID).Inc()
				continue
			}

			claimedTask := &models.TaskRun{}
			if err := tx.First(claimedTask, "id = ?", candidate.ID).Error; err != nil {
				return err
			}
			claimed = claimedTask
			break
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if claimed != nil {
		metrics.WorkerClaimsTotal.WithLabelValues(c.nodeID).Inc()
	}

	return claimed, nil
}

func (c *Claimer) ReclaimExpired(ctx context.Context) error {
	now := time.Now().UTC()
	result := c.store.DB().WithContext(ctx).
		Model(&models.TaskRun{}).
		Where("status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ?", string(run.TaskStatusRunning), now).
		Updates(map[string]interface{}{
			"status":           string(run.TaskStatusPending),
			"claimed_by":       "",
			"claim_expires_at": nil,
			"runtime_id":       "",
			"started_at":       nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		metrics.WorkerLeaseExpirationsTotal.WithLabelValues(c.nodeID).Add(float64(result.RowsAffected))
	}
	return nil
}

func isClaimContentionErr(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked
	}
	return false
}

func ParseNodeLabels(raw string) map[string]string {
	values := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" || value == "" {
			continue
		}
		values[key] = value
	}
	return values
}

func matchesNodeSelector(selector datatypes.JSONMap, nodeLabels map[string]string) bool {
	if len(selector) == 0 {
		return true
	}

	for key, raw := range jsonmap.ToStringMap(selector) {
		expected := strings.TrimSpace(raw)
		if expected == "" {
			continue
		}

		actual, ok := nodeLabels[key]
		if !ok {
			return false
		}
		if actual != expected {
			return false
		}
	}
	return true
}
