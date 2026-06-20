// Package topology provides the service layer for the historical DAG topology
// API (data-plane-memory B2). It reads dag_snapshot rows written by B1 and
// returns them to the controller; it never writes to the snapshot table.
package topology

import (
	"context"
	"errors"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Topology is the read-side interface for dag_snapshot queries.
type Topology interface {
	WithDatabase(*gorm.DB) Topology
	// Latest returns the most-recent snapshot for jobID.
	Latest(jobID uuid.UUID) (*models.DagSnapshot, error)
	// ByContentHash returns the snapshot matching contentHash for jobID.
	ByContentHash(jobID uuid.UUID, contentHash string) (*models.DagSnapshot, error)
	// ByGitCommit returns the most-recent snapshot whose git_commit matches
	// commit for jobID.
	ByGitCommit(jobID uuid.UUID, commit string) (*models.DagSnapshot, error)
	// List returns all snapshots for jobID ordered newest-first.
	List(jobID uuid.UUID) ([]models.DagSnapshot, error)
}

type topologyService struct {
	ctx   context.Context
	query *jobdef.SnapshotQuery
}

// Service returns a Topology backed by the global db connection.
func Service(ctx context.Context) Topology {
	return &topologyService{
		ctx:   ctx,
		query: jobdef.NewSnapshotQuery(ctx, db.Connection()),
	}
}

// ServiceWithDB returns a Topology backed by the supplied database connection,
// bypassing the global db.Connection() singleton. Used by tests and the local
// runner to avoid initialising the production database.
func ServiceWithDB(ctx context.Context, conn *gorm.DB) Topology {
	return &topologyService{
		ctx:   ctx,
		query: jobdef.NewSnapshotQuery(ctx, conn),
	}
}

func (s *topologyService) WithDatabase(conn *gorm.DB) Topology {
	s.query = jobdef.NewSnapshotQuery(s.ctx, conn)
	return s
}

func (s *topologyService) Latest(jobID uuid.UUID) (*models.DagSnapshot, error) {
	snap, err := s.query.Latest(jobID)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *topologyService) ByContentHash(jobID uuid.UUID, contentHash string) (*models.DagSnapshot, error) {
	if contentHash == "" {
		return nil, errors.New("content_hash must not be empty")
	}
	return s.query.ByContentHash(jobID, contentHash)
}

func (s *topologyService) ByGitCommit(jobID uuid.UUID, commit string) (*models.DagSnapshot, error) {
	return s.query.ByGitCommit(jobID, commit)
}

func (s *topologyService) List(jobID uuid.UUID) ([]models.DagSnapshot, error) {
	return s.query.List(jobID)
}
