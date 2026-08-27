// Package why wraps the internal run-level causal explainer (data-plane-memory
// A3) for use by the REST controller. It mirrors the thin service pattern used
// by the lineage and run services: a context + a run.Store, with a WithDatabase
// override for tests.
package why

import (
	"context"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Service explains why a task in a run executed / hit cache / re-ran.
type Service struct {
	ctx   context.Context
	store *runstorage.Store
}

// New creates a Service backed by the default run store.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, store: runstorage.Default()}
}

// WithDatabase returns a copy of the Service whose run store is backed by the
// given connection; used in tests to inject an in-memory database.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return &Service{ctx: s.ctx, store: runstorage.NewStore(conn)}
}

// Why returns the causal explanation for taskRef (a task UUID or name) in run
// runID. A fanned step is answered as a group summary; use WhyPartition to
// select one instance.
func (s *Service) Why(runID uuid.UUID, taskRef string) (*runstorage.WhyExplanation, error) {
	return s.store.WhyTask(s.ctx, runID, taskRef)
}

// WhyPartition returns the causal explanation for ONE fan-out instance of
// taskRef, selected by its partition value. An empty partition is identical to
// Why. runstorage.ErrPartitionNotFound (whose message lists the available
// values) when the group holds no such partition.
func (s *Service) WhyPartition(runID uuid.UUID, taskRef, partition string) (*runstorage.WhyExplanation, error) {
	return s.store.WhyTaskPartition(s.ctx, runID, taskRef, partition)
}
