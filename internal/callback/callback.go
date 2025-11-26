package callback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Handler executes a callback with the provided configuration and metadata.
type Handler interface {
	Handle(ctx context.Context, cfg json.RawMessage, meta Metadata) error
}

// Metadata captures the job/run context sent to callbacks.
type Metadata struct {
	JobID       uuid.UUID   `json:"job_id"`
	JobAlias    string      `json:"job_alias"`
	RunID       uuid.UUID   `json:"run_id"`
	Status      string      `json:"status"`
	Error       string      `json:"error,omitempty"`
	StartedAt   time.Time   `json:"started_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	Tasks       []TaskState `json:"tasks"`
}

// TaskState summarises an individual task run.
type TaskState struct {
	TaskID      uuid.UUID         `json:"task_id"`
	AtomID      uuid.UUID         `json:"atom_id"`
	Engine      models.AtomEngine `json:"engine"`
	Image       string            `json:"image"`
	Command     []string          `json:"command"`
	RuntimeID   string            `json:"runtime_id,omitempty"`
	Status      string            `json:"status"`
	Result      string            `json:"result,omitempty"`
	Error       string            `json:"error,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
}

var (
	defaultDispatcher     *Dispatcher
	defaultDispatcherOnce sync.Once
	handlerRegistry       = make(map[models.CallbackType]Handler)
	registryMu            sync.RWMutex
)

// Register associates a callback type with a handler.
func Register(t models.CallbackType, h Handler) {
	if h == nil {
		panic("callback: handler must not be nil")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	handlerRegistry[t] = h
}

// Default returns the process-wide dispatcher using the shared DB connection.
func Default() *Dispatcher {
	defaultDispatcherOnce.Do(func() {
		defaultDispatcher = NewDispatcher(db.Connection())
	})
	return defaultDispatcher
}

// Dispatcher loads callbacks and invokes handlers.
type Dispatcher struct {
	db      *gorm.DB
	client  *http.Client
	timeout time.Duration
}

// NewDispatcher constructs a Dispatcher backed by the provided DB.
func NewDispatcher(conn *gorm.DB) *Dispatcher {
	if conn == nil {
		panic("callback dispatcher requires a database connection")
	}
	return &Dispatcher{
		db:      conn,
		client:  &http.Client{Timeout: 10 * time.Second},
		timeout: 10 * time.Second,
	}
}

// WithHTTPClient overrides the HTTP client used by handlers (primarily for tests).
func (d *Dispatcher) WithHTTPClient(client *http.Client) {
	if client == nil {
		return
	}
	d.client = client

	registryMu.Lock()
	defer registryMu.Unlock()
	if h, ok := handlerRegistry[models.CallbackTypeNotification]; ok {
		if n, ok := h.(*NotificationHandler); ok {
			n.client = client
		}
	}
}

// Dispatch loads callbacks for the job and executes them sequentially.
func (d *Dispatcher) Dispatch(ctx context.Context, jobID, runID uuid.UUID, runErr error) error {
	dispatchCtx := ensureContext(ctx)
	meta, callbacks, err := d.prepare(dispatchCtx, jobID, runID, runErr)
	if err != nil {
		return err
	}
	return d.execute(dispatchCtx, meta, callbacks)
}

// RetryFailed retries callbacks for the supplied run that most recently failed.
func (d *Dispatcher) RetryFailed(ctx context.Context, runID uuid.UUID) error {
	dispatchCtx := ensureContext(ctx)

	runStore := run.NewStore(d.db)
	runState, err := runStore.Get(runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}

	meta, callbacks, err := d.prepare(dispatchCtx, runState.JobID, runID, nil)
	if err != nil {
		return err
	}

	var failedRuns []models.CallbackRun
	if err := d.db.WithContext(dispatchCtx).
		Where("job_run_id = ? AND status = ?", runID, models.CallbackRunStatusFailed).
		Find(&failedRuns).Error; err != nil {
		return fmt.Errorf("load failed callback runs: %w", err)
	}

	if len(failedRuns) == 0 {
		return nil
	}

	failedSet := make(map[uuid.UUID]struct{}, len(failedRuns))
	for _, cr := range failedRuns {
		failedSet[cr.CallbackID] = struct{}{}
	}

	retryCallbacks := make([]models.Callback, 0, len(failedSet))
	for _, cb := range callbacks {
		if _, ok := failedSet[cb.ID]; ok {
			retryCallbacks = append(retryCallbacks, cb)
		}
	}

	if len(retryCallbacks) == 0 {
		return nil
	}

	return d.execute(dispatchCtx, meta, retryCallbacks)
}

func lookupHandler(t models.CallbackType) (Handler, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	h, ok := handlerRegistry[t]
	return h, ok
}

func init() {
	Register(models.CallbackTypeNotification, NewNotificationHandler(nil))
	log.Debug("callback handlers registered", "count", len(handlerRegistry))
}

func ensureContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (d *Dispatcher) prepare(ctx context.Context, jobID, runID uuid.UUID, runErr error) (Metadata, []models.Callback, error) {
	var job models.Job
	if err := d.db.WithContext(ctx).First(&job, "id = ?", jobID).Error; err != nil {
		return Metadata{}, nil, fmt.Errorf("load job: %w", err)
	}

	runStore := run.NewStore(d.db)
	runState, err := runStore.Get(runID)
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("load run: %w", err)
	}

	var callbacks []models.Callback
	if err := d.db.WithContext(ctx).
		Where("job_id = ?", jobID).
		Order("created_at asc").
		Find(&callbacks).Error; err != nil {
		return Metadata{}, nil, fmt.Errorf("load callbacks: %w", err)
	}

	meta := Metadata{
		JobID:       jobID,
		JobAlias:    job.Alias,
		RunID:       runID,
		Status:      string(runState.Status),
		Error:       runState.Error,
		StartedAt:   runState.StartedAt,
		CompletedAt: runState.CompletedAt,
		Tasks:       make([]TaskState, 0, len(runState.Tasks)),
	}
	if meta.Status == "" {
		if runErr != nil {
			meta.Status = string(run.StatusFailed)
		} else {
			meta.Status = string(run.StatusSucceeded)
		}
	}
	if meta.Error == "" && runErr != nil {
		meta.Error = runErr.Error()
	}

	for _, task := range runState.Tasks {
		if task == nil {
			continue
		}
		taskState := TaskState{
			TaskID:      task.ID,
			AtomID:      task.AtomID,
			Engine:      task.Engine,
			Image:       task.Image,
			Command:     append([]string(nil), task.Command...),
			RuntimeID:   task.RuntimeID,
			Status:      string(task.Status),
			Result:      task.Result,
			Error:       task.Error,
			StartedAt:   task.StartedAt,
			CompletedAt: task.CompletedAt,
		}
		meta.Tasks = append(meta.Tasks, taskState)
	}

	return meta, callbacks, nil
}

func (d *Dispatcher) execute(ctx context.Context, meta Metadata, callbacks []models.Callback) error {
	if len(callbacks) == 0 {
		return nil
	}

	var errs []error
	for _, cb := range callbacks {
		if err := d.invokeCallback(ctx, cb, meta); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (d *Dispatcher) invokeCallback(ctx context.Context, cb models.Callback, meta Metadata) error {
	started := time.Now().UTC()
	runRecord := &models.CallbackRun{
		ID:         uuid.New(),
		CallbackID: cb.ID,
		JobID:      meta.JobID,
		JobRunID:   meta.RunID,
		Status:     models.CallbackRunStatusRunning,
		StartedAt:  started,
	}

	if err := d.db.WithContext(ctx).Create(runRecord).Error; err != nil {
		return fmt.Errorf("record callback run: %w", err)
	}

	handler, ok := lookupHandler(cb.Type)
	if !ok {
		updateErr := d.completeCallbackRun(ctx, runRecord.ID, models.CallbackRunStatusFailed, "no handler registered for callback type")
		return errors.Join(fmt.Errorf("no handler registered for callback type %q", cb.Type), updateErr)
	}

	rawCfg := json.RawMessage(cb.Configuration)
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	err := handler.Handle(callCtx, rawCfg, meta)
	cancel()

	status := models.CallbackRunStatusSucceeded
	errMsg := ""
	if err != nil {
		status = models.CallbackRunStatusFailed
		errMsg = err.Error()
	}

	if updateErr := d.completeCallbackRun(ctx, runRecord.ID, status, errMsg); updateErr != nil {
		return errors.Join(err, updateErr)
	}

	if err != nil {
		return fmt.Errorf("callback %s (%s): %w", cb.ID, cb.Type, err)
	}

	return nil
}

func (d *Dispatcher) completeCallbackRun(ctx context.Context, id uuid.UUID, status models.CallbackRunStatus, errMsg string) error {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":       status,
		"completed_at": &now,
	}
	if errMsg != "" {
		updates["error"] = errMsg
	}
	return d.db.WithContext(ctx).
		Model(&models.CallbackRun{}).
		Where("id = ?", id).
		Updates(updates).Error
}
