package run

import (
	"context"
	"sync"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/event"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/gorm"
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
	ctx     context.Context
	storeMu sync.Mutex
	store   *runstorage.Store
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
	return &runService{ctx: ctx}
}

// runStore captures this service's binding before an operation. Deferring the
// default lookup lets WithStore/WithDatabase bind a cold service without opening
// an unrelated process-wide database. Concurrent operations resolve it once.
func (r *runService) runStore() *runstorage.Store {
	r.storeMu.Lock()
	defer r.storeMu.Unlock()
	if r.store == nil {
		r.store = runstorage.Default()
	}
	return r.store
}

func (r *runService) SetBus(bus event.Bus) {
	store := r.runStore()
	store.SetBus(bus)
	defaultServiceMu.Lock()
	defer defaultServiceMu.Unlock()
	if defaultService == nil {
		defaultService = &runService{store: store}
	}
}

func (r *runService) Start(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (*runstorage.JobRun, error) {
	return r.runStore().Start(jobID, triggerID, opts...)
}

func (r *runService) StartWithResult(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error) {
	return r.runStore().StartWithResult(r.detachedContext(), jobID, triggerID, opts...)
}

func (r *runService) FindIdempotentStart(jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, bool, error) {
	return r.runStore().FindIdempotentStart(r.detachedContext(), jobID, opts...)
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
	run, err := r.runStore().Get(runID)
	r.withholdForConfinedReader(run)
	return run, err
}

func (r *runService) GetTaskLogSnapshot(runID, taskID uuid.UUID) (*runstorage.TaskLogSnapshot, error) {
	return r.runStore().GetTaskLogSnapshot(runID, taskID)
}

// List and Latest run under the service's context, so a history scope the
// handler put on it (authz.WithHistoryScope) narrows them to the run
// namespaces the caller may read (identity-and-access B4).
func (r *runService) List(jobID uuid.UUID, limit, offset int) ([]*runstorage.JobRun, int64, bool, error) {
	runs, total, more, err := r.runStore().ListContext(r.queryContext(), jobID, limit, offset)
	r.withholdForConfinedReader(runs...)
	return runs, total, more, err
}

func (r *runService) Latest(jobID uuid.UUID) (*runstorage.JobRun, error) {
	run, err := r.runStore().LatestContext(r.queryContext(), jobID)
	r.withholdForConfinedReader(run)
	return run, err
}

// withholdForConfinedReader: a namespace-confined reader (a history scope on
// the service's context) never receives baseline evidence recorded before
// baselines were namespace-owned (identity-and-access B4).
func (r *runService) withholdForConfinedReader(runs ...*runstorage.JobRun) {
	if !authz.Restricted(r.queryContext()) {
		return
	}
	for _, run := range runs {
		run.WithholdUnownedBaselineEvidence()
	}
}

func (r *runService) queryContext() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// WithDatabase allows tests to override the database backing the store.
func (r *runService) WithDatabase(conn *gorm.DB) Service {
	if conn == nil {
		return r
	}
	return r.WithStore(runstorage.NewStore(conn))
}

func (r *runService) WithStore(store *runstorage.Store) Service {
	if store == nil {
		return r
	}
	r.storeMu.Lock()
	r.store = store
	r.storeMu.Unlock()
	return r
}
