// Package blame wraps the internal blame query for REST controllers.
package blame

import (
	"context"

	blamequery "github.com/caesium-cloud/caesium/internal/blame"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Service exposes read-side DAG blame operations.
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

// Blame returns per-element attribution for a job DAG.
func (s *Service) Blame(jobID uuid.UUID, opts blamequery.Options) (*blamequery.Result, error) {
	return blamequery.New(s.db).Blame(s.ctx, jobID, opts)
}
