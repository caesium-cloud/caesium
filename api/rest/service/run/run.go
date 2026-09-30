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
	Start(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (*runstorage.JobRun, error)
	StartWithResult(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error)
	FindIdempotentStart(jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, bool, error)
	Get(uuid.UUID) (*runstorage.JobRun, error)
	GetTaskLogSnapshot(runID, taskID uuid.UUID) (*runstorage.TaskLogSnapshot, error)
	List(jobID uuid.UUID, limit, offset int) (runs []*runstorage.JobRun, total int64, hasMore bool, err error)
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

func (r *runService) Start(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (*runstorage.JobRun, error) {
	return r.store.Start(jobID, triggerID, opts...)
}

func (r *runService) StartWithResult(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error) {
	return r.store.StartWithResult(r.detachedContext(), jobID, triggerID, opts...)
}

func (r *runService) FindIdempotentStart(jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, bool, error) {
	return r.store.FindIdempotentStart(r.detachedContext(), jobID, opts...)
}

// detachedContext keeps the request's values but not its cancellation, matching
// Start: a client that disconnects mid-admission must not roll back a start
// whose outcome its retry will ask for.
func (r *runService) detachedContext() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(r.ctx)
}

func (r *runService) Get(runID uuid.UUID) (*runstorage.JobRun, error) {
	return r.store.Get(runID)
}

func (r *runService) GetTaskLogSnapshot(runID, taskID uuid.UUID) (*runstorage.TaskLogSnapshot, error) {
	return r.store.GetTaskLogSnapshot(runID, taskID)
}

func (r *runService) List(jobID uuid.UUID, limit, offset int) ([]*runstorage.JobRun, int64, bool, error) {
	return r.store.List(jobID, limit, offset)
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
