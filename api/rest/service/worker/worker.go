package worker

import (
	"context"
	"strconv"
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
	}
}

func (s *service) WithDatabase(conn *gorm.DB) Service {
	if conn == nil {
		return s
	}
	s.db = conn
	return s
}

func (s *service) connection() *gorm.DB {
	if s.db == nil {
		s.db = db.Connection()
	}
	return s.db
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
	if err := s.connection().WithContext(s.ctx).
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

	stats := map[string]any{}
	if err := s.connection().WithContext(s.ctx).
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
	resp.TotalClaimAttempts = normalizeAggregateInt(stats["total_claim_attempts"])
	resp.ExpiredLeases = normalizeAggregateInt(stats["expired_leases"])
	resp.LastActivityAt = normalizeAggregateTime(stats["last_activity_at"])

	var active []models.TaskRun
	if err := s.connection().WithContext(s.ctx).
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

func normalizeAggregateTime(v any) *time.Time {
	if v == nil {
		return nil
	}

	switch t := v.(type) {
	case time.Time:
		tt := t.UTC()
		return &tt
	case *time.Time:
		if t == nil {
			return nil
		}
		tt := t.UTC()
		return &tt
	case []byte:
		return parseAggregateTime(string(t))
	case string:
		return parseAggregateTime(t)
	default:
		return nil
	}
}

func normalizeAggregateInt(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return int64(t)
	case int8:
		return int64(t)
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case uint:
		return int64(t)
	case uint8:
		return int64(t)
	case uint16:
		return int64(t)
	case uint32:
		return int64(t)
	case uint64:
		return int64(t)
	case float32:
		return int64(t)
	case float64:
		return int64(t)
	case []byte:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return n
		}
		return 0
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n
		}
		return 0
	default:
		return 0
	}
}

func parseAggregateTime(raw string) *time.Time {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
	}

	for _, layout := range layouts {
		if ts, err := time.Parse(layout, raw); err == nil {
			tt := ts.UTC()
			return &tt
		}
		if ts, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			tt := ts.UTC()
			return &tt
		}
	}

	return nil
}
