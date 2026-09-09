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

// pendingApprovalSubquery is the `needs_approval` predicate: a correlated EXISTS
// against the incident's ApprovalRequest rows.
//
// It deliberately does NOT filter on `incidents.status = awaiting_approval`,
// which is what this used to do. The question the feed answers is "does a human
// still owe a decision here", and that is a property of the approval ROWS, not
// of the incident's status column — the two can legitimately disagree:
//
//   - an incident can hold MORE THAN ONE pending ApprovalRequest (a second tier-3
//     proposal finds the incident already parked, so parkAwaitingApprovalTx is a
//     no-op while the approval row is still created). Deciding one of them used to
//     advance the incident and evict every remaining approval from this feed — the
//     second decision was still valid and pending in the database, but no operator
//     surface listed it (#417);
//   - a human can take an incident over (escalate, close) while an approval is
//     still pending, which likewise hides an outstanding decision.
//
// Expiry is not part of the predicate: nothing auto-expires an ApprovalRequest
// today (ExpiresAt is advisory), so a stale-but-pending request is still a
// decision a human owes — surfacing it is the point of the feed. The decision
// gate in decide() uses the same definition, so the feed and the status machine
// can never disagree about what "still pending" means.
func pendingApprovalSubquery(db *gorm.DB) *gorm.DB {
	return db.Model(&models.ApprovalRequest{}).
		Select("1").
		Where("approval_requests.incident_id = incidents.id").
		Where("approval_requests.decision = ?", models.ApprovalDecisionPending)
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
	offset := max(p.Offset, 0)

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
		q = q.Where("EXISTS (?)", pendingApprovalSubquery(s.db.WithContext(s.ctx)))
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
