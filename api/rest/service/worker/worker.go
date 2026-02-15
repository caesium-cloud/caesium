package worker

import (
	"context"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"gorm.io/gorm"
)

type Service interface {
	WithDatabase(*gorm.DB) Service
	Status(string) (*StatusResponse, error)
}

type service struct {
	ctx context.Context
	db  *gorm.DB
}

func New(ctx context.Context) Service {
	return &service{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (s *service) WithDatabase(conn *gorm.DB) Service {
	if conn == nil {
		return s
	}
	s.db = conn
	return s
}

type StatusResponse struct {
	Address            string             `json:"address"`
	ObservedAt         time.Time          `json:"observed_at"`
	TotalClaimedTasks  int64              `json:"total_claimed_tasks"`
	ClaimedByStatus    map[string]int64   `json:"claimed_by_status"`
	RunningClaims      int64              `json:"running_claims"`
	ExpiredLeases      int64              `json:"expired_leases"`
	TotalClaimAttempts int64              `json:"total_claim_attempts"`
	LastActivityAt     *time.Time         `json:"last_activity_at,omitempty"`
	ActiveClaims       []ActiveClaimEntry `json:"active_claims"`
}

type ActiveClaimEntry struct {
	JobRunID       string     `json:"job_run_id"`
	TaskID         string     `json:"task_id"`
	Status         string     `json:"status"`
	ClaimAttempt   int        `json:"claim_attempt"`
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (s *service) Status(address string) (*StatusResponse, error) {
	address = strings.TrimSpace(address)
	now := time.Now().UTC()

	resp := &StatusResponse{
		Address:         address,
		ObservedAt:      now,
		ClaimedByStatus: map[string]int64{},
		ActiveClaims:    []ActiveClaimEntry{},
	}

	if address == "" {
		return resp, nil
	}

	type countByStatus struct {
		Status string
		Count  int64
	}

	var grouped []countByStatus
	if err := s.db.WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Select("status, COUNT(*) as count").
		Where("claimed_by = ?", address).
		Group("status").
		Scan(&grouped).Error; err != nil {
		return nil, err
	}

	for _, row := range grouped {
		resp.ClaimedByStatus[row.Status] = row.Count
		resp.TotalClaimedTasks += row.Count
	}

	runningStatus := string(run.TaskStatusRunning)
	resp.RunningClaims = resp.ClaimedByStatus[runningStatus]

	type aggregatedStats struct {
		TotalClaimAttempts int64      `gorm:"column:total_claim_attempts"`
		LastActivityAt     *time.Time `gorm:"column:last_activity_at"`
		ExpiredLeases      int64      `gorm:"column:expired_leases"`
	}
	var stats aggregatedStats
	if err := s.db.WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Select(
			`COALESCE(SUM(claim_attempt), 0) AS total_claim_attempts,
			MAX(updated_at) AS last_activity_at,
			SUM(CASE WHEN status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ? THEN 1 ELSE 0 END) AS expired_leases`,
			runningStatus,
			now,
		).
		Where("claimed_by = ?", address).
		Take(&stats).Error; err != nil {
		return nil, err
	}
	resp.ExpiredLeases = stats.ExpiredLeases
	resp.TotalClaimAttempts = stats.TotalClaimAttempts
	resp.LastActivityAt = stats.LastActivityAt

	var active []models.TaskRun
	if err := s.db.WithContext(s.ctx).
		Select("job_run_id, task_id, status, claim_attempt, claim_expires_at, updated_at").
		Where("claimed_by = ? AND status = ?", address, runningStatus).
		Order("updated_at DESC").
		Limit(50).
		Find(&active).Error; err != nil {
		return nil, err
	}

	resp.ActiveClaims = make([]ActiveClaimEntry, 0, len(active))
	for _, claim := range active {
		resp.ActiveClaims = append(resp.ActiveClaims, ActiveClaimEntry{
			JobRunID:       claim.JobRunID.String(),
			TaskID:         claim.TaskID.String(),
			Status:         claim.Status,
			ClaimAttempt:   claim.ClaimAttempt,
			ClaimExpiresAt: claim.ClaimExpiresAt,
			UpdatedAt:      claim.UpdatedAt,
		})
	}

	return resp, nil
}
