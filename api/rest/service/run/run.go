package run

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/event"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"sync"
)

type Service interface {
	WithStore(*runstorage.Store) Service
	WithDatabase(*gorm.DB) Service
	SetBus(event.Bus)
	Start(uuid.UUID) (*runstorage.JobRun, error)
	Get(uuid.UUID) (*runstorage.JobRun, error)
	List(uuid.UUID) ([]*runstorage.JobRun, error)
	Latest(uuid.UUID) (*runstorage.JobRun, error)
}

type runService struct {
	ctx   context.Context
	store *runstorage.Store
}

var (
	defaultService   *runService
	defaultServiceMu sync.Mutex
)

func New(ctx context.Context) Service {
	defaultServiceMu.Lock()
	defer defaultServiceMu.Unlock()
	if defaultService != nil {
		return &runService{
			ctx:   ctx,
			store: defaultService.store,
		}
	}
	return &runService{
		ctx:   ctx,
		store: runstorage.Default(),
	}
}

func (r *runService) SetBus(bus event.Bus) {
	r.store.SetBus(bus)
	defaultServiceMu.Lock()
	defer defaultServiceMu.Unlock()
	if defaultService == nil {
		defaultService = &runService{store: r.store}
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
