package lineage

import (
	"context"

	illineage "github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/pkg/db"
	"gorm.io/gorm"
)

// Service wraps the internal lineage query layer for use by REST controllers.
type Service struct {
	ctx context.Context
	db  *gorm.DB
}

// New creates a Service with the default DB connection.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

// WithDatabase returns a copy of the Service using the given connection; used
// in tests to inject an in-memory database without touching the singleton.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	return &Service{ctx: s.ctx, db: conn}
}

// Impact returns the transitive downstream impact of a dataset change.  See
// internal/lineage.QueryImpact for traversal semantics.
func (s *Service) Impact(namespace, name string, maxDepth int) (*illineage.ImpactResult, error) {
	return illineage.QueryImpact(s.ctx, s.db, namespace, name, maxDepth)
}
