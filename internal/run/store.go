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
	"sync/atomic"
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

// effectiveTerminalStatus reconciles a REPORTED terminal status with the result
// string that came with it.
//
// The two can disagree in exactly one direction, and it is the common one: a
// container that ran and exited non-zero is reported as a COMPLETION
// (status=succeeded, result="failure"), because from the executor's point of
// view the task ran to a normal end and the result carries the verdict. Every
// path that records a terminal status must fold the result back in, or the same
// container produces "succeeded" on one route and "failed" on another.
//
// Only succeeded is re-derived. `cached` is a statement about where the result
// came from, not about the result itself, and the cache-hit paths handle an
// unsuccessful cached result on their own.
func effectiveTerminalStatus(status TaskStatus, result string) TaskStatus {
	if status == TaskStatusSucceeded {
		return taskStatusFromResult(result)
	}
	return status
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
	ID               uuid.UUID                 `json:"id"`
	JobRunID         uuid.UUID                 `json:"job_run_id"`
	TaskID           uuid.UUID                 `json:"task_id"`
	JobAlias         string                    `json:"job_alias,omitempty"`
	JobLabels        map[string]string         `json:"job_labels,omitempty"`
	AtomID           uuid.UUID                 `json:"atom_id"`
	Engine           models.AtomEngine         `json:"engine"`
	Image            string                    `json:"image"`
	Command          []string                  `json:"command"`
	RuntimeID        string                    `json:"runtime_id,omitempty"`
	Status           TaskStatus                `json:"status"`
	Priority         int                       `json:"priority"`
	NodeSelector     map[string]string         `json:"node_selector,omitempty"`
	ClaimedBy        string                    `json:"claimed_by,omitempty"`
	ClaimExpiresAt   *time.Time                `json:"claim_expires_at,omitempty"`
	ClaimAttempt     int                       `json:"claim_attempt"`
	Attempt          int                       `json:"attempt"`
	MaxAttempts      int                       `json:"max_attempts"`
	Result           string                    `json:"result,omitempty"`
	Output           map[string]string         `json:"output,omitempty"`
	SchemaViolations []pkgtask.SchemaViolation `json:"schema_violations,omitempty"`
	BranchSelections []string                  `json:"branch_selections,omitempty"`
	Quarantine       bool                      `json:"quarantine"`
	CacheHit         bool                      `json:"cache_hit"`
	ReplaySafe       bool                      `json:"replay_safe"`
	// The remaining frozen execution inputs. They are `json:"-"` because they
	// are not API surface — they exist so the LOCAL executor can run a task from
	// the same row the distributed worker runs it from (issue #354). The
	// scheduler resolves them once at RegisterTasks; both lanes must then read
	// them here rather than re-deriving them from a live catalog that a
	// `job apply` may have moved since the run was registered.
	//
	// CacheEnabled..CacheTTLNever are the seven columns the worker rebuilds
	// jobdefschema.CacheConfig from (internal/worker/runtime_executor.go).
	CacheEnabled    bool          `json:"-"`
	CacheTTL        time.Duration `json:"-"`
	CacheVersion    int           `json:"-"`
	CachePinDigests bool          `json:"-"`
	CacheDigestTTL  time.Duration `json:"-"`
	CacheChain      string        `json:"-"`
	CacheTTLNever   bool          `json:"-"`
	// OutputSchema / SchemaValidation are what the worker validates a task's
	// output against (runtimeExecutor.runSchemaValidation).
	OutputSchema            []byte     `json:"-"`
	SchemaValidation        string     `json:"-"`
	CacheOriginRunID        *uuid.UUID `json:"cache_origin_run_id,omitempty"`
	CacheCreatedAt          *time.Time `json:"cache_created_at,omitempty"`
	CacheExpiresAt          *time.Time `json:"cache_expires_at,omitempty"`
	RateLimitRetryAfter     *time.Time `json:"rate_limit_retry_after,omitempty"`
	StartedAt               *time.Time `json:"started_at,omitempty"`
	CompletedAt             *time.Time `json:"completed_at,omitempty"`
	Error                   string     `json:"error,omitempty"`
	OutstandingPredecessors int        `json:"outstanding_predecessors"`
	PartitionValue          string     `json:"partition_value,omitempty"`
	PartitionIndex          int        `json:"partition_index,omitempty"`
	PartitionCount          int        `json:"partition_count,omitempty"`
	PartitionFingerprint    string     `json:"partition_fingerprint,omitempty"`
	PartitionDependsOn      []string   `json:"partition_depends_on,omitempty"`
	// PartitionStatusCounts is the per-status histogram of a COLLAPSED fan-out
	// group: {"succeeded":2,"failed":1,…}. Set only on the collapsed group entry
	// that run-detail payloads return in place of N instance rows (see
	// collapseFanOutGroups) — omitted for unfanned tasks and for the individual
	// instance rows the partition endpoints return. Without it the UI can render
	// partition_count but not the mix, so a 1000-instance group with one failure
	// looks identical to one with 500.
	PartitionStatusCounts map[string]int `json:"partition_status_counts,omitempty"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
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

	// runStateCache is the in-memory advancement state layered over this store
	// (the OwnerManager, under CAESIUM_RUN_OWNER_IN_MEMORY=true). It is a CACHE
	// of what the task_runs rows say, so the store must tell it when it
	// invalidates those rows out from under it — see invalidateRunState. Nil in
	// every other configuration, and set once at construction; atomic so the
	// race detector is satisfied about the one write.
	runStateCache atomic.Pointer[runStateInvalidator]
}

// runStateInvalidator is the seam a cached in-memory run state exposes so the
// store can discard it when a run is RE-OPENED.
//
// Only Drop is needed: rebuilding is lazy. The dispatch loop recovers a run it
// does not own on its next tick, and that rebuild reads the current rows.
type runStateInvalidator interface {
	Drop(runID uuid.UUID)
}

// SetRunStateCache registers the in-memory run state layered over this store.
// Called by NewOwnerManager so the wiring lives next to the thing being wired
// rather than in the server bootstrap, where forgetting it would look like
// nothing at all.
func (s *Store) SetRunStateCache(inv runStateInvalidator) {
	if s == nil || inv == nil {
		return
	}
	s.runStateCache.Store(&inv)
}

// invalidateRunState discards any cached in-memory advancement state for a run
// whose rows the store has just re-opened.
//
// A retry resets terminal task_runs rows back to pending and flips the run back
// to running. The owner's in-memory RunState is a snapshot taken before that
// and says the run is COMPLETE — and because the dispatch loop only rebuilds a
// run it does not already own (dispatchRunInMemory calls Recover behind
// `!mgr.Owns`), the stale snapshot is never refreshed: ReadyForDispatch returns
// nothing, no task is ever pushed, and the retried run hangs until its harness
// times out. Meanwhile the pull-path claimer will not touch it either, because
// the run still holds a live lease and liveLeaseGuardSQL defers to the owner.
//
// Dropping the state is half the fix. The run's CHECKPOINT is the same snapshot
// made durable, and recovery restores from it before replaying the terminal tail
// — so a rebuild that consulted a checkpoint written when the run was complete
// would reconstruct exactly the stale state that was just discarded. The tail
// cannot correct it either: a reset row stops being terminal, and
// TerminalTaskRunsSince reports rows that ARE terminal, never rows that stopped
// being so. Both copies go, and recovery replays from the task_runs rows, which
// are the system of record.
func (s *Store) invalidateRunState(runID uuid.UUID) {
	if s == nil {
		return
	}
	if inv := s.runStateCache.Load(); inv != nil && *inv != nil {
		(*inv).Drop(runID)
	}
	if err := s.DeleteCheckpoints(runID); err != nil {
		// Best effort: a surviving checkpoint delays the retry until the lease
		// expires and a clean takeover replays the rows, rather than losing work.
		log.Warn("run: failed to discard checkpoints for re-opened run", "run_id", runID, "error", err)
	}
}

type RegisterTaskInput struct {
	Task                    *models.Task
	Atom                    *models.Atom
	OutstandingPredecessors int
	PartitionIndex          int
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
	ErrQueuedRunUnavailable     = errors.New("run: queued run already claimed or unavailable")
	ErrQueuedRunNotFound        = errors.New("run: queued run not found")
	ErrMaxConcurrentRunsReached = errors.New("run: max concurrent runs reached")
	// ErrJobPaused is returned by RetryFromFailureAdmitted when an agent-initiated
	// retry is refused because the job is paused. A human pause outranks an agent
	// retry (design-agent-in-the-loop.md, retry safety valves).
	ErrJobPaused = errors.New("run: cannot retry while job is paused")
	// ErrPartitionNotRetryable is returned by RetryPartition when the addressed
	// instance is terminal but not FAILED. The retryable set is documented at
	// the guard in RetryPartition; controllers surface this as 409 with the
	// reason, distinguishable from ErrTaskRunNotTerminal (still running).
	ErrPartitionNotRetryable = errors.New("run: only a failed partition can be retried")
	// ErrPartitionRunNotRetryable is returned when the partition is failed but
	// its JobRun cannot execute a reset instance. Running runs can hand the work
	// to their live engine; succeeded/failed runs can be reopened. Cancelled,
	// queued, and unknown states must fail closed before any row/event mutation.
	ErrPartitionRunNotRetryable = errors.New("run: partition retry requires a running or completed run")
	// ErrPartitionRetryBlocked is returned by RetryPartition when the addressed
	// instance is FAILED but nothing in this run can ever make it ready again:
	// every cross-step predecessor group is terminal and the step's trigger
	// rule is not satisfied by them, or an in-group dependsOn sibling is
	// terminal without succeeding. Accepting such a retry would reset a row no
	// engine can dispatch. Controllers surface this as 409 and point at
	// whole-run retry, which resets failed and skipped work as a set.
	ErrPartitionRetryBlocked = errors.New("run: partition retry is blocked by an unsatisfied dependency")

	// ErrTaskRunNotTerminal is returned by RetryPartition when the addressed
	// fan-out instance is still pending or running. Resetting a RUNNING instance
	// mid-flight would orphan its container and let the eventual completion
	// overwrite the fresh attempt, so a per-partition retry is terminal-only.
	// The REST layer maps this to 409.
	ErrTaskRunNotTerminal = errors.New("run: task instance is not terminal")

	// ErrRunHasPendingWork is returned by Complete when a dispatchable
	// per-partition retry is still pending. RetryPartition sets the durable
	// partition_retry_pending marker in the same transaction as the reset; every
	// terminal transition clears it. Marking the JobRun terminal while that
	// marker is pending freezes the retried instance: RetryPartition of a
	// still-running local run reports reopened=false (no HTTP kickoff), and the
	// in-process engine may already have left the group. The same transaction as
	// the status write must refuse so this cannot TOCTOU with the retry.
	// Running/claimed retries do not match; a retry still waiting on a live
	// dependency does — it is exactly as stranded by a terminal JobRun as a
	// ready one, and the replacement engine is what releases it.
	ErrRunHasPendingWork = errors.New("run: cannot complete while task work is pending")
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
	ctx              context.Context
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

// SetTaskHash persists a task's identity hash. taskRef follows the
// TaskRun-primary-key-or-catalog-task-ID contract, so a fan-out instance is
// addressed by its own TaskRun ID.
func (s *Store) SetTaskHash(runID, taskRef uuid.UUID, hash string) error {
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("id = ?", row.ID).
		Update("hash", hash).Error
}

// SetTaskHashWithDigest persists the task identity hash together with the
// resolved image digest that was folded into it. The digest may be empty when
// pinning is off or resolution failed; in that case only the hash is written
// and the existing digest column (if any) is left untouched, keeping the row
// consistent with the literal-tag cache key.
func (s *Store) SetTaskHashWithDigest(runID, taskRef uuid.UUID, hash, resolvedImageDigest string) error {
	return s.SetTaskHashWithBlob(runID, taskRef, hash, resolvedImageDigest, nil)
}

// SetTaskHashWithBlob persists the task identity hash, the resolved image
// digest folded into it, and the canonical secret-redacted decomposition of the
// HashInput (the blob) on the same write — the existing hash write-path. The
// digest and blob are optional: an empty digest or a nil/empty blob leaves the
// corresponding column untouched (so a literal-tag, blob-less run stays
// consistent). The blob lets `caesium why` later diff two runs field-by-field
// rather than only observing that the opaque hashes differ.
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract. This
// matters more here than almost anywhere else: with loadUniqueTaskRun, a fanned
// step's per-instance identity write returned ErrAmbiguousTaskRun and the hash
// was NEVER persisted, so `caesium why --partition`, `receipt get` and
// `run retry --partition` had no identity to match and the local lane published
// no per-partition cache entry.
func (s *Store) SetTaskHashWithBlob(runID, taskRef uuid.UUID, hash, resolvedImageDigest string, hashInputBlob []byte) error {
	updates := map[string]any{"hash": hash}
	if resolvedImageDigest != "" {
		updates["resolved_image_digest"] = resolvedImageDigest
	}
	if len(hashInputBlob) > 0 {
		updates["hash_input_blob"] = datatypes.JSON(hashInputBlob)
	}
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("id = ?", row.ID).
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
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract: a
// value-verified short-circuit is proven per INSTANCE (its own output was
// byte-identical), so it must be recorded on that instance's row.
func (s *Store) SetTaskEffectiveHash(runID, taskRef uuid.UUID, effectiveHash string) error {
	if effectiveHash == "" {
		return nil
	}
	if err := s.mutateTaskExecutionDescriptor(runID, taskRef, func(desc *models.TaskExecutionDescriptor) {
		desc.Baseline.EffectiveHash = effectiveHash
		desc.Cache.EffectiveHash = effectiveHash
	}); err != nil {
		log.Warn("failed to update task execution descriptor effective hash", "run_id", runID, "task_ref", taskRef, "error", err)
	}
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("id = ?", row.ID).
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

// TaskExecutionDescriptor returns the frozen descriptor for one TaskRun.
//
// taskRef takes the usual TaskRun-primary-key-or-catalog-task-id contract, and
// callers holding a row MUST pass its primary key. The previous `(job_run_id,
// task_id) … Take` form silently picked an arbitrary sibling once a step could
// be fanned, and the one caller is the worker's quarantined-replay path — which
// executes the container the descriptor describes. Now that quarantined replay
// re-materializes fanned groups, that lookup would hand instance `bravo`'s
// container the descriptor recorded for `alpha`.
func (s *Store) TaskExecutionDescriptor(ctx context.Context, runID, taskRef uuid.UUID) (*models.TaskExecutionDescriptor, error) {
	taskRun, err := loadTaskRunByIDOrUnique(s.db.WithContext(ctx), runID, taskRef)
	if err != nil {
		return nil, err
	}
	if len(taskRun.ExecutionDescriptor) == 0 {
		return nil, fmt.Errorf("run: task execution descriptor missing for run %s task %s", runID, taskRef)
	}
	var descriptor models.TaskExecutionDescriptor
	if err := json.Unmarshal(taskRun.ExecutionDescriptor, &descriptor); err != nil {
		return nil, fmt.Errorf("run: decode task execution descriptor for run %s task %s: %w", runID, taskRef, err)
	}
	if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
		return nil, fmt.Errorf("run: unsupported task execution descriptor version %d for run %s task %s", descriptor.SchemaVersion, runID, taskRef)
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
	ctx := req.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn := s.db.WithContext(ctx)

	model, err := newStartRunModel(req)
	if err != nil {
		return nil, err
	}

	var (
		pendingEvents []event.Event
		admission     admissionResult
	)
	if err := withStoreBusyRetryContext(ctx, func() error {
		attemptEvents := make([]event.Event, 0, 3)
		attemptAdmission := admissionResult{decision: admissionNoPolicy}
		err := conn.Transaction(func(tx *gorm.DB) error {
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
			ctx,
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

	return s.loadRunWithDB(conn, model.ID)
}

// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract so a
// fan-out instance mutates its own execution descriptor.
func (s *Store) mutateTaskExecutionDescriptor(runID, taskRef uuid.UUID, mutate func(*models.TaskExecutionDescriptor)) error {
	if mutate == nil {
		return nil
	}

	for attempt := 0; attempt <= len(storeBusyRetryBackoffs); attempt++ {
		taskRun, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
		if err != nil {
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
			Where("id = ?", taskRun.ID)
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
	return s.StartWithContext(context.Background(), jobID, triggerID, opts...)
}

func (s *Store) StartWithContext(ctx context.Context, jobID uuid.UUID, triggerID *uuid.UUID, opts ...StartOption) (*JobRun, error) {
	startOpts := startOptionsFrom(opts)
	return s.startRun(startRunRequest{
		ctx:              ctx,
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
func (s *Store) StartQueuedRun(ctx context.Context, queued *models.RunQueue) (*JobRun, error) {
	if queued == nil {
		return nil, errors.New("run: queued run is nil")
	}
	return s.startRun(startRunRequest{
		ctx:              ctx,
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

	type instanceKey struct {
		taskID uuid.UUID
		index  int
	}
	taskIDs := make([]uuid.UUID, 0, len(inputs))
	seenInputKeys := make(map[instanceKey]struct{}, len(inputs))
	for _, input := range inputs {
		if input.Task == nil || input.Atom == nil {
			return errors.New("run: task and atom must be provided")
		}
		key := instanceKey{taskID: input.Task.ID, index: input.PartitionIndex}
		if _, ok := seenInputKeys[key]; ok {
			continue
		}
		seenInputKeys[key] = struct{}{}
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
			var existingRows []struct {
				TaskID         uuid.UUID
				PartitionIndex int
			}
			if len(taskIDs) > 0 {
				if err := tx.Model(&models.TaskRun{}).
					Select("task_id", "partition_index").
					Where("job_run_id = ? AND task_id IN ?", runID, taskIDs).
					Find(&existingRows).Error; err != nil {
					return err
				}
			}
			existing := make(map[instanceKey]struct{}, len(existingRows))
			for _, row := range existingRows {
				existing[instanceKey{taskID: row.TaskID, index: row.PartitionIndex}] = struct{}{}
			}

			records := make([]models.TaskRun, 0, len(inputs))
			readyEvents := make([]event.Event, 0, len(inputs))
			seenNewKeys := make(map[instanceKey]struct{}, len(inputs))
			for _, input := range inputs {
				task := input.Task
				atom := input.Atom
				key := instanceKey{taskID: task.ID, index: input.PartitionIndex}
				if _, ok := existing[key]; ok {
					continue
				}
				if _, ok := seenNewKeys[key]; ok {
					continue
				}
				seenNewKeys[key] = struct{}{}

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
					PartitionIndex:          input.PartitionIndex,
					OutstandingPredecessors: input.OutstandingPredecessors,
					CacheEnabled:            resolvedCache.Enabled,
					CacheTTL:                resolvedCache.TTL,
					CacheVersion:            resolvedCache.Version,
					ReplaySafe:              replaySafe,
					CachePinDigests:         resolvedCache.PinDigests,
					CacheDigestTTL:          resolvedCache.DigestTTL,
					CacheChain:              resolvedCache.Chain,
					CacheTTLNever:           resolvedCache.TTLNever,
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
			Chain:      cacheCfg.Chain,
			TTLNever:   cacheCfg.TTLNever,
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

// StartTask marks a task run as running. taskRef is resolved by
// loadTaskRunByIDOrUnique, so it may be either a TaskRun primary key (required
// for a fan-out instance, where the catalog task ID matches N sibling rows) or a
// catalog task ID (unfanned steps, which still have exactly one row).
func (s *Store) StartTask(runID, taskRef uuid.UUID, runtimeID string) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			row, err := loadTaskRunByIDOrUnique(tx, runID, taskRef)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return err
			}
			result := tx.Model(&models.TaskRun{}).
				Where("id = ? AND status NOT IN ?", row.ID, terminalTaskStatuses()).
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
				evt, err := s.recordTaskRunEventTx(tx, event.TypeTaskStarted, runID, row, &counts)
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
			row, loadErr := loadTaskRunByIDOrUnique(tx, runID, taskID)
			if loadErr != nil {
				if errors.Is(loadErr, gorm.ErrRecordNotFound) {
					return ErrTaskClaimMismatch
				}
				return loadErr
			}
			// fanOut.maxParallel is enforced as part of the claim UPDATE, not as a
			// separate pre-check, so the cap and the claim are one statement (see
			// fanOutMaxParallelPredicateTx).
			capSQL, capArgs, capErr := s.fanOutMaxParallelPredicateTx(tx, row)
			if capErr != nil {
				return capErr
			}
			where := "id = ? AND status = ? AND claimed_by = '' AND outstanding_predecessors = 0 AND owner_generation <= ? AND (rate_limit_retry_after IS NULL OR rate_limit_retry_after <= ?)"
			whereArgs := []interface{}{row.ID, string(TaskStatusPending), ownerGeneration, now}
			if trustOwnerReadiness {
				where = "id = ? AND status = ? AND claimed_by = '' AND owner_generation <= ? AND (rate_limit_retry_after IS NULL OR rate_limit_retry_after <= ?)"
			}
			where += capSQL
			whereArgs = append(whereArgs, capArgs...)
			result := tx.Model(&models.TaskRun{}).
				Where(where, whereArgs...).
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
				evt, err := s.recordTaskRunEventTx(tx, event.TypeTaskStarted, runID, row, &counts)
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
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskClaimMismatch
		}
		return nil, err
	}
	var taskRun models.TaskRun
	err = s.db.
		Where("id = ? AND claimed_by = ? AND status = ?",
			row.ID, claimedBy, string(TaskStatusRunning)).
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
		row, err := loadTaskRunByIDOrUnique(s.db, runID, taskID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		result := s.db.Model(&models.TaskRun{}).
			Where("id = ? AND claimed_by = ? AND status = ? AND (owner_generation = ? OR owner_generation = 0)",
				row.ID, claimedBy, string(TaskStatusRunning), ownerGeneration).
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
//
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract, so a
// fan-out instance parks by its own TaskRun ID and its siblings keep running.
//
// G1 open question — settled here: the `status IN (pending, running)` predicate
// is KEPT, and it is deliberate rather than a leftover. Both claim paths flip a
// row to `running` at CLAIM time (ClaimTaskForDispatch, and the claimer's atomic
// UPDATE), and the rate-limit rejection is only discovered afterwards, before any
// container exists — so the row this parks is legitimately `running` and has
// nothing in flight to orphan. What made the predicate dangerous was never the
// status set; it was the `WHERE job_run_id = ? AND task_id = ?` predicate, which
// re-pended every RUNNING sibling of a fanned group and orphaned their live
// containers. Now that the write is keyed to one instance's primary key, matching
// `running` affects exactly the row whose own rate-limit acquisition was
// rejected. Callers must therefore pass the instance's TaskRun ID; passing a
// catalog task ID for an expanded group now fails loudly with ErrAmbiguousTaskRun
// instead of silently parking a sibling.
// RateLimitTask parks ONE task instance until retryAfter, releasing its claim so
// the next tick can re-acquire the rate-limit token.
//
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract; a fanned
// group must be addressed by instance, and a catalog task ID naming N siblings
// is refused (ErrAmbiguousTaskRun) rather than parking an arbitrary one.
//
// The `status IN (pending, running)` predicate was G1's one open question — it
// is KEPT, deliberately. Matching `running` looks like it could re-pend a live
// instance and orphan its container, but the rate-limit rejection is discovered
// AFTER the claim has already flipped the row to running and BEFORE any
// container exists (acquireTaskRateLimit runs at dispatch, not mid-execution),
// so the running row this matches has nothing in flight. What was a real bug is
// now closed by the re-key: the old (job_run_id, task_id) predicate fanned the
// re-pend across every sibling, so parking one instance re-pended its RUNNING
// siblings and did orphan their containers. Pinned by
// TestRateLimitTaskParksOneInstance.
func (s *Store) RateLimitTask(ctx context.Context, runID, taskRef uuid.UUID, retryAfter time.Time) error {
	if retryAfter.IsZero() {
		retryAfter = time.Now().UTC()
	}
	retryAfter = retryAfter.UTC()

	return withStoreBusyRetry(func() error {
		row, err := loadTaskRunByIDOrUnique(s.db.WithContext(ctx), runID, taskRef)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		result := s.db.WithContext(ctx).Model(&models.TaskRun{}).
			Where("id = ? AND status IN ?", row.ID, []string{string(TaskStatusPending), string(TaskStatusRunning)}).
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

// EnsureTaskRunStartable reports whether a claimed task may still be started,
// returning ErrTaskClaimMismatch when it may not.
//
// A worker holds its claimed task for as long as its pool takes to free a slot,
// and in that window the row can be resolved out from under it — fail_fast
// cancelling a sibling of a group that has already failed is the case this
// exists for (markInstanceCancelledBeforeStartTx revokes the claim as part of
// the cancel). The executor calls this immediately before creating the
// container, because engine.Create both creates AND starts it: checking only
// afterwards, in StartTaskClaimed, would mean the cancelled work had already
// run.
//
// Two conditions, either of which means the task is no longer this worker's to
// start: the row reached a terminal status, or its claim now belongs to someone
// else (including "" after a release). The check is advisory by nature — it is
// not the same transaction as the container create — so StartTaskClaimed's
// guarded UPDATE remains the authoritative fence; this only keeps a doomed
// container from being created in the first place.
func (s *Store) EnsureTaskRunStartable(runID, taskRef uuid.UUID, claimedBy string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("run: ensure task run startable: nil store")
	}
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrAmbiguousTaskRun) {
			return ErrTaskClaimMismatch
		}
		return err
	}
	if IsTerminal(TaskStatus(row.Status)) || row.ClaimedBy != claimedBy {
		return ErrTaskClaimMismatch
	}
	return nil
}

// StartTaskClaimed is the distributed-lane counterpart of StartTask. taskRef
// follows the same TaskRun-primary-key-or-catalog-task-ID contract, so a worker
// executing a fan-out instance must pass the instance's TaskRun ID.
func (s *Store) StartTaskClaimed(runID, taskRef uuid.UUID, runtimeID, claimedBy string) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			row, err := loadTaskRunByIDOrUnique(tx, runID, taskRef)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrTaskClaimMismatch
				}
				return err
			}
			result := tx.Model(&models.TaskRun{}).
				Where("id = ? AND claimed_by = ? AND status = ?", row.ID, claimedBy, string(TaskStatusRunning)).
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
				evt, err := s.recordTaskRunEventTx(tx, event.TypeTaskStarted, runID, row, &counts)
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
	skipped, _, err := s.completeTask(runID, taskID, uuid.Nil, result, "", false, output, branchSelections, nil)
	_ = skipped
	return err
}

// CompleteTaskResult holds the result of a task completion, including any
// tasks that were skipped due to branch filtering and any fan-out expansion
// this completion performed.
type CompleteTaskResult struct {
	SkippedTaskIDs []uuid.UUID
	Expansion      *FanOutExpansion
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
	return s.CompleteTaskWithPartitions(runID, taskID, result, output, branchSelections, nil)
}

// CompleteTaskWithPartitions is CompleteTaskWithResult plus the producer's
// parsed partition list, which is expanded inside the completion transaction.
func (s *Store) CompleteTaskWithPartitions(runID, taskID uuid.UUID, result string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) (*CompleteTaskResult, error) {
	skipped, expansion, err := s.completeTask(runID, taskID, uuid.Nil, result, "", false, output, branchSelections, partitions)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped, Expansion: expansion}, nil
}

func (s *Store) CompleteTaskClaimed(runID, taskID uuid.UUID, result, claimedBy string, output map[string]string, branchSelections []string) error {
	_, _, err := s.completeTask(runID, taskID, uuid.Nil, result, claimedBy, true, output, branchSelections, nil)
	return err
}

func (s *Store) CompleteTaskClaimedWithPartitions(runID, taskID uuid.UUID, result, claimedBy string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) error {
	_, _, err := s.completeTask(runID, taskID, uuid.Nil, result, claimedBy, true, output, branchSelections, partitions)
	return err
}

// CompleteTaskInstance completes a specific TaskRun (fan-out instance) by primary key.
func (s *Store) CompleteTaskInstance(taskRunID uuid.UUID, result string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) (*CompleteTaskResult, error) {
	var row models.TaskRun
	if err := s.db.First(&row, "id = ?", taskRunID).Error; err != nil {
		return nil, err
	}
	skipped, expansion, err := s.completeTask(row.JobRunID, row.TaskID, row.ID, result, "", false, output, branchSelections, partitions)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped, Expansion: expansion}, nil
}

// CacheHitTask marks a task as completed via cache hit (local mode).
// It mirrors the CompleteTaskWithResult flow but sets status to "cached".
func (s *Store) CacheHitTask(runID, taskID uuid.UUID, source CacheHitSource, result string, output map[string]string, branchSelections []string) (*CompleteTaskResult, error) {
	return s.CacheHitTaskWithPartitions(runID, taskID, source, result, output, branchSelections, nil)
}

// CacheHitTaskWithPartitions is CacheHitTask plus the producer's parsed
// partition list, expanded inside the SAME transaction that writes the cached
// terminal row.
//
// A cache hit is a completion, and cacheHitTask is a different function from
// completeTask — so an expansion hook placed only on the completion route is
// unreachable whenever a producer's own work cache-hits, and the group silently
// collapses to its single template row. That is the *common* path, not an edge
// case: with per-unit fingerprints the whole point is that repeated work hits
// the cache, and a cached producer still replays its partition list out of the
// cache entry (internal/worker/runtime_executor.go reads entry.Partitions).
//
// The expansion runs the identical rules as the completion route — it calls
// expandFanOutSuccessorsTx, so validation (cycles, dangling dependsOn keys,
// caps), onEmpty handling, in-group indegree seeding and producer-list
// persistence are one implementation, not a second copy that can drift.
func (s *Store) CacheHitTaskWithPartitions(runID, taskID uuid.UUID, source CacheHitSource, result string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) (*CompleteTaskResult, error) {
	skipped, expansion, err := s.cacheHitTask(runID, taskID, source, result, "", false, output, branchSelections, partitions)
	if err != nil {
		return nil, err
	}
	return &CompleteTaskResult{SkippedTaskIDs: skipped, Expansion: expansion}, nil
}

// CacheHitTaskClaimed marks a claimed task as completed via cache hit (distributed mode).
func (s *Store) CacheHitTaskClaimed(runID, taskID uuid.UUID, source CacheHitSource, result, claimedBy string, output map[string]string, branchSelections []string) error {
	return s.CacheHitTaskClaimedWithPartitions(runID, taskID, source, result, claimedBy, output, branchSelections, nil)
}

// CacheHitTaskClaimedWithPartitions is the claim-fenced twin of
// CacheHitTaskWithPartitions, and the method internal/worker/completion_sink.go
// and internal/dispatch/dispatch.go resolve by INTERFACE ASSERTION
// (cacheHitPartitionStore). Because the binding is an assertion rather than a
// compile-time call, a signature drift here does not fail the build — it makes
// the assertion miss, which those sites report as
// "run store cannot persist producer partitions; fan-out group will not expand"
// and then continue without expanding. Keep the signature in lockstep with
// those declarations.
func (s *Store) CacheHitTaskClaimedWithPartitions(runID, taskID uuid.UUID, source CacheHitSource, result, claimedBy string, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) error {
	_, _, err := s.cacheHitTask(runID, taskID, source, result, claimedBy, true, output, branchSelections, partitions)
	return err
}

func (s *Store) cacheHitTask(runID, taskRef uuid.UUID, source CacheHitSource, result, claimedBy string, enforceClaim bool, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) ([]uuid.UUID, *FanOutExpansion, error) {
	var pendingEvents []event.Event
	var skippedTaskIDs []uuid.UUID
	var expansion *FanOutExpansion
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8)
		attemptSkippedTaskIDs := make([]uuid.UUID, 0)
		var attemptExpansion *FanOutExpansion

		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			// taskRef is the caller's IMMUTABLE reference (a TaskRun primary key
			// for a fanned instance, or a catalog task id for an unfanned task);
			// catalogTaskID is this attempt's resolution of it. Keeping them
			// separate is load-bearing: this closure re-runs on transient
			// contention, and overwriting the caller's reference with the catalog
			// id made the RETRY resolve a group by task id — N rows, so
			// ErrAmbiguousTaskRun, surfaced to the worker as a claim mismatch. A
			// completion lost to a single SQLITE_BUSY is a stalled run.
			taskRunPtr, loadErr := loadTaskRunByIDOrUnique(tx, runID, taskRef)
			if loadErr != nil {
				if enforceClaim {
					return ErrTaskClaimMismatch
				}
				return loadErr
			}
			taskRun := *taskRunPtr
			catalogTaskID := taskRun.TaskID
			if enforceClaim && taskRun.ClaimedBy != claimedBy {
				return ErrTaskClaimMismatch
			}
			if IsTerminal(TaskStatus(taskRun.Status)) {
				return nil
			}

			updateQuery := tx.Model(&models.TaskRun{}).
				Where("id = ?", taskRun.ID)
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}

			updates := map[string]interface{}{
				"status":                  string(TaskStatusCached),
				"completed_at":            now,
				"result":                  result,
				"cache_hit":               true,
				"cache_origin_run_id":     source.RunID,
				"cache_created_at":        source.CreatedAt,
				"cache_expires_at":        source.ExpiresAt,
				"partition_retry_pending": false,
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

			descriptor, replayTask, err := s.replayTaskExecutionDescriptorTx(tx, runID, catalogTaskID)
			if err != nil {
				return err
			}

			// Load the task model for edge fallback and branch detection.
			var taskModel models.Task
			taskType := ""
			if replayTask {
				taskModel = models.Task{ID: catalogTaskID}
				taskType = firstNonEmpty(descriptor.Runtime.TaskType, descriptor.DAG.BranchBehavior, "task")
			} else {
				if err := tx.First(&taskModel, "id = ?", catalogTaskID).Error; err != nil {
					return err
				}
				taskType = taskModel.Type
			}

			edges, err := s.successorEdgesForRunTx(tx, runID, catalogTaskID, taskModel)
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
			allSuccessorIDs := make([]uuid.UUID, 0, len(edges))
			for _, edge := range edges {
				allSuccessorIDs = append(allSuccessorIDs, edge.ToTaskID)
				if branchSelectedIDs != nil && !branchSelectedIDs[edge.ToTaskID] {
					reason := fmt.Sprintf("not selected by branch task %s", catalogTaskID)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, reason, &attemptEvents, &counts)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
					continue
				}
				toDecrementIDs = append(toDecrementIDs, edge.ToTaskID)
			}

			if isFanOutInstance(&taskRun) {
				if err := s.decrementInGroupDependentsTx(tx, runID, &taskRun); err != nil {
					return err
				}
				allTerm, gErr := s.groupAllTerminalTx(tx, runID, catalogTaskID)
				if gErr != nil {
					return gErr
				}
				if !allTerm {
					return nil
				}
			}

			// Expansion rides the cache-hit transaction exactly as it rides the
			// completion transaction (completeTask), through the SAME function —
			// so a cached producer's group is never half-expanded and never
			// validated by a second, laxer copy of the rules. Ordering matches
			// completeTask: after the in-group decrement and the group-terminal
			// gate, before the cross-step decrement, so the instances this
			// creates already carry their seeded outstanding_predecessors when
			// the successors are decremented below.
			exp, expErr := s.expandFanOutSuccessorsTx(tx, runID, catalogTaskID, &taskRun, taskModel.Name, allSuccessorIDs, partitions, &attemptEvents, &counts)
			if expErr != nil {
				return expErr
			}
			attemptExpansion = exp

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
				// Build task_cached event and add to batch. Read the row by its
				// primary key: the (job_run_id, task_id) predicate matched an
				// arbitrary sibling, so a fanned group emitted N task_cached
				// events all describing partition 0.
				var taskRunModel models.TaskRun
				if err := tx.Where("id = ?", taskRun.ID).First(&taskRunModel).Error; err != nil {
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
					TaskID:    catalogTaskID,
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
			expansion = attemptExpansion
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return skippedTaskIDs, expansion, err
}

// SaveTaskLogSnapshot persists the captured log snapshot onto exactly one task
// run. taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract: a
// fan-out instance must be addressed by its TaskRun ID, otherwise the snapshot
// would be broadcast across every sibling row sharing (job_run_id, task_id).
func (s *Store) SaveTaskLogSnapshot(runID, taskRef uuid.UUID, snapshot *TaskLogSnapshot) error {
	if snapshot == nil {
		return nil
	}

	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}

	return s.db.Model(&models.TaskRun{}).
		Where("id = ?", row.ID).
		Updates(map[string]interface{}{
			"log_text":      snapshot.Text,
			"log_truncated": snapshot.Truncated,
		}).Error
}

// SetTaskExitCode persists the raw process exit code the engine reported at
// task completion onto the task run. The incident classifier reads this
// alongside SchemaViolations/Result to bucket a failure into a failure_class.
// Best-effort: a nil-safe no-op when the row is gone.
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract so a
// fan-out instance records its own exit code instead of overwriting its
// siblings'.
func (s *Store) SetTaskExitCode(runID, taskRef uuid.UUID, exitCode *int) error {
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("id = ?", row.ID).
		Update("exit_code", exitCode).Error
}

// SaveSchemaViolations persists schema validation violations onto exactly one
// task run. taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract:
// a fan-out instance must be addressed by its TaskRun ID. The old
// `WHERE job_run_id = ? AND task_id = ?` predicate broadcast one instance's
// violations across every sibling row, so a single bad partition made all N look
// schema-invalid. Resolving through loadTaskRunByIDOrUnique means an ambiguous
// catalog task ID now fails loudly (ErrAmbiguousTaskRun) instead of fanning the
// write.
func (s *Store) SaveSchemaViolations(runID, taskRef uuid.UUID, violations []pkgtask.SchemaViolation) error {
	if len(violations) == 0 {
		return nil
	}
	b, err := json.Marshal(violations)
	if err != nil {
		return err
	}
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	return s.db.Model(&models.TaskRun{}).
		Where("id = ?", row.ID).
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
	if counts != nil {
		counts.addEventInsert(1)
	}
	if pendingEvents != nil {
		*pendingEvents = append(*pendingEvents, evt)
	}
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

// batchDecrementSiblingPredecessorsTx decrements outstanding_predecessors for
// specific TaskRun primary keys (in-group edges). Cross-step edges still use
// batchDecrementPredecessorsTx's task_id IN form.
func (s *Store) batchDecrementSiblingPredecessorsTx(tx *gorm.DB, runID uuid.UUID, instanceIDs []uuid.UUID) ([]models.TaskRun, error) {
	if len(instanceIDs) == 0 {
		return nil, nil
	}
	if err := tx.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND id IN ?", runID, instanceIDs).
		UpdateColumn("outstanding_predecessors", gorm.Expr("CASE WHEN outstanding_predecessors > 0 THEN outstanding_predecessors - 1 ELSE 0 END")).Error; err != nil {
		return nil, err
	}
	var updated []models.TaskRun
	if err := tx.Where("job_run_id = ? AND id IN ?", runID, instanceIDs).Find(&updated).Error; err != nil {
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
		found := 0
		for _, id := range ids {
			if _, ok := taskQuarantine[eventQuarantineKey{runID: runID, taskID: id}]; ok {
				found++
			}
		}
		if found != len(ids) {
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

// nextTerminalSequenceTx allocates the next per-run terminal_sequence for a row
// the SQL advancement lane is about to make terminal.
//
// terminal_sequence used to be stamped only by the run-owner path
// (CompleteTaskOwner), so every SQL-lane skip landed at 0 — and
// TerminalTaskRunsSince selects `terminal_sequence > ?`, which excludes 0. A
// skipped fan-out instance was therefore invisible to a recovering owner and to
// the terminal-row replay tail. Allocating MAX(terminal_sequence)+1 for the run
// keeps the space monotonic and, because it is read inside the caller's
// transaction, dense across the rows one transaction marks terminal. It can
// never collide with an owner-allocated value: owner-managed runs return before
// the SQL lane is reached (dispatch.go short-circuits on res.Owned), and taking
// max+1 always exceeds anything already persisted.
func nextTerminalSequenceTx(tx *gorm.DB, runID uuid.UUID) (int64, error) {
	var maxSeq int64
	if err := tx.Model(&models.TaskRun{}).
		Where("job_run_id = ?", runID).
		Select("COALESCE(MAX(terminal_sequence), 0)").
		Scan(&maxSeq).Error; err != nil {
		return 0, err
	}
	return maxSeq + 1, nil
}

// markInstanceSkippedTx is THE terminal-skip primitive: it marks exactly one
// TaskRun row skipped by primary key, stamps that row its own
// terminal_sequence, and emits that row's own task_skipped event.
//
// Every skip route funnels through here — cross-step group skip
// (markTaskSkippedTx), branch/trigger-rule descendant skip
// (skipTaskAndDescendantsTx), and the fan-out in-group dependency cascade
// (skipInGroupDependentsTx) — so a skipped instance is never a row that quietly
// shares a sibling's sequence or a sibling's event payload.
func (s *Store) markInstanceSkippedTx(tx *gorm.DB, runID uuid.UUID, row *models.TaskRun, reason string, pendingEvents *[]event.Event, counts *dbWriteCounts) (bool, error) {
	return s.markInstanceSkippedFromTx(tx, runID, row, reason, []string{string(TaskStatusPending)}, pendingEvents, counts)
}

// markInstanceSkippedFromTx is markInstanceSkippedTx with an explicit set of
// source statuses the transition is allowed from.
//
// It exists so a caller can state that set rather than inherit it. Most routes
// pass {pending} — skipping a RUNNING row would strand a live container whose
// worker still owns the claim, the orphaned-container shape behind the
// local-mode replace-cancel bug — and stating it at the call site is what makes
// that a deliberate choice instead of an accident of this primitive's default.
// The one route that legitimately reaches past pending is cancel-before-start
// (markInstanceCancelledBeforeStartTx), which uses a predicate rather than a
// status list because "claimed but no container yet" is not a status.
func (s *Store) markInstanceSkippedFromTx(tx *gorm.DB, runID uuid.UUID, row *models.TaskRun, reason string, fromStatuses []string, pendingEvents *[]event.Event, counts *dbWriteCounts) (bool, error) {
	if len(fromStatuses) == 0 {
		fromStatuses = []string{string(TaskStatusPending)}
	}
	return s.markInstanceSkippedWhereTx(tx, runID, row, reason, "status IN ?", []interface{}{fromStatuses}, nil, pendingEvents, counts)
}

// markInstanceCancelledBeforeStartTx resolves one instance skipped only while it
// still has nothing running: pending, or claimed with no container yet (see
// taskRunStarted for why runtime_id decides). It is fail_fast's cancel, and the
// window it closes is the one between a claim and a container.
//
// Two things beyond an ordinary skip:
//
//   - The pre-start test is part of the guarded UPDATE, not a pre-check. A
//     worker that creates the container between the sibling SELECT and this
//     write makes the UPDATE match zero rows, so the cancel loses and the row is
//     left to run — the safe direction. The alternative, testing in Go against a
//     row read earlier in the transaction, is exactly how a live container ends
//     up with a terminal row.
//   - The claim is revoked (claimed_by, claim_expires_at, started_at cleared).
//     That is what makes the cancel decisive rather than advisory: the worker's
//     StartTaskClaimed requires `claimed_by = <this worker> AND status =
//     running`, so once this commits the start cannot succeed, and the executor
//     treats the resulting ErrTaskClaimMismatch as "resolved out from under me"
//     — no container, no completion posted. started_at is cleared because
//     ClaimTaskForDispatch stamped it at claim time for a task that never ran;
//     leaving it would report a duration for work that never happened.
func (s *Store) markInstanceCancelledBeforeStartTx(tx *gorm.DB, runID uuid.UUID, row *models.TaskRun, reason string, pendingEvents *[]event.Event, counts *dbWriteCounts) (bool, error) {
	predSQL, predArgs := cancellableBeforeStartPredicate()
	return s.markInstanceSkippedWhereTx(tx, runID, row, reason, predSQL, predArgs, map[string]interface{}{
		"claimed_by":       "",
		"claim_expires_at": nil,
		"started_at":       nil,
	}, pendingEvents, counts)
}

// markInstanceSkippedWhereTx is the shared body: one guarded UPDATE by primary
// key plus the caller's own source predicate, its own terminal_sequence, and its
// own task_skipped event.
func (s *Store) markInstanceSkippedWhereTx(
	tx *gorm.DB,
	runID uuid.UUID,
	row *models.TaskRun,
	reason string,
	predSQL string,
	predArgs []interface{},
	extraUpdates map[string]interface{},
	pendingEvents *[]event.Event,
	counts *dbWriteCounts,
) (bool, error) {
	if row == nil {
		return false, nil
	}
	seq, err := nextTerminalSequenceTx(tx, runID)
	if err != nil {
		return false, err
	}
	updates := map[string]interface{}{
		"status":                  string(TaskStatusSkipped),
		"completed_at":            time.Now().UTC(),
		"error":                   reason,
		"terminal_sequence":       seq,
		"cache_hit":               false,
		"cache_origin_run_id":     nil,
		"cache_created_at":        nil,
		"cache_expires_at":        nil,
		"partition_retry_pending": false,
	}
	for k, v := range extraUpdates {
		updates[k] = v
	}
	result := tx.Model(&models.TaskRun{}).
		Where("id = ? AND "+predSQL, append([]interface{}{row.ID}, predArgs...)...).
		Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	if counts != nil {
		counts.addTaskRunStatus(1)
	}
	row.Status = string(TaskStatusSkipped)
	row.TerminalSequence = seq
	row.Error = reason

	if s.eventStore != nil && pendingEvents != nil {
		evt, err := s.recordTaskRunEventTx(tx, event.TypeTaskSkipped, runID, row, counts)
		if err != nil {
			return false, err
		}
		*pendingEvents = append(*pendingEvents, *evt)
	}
	return true, nil
}

// markTaskSkippedTx marks the whole (runID, taskID) group skipped — the
// cross-step case, where a predecessor step failed so every instance of the
// fanned successor is skipped. It walks the instances and marks each ONE AT A
// TIME through markInstanceSkippedTx, so each gets its own terminal_sequence and
// its own task_skipped event carrying its own partition. The previous single
// task-keyed UPDATE collapsed all N into one event and one (zero) sequence.
// Returns true when at least one instance transitioned.
func (s *Store) markTaskSkippedTx(tx *gorm.DB, runID, taskID uuid.UUID, reason string, pendingEvents *[]event.Event, counts *dbWriteCounts) (bool, error) {
	var rows []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ? AND status = ?", runID, taskID, string(TaskStatusPending)).
		Order("partition_index ASC").
		Find(&rows).Error; err != nil {
		return false, err
	}
	any := false
	for i := range rows {
		marked, err := s.markInstanceSkippedTx(tx, runID, &rows[i], reason, pendingEvents, counts)
		if err != nil {
			return any, err
		}
		any = any || marked
	}
	return any, nil
}

// predecessorRef is one resolved predecessor edge: the catalog task ID plus the
// step name downstream consumers key outputs by. HasCatalogRow is false only on
// the live path when the predecessor's catalog Task row could not be read, which
// is the condition PredecessorOutputs has always treated as "skip this
// predecessor".
type predecessorRef struct {
	TaskID        uuid.UUID
	Name          string
	HasCatalogRow bool
}

// resolvePredecessorsTx is THE predecessor-edge kernel (G7).
//
// Before it, replayPredecessorRefsTx forked the live and replay implementations
// at FIVE separate call sites — predecessorStatusesTx, shouldRunTaskTx,
// PredecessorOutputs, PredecessorDescriptorInputs and PredecessorHashes — each
// of which had to independently remember that a quarantined replay run resolves
// its DAG from the per-TaskRun execution descriptor rather than from the live
// task_edges catalog. That duplication is the shape that produced four P1s in
// one family: teach one copy about a new edge class and the other silently keeps
// the old behavior. Now the fork exists in exactly one function and every
// consumer is a pure function of its output.
//
// Live: task_edges into this task, plus the predecessors' catalog names.
// Replay: the descriptor's frozen predecessor refs, so a later apply cannot
// change what a replay run considers its inputs.
func (s *Store) resolvePredecessorsTx(tx *gorm.DB, runID, taskID uuid.UUID) ([]predecessorRef, error) {
	refs, replay, err := s.replayPredecessorRefsTx(tx, runID, taskID)
	if err != nil {
		return nil, err
	}
	if replay {
		out := make([]predecessorRef, 0, len(refs))
		for _, ref := range refs {
			if ref.TaskID == uuid.Nil {
				continue
			}
			out = append(out, predecessorRef{
				TaskID:        ref.TaskID,
				Name:          firstNonEmpty(ref.TaskName, ref.TaskID.String()),
				HasCatalogRow: true,
			})
		}
		return out, nil
	}

	var edges []models.TaskEdge
	if err := tx.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, nil
	}
	predTaskIDs := make([]uuid.UUID, 0, len(edges))
	for _, edge := range edges {
		predTaskIDs = append(predTaskIDs, edge.FromTaskID)
	}

	// Name resolution is best-effort and deliberately non-fatal: a predecessor
	// whose catalog row is unreadable keeps its place in the STATUS list (so
	// trigger rules are unchanged) but is dropped from the name-keyed OUTPUT map,
	// which is exactly what the pre-G7 live path did.
	namesByID := make(map[uuid.UUID]string, len(predTaskIDs))
	var tasks []models.Task
	if err := tx.Where("id IN ?", predTaskIDs).Find(&tasks).Error; err != nil {
		log.Warn("failed to resolve predecessor task names", "run_id", runID, "task_id", taskID, "error", err)
	} else {
		for i := range tasks {
			namesByID[tasks[i].ID] = firstNonEmpty(tasks[i].Name, tasks[i].ID.String())
		}
	}

	out := make([]predecessorRef, 0, len(edges))
	for _, edge := range edges {
		name, ok := namesByID[edge.FromTaskID]
		out = append(out, predecessorRef{TaskID: edge.FromTaskID, Name: name, HasCatalogRow: ok})
	}
	return out, nil
}

// predecessorTaskIDs projects the kernel's refs onto the ID list the row queries
// take.
func predecessorTaskIDs(refs []predecessorRef) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.TaskID)
	}
	return ids
}

// predecessorTaskRunsTx loads every TaskRun row belonging to the resolved
// predecessors, ordered by partition index so group aggregation is
// deterministic. columns narrows the SELECT; empty means all columns.
func predecessorTaskRunsTx(tx *gorm.DB, runID uuid.UUID, refs []predecessorRef, columns ...string) ([]models.TaskRun, error) {
	ids := predecessorTaskIDs(refs)
	if len(ids) == 0 {
		return nil, nil
	}
	q := tx.Where("job_run_id = ? AND task_id IN ?", runID, ids).Order("partition_index ASC")
	if len(columns) > 0 {
		cols := make([]interface{}, 0, len(columns)-1)
		for _, c := range columns[1:] {
			cols = append(cols, c)
		}
		q = q.Select(columns[0], cols...)
	}
	var rows []models.TaskRun
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) predecessorStatusesTx(tx *gorm.DB, runID, taskID uuid.UUID) ([]TaskStatus, error) {
	refs, err := s.resolvePredecessorsTx(tx, runID, taskID)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}
	taskRuns, err := predecessorTaskRunsTx(tx, runID, refs, "task_id", "status")
	if err != nil {
		return nil, err
	}
	return aggregatePredecessorStatuses(predecessorTaskIDs(refs), taskRuns), nil
}

func aggregatePredecessorStatuses(predIDs []uuid.UUID, taskRuns []models.TaskRun) []TaskStatus {
	byTask := make(map[uuid.UUID][]models.TaskRun, len(predIDs))
	for i := range taskRuns {
		id := taskRuns[i].TaskID
		byTask[id] = append(byTask[id], taskRuns[i])
	}
	statuses := make([]TaskStatus, 0, len(predIDs))
	for _, id := range predIDs {
		statuses = append(statuses, groupStatusFromInstances(byTask[id]))
	}
	return statuses
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

// resolveTriggerRuleTx is the trigger-rule half of the G7 de-duplication: the
// ONE place that knows a quarantined replay run reads its trigger rule from the
// frozen execution descriptor while a live run reads it from the catalog. It
// returns the raw rule (which satisfiesTriggerRule treats "" as all_success) and
// its normalized form for reporting.
func (s *Store) resolveTriggerRuleTx(tx *gorm.DB, runID, taskID uuid.UUID) (raw, normalized string, err error) {
	descriptor, replay, err := s.replayTaskExecutionDescriptorTx(tx, runID, taskID)
	if err != nil {
		return "", "", err
	}
	if replay {
		rule := normalizedTriggerRule(descriptor.DAG.TriggerRule)
		return rule, rule, nil
	}
	var task models.Task
	if err := tx.Select("trigger_rule").First(&task, "id = ?", taskID).Error; err != nil {
		return "", "", err
	}
	return task.TriggerRule, normalizedTriggerRule(task.TriggerRule), nil
}

func (s *Store) shouldRunTaskTx(tx *gorm.DB, runID, taskID uuid.UUID) (bool, string, error) {
	rawRule, rule, err := s.resolveTriggerRuleTx(tx, runID, taskID)
	if err != nil {
		return false, "", err
	}
	predStatuses, err := s.predecessorStatusesTx(tx, runID, taskID)
	if err != nil {
		return false, "", err
	}
	return satisfiesTriggerRule(rawRule, predStatuses), rule, nil
}

func (s *Store) completeTask(runID, taskRef, instanceRef uuid.UUID, result, claimedBy string, enforceClaim bool, output map[string]string, branchSelections []string, partitions []pkgtask.Partition) ([]uuid.UUID, *FanOutExpansion, error) {
	var pendingEvents []event.Event
	var skippedTaskIDs []uuid.UUID
	var expansion *FanOutExpansion
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8)
		attemptSkippedTaskIDs := make([]uuid.UUID, 0)
		var attemptExpansion *FanOutExpansion

		err := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			status := taskStatusFromResult(result)

			// taskRef / instanceRef are the caller's IMMUTABLE references;
			// catalogTaskID / instanceID are THIS attempt's resolution of them.
			// The closure re-runs on transient contention, so resolving into the
			// caller's parameters made attempt 2 see inputs attempt 1 invented —
			// for a fanned step, the catalog task id, which names N rows and
			// fails with ErrAmbiguousTaskRun.
			catalogTaskID := taskRef
			instanceID := instanceRef
			var taskRunPtr *models.TaskRun
			var loadErr error
			if instanceID != uuid.Nil {
				var row models.TaskRun
				loadErr = tx.Where("id = ? AND job_run_id = ?", instanceID, runID).First(&row).Error
				if loadErr == nil {
					taskRunPtr = &row
					catalogTaskID = row.TaskID
				}
			} else {
				taskRunPtr, loadErr = loadTaskRunByIDOrUnique(tx, runID, taskRef)
				if loadErr == nil && taskRunPtr != nil {
					catalogTaskID = taskRunPtr.TaskID
				}
			}
			if loadErr != nil {
				if enforceClaim && (errors.Is(loadErr, gorm.ErrRecordNotFound) || errors.Is(loadErr, ErrAmbiguousTaskRun)) {
					return ErrTaskClaimMismatch
				}
				if errors.Is(loadErr, gorm.ErrRecordNotFound) {
					taskRunPtr = nil
				} else {
					return loadErr
				}
			}
			var taskRun models.TaskRun
			if taskRunPtr != nil {
				taskRun = *taskRunPtr
				if enforceClaim && taskRun.ClaimedBy != claimedBy {
					return ErrTaskClaimMismatch
				}
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
						metrics.TaskRunsTotal.WithLabelValues(jobID, catalogTaskID.String(), engine, string(status)).Inc()
						if taskRun.StartedAt != nil {
							duration := now.Sub(*taskRun.StartedAt).Seconds()
							metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(status)).Observe(duration)
						}
					}
				}
			}

			updateQuery := tx.Model(&models.TaskRun{})
			if taskRunPtr != nil {
				updateQuery = updateQuery.Where("id = ?", taskRun.ID)
			} else {
				updateQuery = updateQuery.Where("job_run_id = ? AND task_id = ?", runID, catalogTaskID)
			}
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}

			updates := map[string]interface{}{
				"status":                  string(status),
				"completed_at":            now,
				"result":                  result,
				"cache_hit":               false,
				"cache_origin_run_id":     nil,
				"cache_created_at":        nil,
				"cache_expires_at":        nil,
				"partition_retry_pending": false,
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
				// A non-zero container exit arrives HERE, not on FailTaskClaimed:
				// the worker reports "the container ran and told us its result"
				// through sink.Succeeded with result "failure". So this route owns
				// the full set of failure consequences — the group's
				// failurePolicy included — and shares one implementation with
				// failTask rather than carrying a smaller copy that silently
				// degraded fail_fast to `continue` in distributed mode.
				var failedRow *models.TaskRun
				if taskRunPtr != nil {
					// resolveInstanceFailureTx reads the row's terminal state; the
					// local copy predates the UPDATE above.
					taskRun.Status = string(status)
					failedRow = &taskRun
				}
				return s.resolveInstanceFailureTx(tx, runID, catalogTaskID, failedRow, &attemptEvents, &attemptSkippedTaskIDs, &counts)
			}

			descriptor, replayTask, err := s.replayTaskExecutionDescriptorTx(tx, runID, catalogTaskID)
			if err != nil {
				return err
			}

			// Load the task model once — needed for both edge fallback and branch
			// type detection.
			var taskModel models.Task
			taskType := ""
			if replayTask {
				taskModel = models.Task{ID: catalogTaskID}
				taskType = firstNonEmpty(descriptor.Runtime.TaskType, descriptor.DAG.BranchBehavior, "task")
			} else {
				if err := tx.First(&taskModel, "id = ?", catalogTaskID).Error; err != nil {
					return err
				}
				taskType = taskModel.Type
			}

			edges, err := s.successorEdgesForRunTx(tx, runID, catalogTaskID, taskModel)
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
			allSuccessorIDs := make([]uuid.UUID, 0, len(edges))
			for _, edge := range edges {
				allSuccessorIDs = append(allSuccessorIDs, edge.ToTaskID)
				if branchSelectedIDs != nil && !branchSelectedIDs[edge.ToTaskID] {
					reason := fmt.Sprintf("not selected by branch task %s", catalogTaskID)
					skipped, err := s.skipTaskAndDescendantsTx(tx, runID, edge.ToTaskID, reason, &attemptEvents, &counts)
					if err != nil {
						return err
					}
					attemptSkippedTaskIDs = append(attemptSkippedTaskIDs, skipped...)
					continue
				}
				toDecrementIDs = append(toDecrementIDs, edge.ToTaskID)
			}

			if taskRunPtr != nil {
				if isFanOutInstance(&taskRun) {
					if err := s.decrementInGroupDependentsTx(tx, runID, &taskRun); err != nil {
						return err
					}
					allTerm, gErr := s.groupAllTerminalTx(tx, runID, catalogTaskID)
					if gErr != nil {
						return gErr
					}
					if !allTerm {
						return nil
					}
				}
				exp, expErr := s.expandFanOutSuccessorsTx(tx, runID, catalogTaskID, &taskRun, taskModel.Name, allSuccessorIDs, partitions, &attemptEvents, &counts)
				if expErr != nil {
					return expErr
				}
				attemptExpansion = exp
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

			// A fanned step announces ONE task_succeeded for the group, on the
			// transition that makes every instance terminal (an individual
			// sibling's success emits nothing — see the !allTerm early return
			// above). Under failurePolicy: continue that transition can be a
			// SUCCESS landing in a group that already contains failures, and the
			// event was emitted anyway: the incident subscriber reads
			// task_succeeded as "this job/task later ran green" and remediated
			// the incident the failed sibling had just opened, closing a live
			// failure with no human involved. A group containing a failed
			// partition has not succeeded.
			emitSucceeded := true
			if taskRunPtr != nil && isFanOutInstance(&taskRun) {
				allSucceeded, gErr := s.groupAllSucceededTx(tx, runID, catalogTaskID)
				if gErr != nil {
					return gErr
				}
				emitSucceeded = allSucceeded
			}

			if s.eventStore != nil && emitSucceeded {
				// Build task_succeeded event and add to batch. Read the row by its
				// primary key so each instance's event carries its own partition
				// rather than an arbitrary sibling's.
				var taskRunModel models.TaskRun
				succeededQuery := tx.Where("job_run_id = ? AND task_id = ?", runID, catalogTaskID)
				if taskRunPtr != nil {
					succeededQuery = tx.Where("id = ?", taskRun.ID)
				}
				if err := succeededQuery.First(&taskRunModel).Error; err != nil {
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
					TaskID:    catalogTaskID,
					Timestamp: time.Now().UTC(),
					Payload:   payload,
				}
				// task_succeeded goes first so sequence ordering is consistent.
				batchEvts = append([]*event.Event{succeededEvt}, batchEvts...)
			}

			// Ready events for newly-released successors are written whether or
			// not the group announced success: suppressing the group's
			// task_succeeded must never suppress the DAG's forward motion.
			if s.eventStore != nil && len(batchEvts) > 0 {
				if err := s.appendBatchEventsTx(tx, batchEvts, &attemptEvents, &counts); err != nil {
					return err
				}
			}

			return nil
		})
		if err == nil {
			pendingEvents = attemptEvents
			skippedTaskIDs = attemptSkippedTaskIDs
			expansion = attemptExpansion
		}
		return err
	})
	if err == nil {
		counts.commit()
		s.publishEvents(pendingEvents...)
	}
	return skippedTaskIDs, expansion, err
}

// CompleteTaskOwner is the run-owner in-memory path's durable terminal write.
// The owner has already advanced the DAG in memory (run.RunState), so this only
// persists terminal rows — it does NOT decrement predecessors, evaluate trigger
// rules, or resolve branches in SQL.  It writes the completed task's terminal
// row (succeeded/failed/cached) plus each owner-decided skip, stamping
// terminal_sequence and owner_generation so a recovering owner can replay in
// order.  Claim-fenced by claimedBy.
//
// Cache hits DO travel this path: a cache hit is a completion, and under
// per-partition fingerprints a cache-hit prerequisite is the common case in an
// ordered group, so the owner's Cached sink carries its TaskRunID through here
// like any other terminal transition. (An earlier docstring claimed cache hits
// stayed on CacheHitTaskClaimed; that stopped being true when the owner sink
// gained instance identity.)
func (s *Store) CompleteTaskOwner(
	runID, taskRef uuid.UUID,
	status TaskStatus,
	result, errMsg, claimedBy string,
	output map[string]string,
	branchSelections []string,
	completedSeq, ownerGen int64,
	skips []SkippedTask,
	expansion *FanOutExpansion,
) error {
	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 8+len(skips))

		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()

			// Metrics for the completed task (mirrors completeTask).
			//
			// taskRef stays the caller's IMMUTABLE reference and catalogTaskID is
			// this attempt's resolution of it: the closure re-runs on transient
			// contention, and writing the catalog id back into taskRef made the
			// retry resolve a fanned group by task id — N rows, ErrAmbiguousTaskRun,
			// returned to the owner as a claim mismatch, which it reads as "someone
			// else owns this task" and stops driving.
			row, loadErr := loadTaskRunByIDOrUnique(tx, runID, taskRef)
			if loadErr != nil {
				if errors.Is(loadErr, gorm.ErrRecordNotFound) || errors.Is(loadErr, ErrAmbiguousTaskRun) {
					return ErrTaskClaimMismatch
				}
				return loadErr
			}
			taskRun := *row
			catalogTaskID := taskRun.TaskID
			tq := tx.Where("id = ? AND claimed_by = ?", taskRun.ID, claimedBy)
			if err := tq.First(&taskRun).Error; err == nil {
				var jobRun models.JobRun
				if err := tx.First(&jobRun, "id = ?", runID).Error; err == nil {
					if !taskRun.Quarantine && !jobRun.Quarantine {
						jobID := jobRun.JobID.String()
						engine := string(taskRun.Engine)
						metrics.TaskRunsTotal.WithLabelValues(jobID, catalogTaskID.String(), engine, string(status)).Inc()
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
				"status":                  string(status),
				"completed_at":            now,
				"result":                  result,
				"terminal_sequence":       completedSeq,
				"owner_generation":        ownerGen,
				"cache_hit":               status == TaskStatusCached,
				"partition_retry_pending": false,
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
				Where("id = ? AND claimed_by = ?", taskRun.ID, claimedBy).
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
				evt, err := s.recordTaskEventTx(tx, evtType, runID, taskRun.ID, &counts)
				if err != nil {
					return err
				}
				attemptEvents = append(attemptEvents, *evt)
			}

			// Persist owner-decided skips (branch + trigger-rule), each stamped
			// with its own terminal_sequence.  RunState already enumerated the
			// full transitive skip set, so this writes them directly without
			// re-walking descendants.
			if err := s.persistExpansionTx(tx, runID, expansion, &attemptEvents, &counts); err != nil {
				return err
			}

			// RunState keys a skip by its INSTANCE identity (the TaskRun ID for a
			// fan-out instance, the catalog task ID for an unfanned step), so
			// resolve it to concrete rows and write each row by primary key. The
			// previous code ran the same UPDATE twice — WHERE id, then, on
			// RowsAffected == 0, WHERE job_run_id + task_id — and that second form
			// is precisely the fan-across-siblings write G1 exists to remove: it
			// would have marked all N instances of a group skipped under ONE
			// terminal_sequence, making N-1 of them invisible to
			// TerminalTaskRunsSince replay. When a skip legitimately names a whole
			// group, each instance now gets its own allocated sequence.
			for _, sk := range skips {
				skipRows, resolveErr := resolveSkipTargetsTx(tx, runID, sk.TaskID)
				if resolveErr != nil {
					return resolveErr
				}
				for i := range skipRows {
					seq := sk.TerminalSequence
					if i > 0 {
						// Siblings must never share a terminal_sequence: the replay
						// tail is a strictly-ordered, dense space.
						allocated, seqErr := nextTerminalSequenceTx(tx, runID)
						if seqErr != nil {
							return seqErr
						}
						seq = allocated
					}
					skipUpdates := map[string]interface{}{
						"status":                  string(TaskStatusSkipped),
						"completed_at":            now,
						"error":                   sk.Reason,
						"terminal_sequence":       seq,
						"owner_generation":        ownerGen,
						"partition_retry_pending": false,
					}
					skipQuery := tx.Model(&models.TaskRun{}).Where("id = ?", skipRows[i].ID)
					if skipRows[i].Status == string(TaskStatusRunning) {
						// The owner decided to cancel a sibling it had already
						// DISPATCHED — accepted by a peer, claimed, queued in a
						// worker pool with no container yet (fail_fast, see
						// RunState.cancellableBeforeStart). Two conditions apply
						// that do not for an ordinary pending skip:
						//
						// The pre-start test goes INSIDE the UPDATE, so a worker
						// that created the container between the owner's decision
						// and this write makes the skip match zero rows and the
						// live task is left alone (RowsAffected == 0 below already
						// means "already resolved; emit nothing"). And the claim is
						// revoked, which is what stops the queued start: the
						// worker's StartTaskClaimed requires its own claim on a
						// running row, so it now fails with ErrTaskClaimMismatch
						// and the executor abandons the task without posting a
						// completion.
						predSQL, predArgs := cancellableBeforeStartPredicate()
						skipQuery = skipQuery.Where(predSQL, predArgs...)
						skipUpdates["claimed_by"] = ""
						skipUpdates["claim_expires_at"] = nil
						skipUpdates["started_at"] = nil
					} else {
						skipQuery = skipQuery.Where("status NOT IN ?", terminalStatusStrings())
					}
					skRes := skipQuery.Updates(skipUpdates)
					if skRes.Error != nil {
						return skRes.Error
					}
					if skRes.RowsAffected == 0 {
						continue // already terminal; nothing to emit
					}
					counts.addTaskRunStatus(1)
					if s.eventStore != nil {
						evt, err := s.recordTaskRunEventTx(tx, event.TypeTaskSkipped, runID, &skipRows[i], &counts)
						if err != nil {
							return err
						}
						attemptEvents = append(attemptEvents, *evt)
					}
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

// resolveSkipTargetsTx turns an owner skip identity into the concrete TaskRun
// rows it names. The identity is a TaskRun primary key for a fan-out instance
// and a catalog task ID for an unfanned step; a catalog task ID that names an
// expanded group resolves to every one of its instances so the caller can stamp
// each with its own terminal_sequence instead of broadcasting one UPDATE.
func resolveSkipTargetsTx(tx *gorm.DB, runID, skipID uuid.UUID) ([]models.TaskRun, error) {
	var byPK models.TaskRun
	err := tx.Where("id = ? AND job_run_id = ?", skipID, runID).First(&byPK).Error
	if err == nil {
		return []models.TaskRun{byPK}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var rows []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ?", runID, skipID).
		Order("partition_index ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
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
		if counts != nil {
			counts.addTaskRunStatus(len(successorIDs))
		}

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

// failTask marks ONE task instance failed and runs the fan-out consequences
// (in-group skip cascade, group resolution, cross-step advancement).
//
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract and is
// NEVER reassigned. It used to be: the metrics pre-read did `taskID =
// loaded.TaskID`, overwriting the caller's argument with the CATALOG task id, so
// the in-transaction re-resolve then looked up an id that names N rows and
// returned ErrAmbiguousTaskRun — `FailTask(runID, instancePK, err)` could not
// fail a fan-out instance at all. Row identity is carried in locals from here on
// and `catalogTaskID` is used only where a catalog id is genuinely wanted
// (metrics labels, group-level queries).
//
// A terminal row is left alone: re-failing would overwrite the first, truer
// cause (a cascade skip, a cancellation, a racing sibling). Same guard
// completeTask carries.
func (s *Store) failTask(runID, taskRef uuid.UUID, failure error, claimedBy string, enforceClaim bool) error {
	now := time.Now().UTC()
	errMsg := ""
	if failure != nil {
		errMsg = failure.Error()
	}

	// Record task failure metrics. This read must not disturb taskRef.
	var metricRow models.TaskRun
	if loaded, loadErr := loadTaskRunByIDOrUnique(s.db, runID, taskRef); loadErr == nil && loaded != nil {
		metricRow = *loaded
	}
	metricQuery := s.db.Where("id = ?", metricRow.ID)
	if metricRow.ID == uuid.Nil {
		metricQuery = s.db.Where("job_run_id = ? AND task_id = ?", runID, taskRef)
	}
	if enforceClaim {
		metricQuery = metricQuery.Where("claimed_by = ?", claimedBy)
	}
	if err := metricQuery.First(&metricRow).Error; err == nil {
		var jobRun models.JobRun
		if err := s.db.First(&jobRun, "id = ?", runID).Error; err == nil {
			if !metricRow.Quarantine && !jobRun.Quarantine {
				jobID := jobRun.JobID.String()
				engine := string(metricRow.Engine)
				metrics.TaskRunsTotal.WithLabelValues(jobID, metricRow.TaskID.String(), engine, string(TaskStatusFailed)).Inc()
				if metricRow.StartedAt != nil {
					duration := now.Sub(*metricRow.StartedAt).Seconds()
					metrics.TaskRunDurationSeconds.WithLabelValues(jobID, engine, string(TaskStatusFailed)).Observe(duration)
				}
			}
		}
	} else if enforceClaim && errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrTaskClaimMismatch
	}

	var pendingEvents []event.Event
	var counts dbWriteCounts
	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 1)
		txErr := s.db.Transaction(func(tx *gorm.DB) error {
			row, loadErr := loadTaskRunByIDOrUnique(tx, runID, taskRef)
			if loadErr != nil {
				if enforceClaim {
					return ErrTaskClaimMismatch
				}
				return loadErr
			}
			if IsTerminal(TaskStatus(row.Status)) {
				return nil
			}
			catalogTaskID := row.TaskID

			updateQuery := tx.Model(&models.TaskRun{}).
				Where("id = ? AND status NOT IN ?", row.ID, terminalTaskStatuses())
			if enforceClaim {
				updateQuery = updateQuery.Where("claimed_by = ?", claimedBy)
			}
			resultUpdate := updateQuery.
				Updates(map[string]interface{}{
					"status":                  string(TaskStatusFailed),
					"completed_at":            now,
					"error":                   errMsg,
					"cache_hit":               false,
					"cache_origin_run_id":     nil,
					"cache_created_at":        nil,
					"cache_expires_at":        nil,
					"partition_retry_pending": false,
				})
			if resultUpdate.Error != nil {
				return resultUpdate.Error
			}
			if resultUpdate.RowsAffected == 0 {
				if enforceClaim {
					return ErrTaskClaimMismatch
				}
				return nil
			}
			counts.addTaskRunStatus(1)
			row.Status = string(TaskStatusFailed)

			// Apply the group's failurePolicy, emit this instance's task_failed
			// event, and release the fanned step's cross-step successors once the
			// whole group is terminal. Shared with completeTask's failure branch —
			// the route a non-zero container exit actually takes — so the two can
			// never disagree about what a failed instance means.
			var skipped []uuid.UUID
			return s.resolveInstanceFailureTx(tx, runID, catalogTaskID, row, &attemptEvents, &skipped, &counts)
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

// RetryTask resets a failed task run back to pending and increments its Attempt
// counter. It is the LOCAL lane's in-run retry: internal/job calls it for each
// `retries:` attempt while it drives the DAG itself.
//
// There is deliberately no claim-fenced twin. RetryTaskClaimed used to be one
// and was unusable: it re-pends the row, while StartTaskClaimed only starts a
// row whose status is `running`, so the attempt that followed a claimed retry
// could never start. The distributed lane uses RetryTaskClaimedInstance
// (internal/run/store_instance.go), which keeps the row `running` under the same
// claim — the truthful state for a worker that never released the task and is
// about to launch the next container itself.
func (s *Store) RetryTask(runID, taskRef uuid.UUID, attempt int) error {
	return s.retryTask(runID, taskRef, attempt)
}

// retryTask resets ONE task instance for its next attempt. taskRef follows the
// TaskRun-primary-key-or-catalog-task-ID contract: a fan-out instance must be
// addressed by its own TaskRun ID, because this write resets output/result and
// under the old (job_run_id, task_id) resolution a retry of one partition
// discarded every sibling's results.
func (s *Store) retryTask(runID, taskRef uuid.UUID, attempt int) error {
	pendingEvents := make([]event.Event, 0, 2)
	var counts dbWriteCounts
	err := s.db.Transaction(func(tx *gorm.DB) error {
		row, loadErr := loadTaskRunByIDOrUnique(tx, runID, taskRef)
		if loadErr != nil {
			return loadErr
		}
		taskID := row.TaskID

		// ONE reset contract, shared with RetryFromFailure, RetryPartition and
		// RetryTaskInstance. This list used to be a hand-maintained copy that
		// predated schema_violations and exit_code, so a locally-retried task
		// re-executed still carrying the previous attempt's violations (what
		// `caesium why` reports) and its exit code (what the incident classifier
		// reads). The two overrides are the genuine differences of an in-run
		// retry: this is attempt N+1 rather than a fresh start at 1, and the
		// claim columns are left to the caller that owns them.
		updates := retryResetColumns()
		updates["attempt"] = attempt
		delete(updates, "claimed_by")
		delete(updates, "claim_expires_at")

		resultUpdate := tx.Model(&models.TaskRun{}).
			Where("id = ?", row.ID).
			Updates(updates)
		if resultUpdate.Error != nil {
			return resultUpdate.Error
		}
		counts.addTaskRunStatus(1)

		if s.eventStore != nil {
			// Build retrying event payload.
			taskRunModel := *row
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

// SkipTask skips a STEP: every caller uses it to skip a successor whose trigger
// rule was not satisfied, so under fan-out it means the whole group. It routes
// through markInstanceSkippedTx per instance, giving each its own
// terminal_sequence and its own task_skipped event rather than one UPDATE
// across the group.
func (s *Store) SkipTask(runID, taskID uuid.UUID, reason string) error {
	pendingEvents := make([]event.Event, 0, 1)
	var counts dbWriteCounts
	err := s.db.Transaction(func(tx *gorm.DB) error {
		_, err := s.markTaskSkippedTx(tx, runID, taskID, reason, &pendingEvents, &counts)
		return err
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
	_, err := s.CompleteIfActive(runID, result)
	return err
}

// CompleteIfActive is Complete that also reports whether THIS call finalized
// the run: false when the run was already terminal (the idempotent no-op).
// Callers that dispatch completion callbacks on their own — an engine
// finalizing a resume that failed before its normal completion path — need
// the distinction so a run another path finalized first is not notified
// twice.
func (s *Store) CompleteIfActive(runID uuid.UUID, result error) (bool, error) {
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

			// Observe retry-reset TaskRuns after the JobRun write so this
			// transaction holds the SQLite write lock before looking at
			// instance rows. Checking first (a deferred read) would let
			// RetryPartition commit a pending row between COUNT and UPDATE.
			// Returning the sentinel rolls the status write back; (*job).Run
			// then starts a replacement engine.
			//
			// Match explicit partition-retry provenance, not an inferred row
			// shape. A terminal-sibling heuristic misses N=1 fan-out groups;
			// partition_count alone misclassifies ordinary never-started
			// instances. RetryPartition is the sole writer of the marker, and
			// terminal task transitions clear it. The predicate deliberately
			// does not require outstanding_predecessors = 0: a retry that still
			// waits on a dependency the engine has not resolved is stranded by
			// a terminal run just the same, and RetryPartition already refuses
			// the retries no engine could ever release.
			var pending int64
			if err := tx.Model(&models.TaskRun{}).
				Where("job_run_id = ? AND status = ? AND started_at IS NULL AND partition_retry_pending = ?",
					runID, string(TaskStatusPending), true).
				Count(&pending).Error; err != nil {
				return err
			}
			if pending > 0 {
				return ErrRunHasPendingWork
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
		return false, nil
	}
	if err != nil {
		return false, err
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
	return true, nil
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
			"status":                  string(TaskStatusCancelled),
			"completed_at":            now,
			"error":                   reason,
			"claimed_by":              "",
			"claim_expires_at":        nil,
			"runtime_id":              "",
			"rate_limit_retry_after":  nil,
			"partition_retry_pending": false,
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

func (s *Store) CancelQueuedRun(ctx context.Context, jobID, queueID uuid.UUID) error {
	result := s.db.WithContext(ctx).
		Where("id = ? AND job_id = ? AND claimed_by = ''", queueID, jobID).
		Delete(&models.RunQueue{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// The conditional delete matched no unclaimed row: distinguish a missing
		// queued run (404) from one the dequeuer has already claimed (409).
		var count int64
		if err := s.db.WithContext(ctx).Model(&models.RunQueue{}).
			Where("id = ? AND job_id = ?", queueID, jobID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return ErrQueuedRunNotFound
		}
		return ErrQueuedRunUnavailable
	}
	if err := s.observeRunQueueDepth(jobID); err != nil {
		log.Warn("run queue: failed to observe depth after cancel", "job_id", jobID, "queue_id", queueID, "error", err)
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
	runValue.Tasks = collapseFanOutGroups(runValue.Tasks)
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
		ID:                      model.ID,
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
		PartitionValue:          model.PartitionValue,
		PartitionIndex:          model.PartitionIndex,
		PartitionCount:          model.PartitionCount,
		PartitionFingerprint:    model.PartitionFingerprint,
		CreatedAt:               model.CreatedAt,
		UpdatedAt:               model.UpdatedAt,
		CacheHit:                model.CacheHit || TaskStatus(model.Status) == TaskStatusCached,
		Quarantine:              model.Quarantine,
		ReplaySafe:              model.ReplaySafe,
		CacheEnabled:            model.CacheEnabled,
		CacheTTL:                model.CacheTTL,
		CacheVersion:            model.CacheVersion,
		CachePinDigests:         model.CachePinDigests,
		CacheDigestTTL:          model.CacheDigestTTL,
		CacheChain:              model.CacheChain,
		CacheTTLNever:           model.CacheTTLNever,
		OutputSchema:            append([]byte(nil), model.OutputSchema...),
		SchemaValidation:        model.SchemaValidation,
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
	if len(model.PartitionDependsOn) > 0 {
		var deps []string
		if err := json.Unmarshal(model.PartitionDependsOn, &deps); err == nil {
			task.PartitionDependsOn = deps
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

func collapseFanOutGroups(rows []*TaskRun) []*TaskRun {
	if len(rows) == 0 {
		return rows
	}
	order := make([]uuid.UUID, 0)
	grouped := make(map[uuid.UUID][]*TaskRun)
	for _, row := range rows {
		if row == nil {
			continue
		}
		if _, ok := grouped[row.TaskID]; !ok {
			order = append(order, row.TaskID)
		}
		grouped[row.TaskID] = append(grouped[row.TaskID], row)
	}
	out := make([]*TaskRun, 0, len(order))
	for _, taskID := range order {
		insts := grouped[taskID]
		head := *insts[0]
		n := len(insts)
		if n > 1 || head.PartitionValue != "" {
			head.PartitionCount = n
		} else {
			head.PartitionCount = 0
		}
		head.ID = taskID
		if n > 1 {
			modelsRows := make([]models.TaskRun, 0, n)
			var firstStart *time.Time
			var lastEnd *time.Time
			for _, inst := range insts {
				modelsRows = append(modelsRows, models.TaskRun{TaskID: inst.TaskID, Status: string(inst.Status)})
				if inst.StartedAt != nil && (firstStart == nil || inst.StartedAt.Before(*firstStart)) {
					t := *inst.StartedAt
					firstStart = &t
				}
				if inst.CompletedAt != nil && (lastEnd == nil || inst.CompletedAt.After(*lastEnd)) {
					t := *inst.CompletedAt
					lastEnd = &t
				}
			}
			head.Status = groupStatusFromInstances(modelsRows)
			head.StartedAt = firstStart
			head.CompletedAt = lastEnd
			// The rolled-up status alone loses the mix; carry the histogram so a
			// collapsed group renders "2 succeeded / 1 failed" without the client
			// having to page the partitions endpoint for every group.
			head.PartitionStatusCounts = PartitionStatusCounts(insts)
		}
		out = append(out, &head)
	}
	return out
}

// PartitionStatusCounts builds the per-status histogram of a fan-out group.
// Keys are TaskStatus values; a status with no instances is absent rather than
// zero, so the map stays small for a 10k-instance group. Returns nil for an
// empty group so the field is omitted from JSON.
func PartitionStatusCounts(instances []*TaskRun) map[string]int {
	if len(instances) == 0 {
		return nil
	}
	counts := make(map[string]int, 4)
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		counts[string(inst.Status)]++
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
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

// recordTaskEventTx records a task lifecycle event for ONE task instance.
// taskRef follows the TaskRun-primary-key-or-catalog-task-ID contract: a caller
// holding a fan-out instance's TaskRun ID gets that instance's event, while an
// unfanned catalog task ID still resolves to its single row. It used to
// `.First()` the (job_run_id, task_id) predicate, so every sibling's event was
// built from an arbitrary instance's row — N task_failed events all reporting
// partition 0's error. Prefer recordTaskRunEventTx when the row is already in
// hand.
func (s *Store) recordTaskEventTx(db *gorm.DB, eventType event.Type, runID, taskRef uuid.UUID, counts *dbWriteCounts) (*event.Event, error) {
	row, err := loadTaskRunByIDOrUnique(db, runID, taskRef)
	if err != nil {
		log.Error("failed to fetch task run for event", "error", err, "run_id", runID, "task_ref", taskRef)
		return nil, err
	}
	return s.recordTaskRunEventTx(db, eventType, runID, row, counts)
}

// recordTaskRunEventTx builds and appends a task lifecycle event from a specific
// TaskRun row. The payload's `id` is the TaskRun primary key and it carries the
// instance's partition_value / partition_index / partition_count /
// partition_fingerprint (see convertRunTaskModel), which is what lets a consumer
// tell sibling events apart. evt.TaskID stays the CATALOG task ID so existing
// per-step correlation is unchanged.
func (s *Store) recordTaskRunEventTx(db *gorm.DB, eventType event.Type, runID uuid.UUID, taskRunRow *models.TaskRun, counts *dbWriteCounts) (*event.Event, error) {
	if taskRunRow == nil {
		return nil, gorm.ErrRecordNotFound
	}
	// Re-read by primary key so the payload reflects the committed row state
	// (callers hand us a pre-update copy on some paths).
	var taskRun models.TaskRun
	if err := db.Where("id = ?", taskRunRow.ID).First(&taskRun).Error; err != nil {
		log.Error("failed to fetch task run for event", "error", err, "run_id", runID, "task_run_id", taskRunRow.ID)
		return nil, err
	}
	taskID := taskRun.TaskID

	var jobRun models.JobRun
	if err := db.Preload("Job").First(&jobRun, "id = ?", runID).Error; err != nil {
		log.Error("failed to fetch job run for event", "error", err, "run_id", runID)
		return nil, err
	}

	taskPayload := convertRunTaskModel(&taskRun)
	taskPayload.JobAlias = jobRun.Job.Alias
	taskPayload.JobLabels = jsonmap.ToStringMap(jobRun.Job.Labels)
	// Use task-run row ID for event payloads so downstream consumers can identify
	// each task execution uniquely across retries/runs — and, under fan-out, tell
	// one instance of a group from its siblings.
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
		if counts != nil {
			counts.addEventInsert(1)
		}
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
	return withStoreBusyRetryContext(context.Background(), fn)
}

func withStoreBusyRetryContext(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	for attempt := 0; ; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = fn()
		if err == nil || !isStoreContentionErr(err) {
			return err
		}
		if attempt >= len(storeBusyRetryBackoffs) {
			return err
		}

		metrics.DBBusyRetriesTotal.Inc()
		if sleepErr := sleepStoreBusyRetry(ctx, jitterStoreBusyRetryBackoff(storeBusyRetryBackoffs[attempt])); sleepErr != nil {
			return sleepErr
		}
	}
}

func sleepStoreBusyRetry(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
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
// PredecessorOutputs returns each predecessor step's outputs keyed by step name.
// One entry per PREDECESSOR (never per row): a fanned predecessor contributes
// the group aggregate, see predecessorGroupOutput.
func (s *Store) PredecessorOutputs(runID, taskID uuid.UUID) (map[string]map[string]string, error) {
	refs, err := s.resolvePredecessorsTx(s.db, runID, taskID)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}

	taskRuns, err := predecessorTaskRunsTx(s.db, runID, refs)
	if err != nil {
		log.Warn("failed to find predecessor task runs for output", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	byTask := groupTaskRunsByTaskID(taskRuns)

	result := make(map[string]map[string]string, len(refs))
	for _, ref := range refs {
		// A predecessor whose catalog row could not be resolved has no step name
		// to key by, and downstream env injection is name-based — so it is
		// dropped, exactly as before G7.
		if !ref.HasCatalogRow {
			continue
		}
		output, ok, err := predecessorGroupOutput(ref.Name, byTask[ref.TaskID])
		if err != nil {
			return nil, err
		}
		if !ok || len(output) == 0 {
			continue
		}
		result[ref.Name] = output
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func groupTaskRunsByTaskID(rows []models.TaskRun) map[uuid.UUID][]models.TaskRun {
	byTask := make(map[uuid.UUID][]models.TaskRun, len(rows))
	for i := range rows {
		byTask[rows[i].TaskID] = append(byTask[rows[i].TaskID], rows[i])
	}
	return byTask
}

// predecessorGroupOutput resolves the ONE output map a predecessor step presents
// downstream.
//
// Unfanned (the only shape before fan-out): the single row's decoded output,
// byte-identical to the previous `taskRunsByTaskID[taskID]` lookup.
//
// Fanned: the group's AGGREGATE, produced by the same pkgtask.AggregateFanInOutputs
// the local executor calls (internal/job/job.go, at the end of the fan-out group
// runner) — each scalar key becomes a JSON object keyed by partition value, plus
// PARTITION_COUNT / SUCCEEDED / FAILED. The map used to be keyed by task ID with
// one row per entry, so N siblings collapsed last-writer-wins and the distributed
// lane silently handed a downstream step ONE arbitrary partition's outputs while
// the local lane handed it the aggregate. The two lanes must not disagree about
// what a fan-in consumer sees.
//
// Counting matches the local lane exactly: terminal successes (succeeded and
// cached) count as succeeded and contribute their output; failed instances count
// as failed; instances that never ran (skipped by the in-group cascade) count as
// neither.
// producer names the predecessor STEP and is used only to attribute an
// over-cap aggregate; the error is propagated rather than swallowed, because a
// silently truncated fan-in contract is the failure mode
// FanInAggregateTooLargeError exists to prevent.
func predecessorGroupOutput(producer string, rows []models.TaskRun) (map[string]string, bool, error) {
	switch {
	case len(rows) == 0:
		return nil, false, nil
	case len(rows) == 1 && !isFanOutInstance(&rows[0]):
		if len(rows[0].Output) == 0 {
			return nil, false, nil
		}
		var output map[string]string
		if err := json.Unmarshal(rows[0].Output, &output); err != nil {
			log.Warn("failed to unmarshal predecessor task output", "predecessor_task_id", rows[0].TaskID, "error", err)
			return nil, false, nil
		}
		return output, true, nil
	}

	byPartition := make(map[string]map[string]string, len(rows))
	succeeded, failed := 0, 0
	for i := range rows {
		row := &rows[i]
		status := TaskStatus(row.Status)
		switch {
		case IsTerminalSuccess(status):
			succeeded++
			if len(row.Output) == 0 {
				continue
			}
			var output map[string]string
			if err := json.Unmarshal(row.Output, &output); err != nil {
				log.Warn("failed to unmarshal fan-out instance output", "predecessor_task_id", row.TaskID, "partition", row.PartitionValue, "error", err)
				continue
			}
			if len(output) > 0 {
				byPartition[row.PartitionValue] = output
			}
		case status == TaskStatusFailed:
			failed++
		}
	}
	aggregate, err := pkgtask.AggregateFanInOutputs(producer, byPartition, succeeded, failed)
	if err != nil {
		return nil, false, err
	}
	return aggregate, true, nil
}

// PredecessorDescriptorInputs returns predecessor outputs and effective hashes
// keyed by predecessor task id for immutable execution-descriptor capture.
//
// Group-aware for the same reason PredecessorOutputs and PredecessorHashes are:
// the descriptor records the inputs a task actually consumed, so it must record
// the same aggregate the cache key was computed from. Unfanned predecessors are
// byte-identical to the pre-fan-out behavior (one row, its own output map, its
// own effective hash).
func (s *Store) PredecessorDescriptorInputs(runID, taskID uuid.UUID) (map[uuid.UUID]map[string]string, map[uuid.UUID]string, error) {
	refs, err := s.resolvePredecessorsTx(s.db, runID, taskID)
	if err != nil {
		return nil, nil, err
	}
	if len(refs) == 0 {
		return nil, nil, nil
	}
	taskRuns, err := predecessorTaskRunsTx(s.db, runID, refs,
		"task_id", "id", "output", "hash", "effective_hash", "status",
		"partition_value", "partition_index", "partition_count")
	if err != nil {
		return nil, nil, err
	}

	byTask := groupTaskRunsByTaskID(taskRuns)
	names := make(map[uuid.UUID]string, len(refs))
	for _, ref := range refs {
		names[ref.TaskID] = ref.Name
	}
	outputs := make(map[uuid.UUID]map[string]string, len(byTask))
	hashes := make(map[uuid.UUID]string, len(byTask))
	for predID, rows := range byTask {
		output, ok, err := predecessorGroupOutput(names[predID], rows)
		if err != nil {
			return nil, nil, err
		}
		if ok && len(output) > 0 {
			outputs[predID] = output
		}
		successes := make([]models.TaskRun, 0, len(rows))
		for i := range rows {
			if IsTerminalSuccess(TaskStatus(rows[i].Status)) {
				successes = append(successes, rows[i])
			}
		}
		if h := predecessorGroupHash(successes); h != "" {
			hashes[predID] = h
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
	refs, err := s.resolvePredecessorsTx(s.db, runID, taskID)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}

	var taskRuns []models.TaskRun
	if err := s.db.
		Select("task_id", "id", "hash", "effective_hash", "partition_value", "partition_index", "partition_count").
		Where("job_run_id = ? AND task_id IN ? AND status IN ? AND hash <> ''",
			runID,
			predecessorTaskIDs(refs),
			[]string{string(TaskStatusSucceeded), string(TaskStatusCached)},
		).
		Order("partition_index ASC").
		Find(&taskRuns).Error; err != nil {
		log.Warn("failed to find predecessor task runs for hashes", "run_id", runID, "task_id", taskID, "error", err)
		return nil, nil
	}
	return predecessorHashList(taskRuns), nil
}

// fanOutGroupHashPrefix namespaces a fan-out group's aggregate identity so it can
// never be confused with a plain task hash.
const fanOutGroupHashPrefix = "fanout-group:"

// predecessorHashList turns predecessor TaskRun rows into the hash list a
// downstream task folds into its cache identity — EXACTLY ONE entry per
// predecessor step.
//
// Unfanned predecessor: its own effective hash, byte-for-byte what this function
// returned before fan-out existed. This is load-bearing — any change here
// re-keys every cached task in every existing deployment — and is pinned by a
// golden test (TestPredecessorHashListUnfannedIsByteIdentical).
//
// Fanned predecessor: ONE deterministic group hash, defined as
//
//	sha256( "fanout-group:" || h(0) || "\n" || h(1) || "\n" || … )
//
// where h(i) is instance i's effective hash and instances are taken in
// PARTITION-INDEX order (emission order — stable across runs and independent of
// row insert order, scheduling order, or which instance finished last). Without
// this a fanned predecessor contributed N entries, which changes the SHAPE of the
// downstream identity key's `pred_hash:` lines: every downstream task would
// cache-miss forever, and adding or removing a single partition would re-key the
// whole subtree. One aggregate identity per predecessor is the same contract the
// design fixes for status and for outputs.
//
// Instances that are not terminal successes never reach here (the caller filters
// on status), so a partially-complete group contributes only the instances that
// have succeeded — matching the old per-row behavior for the rows that existed.
func predecessorHashList(rows []models.TaskRun) []string {
	if len(rows) == 0 {
		return nil
	}
	byTask := groupTaskRunsByTaskID(rows)
	hashes := make([]string, 0, len(byTask))
	for _, group := range byTask {
		if h := predecessorGroupHash(group); h != "" {
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return nil
	}
	sort.Strings(hashes)
	return hashes
}

func predecessorGroupHash(rows []models.TaskRun) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows) == 1 && !isFanOutInstance(&rows[0]) {
		return effectiveTaskHash(rows[0].Hash, rows[0].EffectiveHash)
	}
	ordered := make([]models.TaskRun, len(rows))
	copy(ordered, rows)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].PartitionIndex != ordered[j].PartitionIndex {
			return ordered[i].PartitionIndex < ordered[j].PartitionIndex
		}
		return ordered[i].ID.String() < ordered[j].ID.String()
	})
	hashes := make([]string, 0, len(ordered))
	for i := range ordered {
		hashes = append(hashes, effectiveTaskHash(ordered[i].Hash, ordered[i].EffectiveHash))
	}
	// One definition, shared with the local executor via the exported
	// GroupIdentityHash: the two lanes must not disagree about a group's
	// identity, or a downstream step cache-hits in one lane and misses in the
	// other.
	return GroupIdentityHash(hashes)
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

// readmitRetryTx flips a terminal run back to running as part of a retry. For a
// manual retry (admit=false) it is an unconditional UPDATE. For an agent retry
// (admit=true) on a non-quarantine job that declares a concurrency policy, it is
// a QUEUE-semantics conditional UPDATE: the flip takes effect only if the active
// running count for the job is under maxRuns, so an agent retry can never exceed
// the declared concurrency nor replace-cancel a live run. On a full job it
// returns ErrMaxConcurrentRunsReached and leaves the run terminal.
func (s *Store) readmitRetryTx(tx *gorm.DB, jobRun *models.JobRun, admit bool) error {
	unconditional := func() error {
		return tx.Model(jobRun).Updates(map[string]interface{}{
			"status":       string(StatusRunning),
			"completed_at": nil,
			"error":        "",
		}).Error
	}
	if !admit || jobRun.Quarantine {
		return unconditional()
	}
	cfg, hasPolicy, err := s.concurrencyConfigTx(tx, jobRun.JobID)
	if err != nil {
		return err
	}
	if !hasPolicy {
		return unconditional()
	}
	// Conditional flip: succeed only if a slot is free. The run being retried is
	// currently terminal so it is not in the active count; id <> ? excludes it
	// defensively. The quarantine <> true / backfill_id IS NULL predicates mirror
	// the admission count used by insertRunIfSlotTx so agent retries admit against
	// the same slot definition as new runs.
	res := tx.Exec(`
UPDATE job_runs
SET status = ?, completed_at = NULL, error = ''
WHERE id = ?
	AND status IN (?, ?)
	AND (
		SELECT count(*)
		FROM job_runs
		WHERE job_id = ?
			AND status = ?
			AND quarantine <> true
			AND backfill_id IS NULL
			AND id <> ?
	) < ?`,
		string(StatusRunning),
		jobRun.ID,
		string(StatusFailed), string(StatusSucceeded),
		jobRun.JobID,
		string(StatusRunning),
		jobRun.ID,
		cfg.maxRuns,
	)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrMaxConcurrentRunsReached
	}
	return nil
}

// RetryFromFailure resets a failed run so that previously-succeeded and cached
// tasks are preserved and only failed/pending/skipped tasks are re-executed.
// This is the manual/human retry entry point (caesium run retry); it performs no
// concurrency re-admission and does not consult Job.Paused — a human retrying a
// paused job's run is a deliberate human decision.
func (s *Store) RetryFromFailure(runID uuid.UUID) (*JobRun, error) {
	return s.retryFromFailure(runID, false)
}

// RetryFromFailureAdmitted is the admit-aware retry entry point used by the
// incident action executor's retry actions (retry_from_failure, snooze_retry,
// retry_callbacks re-run). It adds the two safety valves the plain
// RetryFromFailure store call lacks (design-agent-in-the-loop.md):
//
//  1. It refuses while the job is Paused (returns ErrJobPaused) — a human pause
//     outranks an agent retry.
//  2. It re-admits against metadata.concurrency using QUEUE semantics regardless
//     of the job's declared strategy: the run is flipped back to running only if
//     a concurrency slot is free (returns ErrMaxConcurrentRunsReached otherwise).
//     An agent retry must never replace-cancel a live run, nor race the next cron
//     tick for a slot admission never granted.
func (s *Store) RetryFromFailureAdmitted(runID uuid.UUID) (*JobRun, error) {
	return s.retryFromFailure(runID, true)
}

// retryResetColumns is the single definition of what "reset an instance for
// re-execution" means. RetryFromFailure, RetryPartition and RetryTaskInstance
// MUST use the same map, and it must cover EVERY column the previous attempt
// wrote — not just the scheduling ones.
//
// Three groups, each load-bearing:
//
//   - scheduling (status/claim/runtime/attempt): a reset that forgot
//     claimed_by / claim_expires_at / runtime_id leaves the instance owned by a
//     worker that will never run it.
//   - cache: a surviving cache_hit=true makes the run summary count a hit for an
//     instance that is about to re-execute.
//   - EXECUTION EVIDENCE (output, branch_selections, log snapshot, schema
//     violations, exit code, rate-limit park): this is the group that was
//     missing. A pending-but-not-yet-rerun instance that still carries the
//     FAILED attempt's output feeds those values to a downstream step through
//     CAESIUM_OUTPUT_*; its stale log tail is what `caesium logs` serves; its
//     stale exit code is what the incident classifier reads; and a surviving
//     rate_limit_retry_after keeps the "retried" instance parked behind a window
//     that closed on the previous attempt.
//
// A retried instance must be indistinguishable from one that has never run.
func retryResetColumns() map[string]interface{} {
	return map[string]interface{}{
		// Scheduling.
		"status":           string(TaskStatusPending),
		"completed_at":     nil,
		"started_at":       nil,
		"result":           "",
		"error":            "",
		"claimed_by":       "",
		"claim_expires_at": nil,
		"runtime_id":       "",
		"attempt":          1,
		// Cache.
		"cache_hit":           false,
		"cache_origin_run_id": nil,
		"cache_created_at":    nil,
		"cache_expires_at":    nil,
		// Execution evidence from the previous attempt.
		"output":                  nil,
		"branch_selections":       nil,
		"log_text":                "",
		"log_truncated":           false,
		"schema_violations":       nil,
		"exit_code":               nil,
		"rate_limit_retry_after":  nil,
		"partition_retry_pending": false,
	}
}

// satisfiedPredecessorTaskIDsTx returns the catalog task IDs whose whole
// TaskRun group is a terminal success. G5's accounting rule: one succeeded
// sibling does NOT satisfy a fanned predecessor — every live instance must be a
// terminal success.
func satisfiedPredecessorTaskIDsTx(tx *gorm.DB, runID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	var taskRuns []models.TaskRun
	if err := tx.Where("job_run_id = ?", runID).Find(&taskRuns).Error; err != nil {
		return nil, err
	}
	byTask := groupTaskRunsByTaskID(taskRuns)
	satisfied := make(map[uuid.UUID]struct{}, len(byTask))
	for taskID, rows := range byTask {
		if predecessorGroupSatisfied(rows) {
			satisfied[taskID] = struct{}{}
		}
	}
	return satisfied, nil
}

// resetInstanceOutstandingTx recomputes and persists a reset instance's
// outstanding_predecessors: one for every cross-step predecessor task whose
// group is not fully satisfied, plus one for every in-group dependency sibling
// that is not itself a terminal success (0 when the dependency already
// succeeded, 1 when it is being retried alongside this one). Returns the value
// written so the caller can decide whether the instance is immediately ready.
func (s *Store) resetInstanceOutstandingTx(tx *gorm.DB, runID uuid.UUID, instanceID, taskID uuid.UUID, satisfied map[uuid.UUID]struct{}) (int, error) {
	var edges []models.TaskEdge
	if err := tx.Where("to_task_id = ?", taskID).Find(&edges).Error; err != nil {
		return 0, err
	}
	outstanding := 0
	for _, edge := range edges {
		if _, ok := satisfied[edge.FromTaskID]; !ok {
			outstanding++
		}
	}

	var self models.TaskRun
	if err := tx.Where("id = ?", instanceID).First(&self).Error; err != nil {
		return 0, err
	}
	if len(self.PartitionDependsOn) > 0 {
		var deps []string
		_ = json.Unmarshal(self.PartitionDependsOn, &deps)
		var siblings []models.TaskRun
		if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).Find(&siblings).Error; err != nil {
			return 0, err
		}
		byKey := make(map[string]models.TaskRun, len(siblings))
		for i := range siblings {
			byKey[siblings[i].PartitionValue] = siblings[i]
		}
		for _, dep := range deps {
			sib, ok := byKey[dep]
			if !ok || !IsTerminalSuccess(TaskStatus(sib.Status)) {
				outstanding++
			}
		}
	}

	if err := tx.Model(&models.TaskRun{}).
		Where("id = ?", instanceID).
		Update("outstanding_predecessors", outstanding).Error; err != nil {
		return 0, err
	}
	return outstanding, nil
}

// invalidateCheckpointsForRetryTx drops the run's owner checkpoints as part of a
// retry.
//
// Checkpoints are a pure recovery cache: a recovering owner Restores the latest
// full snapshot and replays only the terminal rows AFTER it. A retry mutates
// rows the snapshot already recorded as terminal, so a surviving snapshot would
// make a recovered owner re-adopt the pre-retry state and never dispatch the
// reset instance — the retry would appear to succeed and then silently do
// nothing under CAESIUM_RUN_OWNER_IN_MEMORY. Dropping them forces Recover down
// its from-scratch path (checkpoint == nil → NewRunState + replay of the
// currently-terminal rows), which is exactly the post-retry truth.
func invalidateCheckpointsForRetryTx(tx *gorm.DB, runID uuid.UUID) error {
	// RunCheckpoint keys the run as TEXT (run_id), matching the other
	// run-owner tables.
	return tx.Where("run_id = ?", runID.String()).Delete(&models.RunCheckpoint{}).Error
}

// InvalidateRunCheckpoints drops every owner checkpoint of a run outside a
// retry transaction. The in-memory owner uses it when the store refused its
// completion for a pending per-partition retry: the snapshots it wrote since
// that retry committed describe a run with nothing left to do, and a recovery
// that restored one would never discover the reset instance.
func (s *Store) InvalidateRunCheckpoints(runID uuid.UUID) error {
	return invalidateCheckpointsForRetryTx(s.db, runID)
}

// lockJobRunForPartitionRetryTx serializes RetryPartition with Complete on the
// JobRun row before either path examines or mutates TaskRuns. Complete takes the
// same lock implicitly with its status UPDATE. Without this explicit lock,
// PostgreSQL READ COMMITTED permits this ordering:
//
//  1. RetryPartition reads JobRun=running.
//  2. Complete updates and locks the JobRun, then sees no retry marker.
//  3. RetryPartition resets the TaskRun after Complete commits.
//
// The reset then belongs to a terminal run and reopened=false prevents HTTP
// kickoff. With the shared lock, RetryPartition either commits its marker first
// (so Complete refuses) or reads the newly-terminal run and reopens it.
// SQLite and dqlite serialize writers already and cannot parse FOR UPDATE.
func lockJobRunForPartitionRetryTx(tx *gorm.DB, runID uuid.UUID) error {
	if tx == nil || tx.Dialector == nil {
		return fmt.Errorf("run: partition retry requires a database dialect")
	}
	stmt, err := partitionRetryRunLockSQL(tx.Name())
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	var locked struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	return tx.Raw(stmt, runID).Scan(&locked).Error
}

func partitionRetryRunLockSQL(dialect string) (string, error) {
	switch dialect {
	case "postgres":
		return "SELECT id FROM job_runs WHERE id = ? FOR UPDATE", nil
	case "dqlite", "sqlite", "sqlite3":
		return "", nil
	default:
		return "", fmt.Errorf("run: unsupported dialect %q for partition retry run lock", dialect)
	}
}

// RetryPartition resets ONE fan-out instance of a run for re-execution.
//
// It is the store half of `POST …/tasks/:task_id/partitions/:index/retry`. The
// controller previously did a bare `db.Model(&row).Updates(...)`: no
// transaction, no terminal-only guard (so a RUNNING instance could be reset
// mid-flight, orphaning its container), no reset of claimed_by / started_at /
// runtime_id / claim_expires_at / the cache columns, no
// outstanding_predecessors re-seed (so an ordered instance came back ready even
// though its in-group dependency had also been reset), no run re-open (so
// retrying an instance of an already-terminal run left the run terminal and
// nothing ever dispatched it), and no event.
//
// Semantics, mirroring retryFromFailure for exactly one row:
//   - terminal instances only (ErrTaskRunNotTerminal otherwise)
//   - the full retryResetColumns reset
//   - outstanding_predecessors re-seeded over NON-TERMINAL dependencies only
//   - the run re-opened when it had already finished
//   - checkpoints invalidated so owner in-memory mode sees the reset
//   - a task_ready event when the instance is immediately dispatchable
//
// The group is deliberately NOT re-expanded (the producer is terminal and the
// recorded instances are reused), and dependents that already succeeded are NOT
// cascaded — E2 requires the CLI to say so rather than silently re-running a
// subtree.
//
// The bool is true iff this transaction reopened a terminal run. Callers that
// start an in-process engine (the HTTP handler) MUST use this flag rather than
// a pre-tx status snapshot: a running local run can complete after that read
// and be reopened here, and kicking off from the stale "running" status would
// leave the reset instance pending forever. If the tx still saw the run
// running it does not reopen — the in-process loop is alive — and a second
// Run() would race it.
//
// The returned TaskRun is loaded inside that same transaction after the reset.
// A post-commit refresh must not be the source of the kickoff signal: if it
// failed, returning (nil, false, err) would discard a committed reopen and the
// handler would skip kickoff.
func (s *Store) RetryPartition(ctx context.Context, runID, taskRunID uuid.UUID) (*TaskRun, bool, error) {
	var (
		pendingEvents []event.Event
		counts        dbWriteCounts
		reopened      bool
		jobID         uuid.UUID
		quarantine    bool
		refreshed     models.TaskRun
	)

	err := withStoreBusyRetry(func() error {
		counts.reset()
		attemptEvents := make([]event.Event, 0, 2)
		reopened = false
		var attemptRow models.TaskRun

		txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// Lock order is JobRun then TaskRun, matching Complete. On PostgreSQL
			// this closes the cross-row READ COMMITTED race described above; on
			// SQLite/dqlite the helper is intentionally a no-op.
			if err := lockJobRunForPartitionRetryTx(tx, runID); err != nil {
				return err
			}
			var jobRun models.JobRun
			if err := tx.First(&jobRun, "id = ?", runID).Error; err != nil {
				return err
			}
			jobID = jobRun.JobID
			quarantine = jobRun.Quarantine

			switch jobRun.Status {
			case string(StatusRunning), string(StatusFailed), string(StatusSucceeded):
				// Running has a live executor. Failed/succeeded can be reopened
				// below after the target instance is validated.
			default:
				return fmt.Errorf("%w: run %s is %s", ErrPartitionRunNotRetryable, runID, jobRun.Status)
			}

			var row models.TaskRun
			if err := tx.Where("id = ? AND job_run_id = ?", taskRunID, runID).First(&row).Error; err != nil {
				return err
			}
			if !IsTerminal(TaskStatus(row.Status)) {
				return fmt.Errorf("%w: instance %s is %s", ErrTaskRunNotTerminal, taskRunID, row.Status)
			}
			// Terminal is necessary but not sufficient. The retryable set is
			// exactly {failed}:
			//
			//   succeeded / cached — re-running discards a result downstream steps
			//     may already have consumed (the group aggregate, the group
			//     identity hash, a published cache entry). "Retrying" a green
			//     partition is a re-run, and a re-run is a new run.
			//   skipped — resolved deliberately by fail_fast, the in-group
			//     dependency cascade, a branch selection or a trigger rule.
			//     Reviving one instance without reviving the decision that skipped
			//     it puts the group in a state the DAG never produced; retry the
			//     RUN, which resets skipped work as a set.
			//   cancelled — the run or the group was cancelled; a single instance
			//     cannot un-cancel it.
			if TaskStatus(row.Status) != TaskStatusFailed {
				return fmt.Errorf("%w: instance %s is %s, not failed", ErrPartitionNotRetryable, taskRunID, row.Status)
			}

			// Decide dispatchability BEFORE any mutation. A retry nothing in
			// this run can ever release (ErrPartitionRetryBlocked) must be
			// refused outright rather than reset into a pending row the engine
			// will only ever sweep as "never dispatched".
			outstanding, err := s.partitionRetryOutstandingTx(tx, runID, &row)
			if err != nil {
				return err
			}

			// Re-open a finished run so the reset instance can actually be
			// dispatched. readmitRetryTx with admit=false is the manual-retry
			// path: unconditional, no concurrency re-admission (this is a human
			// action on an existing run, not a new admission).
			if jobRun.Status == string(StatusFailed) || jobRun.Status == string(StatusSucceeded) {
				if err := s.readmitRetryTx(tx, &jobRun, false); err != nil {
					return err
				}
				reopened = true
			}

			updates := retryResetColumns()
			updates["partition_retry_pending"] = true
			updates["outstanding_predecessors"] = outstanding
			if err := tx.Model(&models.TaskRun{}).
				Where("id = ?", row.ID).
				Updates(updates).Error; err != nil {
				return err
			}
			counts.addTaskRunStatus(1)

			if err := invalidateCheckpointsForRetryTx(tx, runID); err != nil {
				return err
			}

			if outstanding == 0 {
				if err := s.appendTaskReadyEventTx(tx, runID, row.TaskID, &attemptEvents, &counts); err != nil {
					return err
				}
			}
			if reopened && s.eventStore != nil {
				loaded, loadErr := s.loadRunWithDB(tx, runID)
				if loadErr != nil {
					return loadErr
				}
				payload, marshalErr := json.Marshal(loaded)
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
				attemptEvents = append(attemptEvents, evt)
			}

			// Load the reset row inside this transaction so a committed reopen
			// cannot be discarded by a post-commit refresh failure.
			if err := tx.Where("id = ?", row.ID).First(&attemptRow).Error; err != nil {
				return err
			}
			return nil
		})
		if txErr == nil {
			pendingEvents = attemptEvents
			refreshed = attemptRow
		}
		return txErr
	})
	if err != nil {
		return nil, false, err
	}

	counts.commit()
	s.publishEvents(pendingEvents...)
	// Even a single-partition retry invalidates the snapshot: the instance is
	// pending again, and a state that still calls it terminal will never
	// dispatch it.
	s.invalidateRunState(runID)

	if reopened && !quarantine {
		s.startedMu.Lock()
		s.startedRuns[runID] = struct{}{}
		s.startedMu.Unlock()
		metrics.JobsActive.WithLabelValues(jobID.String()).Inc()
	}

	return convertRunTaskModel(&refreshed), reopened, nil
}

// partitionRetryOutstandingTx computes the outstanding_predecessors a retried
// instance is re-seeded with, or ErrPartitionRetryBlocked when nothing in this
// run can ever release it.
//
// Cross-step predecessors are judged by the STEP's trigger rule, exactly as
// the dispatch path judges them (shouldRunTaskTx): a consumer that ran under
// all_done after an upstream failure is released by that failure, not
// stranded behind it. When every predecessor group is terminal the rule is
// decisive — satisfied means nothing outstanding, unsatisfied means the retry
// is refused. While any predecessor group is still live, only the LIVE groups
// are counted: outstanding_predecessors is a terminality counter, decremented
// once per predecessor group when that group becomes terminal with the rule
// evaluated at that moment (shouldRunTaskTx), so a group that is already
// terminal without succeeding will never decrement again and counting it
// would strand the row.
//
// In-group dependsOn siblings carry no trigger rule: a terminal sibling that
// did not succeed blocks the retry for good (refused), a non-terminal one
// (pending again because it was retried first) is counted and released when
// it finishes, and a sibling the run never recorded can never be satisfied.
func (s *Store) partitionRetryOutstandingTx(tx *gorm.DB, runID uuid.UUID, row *models.TaskRun) (int, error) {
	outstanding := 0

	refs, err := s.resolvePredecessorsTx(tx, runID, row.TaskID)
	if err != nil {
		return 0, err
	}
	if len(refs) > 0 {
		predRows, err := predecessorTaskRunsTx(tx, runID, refs, "task_id", "status")
		if err != nil {
			return 0, err
		}
		statuses := aggregatePredecessorStatuses(predecessorTaskIDs(refs), predRows)
		allTerminal := true
		for _, status := range statuses {
			if !IsTerminal(status) {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			rawRule, rule, err := s.resolveTriggerRuleTx(tx, runID, row.TaskID)
			if err != nil {
				return 0, err
			}
			if !satisfiesTriggerRule(rawRule, statuses) {
				return 0, fmt.Errorf("%w: trigger rule %q is not satisfied by the step's predecessors", ErrPartitionRetryBlocked, rule)
			}
		} else {
			byTask := groupTaskRunsByTaskID(predRows)
			for _, ref := range refs {
				if !IsTerminal(groupStatusFromInstances(byTask[ref.TaskID])) {
					outstanding++
				}
			}
		}
	}

	if len(row.PartitionDependsOn) > 0 {
		var deps []string
		_ = json.Unmarshal(row.PartitionDependsOn, &deps)
		var siblings []models.TaskRun
		if err := tx.Where("job_run_id = ? AND task_id = ?", runID, row.TaskID).Find(&siblings).Error; err != nil {
			return 0, err
		}
		byKey := make(map[string]models.TaskRun, len(siblings))
		for i := range siblings {
			byKey[siblings[i].PartitionValue] = siblings[i]
		}
		for _, dep := range deps {
			sib, ok := byKey[dep]
			switch {
			case !ok:
				return 0, fmt.Errorf("%w: in-group dependency %q is not recorded in this run", ErrPartitionRetryBlocked, dep)
			case IsTerminalSuccess(TaskStatus(sib.Status)):
				// Released.
			case IsTerminal(TaskStatus(sib.Status)):
				return 0, fmt.Errorf("%w: in-group dependency %q is %s", ErrPartitionRetryBlocked, dep, sib.Status)
			default:
				outstanding++
			}
		}
	}

	return outstanding, nil
}

// PendingPartitionRetries lists the retry-reset instances of a run that are
// still pending — exactly the rows Complete's fence refuses on. Only id and
// task_id are populated.
func (s *Store) PendingPartitionRetries(runID uuid.UUID) ([]models.TaskRun, error) {
	var rows []models.TaskRun
	if err := s.db.Select("id", "task_id").
		Where("job_run_id = ? AND status = ? AND started_at IS NULL AND partition_retry_pending = ?",
			runID, string(TaskStatusPending), true).
		Order("partition_index ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// AbandonPartitionRetries resolves the given retry-reset instances, if still
// pending, as skipped with the given reason and clears their retry
// provenance. It is the bounded end of the completion fence: a replacement
// engine that could not dispatch the retries it was started for must not
// spawn yet another engine for them. Each row is resolved explicitly so the
// run can finalize and the operator can see why. Returns the number of
// instances resolved.
func (s *Store) AbandonPartitionRetries(runID uuid.UUID, taskRunIDs []uuid.UUID, reason string) (int, error) {
	if len(taskRunIDs) == 0 {
		return 0, nil
	}
	var rows []models.TaskRun
	if err := s.db.Select("id").
		Where("job_run_id = ? AND id IN ? AND status = ? AND started_at IS NULL AND partition_retry_pending = ?",
			runID, taskRunIDs, string(TaskStatusPending), true).
		Find(&rows).Error; err != nil {
		return 0, err
	}
	resolved := 0
	for i := range rows {
		if err := s.SkipTaskInstance(runID, rows[i].ID, reason); err != nil {
			return resolved, err
		}
		resolved++
	}
	if resolved > 0 {
		s.invalidateRunState(runID)
	}
	return resolved, nil
}

// AbandonPendingPartitionRetries is AbandonPartitionRetries over every
// retry-reset instance of the run that is still pending. It is for an engine
// that failed before it could execute anything, where no retry can be told
// apart from another.
func (s *Store) AbandonPendingPartitionRetries(runID uuid.UUID, reason string) (int, error) {
	rows, err := s.PendingPartitionRetries(runID)
	if err != nil {
		return 0, err
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	return s.AbandonPartitionRetries(runID, ids, reason)
}

func (s *Store) retryFromFailure(runID uuid.UUID, admit bool) (*JobRun, error) {
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

		// Safety valve 1 (agent retries only): a human pause outranks an agent
		// retry, so refuse while the owning job is paused.
		if admit {
			var job models.Job
			if err := tx.Select("paused").First(&job, "id = ?", jobRun.JobID).Error; err != nil {
				return err
			}
			if job.Paused {
				return ErrJobPaused
			}
		}

		// 2. Reset the job run status to running.
		if err := s.readmitRetryTx(tx, &jobRun, admit); err != nil {
			return err
		}

		// 3. Get all task runs for this run.
		var taskRuns []models.TaskRun
		if err := tx.Where("job_run_id = ?", runID).Find(&taskRuns).Error; err != nil {
			return err
		}

		terminalSuccessIDs, err := satisfiedPredecessorTaskIDsTx(tx, runID)
		if err != nil {
			return err
		}

		// 4. Reset failed and skipped *instances* to pending.
		// Leave succeeded and cached instances as-is. A predecessor group is
		// satisfied only when every live sibling is a terminal success.
		type resetInstance struct {
			id     uuid.UUID
			taskID uuid.UUID
		}
		resetInstances := make([]resetInstance, 0)
		resetTaskReadyOnce := make(map[uuid.UUID]struct{})
		for i := range taskRuns {
			tr := &taskRuns[i]
			status := TaskStatus(tr.Status)
			if status == TaskStatusFailed || status == TaskStatusSkipped {
				if err := tx.Model(tr).Where("id = ?", tr.ID).Updates(retryResetColumns()).Error; err != nil {
					return err
				}
				resetInstances = append(resetInstances, resetInstance{id: tr.ID, taskID: tr.TaskID})
			}
		}

		// 5. Recalculate outstanding_predecessors for each reset instance.
		for _, inst := range resetInstances {
			outstanding, err := s.resetInstanceOutstandingTx(tx, runID, inst.id, inst.taskID, terminalSuccessIDs)
			if err != nil {
				return err
			}

			if outstanding == 0 {
				if _, ok := resetTaskReadyOnce[inst.taskID]; ok {
					continue
				}
				resetTaskReadyOnce[inst.taskID] = struct{}{}
				if err := s.appendTaskReadyEventTx(tx, runID, inst.taskID, &pendingEvents, &counts); err != nil {
					return err
				}
			}
		}

		// 5b. Invalidate the owner checkpoints the reset just made stale, so a
		// run recovered under CAESIUM_RUN_OWNER_IN_MEMORY replays from the
		// post-retry terminal rows instead of re-adopting the pre-retry snapshot.
		if err := invalidateCheckpointsForRetryTx(tx, runID); err != nil {
			return err
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
	// The rows this transaction re-opened contradict any in-memory snapshot of
	// them; drop it so the next dispatch tick rebuilds from what was just
	// written.
	s.invalidateRunState(runID)

	if !quarantine {
		// Track this run in the active set so Complete() will decrement the gauge.
		s.startedMu.Lock()
		s.startedRuns[runID] = struct{}{}
		s.startedMu.Unlock()
		metrics.JobsActive.WithLabelValues(jobID.String()).Inc()
	}

	return s.loadRun(runID)
}
