package callback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Handler executes a callback with the provided configuration and metadata.
type Handler interface {
	Handle(ctx context.Context, cfg json.RawMessage, meta Metadata) error
}

// Result carries the transport-level detail of one delivery attempt. It is what
// turns a CallbackRun's flat error string into something an operator can
// triage: the status the target answered with, the body it answered with, and
// how many requests the handler actually sent.
type Result struct {
	// HTTPStatus is the response status code, or 0 when the attempt never
	// produced a response (connect/TLS failure, timeout, a configuration error
	// that stopped the handler before it sent anything) or the handler is not
	// HTTP-based.
	HTTPStatus int
	// ResponseBody is the response body as read by the handler. The dispatcher
	// scrubs and truncates it before persisting; handlers pass it through raw.
	ResponseBody string
	// Attempts is the number of requests the handler actually sent — 0 when it
	// failed before sending, 1 for a single-shot delivery, N when the handler
	// retried internally.
	Attempts int
}

// DetailedHandler is the richer form of Handler, implemented by handlers that
// can report transport-level detail about a delivery. The dispatcher prefers it
// when a handler provides it and falls back to Handler otherwise, so a
// third-party handler keeps working and simply records no status/body.
type DetailedHandler interface {
	Handler
	HandleWithResult(ctx context.Context, cfg json.RawMessage, meta Metadata) (Result, error)
}

// maxResponseBodyBytes caps the response body persisted on a CallbackRun. A
// target that answers a failed delivery with an HTML error page or a stack
// trace would otherwise push an unbounded blob through Raft on every attempt.
const maxResponseBodyBytes = 4096

// callbackScrubber removes secret-looking material from text a callback target
// sent back before it is persisted and served over the API. It carries no known
// secret values (the dispatcher has no env context), so it applies the
// conservative high-entropy heuristic only — enough to keep a webhook that
// echoes an Authorization header out of the run detail page.
var callbackScrubber = incident.NewScrubber(nil)

// Metadata captures the job/run context sent to callbacks.
type Metadata struct {
	JobID       uuid.UUID         `json:"job_id"`
	JobAlias    string            `json:"job_alias"`
	RunID       uuid.UUID         `json:"run_id"`
	Params      map[string]string `json:"params,omitempty"`
	Status      string            `json:"status"`
	Error       string            `json:"error,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Tasks       []TaskState       `json:"tasks"`
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

	meta, _, err := d.prepare(dispatchCtx, runState.JobID, runID, nil)
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

	retryIDs := make([]uuid.UUID, 0, len(failedSet))
	for id := range failedSet {
		retryIDs = append(retryIDs, id)
	}
	if len(retryIDs) == 0 {
		return nil
	}

	var retryCallbacks []models.Callback
	if err := d.db.WithContext(dispatchCtx).
		Unscoped().
		Where("id IN ?", retryIDs).
		Order("position asc").
		Order("created_at asc").
		Find(&retryCallbacks).Error; err != nil {
		return fmt.Errorf("load retry callbacks: %w", err)
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
	if err := d.db.WithContext(ctx).Unscoped().First(&job, "id = ?", jobID).Error; err != nil {
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
		Order("position asc").
		Order("created_at asc").
		Find(&callbacks).Error; err != nil {
		return Metadata{}, nil, fmt.Errorf("load callbacks: %w", err)
	}

	meta := Metadata{
		JobID:       jobID,
		JobAlias:    job.Alias,
		RunID:       runID,
		Params:      runState.Params,
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
			Command:     slices.Clone(task.Command),
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

	if err := d.createAttempt(ctx, runRecord); err != nil {
		return fmt.Errorf("record callback run: %w", err)
	}
	metrics.DBWritesTotal.WithLabelValues(metrics.DBWriteCategoryCallback).Inc()
	metrics.DBStatementsTotal.WithLabelValues(metrics.DBWriteCategoryCallback).Inc()

	handler, ok := lookupHandler(cb.Type)
	if !ok {
		updateErr := d.completeCallbackRun(ctx, runRecord.ID, models.CallbackRunStatusFailed,
			"no handler registered for callback type", Result{})
		return errors.Join(fmt.Errorf("no handler registered for callback type %q", cb.Type), updateErr)
	}

	rawCfg := json.RawMessage(cb.Configuration)
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	result, err := invokeHandler(callCtx, handler, rawCfg, meta)
	cancel()

	status := models.CallbackRunStatusSucceeded
	errMsg := ""
	if err != nil {
		status = models.CallbackRunStatusFailed
		errMsg = err.Error()
	}

	metrics.CallbackRunsTotal.WithLabelValues(meta.JobID.String(), string(status)).Inc()

	if updateErr := d.completeCallbackRun(ctx, runRecord.ID, status, errMsg, result); updateErr != nil {
		return errors.Join(err, updateErr)
	}

	if err != nil {
		return fmt.Errorf("callback %s (%s): %w", cb.ID, cb.Type, err)
	}

	return nil
}

// invokeHandler prefers the DetailedHandler form so the dispatcher can record
// the status/body/attempt detail, and falls back to the plain Handler contract
// for handlers that do not implement it.
func invokeHandler(ctx context.Context, handler Handler, cfg json.RawMessage, meta Metadata) (Result, error) {
	if detailed, ok := handler.(DetailedHandler); ok {
		return detailed.HandleWithResult(ctx, cfg, meta)
	}
	return Result{}, handler.Handle(ctx, cfg, meta)
}

// attemptOrdinalSQL is the retry ordinal as a scalar sub-select: the attempts
// already recorded for this callback on this run. It is embedded in the INSERT
// that writes the new attempt rather than read by a separate COUNT, so the
// ordinal is allocated by the same statement that consumes it.
const attemptOrdinalSQL = "(SELECT COUNT(*) FROM callback_runs WHERE callback_id = ? AND job_run_id = ?)"

// createAttempt inserts the attempt row and allocates its retry ordinal
// ATOMICALLY, so two deliveries of the same callback on the same run can never
// claim the same ordinal.
//
// The read-then-write shape this replaced (COUNT, then Create) raced across
// server processes: two concurrent `retry-callbacks` requests both read one
// prior row and both persisted retry_count=1 for three actual deliveries. Two
// mechanisms close it, one per dialect family, because no single one is
// sufficient everywhere:
//
//   - sqlite/dqlite: the ordinal is a sub-select inside the INSERT. SQLite
//     evaluates a statement's expressions only after it holds the write lock,
//     and dqlite funnels every writer through the Raft leader, so a second
//     INSERT cannot observe a count taken before the first one committed. A
//     surrounding transaction would NOT be enough on its own — a deferred
//     transaction takes its read snapshot before the write lock, which is
//     exactly the race being fixed.
//   - postgres: MVCC READ COMMITTED lets a sub-select miss a concurrent
//     uncommitted INSERT, so the sub-select alone is not enough. The callback
//     row is locked FOR UPDATE first, which serialises allocation per callback
//     across sessions and therefore across processes. (Same dialect-aware
//     shape as run.lockJobRunForPartitionRetryTx.)
//
// The whole unit is retried on transient contention because statements issued
// inside a transaction bypass the connection pool's per-statement retry (see
// pkg/db/retry.go); a rolled-back attempt leaves no row, so re-running it is
// safe and re-reads the ordinal.
func (d *Dispatcher) createAttempt(ctx context.Context, record *models.CallbackRun) error {
	now := time.Now().UTC()

	return withAttemptContentionRetry(ctx, func() error {
		return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := lockCallbackForAttemptTx(tx, record.CallbackID); err != nil {
				return err
			}
			return tx.Model(&models.CallbackRun{}).Create(map[string]any{
				"id":          record.ID,
				"callback_id": record.CallbackID,
				"job_id":      record.JobID,
				"job_run_id":  record.JobRunID,
				"status":      record.Status,
				"error":       "",
				"http_status": 0,
				// Written by completeCallbackRun; the column is NOT NULL, so a
				// map-based insert has to name it.
				"response_body": "",
				"retry_count":   gorm.Expr(attemptOrdinalSQL, record.CallbackID, record.JobRunID),
				"started_at":    record.StartedAt,
				"created_at":    now,
				"updated_at":    now,
			}).Error
		})
	})
}

// lockCallbackForAttemptTx takes the per-callback allocation lock the dialect
// needs, if any. sqlite and dqlite serialise writers already and cannot parse
// FOR UPDATE, so there is nothing to take there.
func lockCallbackForAttemptTx(tx *gorm.DB, callbackID uuid.UUID) error {
	if tx == nil || tx.Dialector == nil {
		return errors.New("callback: attempt allocation requires a database dialect")
	}
	stmt, err := attemptOrdinalLockSQL(tx.Name())
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	var locked struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	// Unscoped by construction: `callbacks` is soft-deleted and a retired
	// callback's in-flight run still records attempts against it.
	return tx.Raw(stmt, callbackID).Scan(&locked).Error
}

func attemptOrdinalLockSQL(dialect string) (string, error) {
	switch dialect {
	case "postgres":
		return "SELECT id FROM callbacks WHERE id = ? FOR UPDATE", nil
	case "dqlite", "sqlite", "sqlite3":
		return "", nil
	default:
		return "", fmt.Errorf("callback: unsupported dialect %q for retry ordinal allocation", dialect)
	}
}

// withAttemptContentionRetry re-runs a whole attempt transaction on transient
// dqlite/SQLite contention, on the shared repo-wide backoff schedule.
func withAttemptContentionRetry(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	for attempt := 0; ; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = fn()
		if err == nil || !dqlite.IsContentionError(err) {
			return err
		}
		if attempt >= len(db.BusyRetryBackoffs) {
			return err
		}
		metrics.DBBusyRetriesTotal.Inc()
		timer := time.NewTimer(db.BusyRetryBackoffs[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// sanitizeResponseBody prepares a callback target's response body for
// persistence: secret-looking material is scrubbed first (so truncation cannot
// leave an unscrubbable token fragment behind) and the result is then capped.
func sanitizeResponseBody(body string) string {
	if body == "" {
		return ""
	}
	scrubbed := callbackScrubber.Scrub(body)
	if len(scrubbed) > maxResponseBodyBytes {
		scrubbed = scrubbed[:maxResponseBodyBytes]
	}
	return scrubbed
}

func (d *Dispatcher) completeCallbackRun(
	ctx context.Context,
	id uuid.UUID,
	status models.CallbackRunStatus,
	errMsg string,
	result Result,
) error {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":        status,
		"completed_at":  &now,
		"http_status":   result.HTTPStatus,
		"response_body": sanitizeResponseBody(result.ResponseBody),
	}
	if result.Attempts > 1 {
		// A handler that retried internally burned attempts this row is the
		// only record of, so they count as retries too. Added to the ordinal
		// the INSERT allocated rather than recomputed from it, so this update
		// cannot resurrect a stale count.
		updates["retry_count"] = gorm.Expr("retry_count + ?", result.Attempts-1)
	}
	if errMsg != "" {
		// The error quotes the same response body the target sent, so it goes
		// through the same scrubber. It is not truncated: the handler already
		// bounded the body it quotes.
		updates["error"] = callbackScrubber.Scrub(errMsg)
	}
	if err := d.db.WithContext(ctx).
		Model(&models.CallbackRun{}).
		Where("id = ?", id).
		Updates(updates).Error; err != nil {
		return err
	}
	metrics.DBWritesTotal.WithLabelValues(metrics.DBWriteCategoryCallback).Inc()
	metrics.DBStatementsTotal.WithLabelValues(metrics.DBWriteCategoryCallback).Inc()
	return nil
}
