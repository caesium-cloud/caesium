package worker

import (
	"context"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"gorm.io/gorm"
)

const taskStatusRunning = "running"

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

	resp.RunningClaims = resp.ClaimedByStatus[taskStatusRunning]

	if err := s.db.WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Where(
			"claimed_by = ? AND status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ?",
			address,
			taskStatusRunning,
			now,
		).
		Count(&resp.ExpiredLeases).Error; err != nil {
		return nil, err
	}

	var attempts struct {
		Total int64
	}
	if err := s.db.WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Select("COALESCE(SUM(claim_attempt), 0) as total").
		Where("claimed_by = ?", address).
		Scan(&attempts).Error; err != nil {
		return nil, err
	}
	resp.TotalClaimAttempts = attempts.Total

	var lastActivity struct {
		Last *time.Time
	}
	if err := s.db.WithContext(s.ctx).
		Model(&models.TaskRun{}).
		Select("MAX(updated_at) as last").
		Where("claimed_by = ?", address).
		Scan(&lastActivity).Error; err != nil {
		return nil, err
	}
	resp.LastActivityAt = lastActivity.Last

	var active []models.TaskRun
	if err := s.db.WithContext(s.ctx).
		Select("job_run_id, task_id, status, claim_attempt, claim_expires_at, updated_at").
		Where("claimed_by = ? AND status = ?", address, taskStatusRunning).
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
