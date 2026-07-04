// Package incident wraps read-side incident queries and the tier-3 approval
// decision flow for REST controllers (agent-in-the-loop D1/D2). It is a thin
// service over the incident store and the shipped GORM models; all state lives
// in the existing dqlite store.
package incident

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// defaultListLimit bounds an unfiltered list so the operator feed is always
// paginated. maxListLimit caps a caller-supplied limit.
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Service exposes read-side incident operations and the approval decision flow.
type Service struct {
	ctx context.Context
	db  *gorm.DB
}

// New creates a Service backed by the default DB connection.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

// WithDatabase returns a copy of the Service backed by conn; used by tests.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return &Service{ctx: s.ctx, db: conn}
}

// ListParams filters and paginates the incident feed.
type ListParams struct {
	Status        string
	Class         string
	JobID         *uuid.UUID
	NeedsApproval bool
	Limit         int
	Offset        int
}

// ListResult is the paginated incident feed response.
type ListResult struct {
	Incidents []models.Incident `json:"incidents"`
	Total     int64             `json:"total"`
	Limit     int               `json:"limit"`
	Offset    int               `json:"offset"`
}

// List returns a bounded, paginated, filtered slice of incidents newest-first.
func (s *Service) List(p ListParams) (*ListResult, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}

	q := s.db.WithContext(s.ctx).Model(&models.Incident{})
	if p.Status != "" {
		q = q.Where("status = ?", p.Status)
	}
	if p.Class != "" {
		q = q.Where("class = ?", p.Class)
	}
	if p.JobID != nil {
		q = q.Where("job_id = ?", *p.JobID)
	}
	if p.NeedsApproval {
		q = q.Where("status = ?", models.IncidentStatusAwaitingApproval)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}

	var incidents []models.Incident
	if err := q.
		Order("opened_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&incidents).Error; err != nil {
		return nil, err
	}

	return &ListResult{
		Incidents: incidents,
		Total:     total,
		Limit:     limit,
		Offset:    offset,
	}, nil
}

// Detail is the full incident timeline: the incident plus its actions,
// approvals, and agent sessions — every claim one click from primary evidence.
type Detail struct {
	Incident  models.Incident          `json:"incident"`
	Actions   []models.AgentAction     `json:"actions"`
	Approvals []models.ApprovalRequest `json:"approvals"`
	Sessions  []models.AgentSession    `json:"sessions"`
}

// Get returns the full timeline for one incident, or gorm.ErrRecordNotFound.
func (s *Service) Get(id uuid.UUID) (*Detail, error) {
	var inc models.Incident
	if err := s.db.WithContext(s.ctx).First(&inc, "id = ?", id).Error; err != nil {
		return nil, err
	}

	var actions []models.AgentAction
	if err := s.db.WithContext(s.ctx).
		Where("incident_id = ?", id).
		Order("created_at ASC").
		Find(&actions).Error; err != nil {
		return nil, err
	}

	var approvals []models.ApprovalRequest
	if err := s.db.WithContext(s.ctx).
		Where("incident_id = ?", id).
		Order("created_at ASC").
		Find(&approvals).Error; err != nil {
		return nil, err
	}

	var sessions []models.AgentSession
	if err := s.db.WithContext(s.ctx).
		Where("incident_id = ?", id).
		Order("created_at ASC").
		Find(&sessions).Error; err != nil {
		return nil, err
	}

	return &Detail{
		Incident:  inc,
		Actions:   actions,
		Approvals: approvals,
		Sessions:  sessions,
	}, nil
}
