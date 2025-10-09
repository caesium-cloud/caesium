package run

import (
	"context"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service interface {
	WithStore(*runstorage.Store) Service
	WithDatabase(*gorm.DB) Service
	Start(uuid.UUID) (*runstorage.JobRun, error)
	Get(uuid.UUID) (*runstorage.JobRun, error)
	List(uuid.UUID) ([]*runstorage.JobRun, error)
	Latest(uuid.UUID) (*runstorage.JobRun, error)
}

type runService struct {
	ctx   context.Context
	store *runstorage.Store
}

func New(ctx context.Context) Service {
	return &runService{
		ctx:   ctx,
		store: runstorage.NewStore(db.Connection()),
	}
}

func (r *runService) Start(jobID uuid.UUID) (*runstorage.JobRun, error) {
	return r.store.Start(jobID)
}

func (r *runService) Get(runID uuid.UUID) (*runstorage.JobRun, error) {
	return r.store.Get(runID)
}

func (r *runService) List(jobID uuid.UUID) ([]*runstorage.JobRun, error) {
	return r.store.List(jobID)
}

func (r *runService) Latest(jobID uuid.UUID) (*runstorage.JobRun, error) {
	return r.store.Latest(jobID)
}

// WithDatabase allows tests to override the database backing the store.
func (r *runService) WithDatabase(conn *gorm.DB) Service {
	if conn == nil {
		return r
	}
	r.store = runstorage.NewStore(conn)
	return r
}

func (r *runService) WithStore(store *runstorage.Store) Service {
	if store == nil {
		return r
	}
	r.store = store
	return r
}
