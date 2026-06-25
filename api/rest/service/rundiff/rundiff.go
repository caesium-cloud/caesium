// Package rundiff wraps the internal run-diff query for REST controllers.
package rundiff

import (
	"context"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Service exposes read-side run comparison operations.
type Service struct {
	ctx   context.Context
	store *runstorage.Store
}

// New creates a Service backed by the default run store.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, store: runstorage.Default()}
}

// WithDatabase returns a copy of the Service backed by conn; used by tests.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return &Service{ctx: s.ctx, store: runstorage.NewStore(conn)}
}

// Diff returns a machine-readable diff between two runs of the same job.
func (s *Service) Diff(jobID, leftRunID, rightRunID uuid.UUID) (*runstorage.RunDiff, error) {
	return s.store.DiffRuns(s.ctx, jobID, leftRunID, rightRunID)
}
