package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"gorm.io/gorm"
)

const defaultLeaseTTL = 5 * time.Minute

type Claimer struct {
	nodeID   string
	store    *run.Store
	leaseTTL time.Duration
}

func NewClaimer(nodeID string, store *run.Store, leaseTTL time.Duration) *Claimer {
	if store == nil {
		panic("worker claimer requires run store")
	}
	if strings.TrimSpace(nodeID) == "" {
		nodeID = "unknown-node"
	}
	if leaseTTL <= 0 {
		leaseTTL = defaultLeaseTTL
	}

	return &Claimer{
		nodeID:   nodeID,
		store:    store,
		leaseTTL: leaseTTL,
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
		var candidate models.TaskRun
		err := tx.
			Where(
				"status = ? AND outstanding_predecessors = ? AND (claimed_by = '' OR claim_expires_at IS NULL OR claim_expires_at < ?)",
				string(run.TaskStatusPending),
				0,
				now,
			).
			Order("created_at ASC").
			First(&candidate).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
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
			return result.Error
		}
		if result.RowsAffected == 0 {
			// Another node won the race.
			return nil
		}

		claimedTask := &models.TaskRun{}
		if err := tx.First(claimedTask, "id = ?", candidate.ID).Error; err != nil {
			return err
		}
		claimed = claimedTask
		return nil
	})
	if err != nil {
		return nil, err
	}

	return claimed, nil
}

func (c *Claimer) ReclaimExpired(ctx context.Context) error {
	now := time.Now().UTC()
	return c.store.DB().WithContext(ctx).
		Model(&models.TaskRun{}).
		Where("status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ?", string(run.TaskStatusRunning), now).
		Updates(map[string]interface{}{
			"status":           string(run.TaskStatusPending),
			"claimed_by":       "",
			"claim_expires_at": nil,
			"runtime_id":       "",
			"started_at":       nil,
		}).Error
}
