package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Status string

type TaskStatus string

type Result string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = Status(models.JobRunStatusCancelled)
)

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSucceeded TaskStatus = "succeeded"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusSkipped   TaskStatus = "skipped"
	TaskStatusCached    TaskStatus = "cached"
	TaskStatusCancelled TaskStatus = TaskStatus(models.TaskRunStatusCancelled)
)

// IsTerminalSuccess returns true for task statuses that represent successful completion.
func IsTerminalSuccess(status TaskStatus) bool {
	return status == TaskStatusSucceeded || status == TaskStatusCached
}

// IsTerminal reports whether a task status is terminal — the task will not
// transition again.  This is the single definition of the terminal vocabulary
// (succeeded, failed, skipped, cached), reused by owner replay, the recovery
// scan, and archival so the set lives in exactly one place.
func IsTerminal(status TaskStatus) bool {
	switch status {
	case TaskStatusSucceeded, TaskStatusFailed, TaskStatusSkipped, TaskStatusCached, TaskStatusCancelled:
		return true
	default:
		return false
	}
}

type CallbackStatus string

const (
	CallbackStatusRunning   CallbackStatus = "running"
	CallbackStatusSucceeded CallbackStatus = "succeeded"
	CallbackStatusFailed    CallbackStatus = "failed"
)

func IsSuccessfulTaskResult(result string) bool {
	return taskStatusFromResult(result) == TaskStatusSucceeded
}

func taskStatusFromResult(result string) TaskStatus {
	switch Result(result) {
	case "", "success", "ok":
		return TaskStatusSucceeded
	default:
		return TaskStatusFailed
	}
}

type CallbackRun struct {
	ID          uuid.UUID      `json:"id"`
	CallbackID  uuid.UUID      `json:"callback_id"`
	Status      CallbackStatus `json:"status"`
	Error       string         `json:"error,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
}

type TaskRun struct {
	ID                      uuid.UUID                 `json:"id"`
	JobRunID                uuid.UUID                 `json:"job_run_id"`
	TaskID                  uuid.UUID                 `json:"task_id"`
	JobAlias                string                    `json:"job_alias,omitempty"`
	JobLabels               map[string]string         `json:"job_labels,omitempty"`
	AtomID                  uuid.UUID                 `json:"atom_id"`
	Engine                  models.AtomEngine         `json:"engine"`
	Image                   string                    `json:"image"`
	Command                 []string                  `json:"command"`
	RuntimeID               string                    `json:"runtime_id,omitempty"`
	Status                  TaskStatus                `json:"status"`
	Priority                int                       `json:"priority"`
	NodeSelector            map[string]string         `json:"node_selector,omitempty"`
	ClaimedBy               string                    `json:"claimed_by,omitempty"`
	ClaimExpiresAt          *time.Time                `json:"claim_expires_at,omitempty"`
	ClaimAttempt            int                       `json:"claim_attempt"`
	Attempt                 int                       `json:"attempt"`
	MaxAttempts             int                       `json:"max_attempts"`
	Result                  string                    `json:"result,omitempty"`
	Output                  map[string]string         `json:"output,omitempty"`
	SchemaViolations        []pkgtask.SchemaViolation `json:"schema_violations,omitempty"`
	BranchSelections        []string                  `json:"branch_selections,omitempty"`
	Quarantine              bool                      `json:"quarantine"`
	CacheHit                bool                      `json:"cache_hit"`
	ReplaySafe              bool                      `json:"replay_safe"`
	CacheOriginRunID        *uuid.UUID                `json:"cache_origin_run_id,omitempty"`
	CacheCreatedAt          *time.Time                `json:"cache_created_at,omitempty"`
	CacheExpiresAt          *time.Time                `json:"cache_expires_at,omitempty"`
	RateLimitRetryAfter     *time.Time                `json:"rate_limit_retry_after,omitempty"`
	StartedAt               *time.Time                `json:"started_at,omitempty"`
	CompletedAt             *time.Time                `json:"completed_at,omitempty"`
	Error                   string                    `json:"error,omitempty"`
	OutstandingPredecessors int                       `json:"outstanding_predecessors"`
	CreatedAt               time.Time                 `json:"created_at"`
	UpdatedAt               time.Time                 `json:"updated_at"`
}

type JobRun struct {
	ID            uuid.UUID         `json:"id"`
	JobID         uuid.UUID         `json:"job_id"`
	JobAlias      string            `json:"job_alias,omitempty"`
	JobLabels     map[string]string `json:"job_labels,omitempty"`
	BackfillID    *uuid.UUID        `json:"backfill_id,omitempty"`
	TriggerType   string            `json:"trigger_type,omitempty"`
	TriggerAlias  string            `json:"trigger_alias,omitempty"`
	Status        Status            `json:"status"`
	Priority      int               `json:"priority"`
	Params        map[string]string `json:"params,omitempty"`
	Quarantine    bool              `json:"quarantine"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Error         string            `json:"error,omitempty"`
	Tasks         []*TaskRun        `json:"tasks"`
	Callbacks     []*CallbackRun    `json:"callbacks"`
	CacheHits     int               `json:"cache_hits"`
	ExecutedTasks int               `json:"executed_tasks"`
	TotalTasks    int               `json:"total_tasks"`
}

type Store struct {
	db         *gorm.DB
	bus        event.Bus
	eventStore *event.Store

	// startedMu guards startedRuns.
	startedMu sync.Mutex
	// startedRuns tracks run IDs that were started via Start() in this
	// process so that Complete() only decrements the active-jobs gauge
	// for runs it actually incremented.
	startedRuns map[uuid.UUID]struct{}

	// leaseStore is non-nil only when CAESIUM_RUN_OWNER_ENABLED=true.
	// When nil, no run_leases rows are written and the system behaves
	// byte-identically to Phase 1.
	leaseStore *LeaseStore
}

type RegisterTaskInput struct {
	Task                    *models.Task
	Atom                    *models.Atom
	OutstandingPredecessors int
}

type StartOptions struct {
	Params   map[string]string
	Priority string
}

type StartOption func(*StartOptions)

func WithStartParams(params map[string]string) StartOption {
	return func(opts *StartOptions) {
		opts.Params = maps.Clone(params)
	}
}

func WithStartPriority(priority string) StartOption {
	return func(opts *StartOptions) {
		opts.Priority = strings.TrimSpace(priority)
	}
}

func startOptionsFrom(opts []StartOption) StartOptions {
	var out StartOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	return out
}

func startPriorityTx(tx *gorm.DB, jobID uuid.UUID, override string) (int, error) {
	if strings.TrimSpace(override) != "" {
		return PriorityValue(override)
	}

	var job models.Job
	err := tx.Select("priority").First(&job, "id = ?", jobID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PriorityNormalValue, nil
		}
		return 0, err
	}
	return PriorityValue(job.Priority)
}

var (
	defaultStore     *Store
	defaultStoreOnce sync.Once
)

var (
	ErrTaskClaimMismatch        = errors.New("run: task claim mismatch")
	ErrRunSkipped               = errors.New("run: skipped by concurrency policy")
	ErrRunQueued                = errors.New("run: queued by concurrency policy")
	ErrMaxConcurrentRunsReached = errors.New("run: max concurrent runs reached")
)

type admissionDecision int

const (
	admissionNoPolicy admissionDecision = iota
	admissionCreated
	admissionSkipped
	admissionFailed
	admissionQueued
)

type cancelledRunInfo struct {
	ID          uuid.UUID
	JobID       uuid.UUID
	JobAlias    string
	StartedAt   time.Time
	Quarantine  bool
	CancelledAt time.Time
}

type admissionResult struct {
	decision           admissionDecision
	jobAlias           string
	skipReason         string
	replaced           bool
	cancelledRun       *cancelledRunInfo
	cancelledRunEvents []event.Event
}

type startRunRequest struct {
	jobID            uuid.UUID
	triggerID        *uuid.UUID
	backfillID       *uuid.UUID
	params           map[string]string
	priorityOverride string
	fromQueue        bool
	policyOnly       bool
}

// storeBusyRetryBackoffs aliases the shared contention-retry schedule so
// whole-transaction store ops back off on the same budget as the autocommit
// pool retry; see db.BusyRetryBackoffs.
var storeBusyRetryBackoffs = db.BusyRetryBackoffs

func NewStore(conn *gorm.DB) *Store {
	if conn == nil {
		panic("run store requires database connection")
	}
	return &Store{
		db:          conn,
		eventStore:  event.NewStore(conn),
		startedRuns: make(map[uuid.UUID]struct{}),
	}
}

// WithLeaseStore enables run-owner lease writing.  Call this from startup
// code when CAESIUM_RUN_OWNER_ENABLED=true.
func (s *Store) WithLeaseStore(ls *LeaseStore) *Store {
	s.leaseStore = ls
	return s
}

// LeaseStore returns the run lease store, or nil when owner mode is disabled.
func (s *Store) LeaseStore() *LeaseStore {
	return s.leaseStore
}

func Default() *Store {
	defaultStoreOnce.Do(func() {
		defaultStore = NewStore(db.Connection())
	})
	return defaultStore
}

func (s *Store) SetBus(bus event.Bus) {
	s.bus = bus
}

func (s *Store) Bus() event.Bus {
	return s.bus
}

// AdoptStartedRun records that this process should clear active-run bookkeeping
// when Complete sees runID. It is used when a run is created transactionally by
// a short-lived store instance but executed by the default runtime store.
func (s *Store) AdoptStartedRun(runID uuid.UUID) {
	if s == nil || runID == uuid.Nil {
		return
	}
	s.startedMu.Lock()
	s.startedRuns[runID] = struct{}{}
	s.startedMu.Unlock()
}

func (s *Store) EventStore() *event.Store {
	return s.eventStore
}

func (s *Store) RecordEventTx(tx *gorm.DB, evt *event.Event) error {
	if evt == nil || s.eventStore == nil {
		return nil
	}
	if err := s.stampEventQuarantineTx(tx, evt); err != nil {
		return err
	}
	return s.eventStore.AppendTx(tx, evt)
}

func (s *Store) stampEventQuarantineTx(tx *gorm.DB, evt *event.Event) error {
	if tx == nil || evt == nil || evt.RunID == uuid.Nil {
		return nil
	}
	// Event quarantine stamping is deliberately fail-closed: a missing marker
	// aborts the transaction instead of leaking an outward event as production.
	if evt.TaskID != uuid.Nil {
		quarantined, err := s.taskEventQuarantineTx(tx, evt.RunID, evt.TaskID)
		if err != nil {
			return fmt.Errorf("run: stamp event quarantine from task run: %w", err)
		}
		evt.Quarantine = quarantined
		return nil
	}
	quarantined, err := s.runEventQuarantineTx(tx, evt.RunID)
	if err != nil {
		return fmt.Errorf("run: stamp event quarantine from job run: %w", err)
	}
	evt.Quarantine = quarantined
	return nil
}

func (s *Store) runEventQuarantineTx(tx *gorm.DB, runID uuid.UUID) (bool, error) {
	var jobRun models.JobRun
	if err := tx.Select("quarantine").First(&jobRun, "id = ?", runID).Error; err != nil {
		return false, err
	}
	return jobRun.Quarantine, nil
}

func (s *Store) taskEventQuarantineTx(tx *gorm.DB, runID, taskID uuid.UUID) (bool, error) {
	var taskRun models.TaskRun
	if err := tx.Select("quarantine").
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		First(&taskRun).Error; err != nil {
		return false, err
	}
	return taskRun.Quarantine, nil
}

func (s *Store) DB() *gorm.DB {
	return s.db
}

func decodeCacheConfig(raw []byte) interface{} {
	if len(raw) == 0 {
		return nil
	}

	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}

// RenewLeases extends claim_expires_at for all task runs identified by ids that
// are still claimed by nodeID. The WHERE clause on claimed_by ensures that any
// task whose claim was reassigned after expiry is not accidentally extended.
// An empty ids slice is a no-op (no database round-trip). Returns the number of
// rows actually updated so callers can credit metrics accurately and detect the
// case where a claim was reassigned between the renewal decision and the write.
func (s *Store) RenewLeases(ctx context.Context, nodeID string, ids []uuid.UUID, newExpiresAt time.Time) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := s.db.WithContext(ctx).
		Model(&models.TaskRun{}).
		Where("claimed_by = ? AND id IN ?", nodeID, ids).
		Update("claim_expires_at", newExpiresAt)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

func (s *Store) SetTaskHash(runID, taskID uuid.UUID, hash string) error {
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Update("hash", hash).Error
}

// SetTaskHashWithDigest persists the task identity hash together with the
// resolved image digest that was folded into it. The digest may be empty when
// pinning is off or resolution failed; in that case only the hash is written
// and the existing digest column (if any) is left untouched, keeping the row
// consistent with the literal-tag cache key.
func (s *Store) SetTaskHashWithDigest(runID, taskID uuid.UUID, hash, resolvedImageDigest string) error {
	return s.SetTaskHashWithBlob(runID, taskID, hash, resolvedImageDigest, nil)
}

// SetTaskHashWithBlob persists the task identity hash, the resolved image
// digest folded into it, and the canonical secret-redacted decomposition of the
// HashInput (the blob) on the same write — the existing hash write-path. The
// digest and blob are optional: an empty digest or a nil/empty blob leaves the
// corresponding column untouched (so a literal-tag, blob-less run stays
// consistent). The blob lets `caesium why` later diff two runs field-by-field
// rather than only observing that the opaque hashes differ.
func (s *Store) SetTaskHashWithBlob(runID, taskID uuid.UUID, hash, resolvedImageDigest string, hashInputBlob []byte) error {
	updates := map[string]any{"hash": hash}
	if resolvedImageDigest != "" {
		updates["resolved_image_digest"] = resolvedImageDigest
	}
	if len(hashInputBlob) > 0 {
		updates["hash_input_blob"] = datatypes.JSON(hashInputBlob)
	}
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(updates).Error
}

func (s *Store) UpdateTaskExecutionDescriptorInputs(runID, taskID uuid.UUID, predecessorOutputs map[uuid.UUID]map[string]string, predecessorHashes map[uuid.UUID]string, computedHash, resolvedImageDigest string, hashInputBlob []byte) error {
	return s.mutateTaskExecutionDescriptor(runID, taskID, func(desc *models.TaskExecutionDescriptor) {
		desc.CapturedAt = time.Now().UTC()
		if desc.SchemaVersion == 0 {
			desc.SchemaVersion = models.TaskExecutionDescriptorSchemaVersion
		}
		desc.DAG.PredecessorOutputs = predecessorOutputs
		desc.DAG.PredecessorEffectiveHashes = predecessorHashes
		if computedHash != "" {
			desc.Baseline.ComputedHash = computedHash
			desc.Cache.ComputedHash = computedHash
		}
		if resolvedImageDigest != "" {
			desc.Runtime.ResolvedImageDigest = resolvedImageDigest
		}
		if len(hashInputBlob) > 0 {
			desc.Baseline.HashInputBlobStored = true
			desc.Cache.HashInputBlobStored = true
		}
	})
}

func (s *Store) UpdateTaskExecutionDescriptorSecretRefs(runID, taskID uuid.UUID, refs []models.TaskExecutionSecretRef) error {
	if len(refs) == 0 {
		return nil
	}
	return s.mutateTaskExecutionDescriptor(runID, taskID, func(desc *models.TaskExecutionDescriptor) {
		desc.CapturedAt = time.Now().UTC()
		if desc.SchemaVersion == 0 {
			desc.SchemaVersion = models.TaskExecutionDescriptorSchemaVersion
		}
		desc.SecretRefs = mergeDescriptorSecretRefs(desc.SecretRefs, refs)
	})
}

// SetTaskEffectiveHash records the proven-equivalent prior identity a task
// presents to its downstream consumers when a value-verified short-circuit was
// proven (design Component 5 / D2). It writes ONLY the effective_hash column;
// the task's own Hash, output, and result are untouched, so its receipt and
// `caesium why` still reflect its true identity. Passing an empty effectiveHash
// is a no-op (the common case — no short-circuit), keeping the column nil and
// PredecessorHashes falling back to the true hash. This is the only writer of
// effective_hash, so a downstream reader observes either the proven prior
// identity or nothing.
func (s *Store) SetTaskEffectiveHash(runID, taskID uuid.UUID, effectiveHash string) error {
	if effectiveHash == "" {
		return nil
	}
	if err := s.mutateTaskExecutionDescriptor(runID, taskID, func(desc *models.TaskExecutionDescriptor) {
		desc.Baseline.EffectiveHash = effectiveHash
		desc.Cache.EffectiveHash = effectiveHash
	}); err != nil {
		log.Warn("failed to update task execution descriptor effective hash", "run_id", runID, "task_id", taskID, "error", err)
	}
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Update("effective_hash", effectiveHash).Error
}

func (s *Store) TaskQuarantine(ctx context.Context, runID, taskID uuid.UUID) (bool, error) {
	var row struct {
		TaskQuarantine bool
		RunQuarantine  bool
	}
	err := s.db.WithContext(ctx).
		Table("task_runs").
		Select("task_runs.quarantine AS task_quarantine, job_runs.quarantine AS run_quarantine").
		Joins("join job_runs on job_runs.id = task_runs.job_run_id").
		Where("task_runs.job_run_id = ? AND task_runs.task_id = ?", runID, taskID).
		Take(&row).Error
	if err != nil {
		return false, err
	}
	return row.TaskQuarantine || row.RunQuarantine, nil
}

func (s *Store) TaskExecutionDescriptor(ctx context.Context, runID, taskID uuid.UUID) (*models.TaskExecutionDescriptor, error) {
	var taskRun models.TaskRun
	err := s.db.WithContext(ctx).
		Select("execution_descriptor").
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Take(&taskRun).Error
	if err != nil {
		return nil, err
	}
	if len(taskRun.ExecutionDescriptor) == 0 {
		return nil, fmt.Errorf("run: task execution descriptor missing for run %s task %s", runID, taskID)
	}
	var descriptor models.TaskExecutionDescriptor
	if err := json.Unmarshal(taskRun.ExecutionDescriptor, &descriptor); err != nil {
		return nil, fmt.Errorf("run: decode task execution descriptor for run %s task %s: %w", runID, taskID, err)
	}
	if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
		return nil, fmt.Errorf("run: unsupported task execution descriptor version %d for run %s task %s", descriptor.SchemaVersion, runID, taskID)
	}
	return &descriptor, nil
}

func (s *Store) replayTaskExecutionDescriptorTx(tx *gorm.DB, runID, taskID uuid.UUID) (*models.TaskExecutionDescriptor, bool, error) {
	var row struct {
		TaskQuarantine      bool
		RunQuarantine       bool
		ExecutionDescriptor datatypes.JSON
	}
	err := tx.Table("task_runs").
		Select("task_runs.quarantine AS task_quarantine, job_runs.quarantine AS run_quarantine, task_runs.execution_descriptor AS execution_descriptor").
		Joins("join job_runs on job_runs.id = task_runs.job_run_id").
		Where("task_runs.job_run_id = ? AND task_runs.task_id = ?", runID, taskID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !row.TaskQuarantine && !row.RunQuarantine {
		return nil, false, nil
	}
	if len(row.ExecutionDescriptor) == 0 {
		return nil, true, fmt.Errorf("run: replay task execution descriptor missing for run %s task %s", runID, taskID)
	}
	var descriptor models.TaskExecutionDescriptor
	if err := json.Unmarshal(row.ExecutionDescriptor, &descriptor); err != nil {
		return nil, true, fmt.Errorf("run: decode replay task execution descriptor for run %s task %s: %w", runID, taskID, err)
	}
	if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
		return nil, true, fmt.Errorf("run: unsupported replay task execution descriptor version %d for run %s task %s", descriptor.SchemaVersion, runID, taskID)
	}
	return &descriptor, true, nil
}

func (s *Store) replayPredecessorRefsTx(tx *gorm.DB, runID, taskID uuid.UUID) ([]models.TaskExecutionEdgeRef, bool, error) {
	descriptor, replay, err := s.replayTaskExecutionDescriptorTx(tx, runID, taskID)
	if err != nil || !replay {
		return nil, replay, err
	}
	return descriptor.DAG.Predecessors, true, nil
}

func newStartRunModel(req startRunRequest) (*models.JobRun, error) {
	now := time.Now().UTC()
	model := &models.JobRun{
		ID:        uuid.New(),
		JobID:     req.jobID,
		Status:    string(StatusRunning),
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if req.triggerID != nil {
		model.TriggerID = *req.triggerID
	}
	if req.backfillID != nil {
		model.BackfillID = req.backfillID
	}
	if len(req.params) > 0 {
		encoded, err := json.Marshal(req.params)
		if err != nil {
			return nil, fmt.Errorf("run: failed to marshal params: %w", err)
		}
		model.Params = encoded
	}
	return model, nil
}

func (s *Store) appendRunStartedEventTx(tx *gorm.DB, model *models.JobRun) (*event.Event, error) {
	if s.eventStore == nil || model == nil {
		return nil, nil
	}
	payload, err := json.Marshal(&JobRun{
		ID:        model.ID,
		JobID:     model.JobID,
		Status:    Status(model.Status),
		Priority:  model.Priority,
		StartedAt: model.StartedAt,
		CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt,
		Tasks:     []*TaskRun{},
	})
	if err != nil {
		return nil, err
	}
	evt := event.Event{
		Type:       event.TypeRunStarted,
		JobID:      model.JobID,
		RunID:      model.ID,
		Timestamp:  time.Now().UTC(),
		Payload:    payload,
		Quarantine: model.Quarantine,
	}
	if err := s.eventStore.AppendTx(tx, &evt); err != nil {
		return nil, err
	}
	return &evt, nil
}

type concurrencyConfig struct {
	jobAlias string
	maxRuns  int
	strategy string
}

func concurrencyFromJSON(raw []byte) (*jobdefschema.Concurrency, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var cfg *jobdefschema.Concurrency
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (s *Store) concurrencyConfigTx(tx *gorm.DB, jobID uuid.UUID) (concurrencyConfig, bool, error) {
	var row struct {
		Alias       string
		Concurrency datatypes.JSON
	}
	err := tx.Model(&models.Job{}).
		Select("alias", "concurrency").
		Where("id = ?", jobID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return concurrencyConfig{}, false, nil
		}
		return concurrencyConfig{}, false, err
	}
	cfg, err := concurrencyFromJSON(row.Concurrency)
	if err != nil {
		return concurrencyConfig{}, false, fmt.Errorf("run: decode job concurrency metadata: %w", err)
	}
	if cfg == nil || cfg.MaxRuns <= 0 {
		return concurrencyConfig{jobAlias: row.Alias}, false, nil
	}
	strategy := strings.ToLower(strings.TrimSpace(cfg.Strategy))
	if strategy == "" {
		strategy = jobdefschema.ConcurrencyStrategyQueue
	}
	return concurrencyConfig{
		jobAlias: row.Alias,
		maxRuns:  cfg.MaxRuns,
		strategy: strategy,
	}, true, nil
}

func (s *Store) admit(tx *gorm.DB, model *models.JobRun, req startRunRequest) (admissionResult, error) {
	if model == nil {
		return admissionResult{}, errors.New("run: admission requires a run model")
	}
	cfg, ok, err := s.concurrencyConfigTx(tx, model.JobID)
	if err != nil {
		return admissionResult{}, err
	}
	result := admissionResult{decision: admissionNoPolicy, jobAlias: cfg.jobAlias}
	if !ok {
		return result, nil
	}
	if model.BackfillID != nil {
		// Backfills use their own MaxConcurrent semaphore and run_queue does not
		// carry backfill_id, so ordinary run concurrency deliberately excludes
		// backfill rows via backfill_id IS NULL in the active-count predicates.
		return result, nil
	}

	inserted, err := s.insertRunIfSlotTx(tx, model, cfg.maxRuns)
	if err != nil {
		return result, err
	}
	if inserted {
		result.decision = admissionCreated
		return result, nil
	}

	switch cfg.strategy {
	case jobdefschema.ConcurrencyStrategySkip:
		result.decision = admissionSkipped
		result.skipReason = "max_concurrency"
		return result, nil
	case jobdefschema.ConcurrencyStrategyFail:
		result.decision = admissionFailed
		return result, nil
	case jobdefschema.ConcurrencyStrategyQueue:
		if req.fromQueue {
			result.decision = admissionFailed
			return result, nil
		}
		if err := s.enqueueRunTx(tx, model.JobID, model.Params, model.Priority, env.Variables().RunQueueMaxDepth); err != nil {
			return result, err
		}
		result.decision = admissionQueued
		return result, nil
	case jobdefschema.ConcurrencyStrategyReplace:
		cancelled, cancelEvents, err := s.cancelOldestActiveRunTx(tx, model.JobID)
		if err != nil {
			return result, err
		}
		inserted, err := s.insertRunIfSlotTx(tx, model, cfg.maxRuns)
		if err != nil {
			return result, err
		}
		if !inserted {
			result.decision = admissionFailed
			return result, nil
		}
		result.decision = admissionCreated
		result.replaced = cancelled != nil
		result.cancelledRun = cancelled
		result.cancelledRunEvents = cancelEvents
		return result, nil
	default:
		return result, fmt.Errorf("run: unsupported concurrency strategy %q", cfg.strategy)
	}
}

func (s *Store) insertRunIfSlotTx(tx *gorm.DB, model *models.JobRun, maxRuns int) (bool, error) {
	if maxRuns <= 0 {
		return true, tx.Create(model).Error
	}
	var backfillID any
	if model.BackfillID != nil {
		backfillID = *model.BackfillID
	}
	params := any(nil)
	if len(model.Params) > 0 {
		params = string(model.Params)
	}
	// This must remain one conditional INSERT statement: dqlite serializes the
	// statement through Raft, so concurrent nodes derive admission from
	// RowsAffected instead of racing through CountActive-then-Create. Backfill
	// rows are intentionally excluded; they are governed by backfill maxConcurrent.
	result := tx.Exec(`
INSERT INTO job_runs (
	id, job_id, backfill_id, trigger_id, status, priority, params, quarantine,
	started_at, created_at, updated_at
)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE (
	SELECT count(*)
	FROM job_runs
	WHERE job_id = ?
		AND status = ?
		AND quarantine <> true
		AND backfill_id IS NULL
) < ?`,
		model.ID,
		model.JobID,
		backfillID,
		model.TriggerID,
		model.Status,
		model.Priority,
		params,
		model.Quarantine,
		model.StartedAt,
		model.CreatedAt,
		model.UpdatedAt,
		model.JobID,
		string(StatusRunning),
		maxRuns,
	)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func metricJobAlias(jobID uuid.UUID, alias string) string {
	if strings.TrimSpace(alias) != "" {
		return alias
	}
	return jobID.String()
}

func (s *Store) startRun(req startRunRequest) (*JobRun, error) {
	model, err := newStartRunModel(req)
	if err != nil {
		return nil, err
	}

	var (
		pendingEvents []event.Event
		admission     admissionResult
	)
	if err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 3)
		attemptAdmission := admissionResult{decision: admissionNoPolicy}
		err := s.db.Transaction(func(tx *gorm.DB) error {
			priority, err := startPriorityTx(tx, req.jobID, req.priorityOverride)
			if err != nil {
				return err
			}
			model.Priority = priority

			result, err := s.admit(tx, model, req)
			if err != nil {
				return err
			}
			attemptAdmission = result

			switch result.decision {
			case admissionNoPolicy:
				if req.policyOnly {
					return nil
				}
				if err := tx.Create(model).Error; err != nil {
					return err
				}
				attemptAdmission.decision = admissionCreated
			case admissionCreated:
			case admissionSkipped, admissionFailed, admissionQueued:
				return nil
			default:
				return fmt.Errorf("run: unknown admission decision %d", result.decision)
			}

			evt, err := s.appendRunStartedEventTx(tx, model)
			if err != nil {
				return err
			}
			if evt != nil {
				attemptEvents = append(attemptEvents, *evt)
			}
			if result.cancelledRun != nil && s.eventStore != nil {
				// cancelRunTx appends its own events to the event store. They are
				// returned here for bus publication after the surrounding run-start
				// transaction commits.
				attemptEvents = append(result.cancelledRunEvents, attemptEvents...)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			admission = attemptAdmission
		}
		return err
	}); err != nil {
		return nil, err
	}

	switch admission.decision {
	case admissionNoPolicy:
		return nil, nil
	case admissionSkipped:
		reason := admission.skipReason
		if reason == "" {
			reason = "max_concurrency"
		}
		log.Info("run skipped by concurrency policy", "job_id", req.jobID, "job_alias", admission.jobAlias, "reason", reason)
		metrics.RunSkippedTotal.WithLabelValues(metricJobAlias(req.jobID, admission.jobAlias), reason).Inc()
		return nil, ErrRunSkipped
	case admissionFailed:
		return nil, ErrMaxConcurrentRunsReached
	case admissionQueued:
		log.Info("run queued by concurrency policy", "job_id", req.jobID, "job_alias", admission.jobAlias)
		if err := s.observeRunQueueDepth(req.jobID); err != nil {
			log.Warn("run queue: failed to observe depth", "job_id", req.jobID, "error", err)
		}
		return nil, ErrRunQueued
	}

	// Publish events immediately after commit, before loadRun, so that
	// run_started reaches the bus before any task events that the executor
	// may emit once Start returns.
	s.publishEvents(pendingEvents...)

	if admission.replaced {
		metrics.RunReplacedTotal.WithLabelValues(metricJobAlias(req.jobID, admission.jobAlias)).Inc()
	}
	if admission.cancelledRun != nil {
		s.recordCancelledRunMetrics(*admission.cancelledRun)
	}

	// Phase 2: write run_leases row when owner mode is enabled.
	// This is done outside the run-creation transaction so that a lease write
	// failure does not roll back the run itself — the ClaimNext recovery path
	// still picks up the run if no lease is ever acquired.
	if s.leaseStore != nil {
		vars := env.Variables()
		if _, leaseErr := s.leaseStore.AcquireLease(
			context.Background(),
			model.ID,
			vars.NodeAddress,
			vars.RunLeaseTTL,
		); leaseErr != nil {
			log.Warn("run owner: failed to acquire run lease; run will fall back to ClaimNext",
				"run_id", model.ID,
				"error", leaseErr,
			)
		}
	}

	if !model.Quarantine {
		metrics.JobsActive.WithLabelValues(req.jobID.String()).Inc()
		s.startedMu.Lock()
		s.startedRuns[model.ID] = struct{}{}
		s.startedMu.Unlock()
	}

	return s.loadRun(model.ID)
}

func (s *Store) mutateTaskExecutionDescriptor(runID, taskID uuid.UUID, mutate func(*models.TaskExecutionDescriptor)) error {
	if mutate == nil {
		return nil
	}

	for attempt := 0; attempt <= len(storeBusyRetryBackoffs); attempt++ {
		var taskRun models.TaskRun
		if err := s.db.Select("execution_descriptor").Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRun).Error; err != nil {
			return err
		}
		previous := append([]byte(nil), taskRun.ExecutionDescriptor...)
		desc := models.TaskExecutionDescriptor{
			SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
			CapturedAt:    time.Now().UTC(),
		}
		if len(previous) > 0 {
			if err := json.Unmarshal(previous, &desc); err != nil {
				return fmt.Errorf("run: decode task execution descriptor: %w", err)
			}
		}
		mutate(&desc)
		encoded, err := json.Marshal(&desc)
		if err != nil {
			return fmt.Errorf("run: encode task execution descriptor: %w", err)
		}
		if bytes.Equal(previous, encoded) {
			return nil
		}

		update := s.db.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID)
		if len(previous) == 0 {
			update = update.Where("(execution_descriptor IS NULL OR execution_descriptor = '')")
		} else {
			update = update.Where("execution_descriptor = ?", string(previous))
		}
		result := update.Update("execution_descriptor", datatypes.JSON(encoded))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return nil
		}
	}
	return fmt.Errorf("run: update task execution descriptor: concurrent mutation did not settle")
}

func mergeDescriptorSecretRefs(existing, updates []models.TaskExecutionSecretRef) []models.TaskExecutionSecretRef {
	if len(existing) == 0 {
		return append([]models.TaskExecutionSecretRef(nil), updates...)
	}
	merged := append([]models.TaskExecutionSecretRef(nil), existing...)
	index := make(map[string]int, len(merged))
	for i, ref := range merged {
		index[ref.EnvKey+"\x00"+ref.Ref] = i
	}
	for _, ref := range updates {
		key := ref.EnvKey + "\x00" + ref.Ref
		if i, ok := index[key]; ok {
			merged[i] = ref
			continue
		}
		index[key] = len(merged)
		merged = append(merged, ref)
	}
	return merged
}

func (s *Store) Start(jobID uuid.UUID, triggerID *uuid.UUID, opts ...StartOption) (*JobRun, error) {
	startOpts := startOptionsFrom(opts)
	return s.startRun(startRunRequest{
		jobID:            jobID,
		triggerID:        triggerID,
		params:           startOpts.Params,
		priorityOverride: startOpts.Priority,
	})
}

func (s *Store) AdmitRun(jobID uuid.UUID, triggerID *uuid.UUID, opts ...StartOption) (*JobRun, bool, error) {
	startOpts := startOptionsFrom(opts)
	r, err := s.startRun(startRunRequest{
		jobID:            jobID,
		triggerID:        triggerID,
		params:           startOpts.Params,
		priorityOverride: startOpts.Priority,
		policyOnly:       true,
	})
	if r == nil && err == nil {
		return nil, false, nil
	}
	return r, true, err
}

// StartForBackfill creates a JobRun pre-linked to a backfill ID. The caller
// should then execute the job with run.WithContext(ctx, r.ID) so the executor
// resumes from this pre-created record rather than creating a new one.
func (s *Store) StartForBackfill(jobID, backfillID uuid.UUID, params map[string]string) (*JobRun, error) {
	return s.startRun(startRunRequest{
		jobID:      jobID,
		backfillID: &backfillID,
		params:     params,
	})
}

// StartQueuedRun creates a fresh JobRun from an already-claimed run_queue row.
// If a slot disappears between dequeue and insert, the caller should release the
// queue row so a later drain can retry it.
func (s *Store) StartQueuedRun(_ context.Context, queued *models.RunQueue) (*JobRun, error) {
	if queued == nil {
		return nil, errors.New("run: queued run is nil")
	}
	return s.startRun(startRunRequest{
		jobID:            queued.JobID,
		params:           decodeRunParams(queued.Params),
		priorityOverride: PriorityLabel(queued.Priority),
		fromQueue:        true,
	})
}

func (s *Store) RegisterTask(runID uuid.UUID, task *models.Task, atom *models.Atom, outstanding int) error {
	return s.RegisterTasks(runID, []RegisterTaskInput{{
		Task:                    task,
		Atom:                    atom,
		OutstandingPredecessors: outstanding,
	}})
}

func (s *Store) RegisterTasks(runID uuid.UUID, inputs []RegisterTaskInput) error {
	if len(inputs) == 0 {
		metrics.TaskRegisterBatchSize.Observe(0)
		return nil
	}

	taskIDs := make([]uuid.UUID, 0, len(inputs))
	seenInputTaskIDs := make(map[uuid.UUID]struct{}, len(inputs))
	for _, input := range inputs {
		if input.Task == nil || input.Atom == nil {
			return errors.New("run: task and atom must be provided")
		}
		if _, ok := seenInputTaskIDs[input.Task.ID]; ok {
			continue
		}
		seenInputTaskIDs[input.Task.ID] = struct{}{}
		taskIDs = append(taskIDs, input.Task.ID)
	}

	var jobRun models.JobRun
	if err := s.db.Select("id", "job_id", "params", "trigger_id", "trigger_type", "trigger_alias", "priority", "quarantine").First(&jobRun, "id = ?", runID).Error; err != nil {
		return fmt.Errorf("run: job run %s not found: %w", runID, err)
	}
	jobID := jobRun.JobID
	if !jobRun.Quarantine {
		metrics.TaskRegisterBatchSize.Observe(float64(len(inputs)))
	}

	var job models.Job
	jobFound := true
	if err := s.db.Select("id", "alias", "labels", "annotations", "schema_validation", "cache_config", "replay_safe", "max_parallel_tasks", "task_timeout", "run_timeout", "sla").First(&job, "id = ?", jobID).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		jobFound = false
	}

	envCache := cache.ConfigFromEnv()
	jobCacheConfig := interface{}(nil)
	if jobFound {
		jobCacheConfig = decodeCacheConfig(job.CacheConfig)
	}

	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		var attemptEvents []event.Event
		err := s.db.Transaction(func(tx *gorm.DB) error {
			existingTaskIDs := make([]uuid.UUID, 0)
			if len(taskIDs) > 0 {
				if err := tx.Model(&models.TaskRun{}).
					Where("job_run_id = ? AND task_id IN ?", runID, taskIDs).
					Pluck("task_id", &existingTaskIDs).Error; err != nil {
					return err
				}
			}
			existing := make(map[uuid.UUID]struct{}, len(existingTaskIDs))
			for _, taskID := range existingTaskIDs {
				existing[taskID] = struct{}{}
			}

			records := make([]models.TaskRun, 0, len(inputs))
			readyEvents := make([]event.Event, 0, len(inputs))
			seenNewTaskIDs := make(map[uuid.UUID]struct{}, len(inputs))
			for _, input := range inputs {
				task := input.Task
				atom := input.Atom
				if _, ok := existing[task.ID]; ok {
					continue
				}
				if _, ok := seenNewTaskIDs[task.ID]; ok {
					continue
				}
				seenNewTaskIDs[task.ID] = struct{}{}

				command := atom.Command
				if command == "" {
					if cmd := atom.Cmd(); len(cmd) > 0 {
						if encoded, marshalErr := json.Marshal(cmd); marshalErr == nil {
							command = string(encoded)
						}
					}
				}

				maxAttempts := task.Retries + 1
				if maxAttempts < 1 {
					maxAttempts = 1
				}

				schemaValidation := ""
				if jobFound && len(task.OutputSchema) > 0 {
					schemaValidation = job.SchemaValidation
				}

				resolvedCache := jobdefschema.ResolveCacheConfig(
					decodeCacheConfig(task.CacheConfig),
					jobCacheConfig,
					envCache.Enabled,
					envCache.TTL,
					envCache.PinDigests,
					envCache.DigestTTL,
				)
				replaySafe := task.ReplaySafe || atom.ReplaySafe
				if jobFound && job.ReplaySafe {
					replaySafe = true
				}
				descriptor, descriptorErr := s.initialTaskExecutionDescriptorTx(
					tx,
					jobRun,
					job,
					jobFound,
					task,
					atom,
					input.OutstandingPredecessors,
					resolvedCache,
					replaySafe,
				)
				if descriptorErr != nil {
					return descriptorErr
				}

				records = append(records, models.TaskRun{
					ID:                      uuid.New(),
					JobRunID:                runID,
					TaskID:                  task.ID,
					AtomID:                  task.AtomID,
					Engine:                  atom.Engine,
					Image:                   atom.Image,
					Command:                 command,
					Status:                  string(TaskStatusPending),
					Priority:                jobRun.Priority,
					NodeSelector:            maps.Clone(task.NodeSelector),
					Attempt:                 1,
					MaxAttempts:             maxAttempts,
					OutstandingPredecessors: input.OutstandingPredecessors,
					CacheEnabled:            resolvedCache.Enabled,
					CacheTTL:                resolvedCache.TTL,
					CacheVersion:            resolvedCache.Version,
					ReplaySafe:              replaySafe,
					CachePinDigests:         resolvedCache.PinDigests,
					CacheDigestTTL:          resolvedCache.DigestTTL,
					OutputSchema:            append(datatypes.JSON(nil), task.OutputSchema...),
					SchemaValidation:        schemaValidation,
					Quarantine:              jobRun.Quarantine,
					ExecutionDescriptor:     descriptor,
				})

				if input.OutstandingPredecessors == 0 && s.eventStore != nil {
					readyEvents = append(readyEvents, event.Event{
						Type:       event.TypeTaskReady,
						JobID:      jobID,
						RunID:      runID,
						TaskID:     task.ID,
						Timestamp:  time.Now().UTC(),
						Quarantine: jobRun.Quarantine,
					})
				}
			}

			if len(records) == 0 {
				return nil
			}
			if err := tx.Create(&records).Error; err != nil {
				return err
			}
			counts.addTaskRunInsert(len(records))
			if len(readyEvents) > 0 {
				eventRecords := make([]models.ExecutionEvent, 0, len(readyEvents))
				for _, evt := range readyEvents {
					eventRecords = append(eventRecords, executionEventRecord(evt))
				}
				if err := tx.Create(&eventRecords).Error; err != nil {
					return err
				}
				counts.addEventInsert(len(eventRecords))
				for idx := range readyEvents {
					readyEvents[idx].Sequence = eventRecords[idx].Sequence
					readyEvents[idx].Timestamp = eventRecords[idx].CreatedAt
				}
				attemptEvents = readyEvents
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

func executionEventRecord(evt event.Event) models.ExecutionEvent {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	record := models.ExecutionEvent{
		Type:               string(evt.Type),
		Payload:            []byte(evt.Payload),
		Quarantine:         evt.Quarantine,
		BusDispatchPending: true,
		CreatedAt:          evt.Timestamp,
	}
	if evt.JobID != uuid.Nil {
		jobID := evt.JobID
		record.JobID = &jobID
	}
	if evt.RunID != uuid.Nil {
		runID := evt.RunID
		record.RunID = &runID
	}
	if evt.TaskID != uuid.Nil {
		taskID := evt.TaskID
		record.TaskID = &taskID
	}
	return record
}

func (s *Store) initialTaskExecutionDescriptorTx(
	tx *gorm.DB,
	jobRun models.JobRun,
	job models.Job,
	jobFound bool,
	task *models.Task,
	atom *models.Atom,
	outstanding int,
	cacheCfg jobdefschema.CacheConfig,
	replaySafe bool,
) (datatypes.JSON, error) {
	if task == nil || atom == nil {
		return nil, errors.New("run: descriptor requires task and atom")
	}

	spec := atom.ContainerSpec()
	trigger := models.Trigger{}
	if jobRun.TriggerID != uuid.Nil {
		_ = tx.Select("id", "type", "alias", "configuration").First(&trigger, "id = ?", jobRun.TriggerID).Error
	}

	predecessors, successors, edgeMode, err := s.taskDescriptorEdgesTx(tx, *task)
	if err != nil {
		return nil, err
	}

	triggerRule := normalizedTriggerRule(task.TriggerRule)
	taskType := task.Type
	if taskType == "" {
		taskType = "task"
	}

	runParams := decodeRunParams(jobRun.Params)
	command := atom.Cmd()
	if len(command) == 0 && atom.Command != "" {
		command = []string{atom.Command}
	}

	jobAlias := ""
	jobLabels := map[string]string(nil)
	jobAnnotations := map[string]string(nil)
	var jobSLA datatypes.JSON
	var jobCache datatypes.JSON
	var triggerConfig datatypes.JSONMap
	maxParallel := 0
	taskTimeout := time.Duration(0)
	runTimeout := time.Duration(0)
	schemaValidation := ""
	if jobFound {
		jobAlias = job.Alias
		jobLabels = jsonmap.ToStringMap(job.Labels)
		jobAnnotations = jsonmap.ToStringMap(job.Annotations)
		jobSLA = append(datatypes.JSON(nil), job.SLA...)
		jobCache = append(datatypes.JSON(nil), job.CacheConfig...)
		maxParallel = job.MaxParallelTasks
		taskTimeout = job.TaskTimeout
		runTimeout = job.RunTimeout
		schemaValidation = job.SchemaValidation
	}
	if strings.TrimSpace(trigger.Configuration) != "" {
		_ = json.Unmarshal([]byte(trigger.Configuration), &triggerConfig)
	}

	descriptor := models.TaskExecutionDescriptor{
		SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
		CapturedAt:    time.Now().UTC(),
		Baseline: models.TaskExecutionBaseline{
			JobID:         jobRun.JobID,
			JobAlias:      jobAlias,
			TaskID:        task.ID,
			TaskName:      task.Name,
			AtomID:        atom.ID,
			BaselineRunID: jobRun.ID,
			TriggerID:     jobRun.TriggerID,
			TriggerType:   firstNonEmpty(jobRun.TriggerType, string(trigger.Type)),
			TriggerAlias:  firstNonEmpty(jobRun.TriggerAlias, trigger.Alias),
			ReplaySafe:    replaySafe,
			Quarantine:    jobRun.Quarantine,
		},
		DAG: models.TaskExecutionDAG{
			Predecessors:            predecessors,
			Successors:              successors,
			TriggerRule:             triggerRule,
			BranchBehavior:          taskType,
			EdgeMode:                edgeMode,
			TaskPosition:            task.Position,
			OutstandingPredecessors: outstanding,
		},
		Run: models.TaskExecutionRun{
			Params: runParams,
		},
		Runtime: models.TaskExecutionRuntime{
			Engine:       atom.Engine,
			Image:        atom.Image,
			Command:      command,
			CommandRaw:   atom.Command,
			WorkDir:      spec.WorkDir,
			TaskType:     taskType,
			NodeSelector: jsonmap.ToStringMap(task.NodeSelector),
			RetryCount:   task.Retries,
			RetryDelay:   task.RetryDelay,
			RetryBackoff: task.RetryBackoff,
		},
		Timing: models.TaskExecutionTiming{
			TaskTimeout: taskTimeout,
			RunTimeout:  runTimeout,
		},
		Cache: models.TaskExecutionCache{
			Enabled:    cacheCfg.Enabled,
			TTL:        cacheCfg.TTL,
			Version:    cacheCfg.Version,
			PinDigests: cacheCfg.PinDigests,
			DigestTTL:  cacheCfg.DigestTTL,
		},
		Schema: models.TaskExecutionSchema{
			InputSchema:    append(datatypes.JSON(nil), task.InputSchema...),
			OutputSchema:   append(datatypes.JSON(nil), task.OutputSchema...),
			ValidationMode: schemaValidation,
		},
		Job: models.TaskExecutionJob{
			MaxParallelTasks: maxParallel,
			Labels:           jobLabels,
			Annotations:      jobAnnotations,
			SLA:              jobSLA,
			CacheDefaults:    jobCache,
			TriggerConfig:    triggerConfig,
		},
		ContainerSpec:  spec,
		KubernetesSpec: spec.Kubernetes,
		SecretRefs:     descriptorSecretRefs(spec),
	}

	encoded, err := json.Marshal(&descriptor)
	if err != nil {
		return nil, fmt.Errorf("run: marshal task execution descriptor: %w", err)
	}
	return datatypes.JSON(encoded), nil
}

func (s *Store) taskDescriptorEdgesTx(tx *gorm.DB, task models.Task) ([]models.TaskExecutionEdgeRef, []models.TaskExecutionEdgeRef, string, error) {
	var edgeCount int64
	if err := tx.Model(&models.TaskEdge{}).Where("job_id = ?", task.JobID).Count(&edgeCount).Error; err != nil {
		return nil, nil, "", err
	}
	mode := "explicit"
	if edgeCount == 0 {
		mode = "implicit_sequential"
	}

	predecessors := make([]models.TaskExecutionEdgeRef, 0)
	successors := make([]models.TaskExecutionEdgeRef, 0)
	if mode == "explicit" {
		var predEdges []models.TaskEdge
		if err := tx.Where("to_task_id = ?", task.ID).Find(&predEdges).Error; err != nil {
			return nil, nil, "", err
		}
		for _, edge := range predEdges {
			ref, err := taskEdgeRefTx(tx, edge.FromTaskID)
			if err != nil {
				return nil, nil, "", err
			}
			predecessors = append(predecessors, ref)
		}
		var succEdges []models.TaskEdge
		if err := tx.Where("from_task_id = ?", task.ID).Find(&succEdges).Error; err != nil {
			return nil, nil, "", err
		}
		for _, edge := range succEdges {
			ref, err := taskEdgeRefTx(tx, edge.ToTaskID)
			if err != nil {
				return nil, nil, "", err
			}
			successors = append(successors, ref)
		}
		return predecessors, successors, mode, nil
	}

	var prev models.Task
	if err := tx.
		Where("job_id = ? AND (position < ? OR (position = ? AND created_at < ?))", task.JobID, task.Position, task.Position, task.CreatedAt).
		Order("position DESC").
		Order("created_at DESC").
		First(&prev).Error; err == nil {
		predecessors = append(predecessors, models.TaskExecutionEdgeRef{TaskID: prev.ID, TaskName: prev.Name})
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, "", err
	}
	var next models.Task
	if err := tx.
		Where("job_id = ? AND (position > ? OR (position = ? AND created_at > ?))", task.JobID, task.Position, task.Position, task.CreatedAt).
		Order("position ASC").
		Order("created_at ASC").
		First(&next).Error; err == nil {
		successors = append(successors, models.TaskExecutionEdgeRef{TaskID: next.ID, TaskName: next.Name})
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, "", err
	}
	return predecessors, successors, mode, nil
}

func taskEdgeRefTx(tx *gorm.DB, taskID uuid.UUID) (models.TaskExecutionEdgeRef, error) {
	var task models.Task
	if err := tx.Select("id", "name").First(&task, "id = ?", taskID).Error; err != nil {
		return models.TaskExecutionEdgeRef{}, err
	}
	return models.TaskExecutionEdgeRef{TaskID: task.ID, TaskName: task.Name}, nil
}

func decodeRunParams(raw datatypes.JSON) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var params map[string]string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil
	}
	return params
}

func descriptorSecretRefs(spec container.Spec) []models.TaskExecutionSecretRef {
	if len(spec.Env) == 0 {
		return nil
	}
	refs := make([]models.TaskExecutionSecretRef, 0)
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ref := strings.TrimSpace(spec.Env[key])
		if !strings.HasPrefix(ref, "secret://") {
			continue
		}
		refs = append(refs, models.TaskExecutionSecretRef{
			Ref:        ref,
			EnvKey:     key,
			Verifiable: false,
			// The pre-exec descriptor only knows the secret reference; the
			// provider identity is finalized after executeAtom/executeTask
			// resolves the value at container-create time.
			UnverifiableReason: "secret identity not resolved yet",
		})
	}
	return refs
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Store) StartTask(runID, taskID uuid.UUID, runtimeID string) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			result := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ? AND status NOT IN ?", runID, taskID, terminalTaskStatuses()).
				Updates(map[string]interface{}{
					"status":                 string(TaskStatusRunning),
					"runtime_id":             runtimeID,
					"started_at":             now,
					"rate_limit_retry_after": nil,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				// The task is already terminal (e.g. cancelled by a concurrency
				// replace while its orphaned container was starting), so the guarded
				// UPDATE touched no rows. Skip the metric bump and the TypeTaskStarted
				// event — emitting one here would publish a phantom task_started for a
				// task that is still cancelled in the DB. Mirrors StartTaskClaimed's
				// RowsAffected==0 gate (which returns a claim mismatch; a local no-op
				// is benign, so return nil).
				return nil
			}
			counts.addTaskRunStatus(1)
			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskStarted, runID, taskID, &counts)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

// ClaimTaskForDispatch is the Phase 2 dispatch-side equivalent of ClaimNext for
// a specific task.  It atomically transitions a pending task from
// (status=pending, claimed_by="") → (status=running, claimed_by=workerNode)
// in a single UPDATE, mirroring what ClaimNext does but targeting a known task
// rather than picking the next available one.
//
// The ownerGeneration argument is stamped onto owner_generation so subsequent
// coordination writes can fence against a stale owner.  The WHERE clause
// includes `AND owner_generation <= ?` to encode the monotonic-generation
// invariant: a row last touched by the current owner or any *older* generation
// is claimable (this covers pre-Phase-2A rows at implicit generation 0, normal
// re-claims at the same generation, and — critically — failover, where a new
// owner at generation N+1 must re-claim an in-flight task its predecessor
// stamped at generation N).  A row stamped by a *newer* generation means the
// claimer is itself stale, so the claim is rejected.
//
// Returns ErrTaskClaimMismatch if the task was not in the expected state
// (already claimed, wrong status, wrong run, stale generation).  The caller
// should fall back to writing the task with claimed_by="" and letting
// ClaimNext pick it up.
func (s *Store) ClaimTaskForDispatch(runID, taskID uuid.UUID, workerNode string, ownerGeneration int64, leaseTTL time.Duration, trustOwnerReadiness bool) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			leaseExpiry := now.Add(leaseTTL)

			// The owner is authoritative for readiness.  In SQL mode the owner
			// dispatches only outstanding_predecessors=0 tasks, so the check is a
			// redundant safety net.  In in-memory mode the owner advanced the DAG
			// in memory and did NOT decrement the DB counter, so the dispatched
			// successor still shows outstanding>0 here — trustOwnerReadiness drops
			// the predecessor check so the claim reflects the owner's decision.
			where := "job_run_id = ? AND task_id = ? AND status = ? AND claimed_by = '' AND outstanding_predecessors = 0 AND owner_generation <= ? AND (rate_limit_retry_after IS NULL OR rate_limit_retry_after <= ?)"
			if trustOwnerReadiness {
				where = "job_run_id = ? AND task_id = ? AND status = ? AND claimed_by = '' AND owner_generation <= ? AND (rate_limit_retry_after IS NULL OR rate_limit_retry_after <= ?)"
			}
			result := tx.Model(&models.TaskRun{}).
				Where(where, runID, taskID, string(TaskStatusPending), ownerGeneration, now).
				Updates(map[string]interface{}{
					"status":                 string(TaskStatusRunning),
					"claimed_by":             workerNode,
					"claim_expires_at":       leaseExpiry,
					"claim_attempt":          gorm.Expr("claim_attempt + 1"),
					"started_at":             now,
					"owner_generation":       ownerGeneration,
					"rate_limit_retry_after": nil,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			counts.addTaskRunStatus(1)

			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskStarted, runID, taskID, &counts)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

// PendingTasksForDispatch returns up to limit task_runs rows for runID that
// are ready for owner-push dispatch: status=pending, claimed_by="", and
// outstanding_predecessors=0.  The caller (the dispatch loop) uses this to
// find the next batch of tasks to push to workers each tick.
//
// The result is ordered by created_at ASC so earlier-registered tasks are
// dispatched first, preserving FIFO ordering within a run.  The limit cap
// prevents a huge fan-out from saturating a single tick.
func (s *Store) PendingTasksForDispatch(ctx context.Context, runID uuid.UUID, limit int) ([]models.TaskRun, error) {
	if limit <= 0 {
		limit = 64
	}
	var tasks []models.TaskRun
	err := s.db.WithContext(ctx).
		Where("job_run_id = ? AND status = ? AND claimed_by = '' AND outstanding_predecessors = 0 AND (rate_limit_retry_after IS NULL OR rate_limit_retry_after <= ?)",
			runID, string(TaskStatusPending), time.Now().UTC()).
		Order("created_at ASC").
		Limit(limit).
		Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// LoadDispatchedTaskRun loads the full task_runs row for a task that was just
// claimed for dispatch.  The (claimedBy, status=running) predicate ensures the
// row really is the one this node claimed via ClaimTaskForDispatch and not a
// row another node has since reclaimed.  Returns ErrTaskClaimMismatch if no
// matching running row exists.  The dispatch handler uses this to obtain the
// full execution spec (image/command/engine/etc.) to hand to the worker pool.
func (s *Store) LoadDispatchedTaskRun(runID, taskID uuid.UUID, claimedBy string) (*models.TaskRun, error) {
	var taskRun models.TaskRun
	err := s.db.
		Where("job_run_id = ? AND task_id = ? AND claimed_by = ? AND status = ?",
			runID, taskID, claimedBy, string(TaskStatusRunning)).
		First(&taskRun).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskClaimMismatch
		}
		return nil, err
	}
	return &taskRun, nil
}

// ReleaseTaskClaim reverts a task this node claimed for dispatch back to the
// dispatchable pending state (status=running → pending, claimed_by="",
// claim_expires_at=nil, runtime_id="", started_at=nil).  It is the rollback
// used by HandleDispatch when the local worker cannot accept a just-claimed
// task (buffer full / worker not running): rather than leave the task
// claimed-but-orphaned, the owner returns it to the pool so the next dispatch
// tick re-dispatches it (to this or another peer).
//
// The owner_generation predicate keeps the release fenced: only the owner that
// stamped the row (or a legacy generation-0 row) can release it.  The status
// and claimed_by predicates make the release a no-op (zero rows, no error) if
// the task already advanced — e.g. a completion landed in the race window.
func (s *Store) ReleaseTaskClaim(runID, taskID uuid.UUID, claimedBy string, ownerGeneration int64) error {
	return withStoreBusyRetry(func() error {
		result := s.db.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ? AND claimed_by = ? AND status = ? AND (owner_generation = ? OR owner_generation = 0)",
				runID, taskID, claimedBy, string(TaskStatusRunning), ownerGeneration).
			Updates(map[string]interface{}{
				"status":           string(TaskStatusPending),
				"claimed_by":       "",
				"claim_expires_at": nil,
				"runtime_id":       "",
				"started_at":       nil,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			metrics.DBWritesTotal.WithLabelValues(metrics.DBWriteCategoryTaskRunStatus).Add(float64(result.RowsAffected))
			metrics.DBStatementsTotal.WithLabelValues(metrics.DBWriteCategoryTaskRunStatus).Inc()
		}
		return nil
	})
}

// RateLimitTask leaves a task pending until retryAfter so rate-limit rejections
// do not hold worker capacity or spin through immediate reclaims.
func (s *Store) RateLimitTask(ctx context.Context, runID, taskID uuid.UUID, retryAfter time.Time) error {
	if retryAfter.IsZero() {
		retryAfter = time.Now().UTC()
	}
	retryAfter = retryAfter.UTC()

	return withStoreBusyRetry(func() error {
		result := s.db.WithContext(ctx).Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ? AND status IN ?", runID, taskID, []string{string(TaskStatusPending), string(TaskStatusRunning)}).
			Updates(map[string]interface{}{
				"status":                 string(TaskStatusPending),
				"claimed_by":             "",
				"claim_expires_at":       nil,
				"runtime_id":             "",
				"started_at":             nil,
				"rate_limit_retry_after": retryAfter,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			metrics.DBWritesTotal.WithLabelValues(metrics.DBWriteCategoryTaskRunStatus).Add(float64(result.RowsAffected))
			metrics.DBStatementsTotal.WithLabelValues(metrics.DBWriteCategoryTaskRunStatus).Inc()
		}
		return nil
	})
}

func (s *Store) StartTaskClaimed(runID, taskID uuid.UUID, runtimeID, claimedBy string) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			result := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ? AND claimed_by = ? AND status = ?", runID, taskID, claimedBy, string(TaskStatusRunning)).
				Updates(map[string]interface{}{
					"runtime_id":             runtimeID,
					"started_at":             now,
					"rate_limit_retry_after": nil,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			counts.addTaskRunStatus(1)
			if s.eventStore != nil {
				evt, err := s.recordTaskEventTx(tx, event.TypeTaskStarted, runID, taskID, &counts)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) CompleteTask(runID, taskID uuid.UUID, result string, output map[string]string, branchSelections []string) error {
	skipped, err := s.completeTask(runID, taskID, result, "", false, output, branchSelections)
	_ = skipped
	return err
}

// CompleteTaskResult holds the result of a task completion, including any
// tasks that were skipped due to branch filtering.
type CompleteTaskResult struct {
	SkippedTaskIDs []uuid.UUID
}

type TaskLogSnapshot struct {
	Text      string
	Truncated bool
}

type CacheHitSource struct {
	RunID     uuid.UUID
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// CompleteTaskWithResult completes a task and returns details about branch
// skips so the local executor can update its in-memory state.
func (s *Store) CompleteTaskWithResult(runID, taskID uuid.UUID, result string, output map[string]string, branchSelections []string) (*CompleteTaskResult, error) {
	skipped, err := s.completeTask(runID, taskID, result, "", false, output, branchSelections)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped}, nil
}

func (s *Store) CompleteTaskClaimed(runID, taskID uuid.UUID, result, claimedBy string, output map[string]string, branchSelections []string) error {
	_, err := s.completeTask(runID, taskID, result, claimedBy, true, output, branchSelections)
	return err
}

// CacheHitTask marks a task as completed via cache hit (local mode).
// It mirrors the CompleteTaskWithResult flow but sets status to "cached".
func (s *Store) CacheHitTask(runID, taskID uuid.UUID, source CacheHitSource, result string, output map[string]string, branchSelections []string) (*CompleteTaskResult, error) {
	skipped, err := s.cacheHitTask(runID, taskID, source, result, "", false, output, branchSelections)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped}, nil
}

// CacheHitTaskClaimed marks a claimed task as completed via cache hit (distributed mode).
func (s *Store) CacheHitTaskClaimed(runID, taskID uuid.UUID, source CacheHitSource, result, claimedBy string, output map[string]string, branchSelections []string) error {
	_, err := s.cacheHitTask(runID, taskID, source, result, claimedBy, true, output, branchSelections)
	return err
}

func (s *Store) cacheHitTask(runID, taskID uuid.UUID, source CacheHitSource, result, claimedBy string, enforceClaim bool, output map[string]string, branchSelections []string) ([]uuid.UUID, error) {
	var pendingEvents []event.Event
	var skippedTaskIDs []uuid.UUID
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8)
		attemptSkippedTaskIDs := make([]uuid.UUID, 0)

		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			// Verify the task run exists (and matches claim if enforced).
			var taskRun models.TaskRun
			taskQuery := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				taskQuery = taskQuery.Where("claimed_by = ?", claimedBy)
			}
			if err := taskQuery.First(&taskRun).Error; err != nil {
				if enforceClaim {
					return ErrTaskClaimMismatch
				}
				return err
			}
			if IsTerminal(TaskStatus(taskRun.Status)) {
				return nil
			}

			updateQuery := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}

			updates := map[string]interface{}{
				"status":              string(TaskStatusCached),
				"completed_at":        now,
				"result":              result,
				"cache_hit":           true,
				"cache_origin_run_id": source.RunID,
				"cache_created_at":    source.CreatedAt,
				"cache_expires_at":    source.ExpiresAt,
			}
			if len(output) > 0 {
				encoded, marshalErr := json.Marshal(output)
				if marshalErr != nil {
					return fmt.Errorf("marshalling task output: %w", marshalErr)
				}
				updates["output"] = encoded
			}
			if len(branchSelections) > 0 {
				encoded, marshalErr := json.Marshal(branchSelections)
				if marshalErr != nil {
					return fmt.Errorf("marshalling branch selections: %w", marshalErr)
				}
				updates["branch_selections"] = encoded
			}

			resultUpdate := updateQuery.Updates(updates)
			if resultUpdate.Error != nil {
				return resultUpdate.Error
			}
			if enforceClaim && resultUpdate.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			counts.addTaskRunStatus(1)

			descriptor, replayTask, err := s.replayTaskExecutionDescriptorTx(tx, runID, taskID)
			if err != nil {
				return err
			}

			// Load the task model for edge fallback and branch detection.
			var taskModel models.Task
			taskType := ""
			if replayTask {
				taskModel = models.Task{ID: taskID}
				taskType = firstNonEmpty(descriptor.Runtime.TaskType, descriptor.DAG.BranchBehavior, "task")
			} else {
				if err := tx.First(&taskModel, "id = ?", taskID).Error; err != nil {
					return err
				}
				taskType = taskModel.Type
			}

			edges, err := s.successorEdgesForRunTx(tx, runID, taskID, taskModel)
			if err != nil {
				return err
			}

			// Determine branch filtering if this is a branch-type task.
			var branchSelectedIDs map[uuid.UUID]bool
			if len(edges) > 0 && taskType == "branch" {
				successorNameToID, _, err := s.successorNameMapTx(tx, replayTask, descriptor, edges)
				if err != nil {
					return err
				}

				branchSelectedIDs = make(map[uuid.UUID]bool, len(branchSelections))
				for _, name := range branchSelections {
					if id, ok := successorNameToID[name]; ok {
						branchSelectedIDs[id] = true
					}
				}
			}

			// Partition edges: skipped (branch-filtered) vs. predecessors to decrement.
			var toDecrementIDs []uuid.UUID
			for _, edge := range edges {
				if branchSelectedIDs != nil && !branchSelectedIDs[edge.ToTaskID] {
					reason := fmt.Sprintf("not selected by branch task %s", taskID)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, reason, &attemptEvents, &counts)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
					continue
				}
				toDecrementIDs = append(toDecrementIDs, edge.ToTaskID)
			}

			// Batch-decrement outstanding_predecessors for all non-skipped successors.
			updatedSuccessors, err := s.batchDecrementPredecessorsTx(tx, runID, toDecrementIDs)
			if err != nil {
				return err
			}
			counts.addTaskRunStatus(len(toDecrementIDs))

			// Collect all events to emit (task_cached + task_ready for newly-ready successors).
			var batchEvts []*event.Event

			// Evaluate trigger rules and collect task_ready events.
			for i := range updatedSuccessors {
				successor := &updatedSuccessors[i]
				if successor.OutstandingPredecessors != 0 || successor.Status != string(TaskStatusPending) {
					continue
				}
				shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, successor.TaskID)
				if err != nil {
					return err
				}
				if shouldRun {
					var jobRun models.JobRun
					if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
						return err
					}
					readyEvt := &event.Event{
						Type:      event.TypeTaskReady,
						JobID:     jobRun.JobID,
						RunID:     runID,
						TaskID:    successor.TaskID,
						Timestamp: time.Now().UTC(),
					}
					batchEvts = append(batchEvts, readyEvt)
					continue
				}

				skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", rule)
				skipped, err := s.skipTaskAndDescendantsTx(tx, runID, successor.TaskID, skipRuleReason, &attemptEvents, &counts)
				if err != nil {
					return err
				}
				attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
			}

			if s.eventStore != nil {
				// Build task_cached event and add to batch.
				var taskRunModel models.TaskRun
				if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRunModel).Error; err != nil {
					return err
				}
				var jobRun models.JobRun
				if err := tx.Preload("Job").First(&jobRun, "id = ?", runID).Error; err != nil {
					return err
				}
				taskPayload := convertRunTaskModel(&taskRunModel)
				taskPayload.JobAlias = jobRun.Job.Alias
				taskPayload.JobLabels = jsonmap.ToStringMap(jobRun.Job.Labels)
				taskPayload.ID = taskRunModel.ID
				payload, marshalErr := json.Marshal(taskPayload)
				if marshalErr != nil {
					return marshalErr
				}
				cachedEvt := &event.Event{
					Type:      event.TypeTaskCached,
					JobID:     jobRun.JobID,
					RunID:     runID,
					TaskID:    taskID,
					Timestamp: time.Now().UTC(),
					Payload:   payload,
				}
				// task_cached goes first so sequence ordering is consistent.
				batchEvts = append([]*event.Event{cachedEvt}, batchEvts...)

				if err := s.appendBatchEventsTx(tx, batchEvts, &attemptEvents, &counts); err != nil {
					return err
				}
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			skippedTaskIDs = attemptSkippedTaskIDs
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return skippedTaskIDs, err
}

func (s *Store) SaveTaskLogSnapshot(runID, taskID uuid.UUID, snapshot *TaskLogSnapshot) error {
	if snapshot == nil {
		return nil
	}

	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Updates(map[string]interface{}{
			"log_text":      snapshot.Text,
			"log_truncated": snapshot.Truncated,
		}).Error
}

// SaveSchemaViolations persists schema validation violations for a task run.
func (s *Store) SaveSchemaViolations(runID, taskID uuid.UUID, violations []pkgtask.SchemaViolation) error {
	if len(violations) == 0 {
		return nil
	}
	b, err := json.Marshal(violations)
	if err != nil {
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		Update("schema_violations", datatypes.JSON(b)).Error
}

func (s *Store) GetTaskLogSnapshot(runID, taskID uuid.UUID) (*TaskLogSnapshot, error) {
	var task models.TaskRun
	if err := s.db.
		Select("log_text", "log_truncated").
		Where("job_run_id = ? AND task_id = ?", runID, taskID).
		First(&task).Error; err != nil {
		return nil, err
	}

	if task.LogText == "" && !task.LogTruncated {
		return nil, nil
	}

	return &TaskLogSnapshot{
		Text:      task.LogText,
		Truncated: task.LogTruncated,
	}, nil
}

func (s *Store) successorEdgesForRunTx(tx *gorm.DB, runID, taskID uuid.UUID, task models.Task) ([]models.TaskEdge, error) {
	descriptor, replay, err := s.replayTaskExecutionDescriptorTx(tx, runID, taskID)
	if err != nil {
		return nil, err
	}
	if replay {
		edges := make([]models.TaskEdge, 0, len(descriptor.DAG.Successors))
		for _, successor := range descriptor.DAG.Successors {
			if successor.TaskID == uuid.Nil {
				continue
			}
			edges = append(edges, models.TaskEdge{
				FromTaskID: taskID,
				ToTaskID:   successor.TaskID,
			})
		}
		return edges, nil
	}
	return s.successorEdgesTx(tx, task)
}

func (s *Store) successorNameMapTx(tx *gorm.DB, replayTask bool, descriptor *models.TaskExecutionDescriptor, edges []models.TaskEdge) (map[string]uuid.UUID, []string, error) {
	if replayTask {
		successorNameToID := make(map[string]uuid.UUID, len(edges))
		validTargets := make([]string, 0, len(edges))
		if descriptor == nil {
			return successorNameToID, validTargets, nil
		}
		for _, successor := range descriptor.DAG.Successors {
			if successor.TaskID == uuid.Nil {
				continue
			}
			name := firstNonEmpty(successor.TaskName, successor.TaskID.String())
			successorNameToID[name] = successor.TaskID
			validTargets = append(validTargets, name)
		}
		return successorNameToID, validTargets, nil
	}

	successorIDs := make([]uuid.UUID, 0, len(edges))
	for _, edge := range edges {
		successorIDs = append(successorIDs, edge.ToTaskID)
	}
	var successorTasks []models.Task
	if err := tx.Where("id IN ?", successorIDs).Find(&successorTasks).Error; err != nil {
		return nil, nil, err
	}
	successorNameToID := make(map[string]uuid.UUID, len(successorTasks))
	validTargets := make([]string, 0, len(successorTasks))
	for _, st := range successorTasks {
		if st.Name != "" {
			successorNameToID[st.Name] = st.ID
			validTargets = append(validTargets, st.Name)
		}
	}
	return successorNameToID, validTargets, nil
}

func (s *Store) successorEdgesTx(tx *gorm.DB, task models.Task) ([]models.TaskEdge, error) {
	var edges []models.TaskEdge
	if err := tx.Where("from_task_id = ?", task.ID).Find(&edges).Error; err != nil {
		return nil, err
	}
	if len(edges) > 0 {
		return edges, nil
	}

	var jobEdgeCount int64
	if err := tx.Model(&models.TaskEdge{}).
		Where("job_id = ?", task.JobID).
		Limit(1).
		Count(&jobEdgeCount).Error; err != nil {
		return nil, err
	}
	if jobEdgeCount > 0 {
		return edges, nil
	}

	var next models.Task
	err := tx.Where(
		"job_id = ? AND (position > ? OR (position = ? AND created_at > ?))",
		task.JobID,
		task.Position,
		task.Position,
		task.CreatedAt,
	).
		Order("position asc").
		Order("created_at asc").
		First(&next).Error
	if err == nil {
		edges = append(edges, models.TaskEdge{ToTaskID: next.ID})
		return edges, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return edges, nil
	}
	return nil, err
}

func (s *Store) appendTaskReadyEventTx(tx *gorm.DB, runID, taskID uuid.UUID, pendingEvents *[]event.Event, counts *dbWriteCounts) error {
	if s.eventStore == nil {
		return nil
	}

	var jobRun models.JobRun
	if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
		return err
	}

	evt := event.Event{
		Type:      event.TypeTaskReady,
		JobID:     jobRun.JobID,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: time.Now().UTC(),
	}
	if err := s.stampEventQuarantineTx(tx, &evt); err != nil {
		return err
	}
	if err := s.eventStore.AppendTx(tx, &evt); err != nil {
		return err
	}
	counts.addEventInsert(1)
	*pendingEvents = append(*pendingEvents, evt)
	return nil
}

// batchDecrementPredecessorsTx decrements outstanding_predecessors by 1 for
// all successorIDs in a single UPDATE statement and returns the updated rows.
// This replaces the per-successor UPDATE loop in completeTask and cacheHitTask.
func (s *Store) batchDecrementPredecessorsTx(tx *gorm.DB, runID uuid.UUID, successorIDs []uuid.UUID) ([]models.TaskRun, error) {
	if len(successorIDs) == 0 {
		return nil, nil
	}

	if err := tx.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id IN ?", runID, successorIDs).
		UpdateColumn("outstanding_predecessors", gorm.Expr("CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE 0 END")).Error; err != nil {
		return nil, err
	}

	// SELECT the updated rows to determine which successors hit zero and are still pending.
	var updated []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id IN ?", runID, successorIDs).Find(&updated).Error; err != nil {
		return nil, err
	}
	return updated, nil
}

// appendBatchEventsTx inserts all events in evts with a single INSERT statement.
// It back-fills Sequence and Timestamp on each event and appends them to pendingEvents.
func (s *Store) appendBatchEventsTx(tx *gorm.DB, evts []*event.Event, pendingEvents *[]event.Event, counts *dbWriteCounts) error {
	if s.eventStore == nil || len(evts) == 0 {
		return nil
	}
	if err := s.stampBatchEventQuarantineTx(tx, evts); err != nil {
		return err
	}
	if err := s.eventStore.AppendBatchTx(tx, evts); err != nil {
		return err
	}
	counts.addEventInsert(len(evts))
	for _, e := range evts {
		*pendingEvents = append(*pendingEvents, *e)
	}
	return nil
}

type eventQuarantineKey struct {
	runID  uuid.UUID
	taskID uuid.UUID
}

func (s *Store) stampBatchEventQuarantineTx(tx *gorm.DB, evts []*event.Event) error {
	runIDs := make(map[uuid.UUID]struct{})
	taskIDsByRun := make(map[uuid.UUID]map[uuid.UUID]struct{})
	for _, evt := range evts {
		if tx == nil || evt == nil || evt.RunID == uuid.Nil {
			continue
		}
		runIDs[evt.RunID] = struct{}{}
		if evt.TaskID == uuid.Nil {
			continue
		}
		if taskIDsByRun[evt.RunID] == nil {
			taskIDsByRun[evt.RunID] = make(map[uuid.UUID]struct{})
		}
		taskIDsByRun[evt.RunID][evt.TaskID] = struct{}{}
	}
	if len(runIDs) == 0 {
		return nil
	}

	runQuarantine := make(map[uuid.UUID]bool, len(runIDs))
	var runRows []struct {
		ID         uuid.UUID
		Quarantine bool
	}
	if err := tx.Model(&models.JobRun{}).
		Select("id", "quarantine").
		Where("id IN ?", uuidSetValues(runIDs)).
		Find(&runRows).Error; err != nil {
		return fmt.Errorf("run: stamp event quarantine from job run batch: %w", err)
	}
	for _, row := range runRows {
		runQuarantine[row.ID] = row.Quarantine
	}
	if len(runQuarantine) != len(runIDs) {
		return fmt.Errorf("run: stamp event quarantine from job run batch: %w", gorm.ErrRecordNotFound)
	}

	taskQuarantine := make(map[eventQuarantineKey]bool)
	for runID, taskIDs := range taskIDsByRun {
		ids := uuidSetValues(taskIDs)
		var taskRows []struct {
			JobRunID   uuid.UUID
			TaskID     uuid.UUID
			Quarantine bool
		}
		if err := tx.Model(&models.TaskRun{}).
			Select("job_run_id", "task_id", "quarantine").
			Where("job_run_id = ? AND task_id IN ?", runID, ids).
			Find(&taskRows).Error; err != nil {
			return fmt.Errorf("run: stamp event quarantine from task run batch: %w", err)
		}
		for _, row := range taskRows {
			taskQuarantine[eventQuarantineKey{runID: row.JobRunID, taskID: row.TaskID}] = row.Quarantine
		}
		if len(taskRows) != len(ids) {
			return fmt.Errorf("run: stamp event quarantine from task run batch: %w", gorm.ErrRecordNotFound)
		}
	}

	for _, evt := range evts {
		if evt == nil || evt.RunID == uuid.Nil {
			continue
		}
		quarantined := runQuarantine[evt.RunID]
		if evt.TaskID != uuid.Nil {
			quarantined = quarantined || taskQuarantine[eventQuarantineKey{runID: evt.RunID, taskID: evt.TaskID}]
		}
		evt.Quarantine = quarantined
	}
	return nil
}

func uuidSetValues(set map[uuid.UUID]struct{}) []uuid.UUID {
	values := make([]uuid.UUID, 0, len(set))
	for id := range set {
		values = append(values, id)
	}
	return values
}

func (s *Store) markTaskSkippedTx(tx *gorm.DB, runID, taskID uuid.UUID, reason string, pendingEvents *[]event.Event, counts *dbWriteCounts) (bool, error) {
	result := tx.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(TaskStatusPending)).
		Updates(map[string]interface{}{
			"status":              string(TaskStatusSkipped),
			"completed_at":        time.Now().UTC(),
			"error":               reason,
			"cache_hit":           false,
			"cache_origin_run_id": nil,
			"cache_created_at":    nil,
			"cache_expires_at":    nil,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	counts.addTaskRunStatus(1)

	if s.eventStore != nil {
		evt, err := s.recordTaskEventTx(tx, event.TypeTaskSkipped, runID, taskID, counts)
		if err != nil {
			return false, err
		}
		*pendingEvents = append(*pendingEvents, *evt)
	}

	return true, nil
}

func (s *Store) predecessorStatusesTx(tx *gorm.DB, runID, taskID uuid.UUID) ([]TaskStatus, error) {
	if refs, replay, err := s.replayPredecessorRefsTx(tx, runID, taskID); err != nil {
		return nil, err
	} else if replay {
		if len(refs) == 0 {
			return nil, nil
		}
		predIDs := make([]uuid.UUID, 0, len(refs))
		for _, ref := range refs {
			if ref.TaskID != uuid.Nil {
				predIDs = append(predIDs, ref.TaskID)
			}
		}
		if len(predIDs) == 0 {
			return nil, nil
		}
		var taskRuns []models.TaskRun
		if err := tx.Select("status").Where("job_run_id = ? AND task_id IN ?", runID, predIDs).Find(&taskRuns).Error; err != nil {
			return nil, err
		}
		statuses := make([]TaskStatus, 0, len(taskRuns))
		for _, taskRun := range taskRuns {
			statuses = append(statuses, TaskStatus(taskRun.Status))
		}
		return statuses, nil
	}

	var edges []models.TaskEdge
	if err := tx.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, nil
	}

	predIDs := make([]uuid.UUID, 0, len(edges))
	for _, edge := range edges {
		predIDs = append(predIDs, edge.FromTaskID)
	}

	var taskRuns []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id IN ?", runID, predIDs).Find(&taskRuns).Error; err != nil {
		return nil, err
	}

	statuses := make([]TaskStatus, 0, len(taskRuns))
	for _, taskRun := range taskRuns {
		statuses = append(statuses, TaskStatus(taskRun.Status))
	}
	return statuses, nil
}

func satisfiesTriggerRule(rule string, predStatuses []TaskStatus) bool {
	if rule == "" {
		rule = jobdefschema.TriggerRuleAllSuccess
	}
	if len(predStatuses) == 0 {
		return true
	}

	isTerminal := IsTerminal

	switch rule {
	case jobdefschema.TriggerRuleAllSuccess:
		for _, s := range predStatuses {
			if s != TaskStatusSucceeded && s != TaskStatusCached {
				return false
			}
		}
		return true
	case jobdefschema.TriggerRuleAllDone, jobdefschema.TriggerRuleAlways:
		for _, s := range predStatuses {
			if !isTerminal(s) {
				return false
			}
		}
		return true
	case jobdefschema.TriggerRuleAllFailed:
		for _, s := range predStatuses {
			if s != TaskStatusFailed {
				return false
			}
		}
		return true
	case jobdefschema.TriggerRuleOneSuccess:
		for _, s := range predStatuses {
			if s == TaskStatusSucceeded || s == TaskStatusCached {
				return true
			}
		}
		return false
	default:
		for _, s := range predStatuses {
			if s != TaskStatusSucceeded && s != TaskStatusCached {
				return false
			}
		}
		return true
	}
}

func normalizedTriggerRule(rule string) string {
	if rule == "" {
		return jobdefschema.TriggerRuleAllSuccess
	}
	return rule
}

func (s *Store) shouldRunTaskTx(tx *gorm.DB, runID, taskID uuid.UUID) (bool, string, error) {
	if descriptor, replay, err := s.replayTaskExecutionDescriptorTx(tx, runID, taskID); err != nil {
		return false, "", err
	} else if replay {
		rule := normalizedTriggerRule(descriptor.DAG.TriggerRule)
		predStatuses, err := s.predecessorStatusesTx(tx, runID, taskID)
		if err != nil {
			return false, "", err
		}
		return satisfiesTriggerRule(rule, predStatuses), rule, nil
	}

	var task models.Task
	if err := tx.Select("trigger_rule").First(&task, "id = ?", taskID).Error; err != nil {
		return false, "", err
	}

	predStatuses, err := s.predecessorStatusesTx(tx, runID, taskID)
	if err != nil {
		return false, "", err
	}

	return satisfiesTriggerRule(task.TriggerRule, predStatuses), normalizedTriggerRule(task.TriggerRule), nil
}

func (s *Store) completeTask(runID, taskID uuid.UUID, result, claimedBy string, enforceClaim bool, output map[string]string, branchSelections []string) ([]uuid.UUID, error) {
	var pendingEvents []event.Event
	var skippedTaskIDs []uuid.UUID
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8)
		attemptSkippedTaskIDs := make([]uuid.UUID, 0)

		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			status := taskStatusFromResult(result)

			// Capture task metadata for metrics before updating.
			var taskRun models.TaskRun
			taskQuery := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				taskQuery = taskQuery.Where("claimed_by = ?", claimedBy)
			}
			if err := taskQuery.First(&taskRun).Error; err == nil {
				if IsTerminal(TaskStatus(taskRun.Status)) {
					// A terminal task must not be resurrected by a late completion. In local
					// execution mode a concurrency replace cancels the run's task while its
					// orphaned container is still running; without this guard the container's
					// eventual exit would overwrite cancelled -> succeeded. The claimed path is
					// already protected by the claimed_by guard; this covers the unclaimed
					// local path. Returning nil is a no-op that also (correctly) skips the DAG
					// cascade: a terminal task has already been accounted for, and in the
					// replace-cancel case that motivates this the run is cancelled, so no
					// successor should advance.
					return nil
				}
				var jobRun models.JobRun
				if err := tx.First(&jobRun, "id = ?", runID).Error; err == nil {
					if !taskRun.Quarantine && !jobRun.Quarantine {
						jobID := jobRun.JobID.String()
						engine := string(taskRun.Engine)
						metrics.TaskRunsTotal.WithLabelValues(jobID, taskID.String(), engine, string(status)).Inc()
						if taskRun.StartedAt != nil {
							duration := now.Sub(*taskRun.StartedAt).Seconds()
							metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(status)).Observe(duration)
						}
					}
				}
			} else if enforceClaim && errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTaskClaimMismatch
			}

			updateQuery := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID)
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}

			updates := map[string]interface{}{
				"status":              string(status),
				"completed_at":        now,
				"result":              result,
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			}
			if len(output) > 0 {
				encoded, marshalErr := json.Marshal(output)
				if marshalErr != nil {
					return fmt.Errorf("marshalling task output: %w", marshalErr)
				}
				updates["output"] = encoded
			}
			if len(branchSelections) > 0 {
				encoded, marshalErr := json.Marshal(branchSelections)
				if marshalErr != nil {
					return fmt.Errorf("marshalling branch selections: %w", marshalErr)
				}
				updates["branch_selections"] = encoded
			}
			if status == TaskStatusFailed {
				msg := result
				switch Result(result) {
				case "failure":
					msg = "command exited with non-zero status"
				case "startup_failure":
					msg = "atom failed to start (check image/command)"
				case "resource_failure":
					msg = "atom exhausted resources (e.g. OOM)"
				case "killed":
					msg = "atom was forcefully killed"
				case "terminated":
					msg = "atom was gracefully terminated"
				}
				updates["error"] = msg
			}

			resultUpdate := updateQuery.Updates(updates)
			if resultUpdate.Error != nil {
				return resultUpdate.Error
			}
			if enforceClaim && resultUpdate.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			counts.addTaskRunStatus(1)

			if status == TaskStatusFailed {
				if s.eventStore != nil {
					evt, err := s.recordTaskEventTx(tx, event.TypeTaskFailed, runID, taskID, &counts)
					if err != nil {
						return err
					}
					attemptEvents = append(attemptEvents, *evt)
				}
				return nil
			}

			descriptor, replayTask, err := s.replayTaskExecutionDescriptorTx(tx, runID, taskID)
			if err != nil {
				return err
			}

			// Load the task model once — needed for both edge fallback and branch
			// type detection.
			var taskModel models.Task
			taskType := ""
			if replayTask {
				taskModel = models.Task{ID: taskID}
				taskType = firstNonEmpty(descriptor.Runtime.TaskType, descriptor.DAG.BranchBehavior, "task")
			} else {
				if err := tx.First(&taskModel, "id = ?", taskID).Error; err != nil {
					return err
				}
				taskType = taskModel.Type
			}

			edges, err := s.successorEdgesForRunTx(tx, runID, taskID, taskModel)
			if err != nil {
				return err
			}

			// Determine branch filtering if this is a branch-type task.
			var branchSelectedIDs map[uuid.UUID]bool
			if len(edges) > 0 && taskType == "branch" {
				successorNameToID, validTargets, err := s.successorNameMapTx(tx, replayTask, descriptor, edges)
				if err != nil {
					return err
				}

				// Validate selections.
				validSet := make(map[string]bool, len(validTargets))
				for _, name := range validTargets {
					validSet[name] = true
				}
				for _, name := range branchSelections {
					if !validSet[name] {
						return fmt.Errorf("branch selected unknown step %q; valid targets: %v", name, validTargets)
					}
				}

				// Build the set of selected successor IDs.
				// An empty set means "skip all downstream" (no markers emitted).
				branchSelectedIDs = make(map[uuid.UUID]bool, len(branchSelections))
				for _, name := range branchSelections {
					if id, ok := successorNameToID[name]; ok {
						branchSelectedIDs[id] = true
					}
				}
			}

			// Partition edges: skipped (branch-filtered) vs. predecessors to decrement.
			var toDecrementIDs []uuid.UUID
			for _, edge := range edges {
				if branchSelectedIDs != nil && !branchSelectedIDs[edge.ToTaskID] {
					reason := fmt.Sprintf("not selected by branch task %s", taskID)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, reason, &attemptEvents, &counts)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
					continue
				}
				toDecrementIDs = append(toDecrementIDs, edge.ToTaskID)
			}

			// Batch-decrement outstanding_predecessors for all non-skipped successors.
			updatedSuccessors, err := s.batchDecrementPredecessorsTx(tx, runID, toDecrementIDs)
			if err != nil {
				return err
			}
			counts.addTaskRunStatus(len(toDecrementIDs))

			// Collect all events to emit (task_succeeded + task_ready for newly-ready successors).
			var batchEvts []*event.Event

			// Evaluate trigger rules and collect task_ready events.
			for i := range updatedSuccessors {
				successor := &updatedSuccessors[i]
				if successor.OutstandingPredecessors != 0 || successor.Status != string(TaskStatusPending) {
					continue
				}
				shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, successor.TaskID)
				if err != nil {
					return err
				}
				if shouldRun {
					var jobRun models.JobRun
					if err := tx.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
						return err
					}
					readyEvt := &event.Event{
						Type:      event.TypeTaskReady,
						JobID:     jobRun.JobID,
						RunID:     runID,
						TaskID:    successor.TaskID,
						Timestamp: time.Now().UTC(),
					}
					batchEvts = append(batchEvts, readyEvt)
					continue
				}

				skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", rule)
				skipped, err := s.skipTaskAndDescendantsTx(tx, runID, successor.TaskID, skipRuleReason, &attemptEvents, &counts)
				if err != nil {
					return err
				}
				attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
			}

			if s.eventStore != nil {
				// Build task_succeeded event and add to batch.
				var taskRunModel models.TaskRun
				if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRunModel).Error; err != nil {
					return err
				}
				var jobRun models.JobRun
				if err := tx.Preload("Job").First(&jobRun, "id = ?", runID).Error; err != nil {
					return err
				}
				taskPayload := convertRunTaskModel(&taskRunModel)
				taskPayload.JobAlias = jobRun.Job.Alias
				taskPayload.JobLabels = jsonmap.ToStringMap(jobRun.Job.Labels)
				taskPayload.ID = taskRunModel.ID
				payload, marshalErr := json.Marshal(taskPayload)
				if marshalErr != nil {
					return marshalErr
				}
				succeededEvt := &event.Event{
					Type:      event.TypeTaskSucceeded,
					JobID:     jobRun.JobID,
					RunID:     runID,
					TaskID:    taskID,
					Timestamp: time.Now().UTC(),
					Payload:   payload,
				}
				// task_succeeded goes first so sequence ordering is consistent.
				batchEvts = append([]*event.Event{succeededEvt}, batchEvts...)

				if err := s.appendBatchEventsTx(tx, batchEvts, &attemptEvents, &counts); err != nil {
					return err
				}
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			skippedTaskIDs = attemptSkippedTaskIDs
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return skippedTaskIDs, err
}

// CompleteTaskOwner is the run-owner in-memory path's durable terminal write.
// The owner has already advanced the DAG in memory (run.RunState), so this only
// persists terminal rows — it does NOT decrement predecessors, evaluate trigger
// rules, or resolve branches in SQL.  It writes the completed task's terminal
// row (succeeded/failed) plus each owner-decided skip, stamping terminal_sequence
// and owner_generation so a recovering owner can replay in order.  Claim-fenced
// by claimedBy.  Cache-hit completions are not handled here (they remain on the
// CacheHitTaskClaimed path); the owner routes only succeeded/failed through this.
func (s *Store) CompleteTaskOwner(
	runID, taskID uuid.UUID,
	status TaskStatus,
	result, errMsg, claimedBy string,
	output map[string]string,
	branchSelections []string,
	completedSeq, ownerGen int64,
	skips []SkippedTask,
) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8+len(skips))

		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			// Metrics for the completed task (mirrors completeTask).
			var taskRun models.TaskRun
			tq := tx.Where("job_run_id = ? AND task_id = ? AND claimed_by = ?", runID, taskID, claimedBy)
			if err := tq.First(&taskRun).Error; err == nil {
				var jobRun models.JobRun
				if err := tx.First(&jobRun, "id = ?", runID).Error; err == nil {
					if !taskRun.Quarantine && !jobRun.Quarantine {
						jobID := jobRun.JobID.String()
						engine := string(taskRun.Engine)
						metrics.TaskRunsTotal.WithLabelValues(jobID, taskID.String(), engine, string(status)).Inc()
						if taskRun.StartedAt != nil {
							metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(status)).
								Observe(now.Sub(*taskRun.StartedAt).Seconds())
						}
					}
				}
			} else if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTaskClaimMismatch
			}

			updates := map[string]interface{}{
				"status":            string(status),
				"completed_at":      now,
				"result":            result,
				"terminal_sequence": completedSeq,
				"owner_generation":  ownerGen,
				"cache_hit":         status == TaskStatusCached,
			}
			if len(output) > 0 {
				encoded, mErr := json.Marshal(output)
				if mErr != nil {
					return fmt.Errorf("marshalling task output: %w", mErr)
				}
				updates["output"] = encoded
			}
			if len(branchSelections) > 0 {
				encoded, mErr := json.Marshal(branchSelections)
				if mErr != nil {
					return fmt.Errorf("marshalling branch selections: %w", mErr)
				}
				updates["branch_selections"] = encoded
			}
			if status == TaskStatusFailed {
				if errMsg != "" {
					updates["error"] = errMsg
				} else {
					updates["error"] = failureMessage(result)
				}
			}

			res := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ? AND claimed_by = ?", runID, taskID, claimedBy).
				Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return ErrTaskClaimMismatch
			}
			counts.addTaskRunStatus(1)

			if s.eventStore != nil {
				evtType := event.TypeTaskSucceeded
				if status == TaskStatusFailed {
					evtType = event.TypeTaskFailed
				}
				evt, err := s.recordTaskEventTx(tx, evtType, runID, taskID, &counts)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}

			// Persist owner-decided skips (branch + trigger-rule), each stamped
			// with its own terminal_sequence.  RunState already enumerated the
			// full transitive skip set, so this writes them directly without
			// re-walking descendants.
			for _, sk := range skips {
				skRes := tx.Model(&models.TaskRun{}).
					Where("job_run_id = ? AND task_id = ?", runID, sk.TaskID).
					Where("status NOT IN ?", terminalStatusStrings()).
					Updates(map[string]interface{}{
						"status":            string(TaskStatusSkipped),
						"completed_at":      now,
						"error":             sk.Reason,
						"terminal_sequence": sk.TerminalSequence,
						"owner_generation":  ownerGen,
					})
				if skRes.Error != nil {
					return skRes.Error
				}
				if skRes.RowsAffected == 0 {
					continue // already terminal; nothing to emit
				}
				counts.addTaskRunStatus(1)
				if s.eventStore != nil {
					evt, err := s.recordTaskEventTx(tx, event.TypeTaskSkipped, runID, sk.TaskID, &counts)
					if err != nil {
						return err
					}
					attemptEvents = append(attemptEvents, *evt)
				}
			}
			return nil
		})
		if txErr == nil {
			pendingEvents = attemptEvents
		}
		return txErr
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

// failureMessage maps a failure result string to a human-readable error, matching
// the messages completeTask writes.
func failureMessage(result string) string {
	switch Result(result) {
	case "failure":
		return "command exited with non-zero status"
	case "startup_failure":
		return "atom failed to start (check image/command)"
	case "resource_failure":
		return "atom exhausted resources (e.g. OOM)"
	case "killed":
		return "atom was forcefully killed"
	case "terminated":
		return "atom was gracefully terminated"
	default:
		return result
	}
}

// terminalStatusStrings returns the terminal task statuses as strings for SQL IN
// clauses.
func terminalStatusStrings() []string {
	return []string{
		string(TaskStatusSucceeded),
		string(TaskStatusFailed),
		string(TaskStatusSkipped),
		string(TaskStatusCached),
		string(TaskStatusCancelled),
	}
}

// skipTaskAndDescendantsTx marks a task and all its transitive descendants as
// skipped within the given transaction. Descendants are only skipped once all
// of their predecessors are terminal and their trigger rules remain
// unsatisfied.
func (s *Store) skipTaskAndDescendantsTx(tx *gorm.DB, runID, taskID uuid.UUID, reason string, pendingEvents *[]event.Event, counts *dbWriteCounts) ([]uuid.UUID, error) {
	type queuedSkip struct {
		taskID uuid.UUID
		reason string
	}

	queue := []queuedSkip{{taskID: taskID, reason: reason}}
	var skipped []uuid.UUID

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		markedSkipped, err := s.markTaskSkippedTx(tx, runID, current.taskID, current.reason, pendingEvents, counts)
		if err != nil {
			return skipped, err
		}
		if !markedSkipped {
			// Task was not pending (already completed/skipped) — don't propagate.
			continue
		}

		skipped = append(skipped, current.taskID)

		descriptor, replayTask, err := s.replayTaskExecutionDescriptorTx(tx, runID, current.taskID)
		if err != nil {
			return skipped, err
		}
		var task models.Task
		if replayTask {
			task = models.Task{ID: current.taskID}
			_ = descriptor
		} else {
			if err := tx.First(&task, "id = ?", current.taskID).Error; err != nil {
				return skipped, err
			}
		}

		edges, err := s.successorEdgesForRunTx(tx, runID, current.taskID, task)
		if err != nil {
			return skipped, err
		}

		// Batch-decrement outstanding_predecessors for all successors of this skipped task.
		successorIDs := make([]uuid.UUID, 0, len(edges))
		for _, edge := range edges {
			successorIDs = append(successorIDs, edge.ToTaskID)
		}

		updatedSuccessors, err := s.batchDecrementPredecessorsTx(tx, runID, successorIDs)
		if err != nil {
			return skipped, err
		}
		counts.addTaskRunStatus(len(successorIDs))

		// Build a quick lookup map from the updated rows.
		updatedByTaskID := make(map[uuid.UUID]*models.TaskRun, len(updatedSuccessors))
		for i := range updatedSuccessors {
			updatedByTaskID[updatedSuccessors[i].TaskID] = &updatedSuccessors[i]
		}

		for _, edge := range edges {
			successor, ok := updatedByTaskID[edge.ToTaskID]
			if !ok {
				continue
			}
			if successor.Status != string(TaskStatusPending) || successor.OutstandingPredecessors != 0 {
				continue
			}

			shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, edge.ToTaskID)
			if err != nil {
				return skipped, err
			}
			if shouldRun {
				if err := s.appendTaskReadyEventTx(tx, runID, edge.ToTaskID, pendingEvents, counts); err != nil {
					return skipped, err
				}
				continue
			}

			queue = append(queue, queuedSkip{
				taskID: edge.ToTaskID,
				reason: fmt.Sprintf("trigger rule %q not satisfied", rule),
			})
		}
	}

	return skipped, nil
}

func (s *Store) FailTask(runID, taskID uuid.UUID, failure error) error {
	return s.failTask(runID, taskID, failure, "", false)
}

func (s *Store) FailTaskClaimed(runID, taskID uuid.UUID, failure error, claimedBy string) error {
	return s.failTask(runID, taskID, failure, claimedBy, true)
}

func (s *Store) failTask(runID, taskID uuid.UUID, failure error, claimedBy string, enforceClaim bool) error {
	now := time.Now().UTC()
	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}

	// Record task failure metrics.
	var taskRun models.TaskRun
	taskQuery := s.db.Where("job_run_id = ? AND task_id = ?", runID, taskID)
	if enforceClaim {
		taskQuery = taskQuery.Where("claimed_by = ?", claimedBy)
	}
	if err := taskQuery.First(&taskRun).Error; err == nil {
		var jobRun models.JobRun
		if err := s.db.First(&jobRun, "id = ?", runID).Error; err == nil {
			if !taskRun.Quarantine && !jobRun.Quarantine {
				jobID := jobRun.JobID.String()
				engine := string(taskRun.Engine)
				metrics.TaskRunsTotal.WithLabelValues(jobID, taskID.String(), engine, string(TaskStatusFailed)).Inc()
				if taskRun.StartedAt != nil {
					duration := now.Sub(*taskRun.StartedAt).Seconds()
					metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(TaskStatusFailed)).Observe(duration)
				}
			}
		}
	} else if enforceClaim && errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrTaskClaimMismatch
	}

	pendingEvents := make([]event.Event, 0, 1)
	var counts dbWriteCounts
	err := s.db.Transaction(func(tx *gorm.DB) error {
		updateQuery := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID)
		if enforceClaim {
			updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
		}
		resultUpdate := updateQuery.
			Updates(map[string]interface{}{
				"status":              string(TaskStatusFailed),
				"completed_at":        now,
				"error":               errMsg,
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			})
		if resultUpdate.Error != nil {
			return resultUpdate.Error
		}
		if enforceClaim && resultUpdate.RowsAffected == 0 {
			return ErrTaskClaimMismatch
		}
		counts.addTaskRunStatus(1)

		if s.eventStore != nil {
			evt, err := s.recordTaskEventTx(tx, event.TypeTaskFailed, runID, taskID, &counts)
			if err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, *evt)
		}
		return nil
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

// RetryTask resets a failed task run back to pending and increments its Attempt counter.
func (s *Store) RetryTask(runID, taskID uuid.UUID, attempt int) error {
	return s.retryTask(runID, taskID, attempt, "", false)
}

// RetryTaskClaimed resets a claimed failed task run and increments its Attempt counter.
func (s *Store) RetryTaskClaimed(runID, taskID uuid.UUID, attempt int, claimedBy string) error {
	return s.retryTask(runID, taskID, attempt, claimedBy, true)
}

func (s *Store) retryTask(runID, taskID uuid.UUID, attempt int, claimedBy string, enforceClaim bool) error {
	pendingEvents := make([]event.Event, 0, 2)
	var counts dbWriteCounts
	err := s.db.Transaction(func(tx *gorm.DB) error {
		updateQuery := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ?", runID, taskID)
		if enforceClaim {
			updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
		}
		resultUpdate := updateQuery.
			Updates(map[string]interface{}{
				"status":                 string(TaskStatusPending),
				"attempt":                attempt,
				"runtime_id":             "",
				"started_at":             nil,
				"completed_at":           nil,
				"result":                 "",
				"output":                 nil,
				"branch_selections":      nil,
				"log_text":               "",
				"log_truncated":          false,
				"error":                  "",
				"rate_limit_retry_after": nil,
				"cache_hit":              false,
				"cache_origin_run_id":    nil,
				"cache_created_at":       nil,
				"cache_expires_at":       nil,
			})
		if resultUpdate.Error != nil {
			return resultUpdate.Error
		}
		if enforceClaim && resultUpdate.RowsAffected == 0 {
			return ErrTaskClaimMismatch
		}
		counts.addTaskRunStatus(1)

		if s.eventStore != nil {
			// Build retrying event payload.
			var taskRunModel models.TaskRun
			if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRunModel).Error; err != nil {
				return err
			}
			var jobRun models.JobRun
			if err := tx.Preload("Job").First(&jobRun, "id = ?", runID).Error; err != nil {
				return err
			}
			taskPayload := convertRunTaskModel(&taskRunModel)
			taskPayload.JobAlias = jobRun.Job.Alias
			taskPayload.JobLabels = jsonmap.ToStringMap(jobRun.Job.Labels)
			taskPayload.ID = taskRunModel.ID
			payload, marshalErr := json.Marshal(taskPayload)
			if marshalErr != nil {
				return marshalErr
			}

			batchEvts := []*event.Event{
				{
					Type:      event.TypeTaskRetrying,
					JobID:     jobRun.JobID,
					RunID:     runID,
					TaskID:    taskID,
					Timestamp: time.Now().UTC(),
					Payload:   payload,
				},
			}

			// If the task has no outstanding predecessors, also emit task_ready.
			if taskRunModel.OutstandingPredecessors == 0 {
				batchEvts = append(batchEvts, &event.Event{
					Type:      event.TypeTaskReady,
					JobID:     jobRun.JobID,
					RunID:     runID,
					TaskID:    taskID,
					Timestamp: time.Now().UTC(),
				})
			}

			if err := s.appendBatchEventsTx(tx, batchEvts, &pendingEvents, &counts); err != nil {
				return err
			}
		}

		return nil
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

func (s *Store) SkipTask(runID, taskID uuid.UUID, reason string) error {
	now := time.Now().UTC()
	pendingEvents := make([]event.Event, 0, 1)
	var counts dbWriteCounts
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.TaskRun{}).
			Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(TaskStatusPending)).
			Updates(map[string]interface{}{
				"status":              string(TaskStatusSkipped),
				"completed_at":        now,
				"error":               reason,
				"cache_hit":           false,
				"cache_origin_run_id": nil,
				"cache_created_at":    nil,
				"cache_expires_at":    nil,
			}).Error; err != nil {
			return err
		}
		counts.addTaskRunStatus(1)
		if s.eventStore != nil {
			evt, err := s.recordTaskEventTx(tx, event.TypeTaskSkipped, runID, taskID, &counts)
			if err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, *evt)
		}
		return nil
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return err
}

// errRunAlreadyTerminal is an internal sentinel: Complete's idempotency guard
// returns it when the run is already in a terminal status, so the caller treats
// the call as a successful no-op rather than re-emitting completion events.
var errRunAlreadyTerminal = errors.New("run already terminal")

func (s *Store) Complete(runID uuid.UUID, result error) error {
	now := time.Now().UTC()
	status := StatusSucceeded
	errMsg := ""
	if result != nil {
		status = StatusFailed
		errMsg = result.Error()
	}

	// The completion write is on the run-completion path taken by every job
	// run; an unretried transient "database is locked" / "checkpoint in
	// progress" here marks an otherwise-successful run as failed. Retry the
	// whole transaction with bounded backoff. jobID + startedAt (for metrics)
	// and pendingEvents are captured per attempt and promoted only on success,
	// so a retried transaction never double-counts or double-publishes — and
	// the gauge bookkeeping below never depends on a separate best-effort read
	// that could fail and leak the active-runs gauge.
	var (
		pendingEvents []event.Event
		jobID         uuid.UUID
		startedAt     time.Time
		quarantine    bool
	)
	err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 2)
		var (
			attemptJobID      uuid.UUID
			attemptStartedAt  time.Time
			attemptQuarantine bool
		)
		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			// Idempotency guard: skip if the run is already terminal.  Run-owner
			// in-memory mode can finalize a run from the owner (on takeover) and
			// from the triggering node's waitForRunCompletion; this keeps the
			// second call a no-op so run_completed/run_failed events fire once.
			res := tx.Model(&models.JobRun{}).
				Where("id = ? AND status NOT IN ?", runID, []string{string(StatusSucceeded), string(StatusFailed), string(StatusCancelled)}).
				Updates(map[string]interface{}{
					"status":       string(status),
					"completed_at": now,
					"error":        errMsg,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				// Already finalized by another path; nothing more to do.
				return errRunAlreadyTerminal
			}

			// Read jobID + startedAt inside the same retried transaction so the
			// post-commit metrics/gauge bookkeeping always has them.
			var jr models.JobRun
			if err := tx.Select("job_id", "started_at", "quarantine").First(&jr, "id = ?", runID).Error; err != nil {
				return err
			}
			attemptJobID = jr.JobID
			attemptStartedAt = jr.StartedAt
			attemptQuarantine = jr.Quarantine

			if s.eventStore != nil {
				loaded, loadErr := s.loadRunWithDB(tx, runID)
				if loadErr != nil {
					return loadErr
				}

				eventType := event.TypeRunCompleted
				if status == StatusFailed {
					eventType = event.TypeRunFailed
				}
				payload, marshalErr := json.Marshal(loaded)
				if marshalErr != nil {
					return marshalErr
				}

				completionEvent := event.Event{
					Type:       eventType,
					JobID:      loaded.JobID,
					RunID:      runID,
					Timestamp:  now,
					Payload:    payload,
					Quarantine: loaded.Quarantine || attemptQuarantine,
				}
				if err := s.eventStore.AppendTx(tx, &completionEvent); err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, completionEvent)

				terminalEvent := event.Event{
					Type:       event.TypeRunTerminal,
					JobID:      loaded.JobID,
					RunID:      runID,
					Timestamp:  now,
					Payload:    payload,
					Quarantine: loaded.Quarantine || attemptQuarantine,
				}
				if err := s.eventStore.AppendTx(tx, &terminalEvent); err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, terminalEvent)
			}

			return nil
		})
		if txErr == nil {
			pendingEvents = attemptEvents
			jobID = attemptJobID
			startedAt = attemptStartedAt
			quarantine = attemptQuarantine
		}
		return txErr
	})
	if errors.Is(err, errRunAlreadyTerminal) {
		// Run was already finalized by another path (idempotent no-op): no
		// events, metrics, or gauge bookkeeping to repeat.
		return nil
	}
	if err != nil {
		return err
	}

	// Emit metrics and clear active-run bookkeeping exactly once, after the
	// completion write has committed, so retries don't double-count. jobID and
	// startedAt are guaranteed populated because the transaction succeeded.
	jobIDStr := jobID.String()
	// Only decrement the active gauge if this process incremented it.
	s.startedMu.Lock()
	_, started := s.startedRuns[runID]
	if started {
		delete(s.startedRuns, runID)
	}
	s.startedMu.Unlock()
	if !quarantine {
		metrics.JobRunsTotal.WithLabelValues(jobIDStr, string(status)).Inc()
		if started {
			metrics.JobsActive.WithLabelValues(jobIDStr).Dec()
		}
		metrics.JobRunDurationSeconds.WithLabelValues(jobIDStr, string(status)).Observe(now.Sub(startedAt).Seconds())
	}

	s.publishEvents(pendingEvents...)
	return nil
}

func (s *Store) CancelRun(ctx context.Context, runID uuid.UUID) error {
	var (
		pendingEvents []event.Event
		cancelled     *cancelledRunInfo
	)
	if err := withStoreBusyRetry(func() error {
		attemptEvents := make([]event.Event, 0, 2)
		var attemptCancelled *cancelledRunInfo
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			info, events, err := s.cancelRunTx(tx, runID, "cancelled by concurrency replacement")
			if err != nil {
				return err
			}
			attemptCancelled = info
			attemptEvents = events
			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			cancelled = attemptCancelled
		}
		return err
	}); err != nil {
		return err
	}
	s.publishEvents(pendingEvents...)
	if cancelled != nil {
		s.recordCancelledRunMetrics(*cancelled)
	}
	return nil
}

func (s *Store) cancelOldestActiveRunTx(tx *gorm.DB, jobID uuid.UUID) (*cancelledRunInfo, []event.Event, error) {
	var model models.JobRun
	err := tx.
		Where("job_id = ? AND status = ? AND quarantine <> true AND backfill_id IS NULL", jobID, string(StatusRunning)).
		Order("started_at ASC").
		Take(&model).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return s.cancelRunTx(tx, model.ID, "cancelled by concurrency replacement")
}

func (s *Store) cancelRunTx(tx *gorm.DB, runID uuid.UUID, reason string) (*cancelledRunInfo, []event.Event, error) {
	now := time.Now().UTC()
	if strings.TrimSpace(reason) == "" {
		reason = "cancelled"
	}
	res := tx.Model(&models.JobRun{}).
		Where("id = ? AND status = ?", runID, string(StatusRunning)).
		Updates(map[string]interface{}{
			"status":       string(StatusCancelled),
			"completed_at": now,
			"error":        reason,
		})
	if res.Error != nil {
		return nil, nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil, nil
	}

	taskRes := tx.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status NOT IN ?", runID, terminalTaskStatuses()).
		Updates(map[string]interface{}{
			"status":                 string(TaskStatusCancelled),
			"completed_at":           now,
			"error":                  reason,
			"claimed_by":             "",
			"claim_expires_at":       nil,
			"runtime_id":             "",
			"rate_limit_retry_after": nil,
		})
	if taskRes.Error != nil {
		return nil, nil, taskRes.Error
	}
	if err := deleteRunLeaseTx(tx, runID); err != nil {
		return nil, nil, err
	}

	var infoRow struct {
		models.JobRun
		JobAlias string
	}
	if err := tx.Table("job_runs").
		Select("job_runs.*, jobs.alias as job_alias").
		Joins("left join jobs on jobs.id = job_runs.job_id").
		Where("job_runs.id = ?", runID).
		Take(&infoRow).Error; err != nil {
		return nil, nil, err
	}

	info := &cancelledRunInfo{
		ID:          runID,
		JobID:       infoRow.JobID,
		JobAlias:    infoRow.JobAlias,
		StartedAt:   infoRow.StartedAt,
		Quarantine:  infoRow.Quarantine,
		CancelledAt: now,
	}

	if s.eventStore == nil {
		return info, nil, nil
	}
	loaded, err := s.loadRunWithDB(tx, runID)
	if err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(loaded)
	if err != nil {
		return nil, nil, err
	}
	cancelledEvent := event.Event{
		Type:       event.TypeRunCancelled,
		JobID:      loaded.JobID,
		RunID:      runID,
		Timestamp:  now,
		Payload:    payload,
		Quarantine: loaded.Quarantine,
	}
	if err := s.eventStore.AppendTx(tx, &cancelledEvent); err != nil {
		return nil, nil, err
	}
	terminalEvent := event.Event{
		Type:       event.TypeRunTerminal,
		JobID:      loaded.JobID,
		RunID:      runID,
		Timestamp:  now,
		Payload:    payload,
		Quarantine: loaded.Quarantine,
	}
	if err := s.eventStore.AppendTx(tx, &terminalEvent); err != nil {
		return nil, nil, err
	}
	return info, []event.Event{cancelledEvent, terminalEvent}, nil
}

func (s *Store) recordCancelledRunMetrics(info cancelledRunInfo) {
	if info.Quarantine {
		return
	}
	jobLabel := info.JobID.String()
	s.startedMu.Lock()
	_, started := s.startedRuns[info.ID]
	if started {
		delete(s.startedRuns, info.ID)
	}
	s.startedMu.Unlock()
	metrics.JobRunsTotal.WithLabelValues(jobLabel, string(StatusCancelled)).Inc()
	if started {
		metrics.JobsActive.WithLabelValues(jobLabel).Dec()
	}
	if !info.StartedAt.IsZero() {
		metrics.JobRunDurationSeconds.WithLabelValues(jobLabel, string(StatusCancelled)).Observe(info.CancelledAt.Sub(info.StartedAt).Seconds())
	}
}

func (s *Store) ResetInFlightTasks(runID uuid.UUID) error {
	return s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND status = ?", runID, string(TaskStatusRunning)).
		Updates(map[string]interface{}{
			"status": string(TaskStatusPending),
			// Clear the claim too, so a new owner taking over a run can re-claim
			// these rows (ClaimTaskForDispatch requires claimed_by = '').  The old
			// owner's worker that held the claim is gone (its lease expired).
			"claimed_by":             "",
			"claim_expires_at":       nil,
			"runtime_id":             "",
			"started_at":             nil,
			"rate_limit_retry_after": nil,
			"cache_hit":              false,
			"cache_origin_run_id":    nil,
			"cache_created_at":       nil,
			"cache_expires_at":       nil,
		}).Error
}

func (s *Store) CountActive(jobID uuid.UUID) (int64, error) {
	var count int64
	err := s.db.Model(&models.JobRun{}).
		Where("job_id = ? AND status = ? AND quarantine <> true AND backfill_id IS NULL", jobID, string(StatusRunning)).
		Count(&count).Error
	return count, err
}

func (s *Store) enqueueRunTx(tx *gorm.DB, jobID uuid.UUID, params datatypes.JSON, priority, maxDepth int) error {
	if priority <= 0 {
		priority = PriorityNormalValue
	}
	if maxDepth <= 0 {
		maxDepth = 100
	}
	now := time.Now().UTC()
	row := &models.RunQueue{
		ID:        uuid.New(),
		JobID:     jobID,
		Params:    append(datatypes.JSON(nil), params...),
		Priority:  priority,
		ClaimedBy: "",
		CreatedAt: now,
	}
	if err := tx.Create(row).Error; err != nil {
		return err
	}
	var depth int64
	if err := tx.Model(&models.RunQueue{}).
		Where("job_id = ? AND claimed_by = ''", jobID).
		Count(&depth).Error; err != nil {
		return err
	}
	if overflow := int(depth) - maxDepth; overflow > 0 {
		if err := tx.Exec(`
DELETE FROM run_queue
WHERE id IN (
	SELECT id
	FROM run_queue
	WHERE job_id = ? AND claimed_by = ''
	ORDER BY created_at ASC
	LIMIT ?
)`, jobID, overflow).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DequeueNextRun(ctx context.Context, jobID uuid.UUID, claimedBy string) (*models.RunQueue, error) {
	claimedBy = strings.TrimSpace(claimedBy)
	if claimedBy == "" {
		claimedBy = uuid.NewString()
	}
	result := s.db.WithContext(ctx).Exec(`
UPDATE run_queue
SET claimed_by = ?, claimed_at = ?
WHERE id = (
	SELECT id
	FROM run_queue
	WHERE job_id = ? AND claimed_by = ''
	ORDER BY priority DESC, created_at ASC
	LIMIT 1
)
AND claimed_by = ''`, claimedBy, time.Now().UTC(), jobID)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	var queued models.RunQueue
	if err := s.db.WithContext(ctx).
		Where("job_id = ? AND claimed_by = ?", jobID, claimedBy).
		Take(&queued).Error; err != nil {
		return nil, err
	}
	if err := s.observeRunQueueDepth(jobID); err != nil {
		log.Warn("run queue: failed to observe depth after dequeue", "job_id", jobID, "error", err)
	}
	return &queued, nil
}

func (s *Store) ReleaseQueuedRun(ctx context.Context, queueID uuid.UUID, claimedBy string) error {
	result := s.db.WithContext(ctx).
		Model(&models.RunQueue{}).
		Where("id = ? AND claimed_by = ?", queueID, claimedBy).
		Updates(map[string]any{
			"claimed_by": "",
			"claimed_at": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	var queued models.RunQueue
	if err := s.db.WithContext(ctx).Select("job_id").First(&queued, "id = ?", queueID).Error; err == nil {
		if observeErr := s.observeRunQueueDepth(queued.JobID); observeErr != nil {
			log.Warn("run queue: failed to observe depth after release", "job_id", queued.JobID, "error", observeErr)
		}
	}
	return nil
}

func (s *Store) DeleteQueuedRun(ctx context.Context, queued *models.RunQueue) error {
	if queued == nil {
		return nil
	}
	result := s.db.WithContext(ctx).
		Delete(&models.RunQueue{}, "id = ?", queued.ID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		alias, err := s.jobAlias(queued.JobID)
		if err != nil {
			log.Warn("run queue: failed to load job alias for wait metric", "job_id", queued.JobID, "error", err)
		} else {
			metrics.RunQueueWaitSeconds.WithLabelValues(metricJobAlias(queued.JobID, alias)).Observe(time.Since(queued.CreatedAt).Seconds())
		}
	}
	if err := s.observeRunQueueDepth(queued.JobID); err != nil {
		log.Warn("run queue: failed to observe depth after delete", "job_id", queued.JobID, "error", err)
	}
	return nil
}

func (s *Store) jobAlias(jobID uuid.UUID) (string, error) {
	var job models.Job
	if err := s.db.Select("alias").First(&job, "id = ?", jobID).Error; err != nil {
		return "", err
	}
	return job.Alias, nil
}

func (s *Store) observeRunQueueDepth(jobID uuid.UUID) error {
	alias, err := s.jobAlias(jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			alias = jobID.String()
		} else {
			return err
		}
	}
	rows := []struct {
		Priority int
		Depth    int64
	}{}
	if err := s.db.Model(&models.RunQueue{}).
		Select("priority, count(*) as depth").
		Where("job_id = ? AND claimed_by = ''", jobID).
		Group("priority").
		Scan(&rows).Error; err != nil {
		return err
	}
	depths := map[int]int64{
		PriorityLowValue:    0,
		PriorityNormalValue: 0,
		PriorityHighValue:   0,
	}
	for _, row := range rows {
		depths[row.Priority] = row.Depth
	}
	jobLabel := metricJobAlias(jobID, alias)
	for priority, depth := range depths {
		metrics.RunQueueDepth.WithLabelValues(jobLabel, PriorityLabel(priority)).Set(float64(depth))
	}
	return nil
}

func (s *Store) FindRunning(jobID uuid.UUID) (*JobRun, error) {
	var model models.JobRun
	err := s.db.Where("job_id = ? AND status = ? AND quarantine IS NOT TRUE", jobID, string(StatusRunning)).
		Order("started_at DESC").
		First(&model).Error
	if err != nil {
		return nil, err
	}
	return s.loadRun(model.ID)
}

func (s *Store) Get(runID uuid.UUID) (*JobRun, error) {
	return s.loadRun(runID)
}

func (s *Store) List(jobID uuid.UUID) ([]*JobRun, error) {
	var results []struct {
		models.JobRun
		JobAlias     string
		TriggerType  string
		TriggerAlias string
	}

	err := s.db.Table("job_runs").
		Select("job_runs.*, jobs.alias as job_alias, triggers.type as trigger_type, triggers.alias as trigger_alias").
		Joins("join jobs on jobs.id = job_runs.job_id").
		Joins("left join triggers on triggers.id = job_runs.trigger_id").
		Where("job_runs.job_id = ? AND job_runs.quarantine IS NOT TRUE", jobID).
		Order("job_runs.started_at ASC").
		Preload("Tasks").
		Scan(&results).Error

	if err != nil {
		return nil, err
	}

	runs := make([]*JobRun, 0, len(results))
	for i := range results {
		runValue, err := s.convertRunModel(&results[i].JobRun)
		if err != nil {
			return nil, err
		}
		runValue.JobAlias = results[i].JobAlias
		runValue.TriggerType = results[i].TriggerType
		runValue.TriggerAlias = results[i].TriggerAlias
		runs = append(runs, runValue)
	}

	return runs, nil
}

func (s *Store) Latest(jobID uuid.UUID) (*JobRun, error) {
	var model models.JobRun
	err := s.db.Where("job_id = ? AND quarantine IS NOT TRUE", jobID).
		Order("started_at DESC").
		First(&model).Error
	if err != nil {
		return nil, err
	}
	return s.loadRun(model.ID)
}

// LatestSuccessfulCronRun returns the most recent cron-triggered run for a job
// that completed with status "succeeded". It returns gorm.ErrRecordNotFound
// when no such run exists.
func (s *Store) LatestSuccessfulCronRun(jobID uuid.UUID) (*JobRun, error) {
	var model models.JobRun
	err := s.db.
		Where("job_id = ? AND status = ? AND trigger_type = ? AND quarantine IS NOT TRUE", jobID, string(StatusSucceeded), "cron").
		Order("started_at DESC").
		First(&model).Error
	if err != nil {
		return nil, err
	}
	return s.loadRun(model.ID)
}

func (s *Store) loadRun(runID uuid.UUID) (*JobRun, error) {
	return s.loadRunWithDB(s.db, runID)
}

func (s *Store) loadRunWithDB(conn *gorm.DB, runID uuid.UUID) (*JobRun, error) {
	var result struct {
		models.JobRun
		JobAlias     string
		JobLabels    datatypes.JSONMap
		TriggerType  string
		TriggerAlias string
	}

	// Use a JOIN to fetch job and trigger information for human readability
	err := conn.Table("job_runs").
		Select("job_runs.*, jobs.alias as job_alias, jobs.labels as job_labels, triggers.type as trigger_type, triggers.alias as trigger_alias").
		Joins("left join jobs on jobs.id = job_runs.job_id").
		Joins("left join triggers on triggers.id = job_runs.trigger_id").
		Where("job_runs.id = ?", runID).
		Preload("Tasks").
		First(&result).Error
	if err != nil {
		return nil, err
	}

	runValue, err := s.convertRunModelWithDB(conn, &result.JobRun)
	if err != nil {
		return nil, err
	}

	runValue.JobAlias = result.JobAlias
	runValue.JobLabels = jsonmap.ToStringMap(result.JobLabels)
	runValue.TriggerType = result.TriggerType
	runValue.TriggerAlias = result.TriggerAlias

	// Propagate job metadata to task runs for downstream event payloads.
	for i := range runValue.Tasks {
		runValue.Tasks[i].JobAlias = runValue.JobAlias
		runValue.Tasks[i].JobLabels = runValue.JobLabels
	}

	return runValue, nil
}

func (s *Store) convertRunModel(model *models.JobRun) (*JobRun, error) {
	return s.convertRunModelWithDB(s.db, model)
}

func (s *Store) convertRunModelWithDB(conn *gorm.DB, model *models.JobRun) (*JobRun, error) {
	if model == nil {
		return nil, nil
	}

	runValue := &JobRun{
		ID:         model.ID,
		JobID:      model.JobID,
		BackfillID: model.BackfillID,
		Status:     Status(model.Status),
		Priority:   model.Priority,
		Quarantine: model.Quarantine,
		StartedAt:  model.StartedAt,
		CreatedAt:  model.CreatedAt,
		UpdatedAt:  model.UpdatedAt,
		Error:      model.Error,
	}

	if len(model.Params) > 0 {
		var p map[string]string
		if err := json.Unmarshal(model.Params, &p); err == nil {
			runValue.Params = p
		}
	}

	if model.CompletedAt != nil {
		completed := *model.CompletedAt
		runValue.CompletedAt = &completed
	}

	runValue.Tasks = make([]*TaskRun, 0, len(model.Tasks))
	for _, task := range model.Tasks {
		if task == nil {
			continue
		}
		runValue.Tasks = append(runValue.Tasks, convertRunTaskModel(task))
	}
	runValue.CacheHits, runValue.ExecutedTasks, runValue.TotalTasks = summarizeTasks(runValue.Tasks)

	callbackRuns, err := s.loadCallbackRunsWithDB(conn, model.ID)
	if err != nil {
		return nil, err
	}
	runValue.Callbacks = callbackRuns

	return runValue, nil
}

func convertRunTaskModel(model *models.TaskRun) *TaskRun {
	if model == nil {
		return nil
	}

	var command []string
	if model.Command != "" {
		if err := json.Unmarshal([]byte(model.Command), &command); err != nil {
			command = []string{model.Command}
		}
	}

	task := &TaskRun{
		ID:                      model.TaskID,
		JobRunID:                model.JobRunID,
		TaskID:                  model.TaskID,
		AtomID:                  model.AtomID,
		Engine:                  model.Engine,
		Image:                   model.Image,
		Command:                 command,
		RuntimeID:               model.RuntimeID,
		Status:                  TaskStatus(model.Status),
		Priority:                model.Priority,
		NodeSelector:            jsonmap.ToStringMap(model.NodeSelector),
		ClaimedBy:               model.ClaimedBy,
		ClaimAttempt:            model.ClaimAttempt,
		Attempt:                 model.Attempt,
		MaxAttempts:             model.MaxAttempts,
		Result:                  model.Result,
		Error:                   model.Error,
		OutstandingPredecessors: model.OutstandingPredecessors,
		CreatedAt:               model.CreatedAt,
		UpdatedAt:               model.UpdatedAt,
		CacheHit:                model.CacheHit || TaskStatus(model.Status) == TaskStatusCached,
		Quarantine:              model.Quarantine,
		ReplaySafe:              model.ReplaySafe,
	}

	if len(model.Output) > 0 {
		var out map[string]string
		if err := json.Unmarshal(model.Output, &out); err == nil {
			task.Output = out
		}
	}

	if len(model.SchemaViolations) > 0 {
		var violations []pkgtask.SchemaViolation
		if err := json.Unmarshal(model.SchemaViolations, &violations); err == nil {
			task.SchemaViolations = violations
		}
	}

	if len(model.BranchSelections) > 0 {
		var bs []string
		if err := json.Unmarshal(model.BranchSelections, &bs); err == nil {
			task.BranchSelections = bs
		}
	}

	if model.StartedAt != nil {
		started := *model.StartedAt
		task.StartedAt = &started
	}
	if model.ClaimExpiresAt != nil {
		expiresAt := *model.ClaimExpiresAt
		task.ClaimExpiresAt = &expiresAt
	}
	if model.RateLimitRetryAfter != nil {
		retryAfter := *model.RateLimitRetryAfter
		task.RateLimitRetryAfter = &retryAfter
	}
	if model.CompletedAt != nil {
		completed := *model.CompletedAt
		task.CompletedAt = &completed
	}
	if model.CacheOriginRunID != nil {
		originRunID := *model.CacheOriginRunID
		task.CacheOriginRunID = &originRunID
	}
	if model.CacheCreatedAt != nil {
		cacheCreatedAt := *model.CacheCreatedAt
		task.CacheCreatedAt = &cacheCreatedAt
	}
	if model.CacheExpiresAt != nil {
		cacheExpiresAt := *model.CacheExpiresAt
		task.CacheExpiresAt = &cacheExpiresAt
	}

	return task
}

func summarizeTasks(tasks []*TaskRun) (cacheHits, executedTasks, totalTasks int) {
	totalTasks = len(tasks)
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if task.CacheHit || task.Status == TaskStatusCached {
			cacheHits++
			continue
		}
		switch task.Status {
		case TaskStatusRunning, TaskStatusSucceeded, TaskStatusFailed:
			executedTasks++
		}
	}
	return cacheHits, executedTasks, totalTasks
}

func (s *Store) loadCallbackRunsWithDB(conn *gorm.DB, runID uuid.UUID) ([]*CallbackRun, error) {
	var modelRuns []models.CallbackRun
	if err := conn.
		Where("job_run_id = ?", runID).
		Order("started_at ASC").
		Find(&modelRuns).Error; err != nil {
		return nil, err
	}

	result := make([]*CallbackRun, 0, len(modelRuns))
	for idx := range modelRuns {
		result = append(result, convertCallbackRunModel(&modelRuns[idx]))
	}
	return result, nil
}

func convertCallbackRunModel(model *models.CallbackRun) *CallbackRun {
	if model == nil {
		return nil
	}
	return &CallbackRun{
		ID:          model.ID,
		CallbackID:  model.CallbackID,
		Status:      CallbackStatus(model.Status),
		Error:       model.Error,
		StartedAt:   model.StartedAt,
		CompletedAt: model.CompletedAt,
	}
}

func (s *Store) recordTaskEventTx(db *gorm.DB, eventType event.Type, runID, taskID uuid.UUID, counts *dbWriteCounts) (*event.Event, error) {
	var taskRun models.TaskRun
	if err := db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&taskRun).Error; err != nil {
		log.Error("failed to fetch task run for event", "error", err, "run_id", runID, "task_id", taskID)
		return nil, err
	}

	var jobRun models.JobRun
	if err := db.Preload("Job").First(&jobRun, "id = ?", runID).Error; err != nil {
		log.Error("failed to fetch job run for event", "error", err, "run_id", runID)
		return nil, err
	}

	taskPayload := convertRunTaskModel(&taskRun)
	taskPayload.JobAlias = jobRun.Job.Alias
	taskPayload.JobLabels = jsonmap.ToStringMap(jobRun.Job.Labels)
	// Use task-run row ID for event payloads so downstream consumers can identify
	// each task execution uniquely across retries/runs.
	taskPayload.ID = taskRun.ID

	payload, err := json.Marshal(taskPayload)
	if err != nil {
		log.Error("failed to marshal task run for event", "error", err, "run_id", runID, "task_id", taskID)
		return nil, err
	}

	evt := event.Event{
		Type:       eventType,
		JobID:      jobRun.JobID,
		RunID:      runID,
		TaskID:     taskID,
		Timestamp:  time.Now().UTC(),
		Payload:    payload,
		Quarantine: taskRun.Quarantine || jobRun.Quarantine,
	}
	if s.eventStore != nil {
		if err := s.eventStore.AppendTx(db, &evt); err != nil {
			return nil, err
		}
		counts.addEventInsert(1)
	}
	return &evt, nil
}

func (s *Store) publishEvents(events ...event.Event) {
	event.PublishAndMarkBusDispatched(context.Background(), s.bus, s.eventStore, events...)
}

func (s *Store) PublishEvents(events ...event.Event) {
	s.publishEvents(events...)
}

// dbWriteCounts accumulates per-category DB write counts during a single retry
// attempt. Must be reset() at the start of each retry closure and commit()'d
// only after the retry returns nil; otherwise transactions retried due to
// busy/locked errors will over-count.
//
// Each category tracks both rows (total work) and stmts (round-trips). A
// batched UPDATE/INSERT bumps stmts by 1 and rows by N. The two counters
// together let dashboards compute "rows per statement" to quantify how
// effective batching is — the headline indicator for Phase 1.1 / 1.4 wins.
type dbWriteCounts struct {
	taskRunInsertRows  int
	taskRunInsertStmts int
	taskRunStatusRows  int
	taskRunStatusStmts int
	eventInsertRows    int
	eventInsertStmts   int
	callbackRows       int
	callbackStmts      int
	leaseRenewalRows   int
	leaseRenewalStmts  int
}

func (c *dbWriteCounts) reset() { *c = dbWriteCounts{} }

// addTaskRunInsert records one batched INSERT touching n rows.
func (c *dbWriteCounts) addTaskRunInsert(rows int) {
	if rows <= 0 {
		return
	}
	c.taskRunInsertRows += rows
	c.taskRunInsertStmts++
}

// addTaskRunStatus records one batched UPDATE touching n rows.
func (c *dbWriteCounts) addTaskRunStatus(rows int) {
	if rows <= 0 {
		return
	}
	c.taskRunStatusRows += rows
	c.taskRunStatusStmts++
}

// addEventInsert records one batched INSERT touching n rows.
func (c *dbWriteCounts) addEventInsert(rows int) {
	if rows <= 0 {
		return
	}
	c.eventInsertRows += rows
	c.eventInsertStmts++
}

// NOTE: addCallback and addLeaseRenewal accessors are intentionally omitted —
// the callback and lease_renewal categories don't flow through the
// accumulator-with-retry pattern (callback.go and worker.go emit metrics
// directly outside retry loops). commit() still emits both counters if the
// fields are non-zero so future callers can drop the accessors back in.

func (c *dbWriteCounts) commit() {
	emit := func(category string, rows, stmts int) {
		if rows > 0 {
			metrics.DBWritesTotal.WithLabelValues(category).Add(float64(rows))
		}
		if stmts > 0 {
			metrics.DBStatementsTotal.WithLabelValues(category).Add(float64(stmts))
		}
	}
	emit(metrics.DBWriteCategoryTaskRunInsert, c.taskRunInsertRows, c.taskRunInsertStmts)
	emit(metrics.DBWriteCategoryTaskRunStatus, c.taskRunStatusRows, c.taskRunStatusStmts)
	emit(metrics.DBWriteCategoryEventInsert, c.eventInsertRows, c.eventInsertStmts)
	emit(metrics.DBWriteCategoryCallback, c.callbackRows, c.callbackStmts)
	emit(metrics.DBWriteCategoryLeaseRenewal, c.leaseRenewalRows, c.leaseRenewalStmts)
}

func withStoreBusyRetry(fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = fn()
		if err == nil || !isStoreContentionErr(err) {
			return err
		}
		if attempt >= len(storeBusyRetryBackoffs) {
			return err
		}

		metrics.DBBusyRetriesTotal.Inc()
		time.Sleep(jitterStoreBusyRetryBackoff(storeBusyRetryBackoffs[attempt]))
	}
}

func jitterStoreBusyRetryBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	maxJitter := int64(base / 5)
	if maxJitter <= 0 {
		return base
	}
	return base - time.Duration(rand.Int64N(maxJitter+1))
}

func isStoreContentionErr(err error) bool {
	// Delegate to the single shared classifier so the matched error strings
	// live in exactly one place (pkg/dqlite). This helper retries whole
	// transaction closures; the pkg/db connection-pool retry covers single
	// autocommit statements.
	return dqlite.IsContentionError(err)
}

// PredecessorOutputs returns a map of step-name → output key-values for all
// predecessors of the given task within a run.  This is used by the distributed
// executor to inject CAESIUM_OUTPUT_* env vars before starting a task.
func (s *Store) PredecessorOutputs(runID, taskID uuid.UUID) (map[string]map[string]string, error) {
	if refs, replay, err := s.replayPredecessorRefsTx(s.db, runID, taskID); err != nil {
		return nil, err
	} else if replay {
		return s.predecessorOutputsFromRefsTx(s.db, runID, refs)
	}

	// Find predecessor task IDs via edges.
	var edges []models.TaskEdge
	if err := s.db.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}

	if len(edges) == 0 {
		return nil, nil
	}

	// Batch-fetch predecessor tasks and task runs to avoid N+1 queries.
	predTaskIDs := make([]uuid.UUID, len(edges))
	for i, edge := range edges {
		predTaskIDs[i] = edge.FromTaskID
	}

	var tasks []models.Task
	if err := s.db.Where("id IN ?", predTaskIDs).Find(&tasks).Error; err != nil {
		log.Warn("failed to find predecessor tasks for output", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	tasksByID := make(map[uuid.UUID]models.Task, len(tasks))
	for i := range tasks {
		tasksByID[tasks[i].ID] = tasks[i]
	}

	var taskRuns []models.TaskRun
	if err := s.db.Where("job_run_id = ? AND task_id IN ?", runID, predTaskIDs).Find(&taskRuns).Error; err != nil {
		log.Warn("failed to find predecessor task runs for output", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	taskRunsByTaskID := make(map[uuid.UUID]models.TaskRun, len(taskRuns))
	for i := range taskRuns {
		taskRunsByTaskID[taskRuns[i].TaskID] = taskRuns[i]
	}

	result := make(map[string]map[string]string, len(edges))
	for _, edge := range edges {
		task, ok := tasksByID[edge.FromTaskID]
		if !ok {
			continue
		}
		taskRun, ok := taskRunsByTaskID[edge.FromTaskID]
		if !ok || len(taskRun.Output) == 0 {
			continue
		}

		var output map[string]string
		if err := json.Unmarshal(taskRun.Output, &output); err != nil {
			log.Warn("failed to unmarshal predecessor task output", "run_id", runID, "task_id", taskID, "predecessor_task_id", edge.FromTaskID, "error", err)
			continue
		}

		stepName := task.Name
		if stepName == "" {
			stepName = task.ID.String()
		}
		result[stepName] = output
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func (s *Store) predecessorOutputsFromRefsTx(tx *gorm.DB, runID uuid.UUID, refs []models.TaskExecutionEdgeRef) (map[string]map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	predTaskIDs := make([]uuid.UUID, 0, len(refs))
	nameByID := make(map[uuid.UUID]string, len(refs))
	for _, ref := range refs {
		if ref.TaskID == uuid.Nil {
			continue
		}
		predTaskIDs = append(predTaskIDs, ref.TaskID)
		nameByID[ref.TaskID] = firstNonEmpty(ref.TaskName, ref.TaskID.String())
	}
	if len(predTaskIDs) == 0 {
		return nil, nil
	}

	var taskRuns []models.TaskRun
	if err := tx.Select("task_id", "output").
		Where("job_run_id = ? AND task_id IN ?", runID, predTaskIDs).
		Find(&taskRuns).Error; err != nil {
		return nil, err
	}
	result := make(map[string]map[string]string, len(taskRuns))
	for _, taskRun := range taskRuns {
		if len(taskRun.Output) == 0 {
			continue
		}
		var output map[string]string
		if err := json.Unmarshal(taskRun.Output, &output); err != nil {
			log.Warn("failed to unmarshal replay predecessor task output", "run_id", runID, "predecessor_task_id", taskRun.TaskID, "error", err)
			continue
		}
		if len(output) == 0 {
			continue
		}
		result[nameByID[taskRun.TaskID]] = output
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// PredecessorDescriptorInputs returns predecessor outputs and effective hashes
// keyed by predecessor task id for immutable execution-descriptor capture.
func (s *Store) PredecessorDescriptorInputs(runID, taskID uuid.UUID) (map[uuid.UUID]map[string]string, map[uuid.UUID]string, error) {
	if refs, replay, err := s.replayPredecessorRefsTx(s.db, runID, taskID); err != nil {
		return nil, nil, err
	} else if replay {
		return s.predecessorDescriptorInputsFromRefsTx(s.db, runID, refs)
	}

	var edges []models.TaskEdge
	if err := s.db.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, nil, err
	}
	if len(edges) == 0 {
		return nil, nil, nil
	}

	predTaskIDs := make([]uuid.UUID, len(edges))
	for i, edge := range edges {
		predTaskIDs[i] = edge.FromTaskID
	}

	var taskRuns []models.TaskRun
	if err := s.db.
		Select("task_id", "output", "hash", "effective_hash", "status").
		Where("job_run_id = ? AND task_id IN ?", runID, predTaskIDs).
		Find(&taskRuns).Error; err != nil {
		return nil, nil, err
	}

	outputs := make(map[uuid.UUID]map[string]string, len(taskRuns))
	hashes := make(map[uuid.UUID]string, len(taskRuns))
	for _, taskRun := range taskRuns {
		if len(taskRun.Output) > 0 {
			var output map[string]string
			if err := json.Unmarshal(taskRun.Output, &output); err == nil && len(output) > 0 {
				outputs[taskRun.TaskID] = output
			}
		}
		if taskRun.Status == string(TaskStatusSucceeded) || taskRun.Status == string(TaskStatusCached) {
			hash := taskRun.Hash
			if taskRun.EffectiveHash != "" {
				hash = taskRun.EffectiveHash
			}
			if hash != "" {
				hashes[taskRun.TaskID] = hash
			}
		}
	}
	return outputs, hashes, nil
}

func (s *Store) predecessorDescriptorInputsFromRefsTx(tx *gorm.DB, runID uuid.UUID, refs []models.TaskExecutionEdgeRef) (map[uuid.UUID]map[string]string, map[uuid.UUID]string, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	predTaskIDs := make([]uuid.UUID, 0, len(refs))
	for _, ref := range refs {
		if ref.TaskID != uuid.Nil {
			predTaskIDs = append(predTaskIDs, ref.TaskID)
		}
	}
	if len(predTaskIDs) == 0 {
		return nil, nil, nil
	}
	var taskRuns []models.TaskRun
	if err := tx.Select("task_id", "output", "hash", "effective_hash", "status").
		Where("job_run_id = ? AND task_id IN ?", runID, predTaskIDs).
		Find(&taskRuns).Error; err != nil {
		return nil, nil, err
	}
	outputs := make(map[uuid.UUID]map[string]string, len(taskRuns))
	hashes := make(map[uuid.UUID]string, len(taskRuns))
	for _, taskRun := range taskRuns {
		if len(taskRun.Output) > 0 {
			var output map[string]string
			if err := json.Unmarshal(taskRun.Output, &output); err == nil && len(output) > 0 {
				outputs[taskRun.TaskID] = output
			}
		}
		if taskRun.Status == string(TaskStatusSucceeded) || taskRun.Status == string(TaskStatusCached) {
			if hash := effectiveTaskHash(taskRun.Hash, taskRun.EffectiveHash); hash != "" {
				hashes[taskRun.TaskID] = hash
			}
		}
	}
	return outputs, hashes, nil
}

// PredecessorHashes returns the execution hashes recorded on predecessor task
// runs that completed successfully in the current run. This keeps distributed
// cache hashing aligned with local execution, including transitive cache hits.
//
// The hash returned per predecessor is its EFFECTIVE identity: effective_hash
// when a value-verified short-circuit was proven for that predecessor (its code
// changed but it produced byte-identical output, see cache.EquivalentPriorHash
// and TaskRun.EffectiveHash), otherwise its own hash. Reading the effective
// hash is what stops a no-op upstream change from cascading a re-run downstream:
// the predecessor presents its prior, proven-equivalent identity, so a
// downstream whose only changed input was this predecessor sees an unchanged
// hash and cache-hits. Falling back to hash (effective_hash empty) is the
// common case and is byte-identical to the pre-D2 behavior.
func (s *Store) PredecessorHashes(runID, taskID uuid.UUID) ([]string, error) {
	if refs, replay, err := s.replayPredecessorRefsTx(s.db, runID, taskID); err != nil {
		return nil, err
	} else if replay {
		return s.predecessorHashesFromRefsTx(s.db, runID, refs)
	}

	var edges []models.TaskEdge
	if err := s.db.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}

	if len(edges) == 0 {
		return nil, nil
	}

	predTaskIDs := make([]uuid.UUID, len(edges))
	for i, edge := range edges {
		predTaskIDs[i] = edge.FromTaskID
	}

	var taskRuns []models.TaskRun
	if err := s.db.
		Select("hash", "effective_hash").
		Where("job_run_id = ? AND task_id IN ? AND status IN ? AND hash <> ''",
			runID,
			predTaskIDs,
			[]string{string(TaskStatusSucceeded), string(TaskStatusCached)},
		).
		Find(&taskRuns).Error; err != nil {
		log.Warn("failed to find predecessor task runs for hashes", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	if len(taskRuns) == 0 {
		return nil, nil
	}

	hashes := make([]string, 0, len(taskRuns))
	for _, taskRun := range taskRuns {
		if h := effectiveTaskHash(taskRun.Hash, taskRun.EffectiveHash); h != "" {
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	sort.Strings(hashes)
	return hashes, nil
}

func (s *Store) predecessorHashesFromRefsTx(tx *gorm.DB, runID uuid.UUID, refs []models.TaskExecutionEdgeRef) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	predTaskIDs := make([]uuid.UUID, 0, len(refs))
	for _, ref := range refs {
		if ref.TaskID != uuid.Nil {
			predTaskIDs = append(predTaskIDs, ref.TaskID)
		}
	}
	if len(predTaskIDs) == 0 {
		return nil, nil
	}
	var taskRuns []models.TaskRun
	if err := tx.Select("hash", "effective_hash").
		Where("job_run_id = ? AND task_id IN ? AND status IN ? AND hash <> ''",
			runID,
			predTaskIDs,
			[]string{string(TaskStatusSucceeded), string(TaskStatusCached)},
		).
		Find(&taskRuns).Error; err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(taskRuns))
	for _, taskRun := range taskRuns {
		if h := effectiveTaskHash(taskRun.Hash, taskRun.EffectiveHash); h != "" {
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	sort.Strings(hashes)
	return hashes, nil
}

// effectiveTaskHash returns the identity a predecessor presents to downstream
// cache hashing: its proven-equivalent effectiveHash when set, otherwise its
// own hash. Centralized so the local and distributed paths agree on the rule.
func effectiveTaskHash(hash, effectiveHash string) string {
	if effectiveHash != "" {
		return effectiveHash
	}
	return hash
}

// RetryFromFailure resets a failed run so that previously-succeeded and cached
// tasks are preserved and only failed/pending/skipped tasks are re-executed.
func (s *Store) RetryFromFailure(runID uuid.UUID) (*JobRun, error) {
	pendingEvents := make([]event.Event, 0, 2)
	var jobID uuid.UUID
	var quarantine bool
	var counts dbWriteCounts

	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 1. Verify the run exists and is in a terminal state (failed/succeeded).
		var jobRun models.JobRun
		if err := tx.First(&jobRun, "id = ?", runID).Error; err != nil {
			return err
		}
		if jobRun.Status != string(StatusFailed) && jobRun.Status != string(StatusSucceeded) {
			return fmt.Errorf("can only retry runs in terminal state, current: %s", jobRun.Status)
		}
		jobID = jobRun.JobID
		quarantine = jobRun.Quarantine

		// 2. Reset the job run status to running.
		if err := tx.Model(&jobRun).Updates(map[string]interface{}{
			"status":       string(StatusRunning),
			"completed_at": nil,
			"error":        "",
		}).Error; err != nil {
			return err
		}

		// 3. Get all task runs for this run.
		var taskRuns []models.TaskRun
		if err := tx.Where("job_run_id = ?", runID).Find(&taskRuns).Error; err != nil {
			return err
		}

		// Build a set of task IDs that are in a terminal success state.
		terminalSuccessIDs := make(map[uuid.UUID]struct{})
		for i := range taskRuns {
			tr := &taskRuns[i]
			if IsTerminalSuccess(TaskStatus(tr.Status)) {
				terminalSuccessIDs[tr.TaskID] = struct{}{}
			}
		}

		// 4. Reset failed and skipped tasks to pending.
		// Leave succeeded and cached tasks as-is.
		resetTaskIDs := make([]uuid.UUID, 0)
		for i := range taskRuns {
			tr := &taskRuns[i]
			status := TaskStatus(tr.Status)
			if status == TaskStatusFailed || status == TaskStatusSkipped {
				updates := map[string]interface{}{
					"status":              string(TaskStatusPending),
					"completed_at":        nil,
					"result":              "",
					"error":               "",
					"started_at":          nil,
					"claimed_by":          "",
					"claim_expires_at":    nil,
					"runtime_id":          "",
					"attempt":             1,
					"cache_hit":           false,
					"cache_origin_run_id": nil,
					"cache_created_at":    nil,
					"cache_expires_at":    nil,
				}
				if err := tx.Model(tr).Updates(updates).Error; err != nil {
					return err
				}
				resetTaskIDs = append(resetTaskIDs, tr.TaskID)
			}
		}

		// 5. Recalculate outstanding_predecessors for each reset (pending) task.
		// For each pending task, count predecessors that are NOT in terminal success state.
		for _, taskID := range resetTaskIDs {
			var edges []models.TaskEdge
			if err := tx.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
				return err
			}

			outstanding := 0
			for _, edge := range edges {
				if _, ok := terminalSuccessIDs[edge.FromTaskID]; !ok {
					outstanding++
				}
			}

			if err := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND task_id = ?", runID, taskID).
				Update("outstanding_predecessors", outstanding).Error; err != nil {
				return err
			}

			// If all predecessors are already done, emit a task_ready event.
			if outstanding == 0 {
				if err := s.appendTaskReadyEventTx(tx, runID, taskID, &pendingEvents, &counts); err != nil {
					return err
				}
			}
		}

		// 6. Emit a run_retried event.
		if s.eventStore != nil {
			run, loadErr := s.loadRunWithDB(tx, runID)
			if loadErr != nil {
				return loadErr
			}
			payload, marshalErr := json.Marshal(run)
			if marshalErr != nil {
				return marshalErr
			}
			evt := event.Event{
				Type:       event.TypeRunRetried,
				JobID:      jobRun.JobID,
				RunID:      runID,
				Timestamp:  time.Now().UTC(),
				Payload:    payload,
				Quarantine: jobRun.Quarantine,
			}
			if err := s.eventStore.AppendTx(tx, &evt); err != nil {
				return err
			}
			pendingEvents = append(pendingEvents, evt)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	counts.commit()
	s.publishEvents(pendingEvents...)

	if !quarantine {
		// Track this run in the active set so Complete() will decrement the gauge.
		s.startedMu.Lock()
		s.startedRuns[runID] = struct{}{}
		s.startedMu.Unlock()
		metrics.JobsActive.WithLabelValues(jobID.String()).Inc()
	}

	return s.loadRun(runID)
}
