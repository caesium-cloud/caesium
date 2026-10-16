package jobdef

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	contractenforce "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/dbretry"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrDuplicateJob       = errors.New("job alias already exists")
	ErrProvenanceConflict = errors.New("job definition provenance conflict")
	ErrJobRunning         = errors.New("job has a running run")
	ErrContractBreak      = contractenforce.ErrContractBreakBlocked
	ErrContractAck        = contractenforce.ErrInvalidContractAck
	// ErrNamespaceNotAllowed is wrapped by *NamespaceAllowlistError.
	ErrNamespaceNotAllowed = errors.New("job namespace is outside the source namespace allowlist")

	importerBusyRetryBackoffs = []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
	}

	triggerMutationHookMu sync.RWMutex
	triggerMutationHook   TriggerMutationHook
)

type TriggerMutationHook func(context.Context) error

func SetTriggerMutationHook(hook TriggerMutationHook) {
	triggerMutationHookMu.Lock()
	defer triggerMutationHookMu.Unlock()
	triggerMutationHook = hook
}

func notifyTriggerMutation(ctx context.Context) error {
	triggerMutationHookMu.RLock()
	hook := triggerMutationHook
	triggerMutationHookMu.RUnlock()
	if hook == nil {
		return nil
	}
	return hook(ctx)
}

func notifyTriggerMutationBestEffort(ctx context.Context, action string) {
	if err := notifyTriggerMutation(ctx); err != nil {
		log.Warn("event trigger router reload failed after jobdef mutation", "action", action, "error", err)
	}
}

// Reasons a NamespaceAllowlistError reports.
const (
	NamespaceReasonRequested = "requested_namespace_not_allowed"
	NamespaceReasonStored    = "stored_namespace_not_allowed"
)

// NamespaceAllowlistError reports which side of an apply left the allowlist:
// the namespace the definition requests, or the one its stored row has.
type NamespaceAllowlistError struct {
	Alias              string
	RequestedNamespace string
	StoredNamespace    string // "" when the alias has no row
	Allowed            []string
	Reason             string // NamespaceReasonRequested or NamespaceReasonStored
}

func (e *NamespaceAllowlistError) Error() string {
	return fmt.Sprintf("%s: alias %s (%s; requested %q, stored %q, allowed %q)",
		ErrNamespaceNotAllowed, e.Alias, e.Reason, e.RequestedNamespace, e.StoredNamespace, e.Allowed)
}

func (e *NamespaceAllowlistError) Unwrap() error { return ErrNamespaceNotAllowed }

// namespaceAllowed reports whether ns is listed; a nil allowlist allows all.
func namespaceAllowed(allowlist []string, ns string) bool {
	return allowlist == nil || slices.Contains(allowlist, models.NamespaceOrDefault(ns))
}

// Importer coordinates persistence of job definitions.
type Importer struct {
	db *gorm.DB
}

// DB returns the connection the importer writes to; git sync records its
// audit row in the same catalog.
func (i *Importer) DB() *gorm.DB { return i.db }

// StoredNamespaces maps each alias in aliases that has a jobs row, soft-deleted
// rows included (the same view as findJobByAliasTx), to that row's namespace.
// It reads outside any transaction so callers can keep definitions that the
// apply guard will refuse out of ValidateBatch; the guard stays the authority.
func (i *Importer) StoredNamespaces(ctx context.Context, aliases []string) (map[string]string, error) {
	stored := make(map[string]string, len(aliases))
	if len(aliases) == 0 {
		return stored, nil
	}
	var rows []struct {
		Alias     string
		Namespace string
	}
	if err := i.db.WithContext(ctx).Unscoped().Model(&models.Job{}).Select("alias", "namespace").
		Where("alias IN ?", aliases).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		stored[row.Alias] = models.NamespaceOrDefault(row.Namespace)
	}
	return stored, nil
}

// NewImporter creates a new importer. The provided db connection must be non-nil.
func NewImporter(dbConn *gorm.DB) *Importer {
	if dbConn == nil {
		panic("jobdef importer requires a database connection")
	}
	return &Importer{db: dbConn}
}

// Apply persists the provided definition and returns the reconciled job record.
func (i *Importer) Apply(ctx context.Context, def *schema.Definition) (*models.Job, error) {
	return i.ApplyWithOptions(ctx, def, nil)
}

// ApplyOptions control optional behaviors for ApplyWithOptions.
type ApplyOptions struct {
	Provenance       *Provenance
	Force            bool
	ContractAck      *contractenforce.AllowBreaking
	ContractWarnings *[]contractenforce.ContractWarning
	// NamespaceAllowlist confines the apply: both the requested namespace and
	// the namespace of an existing row (retired rows included) must be listed.
	// nil means unrestricted. Force does not bypass it.
	NamespaceAllowlist []string
}

// PruneSkip names a job PruneMissing left in place because its namespace is
// outside PruneOptions.NamespaceAllowlist.
type PruneSkip struct {
	Alias     string `json:"alias"`
	Namespace string `json:"namespace"`
}

// PruneOptions scope the set of jobs that should be retired when pruning.
type PruneOptions struct {
	SourceID string
	// NamespaceAllowlist, when non-nil, limits pruning to jobs whose namespace
	// is listed; the others are left untouched.
	NamespaceAllowlist []string
	// Skipped, when non-nil, receives the jobs left untouched because of
	// NamespaceAllowlist, sorted by alias.
	Skipped *[]PruneSkip
}

// Provenance captures metadata describing the origin of a job definition.
type Provenance struct {
	SourceID string
	Repo     string
	Ref      string
	Commit   string
	Path     string
}

// ApplyWithOptions persists the provided definition using the supplied options.
// Existing jobs are reconciled in place so history remains attached to the same job ID.
func (i *Importer) ApplyWithOptions(ctx context.Context, def *schema.Definition, opts *ApplyOptions) (*models.Job, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}

	var result *models.Job
	err := withImporterBusyRetry(ctx, func() error {
		var attemptResult *models.Job
		err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			jobModel, err := i.applyTx(ctx, tx, def, opts)
			attemptResult = jobModel
			return err
		})
		if err != nil {
			return err
		}
		result = attemptResult
		return nil
	})
	if err != nil {
		return nil, err
	}
	notifyTriggerMutationBestEffort(ctx, "apply")

	return result, nil
}

// applyTx reconciles one validated definition inside tx. The guard runs before
// any write, so a refused definition writes nothing.
func (i *Importer) applyTx(ctx context.Context, tx *gorm.DB, def *schema.Definition, opts *ApplyOptions) (*models.Job, error) {
	existing, err := i.findJobByAliasTx(tx, def.Metadata.Alias)
	if err != nil {
		return nil, err
	}
	if err := i.guardJobMutationTx(tx, existing, def, opts); err != nil {
		return nil, err
	}

	jobModel, triggerModel, err := i.upsertJobAndTriggerTx(tx, existing, def, opts)
	if err != nil {
		return nil, err
	}
	if triggerModel != nil {
		jobModel.TriggerID = triggerModel.ID
	}

	taskByName, _, retiredAtomIDs, err := i.reconcileTasksTx(tx, jobModel, def, opts)
	if err != nil {
		return nil, err
	}
	successors, err := schema.DeriveStepSuccessors(def.Steps)
	if err != nil {
		return nil, err
	}
	if err := i.reconcileEdgesTx(tx, jobModel, def.Steps, successors, taskByName, opts); err != nil {
		return nil, err
	}
	if err := i.writeSnapshotTx(tx, jobModel, def.Steps, successors, taskByName, opts); err != nil {
		return nil, err
	}
	if err := i.reconcileCallbacksTx(tx, jobModel.ID, def.Callbacks); err != nil {
		return nil, err
	}
	if err := i.reconcileDatasetDeclarationsTx(tx, jobModel, def); err != nil {
		return nil, err
	}
	if err := i.enforceContractsWithOptionsTx(ctx, tx, def, opts); err != nil {
		return nil, err
	}
	if err := i.softDeleteAtomsTx(tx, retiredAtomIDs); err != nil {
		return nil, err
	}
	return jobModel, nil
}

// BatchPlan is one definition of an ApplyBatchWithOptions call.
type BatchPlan struct {
	Def  *schema.Definition
	Opts *ApplyOptions
}

// BatchRefusal is a plan whose namespace allowlist refused it.
type BatchRefusal struct {
	Index int
	Err   *NamespaceAllowlistError
}

// BatchResult reports an ApplyBatchWithOptions call. On error Applied is 0
// and Refused holds the refusals of the attempt that failed.
type BatchResult struct {
	Applied int
	Refused []BatchRefusal
}

// ApplyBatchWithOptions applies every plan in one transaction: all of them or
// none. A plan refused by its namespace allowlist writes nothing and is
// reported in Refused; any other error rolls the whole batch back. Before
// commit the trigger-chain and dataset graphs of the stored jobs are validated
// as the transaction sees them, so a refusal can never leave the effective
// graph invalid; a failure there wraps ErrBatchInvalid and rolls back too.
func (i *Importer) ApplyBatchWithOptions(ctx context.Context, plans []BatchPlan) (BatchResult, error) {
	for _, plan := range plans {
		if err := plan.Def.Validate(); err != nil {
			return BatchResult{}, err
		}
	}

	aliases := make([]string, 0, len(plans))
	for _, plan := range plans {
		aliases = append(aliases, plan.Def.Metadata.Alias)
	}

	var result BatchResult
	err := withImporterBusyRetry(ctx, func() error {
		attempt := BatchResult{}
		err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := lockBatchJobsTx(tx, aliases); err != nil {
				return err
			}
			for idx, plan := range plans {
				if _, err := i.applyTx(ctx, tx, plan.Def, plan.Opts); err != nil {
					nsErr, ok := errors.AsType[*NamespaceAllowlistError](err)
					if !ok {
						return err
					}
					attempt.Refused = append(attempt.Refused, BatchRefusal{Index: idx, Err: nsErr})
					continue
				}
				attempt.Applied++
			}
			if err := ValidateTriggerChains(ctx, tx, nil); err != nil {
				return batchInvalid(err)
			}
			if err := ValidateDatasetGraph(ctx, tx, nil); err != nil {
				return batchInvalid(err)
			}
			return nil
		})
		result = attempt
		return err
	})
	if err != nil {
		result.Applied = 0
		return result, err
	}
	if result.Applied > 0 {
		notifyTriggerMutationBestEffort(ctx, "apply")
	}
	return result, nil
}

// ErrBatchInvalid marks an ApplyBatchWithOptions batch whose effective stored
// graph failed validation; nothing from the batch was written.
var ErrBatchInvalid = errors.New("batch leaves the stored job graph invalid")

// batchInvalidVerdicts are the validator errors that mean the effective graph
// is invalid. Anything else (a cancelled context, a read or leadership error,
// contention) is returned as itself, not blamed on the batch.
var batchInvalidVerdicts = []error{
	ErrTriggerChainCycle,
	freshness.ErrDatasetMultipleProducers,
	freshness.ErrDatasetUnresolvedConsumes,
	freshness.ErrDatasetGraphCycle,
}

func batchInvalid(err error) error {
	for _, verdict := range batchInvalidVerdicts {
		if errors.Is(err, verdict) {
			return fmt.Errorf("%w: %w", ErrBatchInvalid, err)
		}
	}
	return err
}

// ContractBreakResponse returns the stable 409 payload for an apply-time
// contract enforcement error.
func ContractBreakResponse(err error) (any, bool) {
	enforcementErr, ok := errors.AsType[*contractenforce.EnforcementError](err)
	if !ok {
		return nil, false
	}
	return enforcementErr.Response, true
}

func (i *Importer) enforceContractsWithOptionsTx(ctx context.Context, tx *gorm.DB, def *schema.Definition, opts *ApplyOptions) error {
	vars := env.Variables()
	mode := vars.ContractEnforcement
	if strings.TrimSpace(mode) == "" {
		return nil
	}
	enforceOpts := contractenforce.ApplyOptions{
		DeprecationWindow: vars.ContractDeprecationWindow,
		EventStore:        event.NewStore(i.db),
	}
	if opts != nil {
		enforceOpts.AllowBreaking = opts.ContractAck
	}
	result, err := contractenforce.EnforceApplyWithOptions(ctx, tx, []schema.Definition{*def}, mode, time.Now().UTC(), enforceOpts)
	if err != nil {
		return err
	}
	if opts != nil && opts.ContractWarnings != nil && len(result.Warnings) > 0 {
		*opts.ContractWarnings = append(*opts.ContractWarnings, result.Warnings...)
	}
	return nil
}

// PruneMissing retires active jobs that are absent from the desired alias set.
// When opts.SourceID is set, pruning is scoped to jobs imported by that source.
func (i *Importer) PruneMissing(ctx context.Context, desiredAliases []string, opts *PruneOptions) (int, error) {
	desiredSet := make(map[string]struct{}, len(desiredAliases))
	for _, alias := range desiredAliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		desiredSet[alias] = struct{}{}
	}

	var allowlist []string
	if opts != nil {
		allowlist = opts.NamespaceAllowlist
	}

	var pruned int
	var skipped []PruneSkip
	err := withImporterBusyRetry(ctx, func() error {
		var attemptPruned int
		attemptSkipped := make([]PruneSkip, 0)
		err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var jobs []models.Job
			if err := pruneScopeTx(tx, opts).Find(&jobs).Error; err != nil {
				return err
			}

			toRetire := make([]*models.Job, 0, len(jobs))
			for idx := range jobs {
				jobModel := &jobs[idx]
				if _, ok := desiredSet[jobModel.Alias]; ok {
					continue
				}
				if !namespaceAllowed(allowlist, jobModel.Namespace) {
					attemptSkipped = append(attemptSkipped, PruneSkip{Alias: jobModel.Alias, Namespace: models.NamespaceOrDefault(jobModel.Namespace)})
					continue
				}
				toRetire = append(toRetire, jobModel)
			}
			if len(toRetire) == 0 {
				return nil
			}
			if err := i.ensureJobsNotRunningTx(tx, toRetire); err != nil {
				return err
			}
			if err := i.retireJobsTx(tx, toRetire); err != nil {
				return err
			}
			attemptPruned = len(toRetire)
			return nil
		})
		if err != nil {
			return err
		}
		pruned = attemptPruned
		skipped = attemptSkipped
		return nil
	})
	if err != nil {
		return pruned, err
	}
	if opts != nil && opts.Skipped != nil {
		slices.SortFunc(skipped, func(a, b PruneSkip) int { return strings.Compare(a.Alias, b.Alias) })
		*opts.Skipped = skipped
	}
	if pruned > 0 {
		notifyTriggerMutationBestEffort(ctx, "prune")
	}
	return pruned, nil
}

func withImporterBusyRetry(ctx context.Context, fn func() error) error {
	return dbretry.Retry(ctx, dbretry.Policy{
		Backoffs: importerBusyRetryBackoffs, Retryable: isImporterContentionErr, BeforeAttempt: true,
		OnRetry: func(error) { metrics.DBBusyRetriesTotal.Inc() }, Wait: sleepImporterBusyRetry,
	}, fn)
}

func sleepImporterBusyRetry(ctx context.Context, base time.Duration) error {
	timer := time.NewTimer(jitterImporterBusyRetryBackoff(base))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitterImporterBusyRetryBackoff(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	maxJitter := int64(base / 5)
	if maxJitter <= 0 {
		return base
	}
	return base - time.Duration(rand.Int64N(maxJitter+1))
}

func isImporterContentionErr(err error) bool {
	// PostgreSQL aborts one side of a row-lock deadlock (40P01) or a
	// serialization conflict (40001); re-running the whole transaction is the
	// documented remedy. pgconn.PgError exposes the code as SQLState.
	if pgErr, ok := errors.AsType[interface {
		error
		SQLState() string
	}](err); ok && (pgErr.SQLState() == "40P01" || pgErr.SQLState() == "40001") {
		return true
	}
	// Delegate to the single shared classifier so the matched error strings
	// live in exactly one place (pkg/dqlite).
	return dqlite.IsContentionError(err)
}

// lockJobRows makes a jobs read take row locks (SELECT ... FOR UPDATE) where
// the backend has them. The apply guard and PruneMissing check a row's
// namespace and provenance and then write it in the same transaction; on
// PostgreSQL READ COMMITTED a concurrent apply could otherwise move the row in
// between. The SQLite and dqlite dialects drop the clause: their transactions
// are already serializable.
func lockJobRows(tx *gorm.DB) *gorm.DB {
	return tx.Clauses(clause.Locking{Strength: "UPDATE"})
}

// jobAliasLockOrder orders multi-row job locks: PruneMissing and
// ApplyBatchWithOptions take theirs in this one order, so they cannot deadlock
// each other on PostgreSQL. COLLATE "C" pins byte order there regardless of the
// database collation; SQLite's default BINARY collation is already byte order.
func jobAliasLockOrder(tx *gorm.DB) string {
	if tx.Name() == "postgres" {
		return `alias COLLATE "C"`
	}
	return "alias"
}

// lockBatchJobsTx locks every existing row (retired ones included) of a batch's
// aliases up front, in jobAliasLockOrder, before the batch applies them in its
// own order.
func lockBatchJobsTx(tx *gorm.DB, aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	var ids []uuid.UUID
	return lockJobRows(tx).Unscoped().Model(&models.Job{}).Where("alias IN ?", aliases).
		Order(jobAliasLockOrder(tx)).Pluck("id", &ids).Error
}

// pruneScopeTx selects the active jobs PruneMissing may retire, locked in
// jobAliasLockOrder.
func pruneScopeTx(tx *gorm.DB, opts *PruneOptions) *gorm.DB {
	query := lockJobRows(tx).Model(&models.Job{}).Order(jobAliasLockOrder(tx))
	if opts != nil && strings.TrimSpace(opts.SourceID) != "" {
		query = query.Where("provenance_source_id = ?", strings.TrimSpace(opts.SourceID))
	}
	return query
}

func (i *Importer) findJobByAliasTx(tx *gorm.DB, alias string) (*models.Job, error) {
	var jobModel models.Job
	err := lockJobRows(tx).Unscoped().Where("alias = ?", alias).First(&jobModel).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return &jobModel, nil
	}
}

func (i *Importer) guardJobMutationTx(tx *gorm.DB, jobModel *models.Job, def *schema.Definition, opts *ApplyOptions) error {
	// The allowlist runs first: Force must not bypass it, and a foreign-owned
	// row in a foreign namespace is a namespace refusal, not a provenance one.
	if opts != nil && opts.NamespaceAllowlist != nil {
		reason := ""
		stored := ""
		if jobModel != nil {
			stored = models.NamespaceOrDefault(jobModel.Namespace)
		}
		switch {
		case !namespaceAllowed(opts.NamespaceAllowlist, def.Metadata.Namespace):
			reason = NamespaceReasonRequested
		case jobModel != nil && !namespaceAllowed(opts.NamespaceAllowlist, stored):
			reason = NamespaceReasonStored
		}
		if reason != "" {
			return &NamespaceAllowlistError{Alias: def.Metadata.Alias, RequestedNamespace: models.NamespaceOrDefault(def.Metadata.Namespace),
				StoredNamespace: stored, Allowed: slices.Clone(opts.NamespaceAllowlist), Reason: reason}
		}
	}
	if jobModel == nil {
		return nil
	}

	if err := i.ensureJobNotRunningTx(tx, jobModel); err != nil {
		return err
	}

	if opts != nil && opts.Force {
		return nil
	}

	incomingSourceID := ""
	if opts != nil && opts.Provenance != nil {
		incomingSourceID = strings.TrimSpace(opts.Provenance.SourceID)
	}
	existingSourceID := strings.TrimSpace(jobModel.ProvenanceSourceID)
	if existingSourceID != incomingSourceID && (existingSourceID != "" || incomingSourceID != "") {
		return fmt.Errorf("%w: alias %s managed by %q, incoming source %q", ErrProvenanceConflict, jobModel.Alias, existingSourceID, incomingSourceID)
	}
	return nil
}

func (i *Importer) ensureJobNotRunningTx(tx *gorm.DB, jobModel *models.Job) error {
	if jobModel == nil {
		return nil
	}

	var running int64
	if err := tx.Model(&models.JobRun{}).
		Where("job_id = ? AND status = ?", jobModel.ID, "running").
		Count(&running).Error; err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("%w: %s", ErrJobRunning, jobModel.Alias)
	}
	return nil
}

func (i *Importer) ensureJobsNotRunningTx(tx *gorm.DB, jobs []*models.Job) error {
	if len(jobs) == 0 {
		return nil
	}

	jobIDs := make([]uuid.UUID, 0, len(jobs))
	aliasByID := make(map[uuid.UUID]string, len(jobs))
	for _, jobModel := range jobs {
		if jobModel == nil {
			continue
		}
		jobIDs = append(jobIDs, jobModel.ID)
		aliasByID[jobModel.ID] = jobModel.Alias
	}
	if len(jobIDs) == 0 {
		return nil
	}

	var running []struct {
		JobID uuid.UUID
	}
	if err := tx.Model(&models.JobRun{}).
		Select("job_id").
		Where("job_id IN ? AND status = ?", jobIDs, "running").
		Group("job_id").
		Find(&running).Error; err != nil {
		return err
	}
	if len(running) == 0 {
		return nil
	}
	alias := aliasByID[running[0].JobID]
	if alias == "" {
		alias = running[0].JobID.String()
	}
	return fmt.Errorf("%w: %s", ErrJobRunning, alias)
}

func (i *Importer) upsertJobAndTriggerTx(tx *gorm.DB, existing *models.Job, def *schema.Definition, opts *ApplyOptions) (*models.Job, *models.Trigger, error) {
	triggerModel, err := i.upsertTriggerTx(tx, existing, def.Metadata.Alias, &def.Trigger, opts)
	if err != nil {
		return nil, nil, err
	}

	cacheConfig, err := marshalOptionalJSON(def.Metadata.Cache)
	if err != nil {
		return nil, nil, fmt.Errorf("metadata.cache: %w", err)
	}
	concurrency, err := marshalOptionalJSON(def.Metadata.Concurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("metadata.concurrency: %w", err)
	}
	rateLimits, err := marshalRateLimits(def.Metadata.RateLimits)
	if err != nil {
		return nil, nil, fmt.Errorf("metadata.rateLimits: %w", err)
	}
	// The remediation block is persisted, not merely validated: it is the ONLY
	// way the action executor can evaluate an agent proposal under the job's own
	// policy. Dropping it here (as this mapping used to) meant every proposal was
	// judged by the deployment-wide default profile, so a job that narrowed the
	// autonomous allowlist was silently widened back to the default's.
	remediation, err := marshalOptionalJSON(def.Metadata.Remediation)
	if err != nil {
		return nil, nil, fmt.Errorf("metadata.remediation: %w", err)
	}

	if existing == nil {
		jobModel := &models.Job{
			ID:               uuid.New(),
			Alias:            def.Metadata.Alias,
			Namespace:        def.Metadata.Namespace,
			TriggerID:        triggerModel.ID,
			Labels:           jsonmap.FromStringMap(def.Metadata.Labels),
			Annotations:      jsonmap.FromStringMap(def.Metadata.Annotations),
			MaxParallelTasks: def.Metadata.MaxParallelTasks,
			TaskTimeout:      def.Metadata.TaskTimeout,
			RunTimeout:       def.Metadata.RunTimeout,
			Priority:         def.Metadata.Priority,
			Concurrency:      concurrency,
			RateLimits:       rateLimits,
			SLA:              marshalSLA(def.Metadata.SLA),
			SchemaValidation: def.Metadata.SchemaValidation,
			OnUpstreamHold:   def.Metadata.OnUpstreamHold,
			ReplaySafe:       def.Metadata.ReplaySafe,
			CacheConfig:      cacheConfig,
			Remediation:      remediation,
		}
		applyJobProvenance(jobModel, opts)
		if err := tx.Create(jobModel).Error; err != nil {
			return nil, nil, err
		}
		return jobModel, triggerModel, nil
	}

	existing.Alias = def.Metadata.Alias
	existing.Namespace = def.Metadata.Namespace
	existing.TriggerID = triggerModel.ID
	existing.Labels = jsonmap.FromStringMap(def.Metadata.Labels)
	existing.Annotations = jsonmap.FromStringMap(def.Metadata.Annotations)
	existing.MaxParallelTasks = def.Metadata.MaxParallelTasks
	existing.TaskTimeout = def.Metadata.TaskTimeout
	existing.RunTimeout = def.Metadata.RunTimeout
	existing.Priority = def.Metadata.Priority
	existing.Concurrency = concurrency
	existing.RateLimits = rateLimits
	existing.SLA = marshalSLA(def.Metadata.SLA)
	existing.SchemaValidation = def.Metadata.SchemaValidation
	existing.OnUpstreamHold = def.Metadata.OnUpstreamHold
	existing.ReplaySafe = def.Metadata.ReplaySafe
	existing.CacheConfig = cacheConfig
	existing.Remediation = remediation
	applyJobProvenance(existing, opts)

	updates := map[string]any{
		"alias":              existing.Alias,
		"namespace":          existing.Namespace,
		"trigger_id":         existing.TriggerID,
		"labels":             existing.Labels,
		"annotations":        existing.Annotations,
		"max_parallel_tasks": existing.MaxParallelTasks,
		"task_timeout":       existing.TaskTimeout,
		"run_timeout":        existing.RunTimeout,
		"priority":           existing.Priority,
		"concurrency":        existing.Concurrency,
		"rate_limits":        existing.RateLimits,
		"sla":                existing.SLA,
		"schema_validation":  existing.SchemaValidation,
		"on_upstream_hold":   existing.OnUpstreamHold,
		"replay_safe":        existing.ReplaySafe,
		"cache_config":       existing.CacheConfig,
		// Written unconditionally (nil when the block is removed) so deleting the
		// remediation block from a job actually revokes the policy rather than
		// leaving the previous, possibly wider, one in force.
		"remediation":          existing.Remediation,
		"provenance_source_id": existing.ProvenanceSourceID,
		"provenance_repo":      existing.ProvenanceRepo,
		"provenance_ref":       existing.ProvenanceRef,
		"provenance_commit":    existing.ProvenanceCommit,
		"provenance_path":      existing.ProvenancePath,
		"deleted_at":           nil,
	}
	if err := tx.Unscoped().Model(existing).Updates(updates).Error; err != nil {
		return nil, nil, err
	}

	return existing, triggerModel, nil
}

func (i *Importer) upsertTriggerTx(tx *gorm.DB, existingJob *models.Job, alias string, trig *schema.Trigger, opts *ApplyOptions) (*models.Trigger, error) {
	cfgMap := cloneMap(trig.Configuration)
	if cfgMap == nil {
		cfgMap = make(map[string]any)
	}
	if len(trig.DefaultParams) > 0 {
		cfgMap["defaultParams"] = trig.DefaultParams
	}
	cfg, err := jsonutil.MarshalMapString(cfgMap)
	if err != nil {
		return nil, err
	}

	var triggerModel *models.Trigger
	if existingJob != nil && existingJob.TriggerID != uuid.Nil {
		var existingTrigger models.Trigger
		if err := tx.Unscoped().First(&existingTrigger, "id = ?", existingJob.TriggerID).Error; err == nil {
			triggerModel = &existingTrigger
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}

	if triggerModel == nil {
		triggerModel = &models.Trigger{ID: uuid.New()}
	}

	triggerModel.Alias = alias
	triggerModel.Type = models.TriggerType(trig.Type)
	triggerModel.Configuration = cfg
	if err := triggerModel.ApplyDerivedFields(); err != nil {
		return nil, err
	}
	applyTriggerProvenance(triggerModel, opts)

	if triggerModel.CreatedAt.IsZero() {
		if err := tx.Create(triggerModel).Error; err != nil {
			return nil, err
		}
		return triggerModel, nil
	}

	updates := map[string]any{
		"alias":                triggerModel.Alias,
		"type":                 triggerModel.Type,
		"normalized_path":      triggerModel.NormalizedPath,
		"configuration":        triggerModel.Configuration,
		"provenance_source_id": triggerModel.ProvenanceSourceID,
		"provenance_repo":      triggerModel.ProvenanceRepo,
		"provenance_ref":       triggerModel.ProvenanceRef,
		"provenance_commit":    triggerModel.ProvenanceCommit,
		"provenance_path":      triggerModel.ProvenancePath,
		"deleted_at":           nil,
	}
	if err := tx.Unscoped().Model(triggerModel).Updates(updates).Error; err != nil {
		return nil, err
	}
	return triggerModel, nil
}

func (i *Importer) reconcileTasksTx(tx *gorm.DB, jobModel *models.Job, def *schema.Definition, opts *ApplyOptions) (map[string]*models.Task, []uuid.UUID, []uuid.UUID, error) {
	steps := def.Steps
	var existingTasks []models.Task
	if err := tx.Unscoped().Where("job_id = ?", jobModel.ID).Order("position asc").Order("created_at asc").Find(&existingTasks).Error; err != nil {
		return nil, nil, nil, err
	}

	existingByName := make(map[string]*models.Task, len(existingTasks))
	for idx := range existingTasks {
		taskModel := &existingTasks[idx]
		existingByName[taskModel.Name] = taskModel
	}

	taskByName := make(map[string]*models.Task, len(steps))
	seenNames := make(map[string]struct{}, len(steps))

	for idx := range steps {
		step := &steps[idx]
		taskModel := existingByName[step.Name]

		command, err := jsonutil.MarshalSliceString(step.Command)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("step %s: %w", step.Name, err)
		}
		runtimeSpec, err := def.RuntimeSpecForStep(step)
		if err != nil {
			return nil, nil, nil, err
		}
		specJSON, err := json.Marshal(runtimeSpec)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("step %s: %w", step.Name, err)
		}
		effectiveReplaySafe := def.EffectiveReplaySafeForStep(step)

		var atomModel *models.Atom
		if taskModel != nil && taskModel.AtomID != uuid.Nil {
			var existingAtom models.Atom
			if err := tx.Unscoped().First(&existingAtom, "id = ?", taskModel.AtomID).Error; err == nil {
				atomModel = &existingAtom
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil, nil, err
			}
		}
		if atomModel == nil {
			atomModel = &models.Atom{ID: uuid.New()}
		}
		atomModel.Engine = models.AtomEngine(step.Engine)
		if atomModel.Engine == "" {
			atomModel.Engine = models.AtomEngine(schema.EngineDocker)
		}
		atomModel.Image = step.Image
		atomModel.Command = command
		atomModel.Spec = datatypes.JSON(specJSON)
		atomModel.ReplaySafe = effectiveReplaySafe
		applyAtomProvenance(atomModel, step.Name, opts)

		if atomModel.CreatedAt.IsZero() {
			if err := tx.Create(atomModel).Error; err != nil {
				return nil, nil, nil, err
			}
		} else {
			atomUpdates := map[string]any{
				"engine":               atomModel.Engine,
				"image":                atomModel.Image,
				"command":              atomModel.Command,
				"spec":                 atomModel.Spec,
				"replay_safe":          atomModel.ReplaySafe,
				"provenance_source_id": atomModel.ProvenanceSourceID,
				"provenance_repo":      atomModel.ProvenanceRepo,
				"provenance_ref":       atomModel.ProvenanceRef,
				"provenance_commit":    atomModel.ProvenanceCommit,
				"provenance_path":      atomModel.ProvenancePath,
				"deleted_at":           nil,
			}
			if err := tx.Unscoped().Model(atomModel).Updates(atomUpdates).Error; err != nil {
				return nil, nil, nil, err
			}
		}

		if taskModel == nil {
			taskModel = &models.Task{ID: uuid.New(), JobID: jobModel.ID}
		}
		if err := populateTaskFromStep(taskModel, atomModel.ID, step, effectiveReplaySafe); err != nil {
			return nil, nil, nil, err
		}
		taskModel.Position = idx

		if taskModel.UpdatedAt.IsZero() {
			if err := tx.Create(taskModel).Error; err != nil {
				return nil, nil, nil, err
			}
		} else {
			taskUpdates := map[string]any{
				"atom_id":             taskModel.AtomID,
				"name":                taskModel.Name,
				"type":                taskModel.Type,
				"node_selector":       taskModel.NodeSelector,
				"retries":             taskModel.Retries,
				"retry_delay":         taskModel.RetryDelay,
				"retry_backoff":       taskModel.RetryBackoff,
				"trigger_rule":        taskModel.TriggerRule,
				"replay_safe":         taskModel.ReplaySafe,
				"rate_limit_resource": taskModel.RateLimitResource,
				"rate_limit_units":    taskModel.RateLimitUnits,
				"fan_out_config":      taskModel.FanOutConfig,
				"cache_config":        taskModel.CacheConfig,
				"output_schema":       taskModel.OutputSchema,
				"input_schema":        taskModel.InputSchema,
				"position":            taskModel.Position,
				"deleted_at":          nil,
			}
			if err := tx.Unscoped().Model(taskModel).Updates(taskUpdates).Error; err != nil {
				return nil, nil, nil, err
			}
		}

		taskByName[step.Name] = taskModel
		seenNames[step.Name] = struct{}{}
	}

	var retiredTaskIDs []uuid.UUID
	var retiredAtomIDs []uuid.UUID
	for idx := range existingTasks {
		taskModel := &existingTasks[idx]
		if _, ok := seenNames[taskModel.Name]; ok {
			continue
		}
		if err := tx.Delete(taskModel).Error; err != nil {
			return nil, nil, nil, err
		}
		retiredTaskIDs = append(retiredTaskIDs, taskModel.ID)
		if taskModel.AtomID != uuid.Nil {
			retiredAtomIDs = append(retiredAtomIDs, taskModel.AtomID)
		}
	}

	return taskByName, retiredTaskIDs, retiredAtomIDs, nil
}

func (i *Importer) reconcileEdgesTx(tx *gorm.DB, jobModel *models.Job, steps []schema.Step, successors map[string][]string, taskByName map[string]*models.Task, opts *ApplyOptions) error {
	if err := tx.Unscoped().Where("job_id = ?", jobModel.ID).Delete(&models.TaskEdge{}).Error; err != nil {
		return err
	}

	totalEdges := 0
	for _, succs := range successors {
		totalEdges += len(succs)
	}
	edges := make([]*models.TaskEdge, 0, totalEdges)
	sequenceBase := time.Now().UTC()
	edgeIdx := 0
	stepOrder := make(map[string]int, len(steps))
	for idx, step := range steps {
		stepOrder[step.Name] = idx
	}
	fromNames := make([]string, 0, len(successors))
	for fromName := range successors {
		fromNames = append(fromNames, fromName)
	}
	sort.Slice(fromNames, func(i, j int) bool {
		left := stepOrder[fromNames[i]]
		right := stepOrder[fromNames[j]]
		if left != right {
			return left < right
		}
		return fromNames[i] < fromNames[j]
	})

	for _, fromName := range fromNames {
		succs := successors[fromName]
		fromTask := taskByName[fromName]
		if fromTask == nil {
			return fmt.Errorf("step %s missing task mapping", fromName)
		}
		for _, toName := range succs {
			toTask := taskByName[toName]
			if toTask == nil {
				return fmt.Errorf("step %s successor %s missing task mapping", fromName, toName)
			}

			edge := &models.TaskEdge{
				ID:         uuid.New(),
				JobID:      jobModel.ID,
				FromTaskID: fromTask.ID,
				ToTaskID:   toTask.ID,
				CreatedAt:  sequenceBase.Add(time.Duration(edgeIdx) * time.Microsecond),
			}
			if opts != nil && opts.Provenance != nil {
				suffix := fmt.Sprintf("edge/%s->%s", url.PathEscape(fromName), url.PathEscape(toName))
				copyProvenance(&edge.ProvenanceSourceID, &edge.ProvenanceRepo, &edge.ProvenanceRef, &edge.ProvenanceCommit, &edge.ProvenancePath, opts.Provenance, suffix)
			}
			edges = append(edges, edge)
			edgeIdx++
		}
	}

	if len(edges) == 0 {
		return nil
	}
	return tx.Create(&edges).Error
}

// dagTopologyHash computes a stable SHA-256 content hash from the DAG topology:
// sorted task names each with image+command, plus sorted from→to edge pairs.
// Folding image and command into the hash ensures that changing a step's image
// or command produces a new snapshot even when the graph structure is unchanged.
// successors must be pre-computed by the caller (avoids redundant validation).
func dagTopologyHash(steps []schema.Step, successors map[string][]string) string {
	// Build a name→step lookup for stable, sorted iteration.
	stepByName := make(map[string]*schema.Step, len(steps))
	taskNames := make([]string, 0, len(steps))
	for idx := range steps {
		s := &steps[idx]
		stepByName[s.Name] = s
		taskNames = append(taskNames, s.Name)
	}
	sort.Strings(taskNames)

	type edgePair struct{ from, to string }
	pairs := make([]edgePair, 0)
	for from, tos := range successors {
		for _, to := range tos {
			pairs = append(pairs, edgePair{from, to})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].from != pairs[j].from {
			return pairs[i].from < pairs[j].from
		}
		return pairs[i].to < pairs[j].to
	})

	h := sha256.New()
	for _, name := range taskNames {
		s := stepByName[name]
		_, _ = fmt.Fprintf(h, "task:%s image:%s command:%v\n", name, s.Image, s.Command)
	}
	for _, p := range pairs {
		_, _ = fmt.Fprintf(h, "edge:%s->%s\n", p.from, p.to)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeSnapshotTx appends a DagSnapshot row if the topology changed since the
// last recorded snapshot.  If the latest snapshot for this job already carries
// the same content hash, no row is written (dedup on unchanged topology).
// successors must be pre-computed by the caller (avoids a redundant call to
// DeriveStepSuccessors which performs full validation on every apply).
func (i *Importer) writeSnapshotTx(tx *gorm.DB, jobModel *models.Job, steps []schema.Step, successors map[string][]string, taskByName map[string]*models.Task, opts *ApplyOptions) error {
	contentHash := dagTopologyHash(steps, successors)

	// Check whether the most recent snapshot for this job already matches.
	// The idx_dag_snapshot_job_created index covers this query.
	var latest models.DagSnapshot
	err := tx.Where("job_id = ?", jobModel.ID).
		Order("created_at desc").
		Limit(1).
		First(&latest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("dag snapshot: query latest: %w", err)
	}
	if err == nil && latest.ContentHash == contentHash {
		// Topology unchanged — no new snapshot needed.
		return nil
	}

	// Build the task descriptors. Command is stored as []string to preserve
	// argument boundaries (no lossy space-joining).
	taskDescs := make([]models.DagSnapshotTask, 0, len(steps))
	for _, step := range steps {
		taskDescs = append(taskDescs, models.DagSnapshotTask{
			Name:    step.Name,
			Image:   step.Image,
			Command: step.Command,
		})
	}
	tasksJSON, err := json.Marshal(taskDescs)
	if err != nil {
		return fmt.Errorf("dag snapshot: marshal tasks: %w", err)
	}

	stepOrder := make(map[string]int, len(steps))
	for idx, step := range steps {
		stepOrder[step.Name] = idx
	}
	fromNames := make([]string, 0, len(successors))
	for fromName := range successors {
		fromNames = append(fromNames, fromName)
	}
	sort.Slice(fromNames, func(a, b int) bool {
		la, lb := stepOrder[fromNames[a]], stepOrder[fromNames[b]]
		if la != lb {
			return la < lb
		}
		return fromNames[a] < fromNames[b]
	})

	edgeDescs := make([]models.DagSnapshotEdge, 0)
	for _, fromName := range fromNames {
		for _, toName := range successors[fromName] {
			ed := models.DagSnapshotEdge{From: fromName, To: toName}
			if opts != nil && opts.Provenance != nil {
				ed.ProvenanceCommit = opts.Provenance.Commit
			}
			edgeDescs = append(edgeDescs, ed)
		}
	}
	edgesJSON, err := json.Marshal(edgeDescs)
	if err != nil {
		return fmt.Errorf("dag snapshot: marshal edges: %w", err)
	}

	gitCommit := ""
	if opts != nil && opts.Provenance != nil {
		gitCommit = opts.Provenance.Commit
	}

	snap := &models.DagSnapshot{
		ID:          uuid.New(),
		JobID:       jobModel.ID,
		ContentHash: contentHash,
		GitCommit:   gitCommit,
		Tasks:       datatypes.JSON(tasksJSON),
		Edges:       datatypes.JSON(edgesJSON),
		CreatedAt:   time.Now().UTC(),
	}
	if err := tx.Create(snap).Error; err != nil {
		return fmt.Errorf("dag snapshot: create: %w", err)
	}
	return nil
}

func (i *Importer) reconcileCallbacksTx(tx *gorm.DB, jobID uuid.UUID, callbacks []schema.Callback) error {
	var existing []models.Callback
	if err := tx.Unscoped().Where("job_id = ?", jobID).Order("position asc").Order("created_at asc").Find(&existing).Error; err != nil {
		return err
	}

	pools := make(map[string][]*models.Callback, len(existing))
	for idx := range existing {
		cb := &existing[idx]
		key := callbackSignature(cb.Type, cb.Configuration)
		pools[key] = append(pools[key], cb)
	}

	claimed := make(map[uuid.UUID]struct{}, len(existing))
	for idx := range callbacks {
		cfg, err := jsonutil.MarshalMapString(callbacks[idx].Configuration)
		if err != nil {
			return err
		}

		cbType := models.CallbackType(callbacks[idx].Type)
		key := callbackSignature(cbType, cfg)

		var callbackModel *models.Callback
		for _, candidate := range pools[key] {
			if _, ok := claimed[candidate.ID]; ok {
				continue
			}
			callbackModel = candidate
			break
		}

		if callbackModel != nil {
			claimed[callbackModel.ID] = struct{}{}
			callbackModel.Position = idx
			if err := tx.Unscoped().Model(callbackModel).Updates(map[string]any{
				"position":   callbackModel.Position,
				"deleted_at": nil,
			}).Error; err != nil {
				return err
			}
			continue
		}

		callbackModel = &models.Callback{
			ID:            uuid.New(),
			JobID:         jobID,
			Type:          cbType,
			Configuration: cfg,
			Position:      idx,
		}
		if err := tx.Create(callbackModel).Error; err != nil {
			return err
		}
	}

	for idx := range existing {
		if _, ok := claimed[existing[idx].ID]; ok {
			continue
		}
		if err := tx.Delete(&existing[idx]).Error; err != nil {
			return err
		}
	}
	return nil
}

func callbackSignature(cbType models.CallbackType, cfg string) string {
	return string(cbType) + "\x00" + cfg
}

// reconcileDatasetDeclarationsTx rebuilds this job's slice of the declared
// dataset graph from the manifest. Rebuilding (rather than diffing) means a
// declaration removed from the manifest is pruned. Declarations are scheduling
// metadata only and never touch the cache identity or the execution path.
func (i *Importer) reconcileDatasetDeclarationsTx(tx *gorm.DB, jobModel *models.Job, def *schema.Definition) error {
	decls, err := freshness.BuildDeclarations(def, jobModel.ID, jobModel.Alias)
	if err != nil {
		return fmt.Errorf("dataset declarations: %w", err)
	}
	if err := freshness.ReplaceForJobTx(tx, jobModel.ID, decls); err != nil {
		return fmt.Errorf("dataset declarations: %w", err)
	}
	return nil
}

func (i *Importer) softDeleteAtomsTx(tx *gorm.DB, atomIDs []uuid.UUID) error {
	if len(atomIDs) == 0 {
		return nil
	}
	return tx.Where("id IN ?", atomIDs).Delete(&models.Atom{}).Error
}

func (i *Importer) retireJobsTx(tx *gorm.DB, jobs []*models.Job) error {
	if len(jobs) == 0 {
		return nil
	}

	jobIDs := make([]uuid.UUID, 0, len(jobs))
	triggerIDs := make([]uuid.UUID, 0, len(jobs))
	for _, jobModel := range jobs {
		if jobModel == nil {
			continue
		}
		jobIDs = append(jobIDs, jobModel.ID)
		if jobModel.TriggerID != uuid.Nil {
			triggerIDs = append(triggerIDs, jobModel.TriggerID)
		}
	}
	if len(jobIDs) == 0 {
		return nil
	}

	if err := tx.Unscoped().Where("job_id IN ?", jobIDs).Delete(&models.TaskEdge{}).Error; err != nil {
		return err
	}

	if err := tx.Where("job_id IN ?", jobIDs).Delete(&models.Callback{}).Error; err != nil {
		return err
	}

	// dataset_declarations is a rebuilt-from-manifest projection, so a retired
	// job's declarations are hard-deleted rather than left dangling.
	if err := freshness.DeleteForJobsTx(tx, jobIDs); err != nil {
		return err
	}

	var tasks []models.Task
	if err := tx.Where("job_id IN ?", jobIDs).Find(&tasks).Error; err != nil {
		return err
	}

	atomIDs := make([]uuid.UUID, 0, len(tasks))
	for idx := range tasks {
		if tasks[idx].AtomID != uuid.Nil {
			atomIDs = append(atomIDs, tasks[idx].AtomID)
		}
	}

	if err := tx.Where("job_id IN ?", jobIDs).Delete(&models.Task{}).Error; err != nil {
		return err
	}
	if err := i.softDeleteAtomsTx(tx, atomIDs); err != nil {
		return err
	}
	if len(triggerIDs) > 0 {
		if err := tx.Delete(&models.Trigger{}, triggerIDs).Error; err != nil {
			return err
		}
	}
	return tx.Delete(&models.Job{}, jobIDs).Error
}

func populateTaskFromStep(taskModel *models.Task, atomID uuid.UUID, step *schema.Step, replaySafe bool) error {
	triggerRule := strings.TrimSpace(step.TriggerRule)
	if triggerRule == "" {
		triggerRule = schema.TriggerRuleAllSuccess
	}

	stepType := strings.TrimSpace(step.Type)
	if stepType == "" {
		stepType = schema.StepTypeTask
	}

	cacheConfig, err := marshalOptionalJSON(step.Cache)
	if err != nil {
		return fmt.Errorf("step %s: cache: %w", step.Name, err)
	}
	outputSchema, err := marshalOptionalJSON(step.OutputSchema)
	if err != nil {
		return fmt.Errorf("step %s: outputSchema: %w", step.Name, err)
	}
	inputSchema, err := marshalOptionalJSON(step.InputSchema)
	if err != nil {
		return fmt.Errorf("step %s: inputSchema: %w", step.Name, err)
	}

	taskModel.AtomID = atomID
	taskModel.Name = step.Name
	taskModel.Type = stepType
	taskModel.NodeSelector = jsonmap.FromStringMap(step.NodeSelector)
	taskModel.Retries = step.Retries
	taskModel.RetryDelay = step.RetryDelay
	taskModel.RetryBackoff = step.RetryBackoff
	taskModel.TriggerRule = triggerRule
	taskModel.ReplaySafe = replaySafe
	taskModel.RateLimitResource = ""
	taskModel.RateLimitUnits = 0
	if step.RateLimit != nil {
		taskModel.RateLimitResource = strings.TrimSpace(step.RateLimit.Resource)
		taskModel.RateLimitUnits = step.RateLimit.Units
	}
	taskModel.FanOutConfig = nil
	if step.FanOut != nil {
		encoded, err := marshalOptionalJSON(step.FanOut)
		if err != nil {
			return fmt.Errorf("step %s: fanOut: %w", step.Name, err)
		}
		taskModel.FanOutConfig = encoded
	}
	taskModel.CacheConfig = cacheConfig
	taskModel.OutputSchema = outputSchema
	taskModel.InputSchema = inputSchema
	return nil
}

func marshalOptionalJSON(value any) (datatypes.JSON, error) {
	if value == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if rv.IsNil() {
			return nil, nil
		}
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}

func marshalRateLimits(rateLimits []schema.RateLimit) (datatypes.JSON, error) {
	if len(rateLimits) == 0 {
		return nil, nil
	}
	return marshalOptionalJSON(rateLimits)
}

func applyJobProvenance(jobModel *models.Job, opts *ApplyOptions) {
	if jobModel == nil {
		return
	}
	if opts != nil && opts.Provenance != nil {
		prov := opts.Provenance
		jobModel.ProvenanceSourceID = strings.TrimSpace(prov.SourceID)
		jobModel.ProvenanceRepo = strings.TrimSpace(prov.Repo)
		jobModel.ProvenanceRef = strings.TrimSpace(prov.Ref)
		jobModel.ProvenanceCommit = strings.TrimSpace(prov.Commit)
		jobModel.ProvenancePath = strings.TrimSpace(prov.Path)
		return
	}
	jobModel.ProvenanceSourceID = ""
	jobModel.ProvenanceRepo = ""
	jobModel.ProvenanceRef = ""
	jobModel.ProvenanceCommit = ""
	jobModel.ProvenancePath = ""
}

func applyTriggerProvenance(triggerModel *models.Trigger, opts *ApplyOptions) {
	if triggerModel == nil {
		return
	}
	if opts != nil && opts.Provenance != nil {
		copyProvenance(&triggerModel.ProvenanceSourceID, &triggerModel.ProvenanceRepo, &triggerModel.ProvenanceRef, &triggerModel.ProvenanceCommit, &triggerModel.ProvenancePath, opts.Provenance, "trigger")
		return
	}
	triggerModel.ProvenanceSourceID = ""
	triggerModel.ProvenanceRepo = ""
	triggerModel.ProvenanceRef = ""
	triggerModel.ProvenanceCommit = ""
	triggerModel.ProvenancePath = ""
}

func applyAtomProvenance(atomModel *models.Atom, stepName string, opts *ApplyOptions) {
	if atomModel == nil {
		return
	}
	if opts != nil && opts.Provenance != nil {
		suffix := fmt.Sprintf("step/%s", url.PathEscape(stepName))
		copyProvenance(&atomModel.ProvenanceSourceID, &atomModel.ProvenanceRepo, &atomModel.ProvenanceRef, &atomModel.ProvenanceCommit, &atomModel.ProvenancePath, opts.Provenance, suffix)
		return
	}
	atomModel.ProvenanceSourceID = ""
	atomModel.ProvenanceRepo = ""
	atomModel.ProvenanceRef = ""
	atomModel.ProvenanceCommit = ""
	atomModel.ProvenancePath = ""
}

func copyProvenance(sourceID, repo, ref, commit, path *string, prov *Provenance, suffix string) {
	if prov == nil {
		return
	}

	*sourceID = strings.TrimSpace(prov.SourceID)
	*repo = strings.TrimSpace(prov.Repo)
	*ref = strings.TrimSpace(prov.Ref)
	*commit = strings.TrimSpace(prov.Commit)
	basePath := strings.TrimSpace(prov.Path)
	if basePath == "" {
		*path = ""
		return
	}
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		*path = basePath
		return
	}
	if strings.HasPrefix(suffix, "#") {
		*path = basePath + suffix
		return
	}
	*path = basePath + "#" + suffix
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	if len(in) == 0 {
		return nil
	}
	return maps.Clone(in)
}

// marshalSLA converts an SLAConfig pointer to JSON for storage.
// Returns nil when no SLA is configured so the column stays NULL.
func marshalSLA(sla *schema.SLAConfig) datatypes.JSON {
	if sla == nil || !sla.HasSLA() {
		return nil
	}
	data, err := json.Marshal(sla)
	if err != nil {
		return nil
	}
	return data
}
