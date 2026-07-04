// Package agent implements the service layer behind the scoped /v1/agent/* tool
// surface: the triage bundle, the read-only context passthroughs, timeline
// notes, and the typed-action proposal path that delegates to Stream B's action
// executor. Every method here is reached only through the auth middleware's
// agent-scope switch, which 403s an agent token on any incident but its own.
package agent

import (
	"context"
	"errors"

	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrIncidentNotFound is returned when the addressed incident does not exist.
var ErrIncidentNotFound = errors.New("agent: incident not found")

// Service wraps the incident package for the REST controllers, mirroring the
// thin service pattern used by lineage/why (a context + a *gorm.DB, with a
// WithDatabase override for tests).
type Service struct {
	ctx context.Context
	db  *gorm.DB
}

// New creates a Service with the default DB connection.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

// WithDatabase returns a copy of the Service using the given connection.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return &Service{ctx: s.ctx, db: conn}
}

// Incident loads the addressed incident, translating not-found.
func (s *Service) Incident(id uuid.UUID) (*models.Incident, error) {
	var inc models.Incident
	if err := s.db.WithContext(s.ctx).First(&inc, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIncidentNotFound
		}
		return nil, err
	}
	return &inc, nil
}

// Bundle assembles the triage bundle for an incident. The effective profile
// (whose playbook is surfaced) is resolved best-effort from the bootstrap
// default-profile env; the remediation-block → profile resolution lands with
// Stream E's declarative policy and supersedes this once available.
func (s *Service) Bundle(id uuid.UUID) (*iincident.Bundle, error) {
	profile := s.defaultProfile()
	b, err := iincident.BuildBundle(s.ctx, s.db, id, profile)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIncidentNotFound
		}
		return nil, err
	}
	return b, nil
}

// AllowedJobs returns the incident's frozen agent read-scope allowlist.
func (s *Service) AllowedJobs(id uuid.UUID) ([]string, error) {
	return iincident.NewStore(s.db).AllowedJobsForIncident(s.ctx, id)
}

// Note appends a free-text finding to the incident timeline.
func (s *Service) Note(inc *models.Incident, text string) (*models.AgentAction, error) {
	return iincident.RecordNote(s.ctx, s.db, inc.ID, nil, inc.Namespace, text)
}

// defaultProfile resolves the bootstrap default agent profile by name, or nil.
func (s *Service) defaultProfile() *models.AgentProfile {
	name := env.Variables().AgentDefaultProfile
	if name == "" {
		return nil
	}
	var profile models.AgentProfile
	if err := s.db.WithContext(s.ctx).First(&profile, "name = ?", name).Error; err != nil {
		return nil
	}
	return &profile
}
