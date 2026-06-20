// Package receipt is the REST service wrapper around internal/receipt: it
// builds a reproducibility receipt for a run and verifies a committed receipt
// against a run's current persisted state.
package receipt

import (
	"context"

	ireceipt "github.com/caesium-cloud/caesium/internal/receipt"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Service wraps the internal receipt build/verify layer for REST controllers.
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

// Build re-derives the reproducibility receipt for a run from persisted state.
func (s *Service) Build(runID uuid.UUID) (*ireceipt.Receipt, error) {
	return ireceipt.Build(s.ctx, s.db, runID)
}

// Verify re-derives the receipt for committed.RunID and reports drift against
// the committed receipt.
func (s *Service) Verify(committed *ireceipt.Receipt) (*ireceipt.VerifyResult, error) {
	return ireceipt.Verify(s.ctx, s.db, committed)
}
