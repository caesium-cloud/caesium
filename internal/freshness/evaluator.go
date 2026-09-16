package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultEvaluatorInterval          = time.Minute
	defaultMaxDerivationsPerTick      = 50
	defaultFreshnessTriggerDepthLimit = 10
	freshnessTriggerDepthParam        = "_trigger_depth"
	freshnessDerivedFromDatasetParam  = "_derived_from_dataset"
	freshnessConsumedWatermarksParam  = "_consumed_watermarks"
	freshnessLogicalDateParam         = "logical_date"
	unknownFreshnessReason            = "waiting for first observation"
)

type LeaderCheck func(context.Context) (bool, error)

// RunStarter creates a derived run. It is deliberately the SAME seam every
// other trigger uses — internal/run.Store.StartWithContext → startRun → admit —
// so a freshness derivation passes the job's declared concurrency policy, the
// data circuit breaker's upstream-hold gate and every other admission rule, and
// so a derived run is created exactly the way a cron, HTTP, event or manual run
// is.
//
// Store.AdmitRun is deliberately NOT used here. Its policy-only mode means "do
// nothing unless a concurrency policy decides", which exists so internal/job can
// fall back to reusing an already-running run; for a job with no
// metadata.concurrency it creates nothing and returns (nil, false, nil), which
// the evaluator could only record as `skipped_admission: admission declined` —
// issue #501's first reproduction.
type RunStarter interface {
	StartWithContext(context.Context, uuid.UUID, *uuid.UUID, ...runstorage.StartOption) (*runstorage.JobRun, error)
}

// RunLauncher executes an admitted derived run's DAG: it is handed the run the
// evaluator just created and must build and dispatch its tasks.
//
// It is injected rather than called directly because this package cannot import
// internal/job: internal/job → internal/jobdef/git → internal/jobdef →
// internal/freshness is a real import cycle. cmd/start wires the production
// launcher, the same shape as runqueue.Config.LaunchRun. Without it a derived
// run would be admitted and then never executed — a `running` row with zero
// task rows, issue #501's second reproduction — so derive() refuses to create a
// run at all when no launcher is configured.
type RunLauncher func(ctx context.Context, run *runstorage.JobRun)

type Config struct {
	DB                      *gorm.DB
	Bus                     event.Bus
	RunStore                RunStarter
	LaunchRun               RunLauncher
	Interval                time.Duration
	MaxDerivationsPerTick   int
	MaxTriggerDepth         int
	LeaderCheck             LeaderCheck
	Now                     func() time.Time
	Namespace               *string
	ReactiveSubscriberTypes []event.Type
}

type Evaluator struct {
	db                    *gorm.DB
	bus                   event.Bus
	store                 *Store
	registry              *Registry
	runStore              RunStarter
	launchRun             RunLauncher
	interval              time.Duration
	maxDerivationsPerTick int
	maxTriggerDepth       int
	leaderCheck           LeaderCheck
	now                   func() time.Time
	namespace             *string
	reactiveTypes         []event.Type
}

func NewEvaluator(cfg Config) *Evaluator {
	if cfg.DB == nil {
		panic("freshness evaluator requires database connection")
	}
	runStore := cfg.RunStore
	if runStore == nil {
		runStore = runstorage.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultEvaluatorInterval
	}
	maxDerivations := cfg.MaxDerivationsPerTick
	if maxDerivations <= 0 {
		maxDerivations = defaultMaxDerivationsPerTick
	}
	maxDepth := cfg.MaxTriggerDepth
	if maxDepth <= 0 {
		maxDepth = env.Variables().MaxTriggerDepth
	}
	if maxDepth <= 0 {
		maxDepth = defaultFreshnessTriggerDepthLimit
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	reactiveTypes := cfg.ReactiveSubscriberTypes
	if len(reactiveTypes) == 0 {
		// React to dataset_advanced (published post-Advance by the capturer and
		// arrival observer), NOT run_completed: the latter races the capturer's own
		// Advance, so the evaluator could read pre-advance state and derive a
		// redundant producer run. The timer loop remains the correctness backstop.
		reactiveTypes = []event.Type{event.TypeDatasetAdvanced}
	}
	return &Evaluator{
		db:                    cfg.DB,
		bus:                   cfg.Bus,
		store:                 NewStore(cfg.DB),
		registry:              NewRegistry(cfg.DB),
		runStore:              runStore,
		launchRun:             cfg.LaunchRun,
		interval:              interval,
		maxDerivationsPerTick: maxDerivations,
		maxTriggerDepth:       maxDepth,
		leaderCheck:           cfg.LeaderCheck,
		now:                   now,
		namespace:             cfg.Namespace,
		reactiveTypes:         reactiveTypes,
	}
}

func (e *Evaluator) Run(ctx context.Context) {
	events, err := e.subscribeReactive(ctx)
	if err != nil && ctx.Err() == nil {
		log.Error("freshness evaluator subscription failed", "error", err)
	}

	if err := e.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
		log.Error("freshness evaluator tick failed", "error", err)
	}

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("freshness evaluator tick failed", "error", err)
			}
		case evt, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := e.EvaluateEvent(ctx, evt); err != nil && ctx.Err() == nil {
				log.Error("freshness evaluator reactive evaluation failed", "type", evt.Type, "run_id", evt.RunID, "error", err)
			}
		}
	}
}

func (e *Evaluator) isReactiveType(t event.Type) bool {
	return slices.Contains(e.reactiveTypes, t)
}

func (e *Evaluator) subscribeReactive(ctx context.Context) (<-chan event.Event, error) {
	if e.bus == nil || len(e.reactiveTypes) == 0 {
		return nil, nil
	}
	return e.bus.Subscribe(ctx, event.Filter{Types: e.reactiveTypes})
}

func (e *Evaluator) EvaluateOnce(ctx context.Context) error {
	return e.evaluate(ctx, nil, 0)
}

func (e *Evaluator) EvaluateDatasetNames(ctx context.Context, names []string) error {
	targets := make(map[datasetIdentity]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		targets[datasetIdentity{namespace: nsValue(e.namespace), name: name}] = struct{}{}
	}
	return e.evaluate(ctx, targets, 0)
}

func (e *Evaluator) EvaluateEvent(ctx context.Context, evt event.Event) error {
	if !e.isReactiveType(evt.Type) {
		return nil
	}
	// Target the dataset that just advanced (from the event payload) plus its
	// downstream consumers, rather than everything the event's job produces. This
	// keys off the advanced dataset's identity, so arrival advances (which have no
	// producing job) drive derivation too.
	id, ok := datasetIdentityFromPayload(evt.Payload)
	if !ok {
		return nil
	}
	if e.leaderCheck != nil {
		leader, err := e.leaderCheck(ctx)
		if err != nil {
			return err
		}
		if !leader {
			return nil
		}
	}

	decls, err := e.registry.ListAll(ctx)
	if err != nil {
		return err
	}
	graph := newRegistrySnapshot(decls)

	start := []datasetIdentity{id}
	targets := map[datasetIdentity]struct{}{id: {}}
	for _, downstream := range graph.downstreamOf(start) {
		targets[downstream] = struct{}{}
	}

	depth, err := e.runTriggerDepth(ctx, evt.RunID)
	if err != nil {
		return err
	}
	return e.evaluateWithSnapshot(ctx, graph, targets, depth)
}

// datasetIdentityFromPayload extracts the {namespace, name} dataset identity a
// dataset_advanced event carries. Namespace is already in nsValue (nil→"") form.
func datasetIdentityFromPayload(payload json.RawMessage) (datasetIdentity, bool) {
	if len(payload) == 0 {
		return datasetIdentity{}, false
	}
	var p struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return datasetIdentity{}, false
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return datasetIdentity{}, false
	}
	return datasetIdentity{namespace: p.Namespace, name: name}, true
}

func (e *Evaluator) evaluate(ctx context.Context, targets map[datasetIdentity]struct{}, triggerDepth int) error {
	if e.leaderCheck != nil {
		leader, err := e.leaderCheck(ctx)
		if err != nil {
			return err
		}
		if !leader {
			return nil
		}
	}

	decls, err := e.registry.ListAll(ctx)
	if err != nil {
		return err
	}
	return e.evaluateWithSnapshot(ctx, newRegistrySnapshot(decls), targets, triggerDepth)
}

func (e *Evaluator) evaluateWithSnapshot(ctx context.Context, graph registrySnapshot, targets map[datasetIdentity]struct{}, triggerDepth int) error {
	budget := e.maxDerivationsPerTick
	for _, decl := range graph.produced {
		id := declarationIdentity(decl)
		if len(targets) > 0 {
			if _, ok := targets[id]; !ok {
				continue
			}
		}
		if strings.TrimSpace(decl.Freshness) == "" {
			continue
		}
		if err := e.evaluateProducedDataset(ctx, graph, decl, triggerDepth, &budget); err != nil {
			return err
		}
	}
	return nil
}

func (e *Evaluator) evaluateProducedDataset(ctx context.Context, graph registrySnapshot, decl models.DatasetDeclaration, triggerDepth int, budget *int) error {
	now := e.now().UTC()
	freshness, err := parsePositiveDuration(decl.Freshness)
	if err != nil {
		return fmt.Errorf("freshness evaluator: parse freshness for %s: %w", decl.Name, err)
	}
	maxStaleness, err := parseOptionalDuration(decl.MaxStaleness)
	if err != nil {
		return fmt.Errorf("freshness evaluator: parse maxStaleness for %s: %w", decl.Name, err)
	}

	state, exists, err := e.store.Get(ctx, decl.Namespace, decl.Name)
	if err != nil {
		return err
	}
	if !exists {
		state = models.DatasetState{
			Namespace: nsValue(decl.Namespace),
			Name:      decl.Name,
			Status:    models.DatasetStatusUnknown,
		}
	}

	consumes := graph.consumesByJob[decl.JobID]
	upstreamReady, consumed, upstreamReason, err := e.upstreamReady(ctx, state, consumes)
	if err != nil {
		return err
	}

	status, reason, staleness, seen := e.statusFor(decl, state, now, freshness, maxStaleness, upstreamReady, upstreamReason)
	if status != models.DatasetStatusUnknown && status != models.DatasetStatusQuarantined {
		if err := e.updateStatus(ctx, decl.Namespace, decl.Name, status, reason); err != nil {
			return err
		}
	}
	if seen {
		metrics.DatasetStalenessSeconds.WithLabelValues(datasetParamName(decl.Namespace, decl.Name)).Set(staleness.Seconds())
	}

	switch status {
	case models.DatasetStatusFresh:
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedFresh, reason, consumed, nil)
	case models.DatasetStatusStaleUpstream:
		if err := e.publishAtRiskOncePerWindow(ctx, decl, status, reason, staleness, freshness, now); err != nil {
			return err
		}
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedUpstream, reason, consumed, nil)
	case models.DatasetStatusViolated:
		e.publishFreshnessViolation(decl, status, reason, staleness, freshness, maxStaleness, now)
		if !upstreamReady {
			return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedUpstream, reason, consumed, nil)
		}
		return e.deriveIfFreshnessTriggered(ctx, decl, reason, consumed, triggerDepth, budget)
	case models.DatasetStatusStale:
		return e.deriveIfFreshnessTriggered(ctx, decl, reason, consumed, triggerDepth, budget)
	case models.DatasetStatusQuarantined:
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "dataset quarantined", consumed, nil)
	default:
		if !seen {
			return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedFresh, unknownFreshnessReason, consumed, nil)
		}
		return nil
	}
}

func (e *Evaluator) statusFor(decl models.DatasetDeclaration, state models.DatasetState, now time.Time, freshness, maxStaleness time.Duration, upstreamReady bool, upstreamReason string) (string, string, time.Duration, bool) {
	if state.Status == models.DatasetStatusQuarantined {
		return models.DatasetStatusQuarantined, "dataset quarantined", 0, false
	}

	freshAt, seen := FreshAt(state)
	if !seen {
		baseline := decl.CreatedAt
		if baseline.IsZero() {
			baseline = decl.UpdatedAt
		}
		if baseline.IsZero() {
			baseline = now
		}
		staleness := durationSince(now, baseline)
		if staleness <= freshness {
			return models.DatasetStatusUnknown, unknownFreshnessReason, staleness, false
		}
		if maxStaleness > 0 && staleness > maxStaleness {
			return models.DatasetStatusViolated, fmt.Sprintf("freshness maxStaleness breached (%s/%s)", formatDuration(staleness), maxStaleness), staleness, true
		}
		if !upstreamReady {
			if upstreamReason == "" {
				upstreamReason = "waiting on upstream dataset"
			}
			return models.DatasetStatusStaleUpstream, upstreamReason, staleness, true
		}
		return models.DatasetStatusStale, fmt.Sprintf("freshness SLO exceeded (%s/%s)", formatDuration(staleness), freshness), staleness, true
	}

	staleness := durationSince(now, freshAt)
	if staleness <= freshness {
		return models.DatasetStatusFresh, fmt.Sprintf("fresh (%s/%s)", formatDuration(staleness), freshness), staleness, true
	}
	if maxStaleness > 0 && staleness > maxStaleness {
		return models.DatasetStatusViolated, fmt.Sprintf("freshness maxStaleness breached (%s/%s)", formatDuration(staleness), maxStaleness), staleness, true
	}
	if !upstreamReady {
		if upstreamReason == "" {
			upstreamReason = "waiting on upstream dataset"
		}
		return models.DatasetStatusStaleUpstream, upstreamReason, staleness, true
	}
	return models.DatasetStatusStale, fmt.Sprintf("freshness SLO exceeded (%s/%s)", formatDuration(staleness), freshness), staleness, true
}

func (e *Evaluator) updateStatus(ctx context.Context, namespace *string, name, status, reason string) error {
	return e.updateStatusTx(ctx, e.db, namespace, name, status, reason)
}

// updateStatusTx is updateStatus bound to an explicit connection (a caller's
// transaction or e.db). It threads the *gorm.DB rather than reassigning e.db so
// a shared evaluator stays safe under concurrent ticks.
func (e *Evaluator) updateStatusTx(ctx context.Context, db *gorm.DB, namespace *string, name, status, reason string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errEmptyDatasetName
	}
	row := models.DatasetState{
		ID:        uuid.New(),
		Namespace: nsValue(namespace),
		Name:      name,
		Status:    status,
		Reason:    reason,
	}
	// One upsert carrying the real status/reason: insert the row on first
	// observation, otherwise update the evaluator-owned columns in place. dqlite
	// accepts ON CONFLICT DO UPDATE for plain column assignments.
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "namespace"}, {Name: "name"}},
		DoUpdates: clause.Assignments(map[string]any{
			"status":     status,
			"reason":     reason,
			"updated_at": e.now().UTC(),
		}),
	}).Create(&row).Error
}

func (e *Evaluator) upstreamReady(ctx context.Context, outputState models.DatasetState, consumes []models.DatasetDeclaration) (bool, map[string]string, string, error) {
	if len(consumes) == 0 {
		return true, map[string]string{}, "", nil
	}

	lastConsumed := decodeConsumedWatermarks(outputState.ConsumedWatermarks)
	current := make(map[string]string, len(consumes))
	waiting := make([]string, 0)
	for _, consume := range consumes {
		name := strings.TrimSpace(consume.Name)
		if name == "" {
			continue
		}
		// Key on (namespace,name) identity so two inputs sharing a name across
		// namespaces do not collide in the consumed-watermark snapshot. For the
		// v1 default (empty namespace) this is just the name.
		key := datasetParamName(consume.Namespace, name)
		state, ok, err := e.store.Get(ctx, consume.Namespace, name)
		if err != nil {
			return false, nil, "", err
		}
		watermark := ""
		observed := false
		if ok {
			watermark = state.Watermark
			_, observed = FreshAt(state)
		}
		current[key] = watermark
		previous, hadPrevious := lastConsumed[key]
		if !hadPrevious {
			// No prior consumption record. The upstream is ready to bootstrap a
			// first derivation if it has a watermark to consume OR has been
			// observed successfully in degraded (verify-only) mode with an empty
			// watermark. Only block when it has never been observed at all —
			// otherwise a watermarkless-but-fresh upstream would deadlock its
			// consumers at stale-upstream forever.
			if strings.TrimSpace(watermark) == "" && !observed {
				waiting = append(waiting, key)
			}
			continue
		}
		if !watermarkAdvancedPast(previous, watermark) {
			waiting = append(waiting, key)
		}
	}
	if len(waiting) > 0 {
		sort.Strings(waiting)
		return false, current, "waiting on upstream dataset " + strings.Join(waiting, ","), nil
	}
	return true, current, "", nil
}

func watermarkAdvancedPast(previous, current string) bool {
	previous = strings.TrimSpace(previous)
	current = strings.TrimSpace(current)
	if current == "" {
		return false
	}
	if previous == "" {
		return true
	}
	if current == previous {
		return false
	}
	if greater, ok := orderableGreater(previous, current); ok {
		return greater
	}
	return true
}

func (e *Evaluator) deriveIfFreshnessTriggered(ctx context.Context, decl models.DatasetDeclaration, reason string, consumed map[string]string, triggerDepth int, budget *int) error {
	trigger, err := e.freshnessTriggerForJob(ctx, decl.JobID)
	if err != nil {
		return err
	}
	if !trigger.freshnessTriggered {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "job trigger is not freshness; waiting for scheduler", consumed, nil)
	}
	// Pause is a job-level "start nothing new" switch, and derivation is the one
	// scheduler path that reaches an engine without going through a trigger
	// object: cron (internal/trigger/cron), http, event and the webhook
	// controller each check models.Job.Paused before calling job.Run, and the
	// manual-run controller answers 409. Neither the run store's admission nor
	// job.Run re-checks it, so a paused freshness job would otherwise execute on
	// every arrival. The decision stays `skipped_admission` (the closed set the
	// Console renders); the reason carries the cause.
	if trigger.paused {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "job is paused", consumed, nil)
	}
	return e.derive(ctx, decl, trigger, reason, consumed, triggerDepth, budget)
}

// freshnessTriggerState is what the evaluator needs to know about a produced
// dataset's owning job before deriving: whether the job is freshness-triggered
// at all, its trigger id, whether an operator has paused it, and the
// trigger-level default params every run of that job is entitled to.
type freshnessTriggerState struct {
	id                 *uuid.UUID
	freshnessTriggered bool
	paused             bool
	defaultParams      map[string]string
}

func (e *Evaluator) freshnessTriggerForJob(ctx context.Context, jobID uuid.UUID) (freshnessTriggerState, error) {
	var row struct {
		TriggerID     uuid.UUID
		TriggerType   models.TriggerType
		Paused        bool
		Configuration string
	}
	// Filter GORM soft-deletes on both sides of the raw join: a plain Joins does
	// not apply the deleted_at scope, so a soft-deleted trigger could otherwise
	// still match an active job's trigger_id.
	err := e.db.WithContext(ctx).Table("jobs").
		Select("jobs.trigger_id AS trigger_id, jobs.paused AS paused, triggers.type AS trigger_type, triggers.configuration AS configuration").
		Joins("JOIN triggers ON triggers.id = jobs.trigger_id AND triggers.deleted_at IS NULL").
		Where("jobs.id = ? AND jobs.deleted_at IS NULL", jobID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return freshnessTriggerState{}, nil
		}
		return freshnessTriggerState{}, err
	}
	if row.TriggerType != models.TriggerTypeFreshness || row.TriggerID == uuid.Nil {
		return freshnessTriggerState{}, nil
	}
	id := row.TriggerID
	return freshnessTriggerState{
		id:                 &id,
		freshnessTriggered: true,
		paused:             row.Paused,
		defaultParams:      triggerDefaultParams(row.Configuration),
	}, nil
}

// triggerDefaultParams reads `defaultParams` out of a persisted trigger
// configuration. A manifest may spell them either as `trigger.defaultParams` or
// as `trigger.configuration.defaultParams`; the importer folds the former into
// the latter (internal/jobdef/importer.go), so reading the configuration covers
// both. The value coercion mirrors internal/trigger/cron's extractDefaultParams
// - that helper is unexported and lives in a package which imports this one, so
// it cannot be shared without an import cycle.
//
// A malformed block yields no defaults rather than an error: the schema already
// validates the shape at apply time, and a derivation must not be blocked by a
// configuration key it does not own.
func triggerDefaultParams(configuration string) map[string]string {
	configuration = strings.TrimSpace(configuration)
	if configuration == "" {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(configuration), &cfg); err != nil {
		return nil
	}
	raw, ok := cfg["defaultParams"]
	if !ok || raw == nil {
		return nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if str, ok := value.(string); ok {
			out[key] = str
			continue
		}
		out[key] = fmt.Sprintf("%v", value)
	}
	return out
}

func (e *Evaluator) derive(ctx context.Context, decl models.DatasetDeclaration, trigger freshnessTriggerState, reason string, consumed map[string]string, triggerDepth int, budget *int) error {
	if budget != nil && *budget <= 0 {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "freshness derivation cap reached", consumed, nil)
	}

	nextDepth := triggerDepth + 1
	if e.maxTriggerDepth > 0 && triggerDepth >= e.maxTriggerDepth {
		metrics.TriggerChainRejectedTotal.Inc()
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, fmt.Sprintf("trigger depth %d exceeds max %d", nextDepth, e.maxTriggerDepth), consumed, nil)
	}

	now := e.now().UTC()
	consumedJSON := canonicalConsumedJSON(consumed)
	// The job's trigger-level defaultParams go in FIRST, so a derived run is
	// entitled to them exactly like a cron tick (internal/trigger/cron's
	// scheduledRunParams) or an HTTP fire (internal/trigger/http's config
	// merge). Without them a freshness job that declares defaults fails
	// ${CAESIUM_PARAM_X} interpolation and the whole run fails closed. The
	// evaluator-owned keys are layered ON TOP, so a default can never shadow the
	// derivation identity the dedupe and the audit trail depend on.
	//
	// This happens before admission on purpose: the merged set is what lands on
	// the job_runs row (and on run_queue, for a `queue` concurrency policy), so
	// a run promoted out of the queue later keeps them too.
	params := make(map[string]string, len(trigger.defaultParams)+4)
	maps.Copy(params, trigger.defaultParams)
	params[freshnessTriggerDepthParam] = strconv.Itoa(nextDepth)
	params[freshnessLogicalDateParam] = now.Format(time.RFC3339)
	params[freshnessDerivedFromDatasetParam] = datasetParamName(decl.Namespace, decl.Name)
	params[freshnessConsumedWatermarksParam] = string(consumedJSON)

	activeRunID, active, err := e.activeRunForWatermarks(ctx, decl.JobID, params)
	if err != nil {
		return err
	}
	if active {
		// Linked to the covering run when there is one, so "why didn't this
		// dataset derive" answers itself: because that run is already
		// refreshing it.
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedActiveRun, "producer already has an active or queued run for the consumed watermarks", consumed, activeRunID)
	}

	if e.launchRun == nil {
		// Creating the run here would admit it and then strand it: nothing else
		// in the system builds a DAG for a freshness-derived run. Refuse loudly
		// instead of leaving a `running` row with zero tasks.
		log.Error("freshness evaluator has no run launcher configured; refusing to derive a run that cannot execute",
			"job_id", decl.JobID, "dataset", datasetParamName(decl.Namespace, decl.Name))
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "freshness run launcher not configured", consumed, nil)
	}

	// ErrRunHeldUpstream wraps ErrRunSkipped, so the data circuit breaker's
	// refusal is recorded as an admission skip rather than aborting the tick.
	runRecord, err := e.runStore.StartWithContext(ctx, decl.JobID, trigger.id, runstorage.WithStartParams(params))
	if err != nil {
		if errors.Is(err, runstorage.ErrRunSkipped) || errors.Is(err, runstorage.ErrRunQueued) || errors.Is(err, runstorage.ErrMaxConcurrentRunsReached) {
			return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, err.Error(), consumed, nil)
		}
		// A start can fail AFTER it has committed the run: Store.startRun
		// publishes run_started and takes the run lease before it reads the
		// record back, and the tick context may itself be what failed (server
		// shutdown). Dropping the error here would leave a live `running` row
		// with no tasks and no engine — the strand this whole path exists to
		// prevent.
		//
		// Recover ONLY the run this attempt committed, by the exact id the store
		// reports. Searching for "a matching running run" would be unsafe across
		// a leader change: a run the new leader created and is already executing
		// matches the same (dataset, consumed-watermark) identity, and adopting
		// it would execute it a second time on this node. The lookup runs on a
		// context that cannot be the reason it fails too.
		committedID, ok := runstorage.CommittedRunID(err)
		if !ok {
			return err
		}
		recoverCtx := context.WithoutCancel(ctx)
		adopted := e.recoverCommittedRun(recoverCtx, committedID, decl.JobID, params)
		if adopted == nil {
			return err
		}
		log.Warn("freshness: run start reported an error after committing the run; driving the committed run",
			"job_id", decl.JobID, "run_id", adopted.ID,
			"dataset", datasetParamName(decl.Namespace, decl.Name), "error", err)
		ctx, runRecord = recoverCtx, adopted
	}
	if runRecord == nil {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "admission declined", consumed, nil)
	}

	if budget != nil {
		*budget--
	}
	metrics.TriggerChainDepth.Observe(float64(nextDepth))
	// Record the audit row before dispatching so the derivation names the run
	// before the run can complete and advance the dataset. The run is launched
	// either way: a failed audit write must not strand an admitted run.
	derivationErr := e.recordDerivation(ctx, decl, models.DatasetDecisionDerived, reason, consumed, &runRecord.ID)
	e.launchRun(ctx, runRecord)
	return derivationErr
}

// committedRunReadBackoffs bounds the retry of the post-failure read-back of a
// committed run. Responsibility for that run does not end with one failed
// SELECT: if it is neither launched nor finalized, hasActiveOrQueuedRun
// suppresses every later derivation for the same watermarks and a maxRuns
// policy holds its slot forever, with no task rows for a worker to recover.
var committedRunReadBackoffs = []time.Duration{
	50 * time.Millisecond,
	250 * time.Millisecond,
	time.Second,
}

// recoverCommittedRun resolves the run a failed start committed, retrying the
// read on a bounded schedule.
//
// It returns nil only when the row was READ and needs no driving — missing,
// terminal, or another job's. When the row can never be read it returns a
// minimal record built from the identity the store reported, so the run still
// reaches the launcher: the launcher loads the job, fences on the run's status
// and, when neither can be resolved, finalizes the run conditionally. Either
// way the run ends up executed or terminal, never stranded.
func (e *Evaluator) recoverCommittedRun(ctx context.Context, runID, jobID uuid.UUID, params map[string]string) *runstorage.JobRun {
	var lastErr error
retry:
	for attempt := 0; attempt <= len(committedRunReadBackoffs); attempt++ {
		adopted, err := e.runByID(ctx, runID, jobID)
		if err == nil {
			return adopted
		}
		lastErr = err
		if attempt == len(committedRunReadBackoffs) {
			break
		}
		select {
		case <-ctx.Done():
			break retry
		case <-time.After(committedRunReadBackoffs[attempt]):
		}
	}
	log.Error("freshness: a committed run could not be read back; handing its identity to the launcher so it is executed or finalized",
		"job_id", jobID, "run_id", runID, "error", lastErr)
	return &runstorage.JobRun{
		ID:     runID,
		JobID:  jobID,
		Status: runstorage.StatusRunning,
		Params: params,
	}
}

// runByID loads the exact run a failed start committed. It is addressed by id
// — never matched by shape — so this evaluator can only ever adopt the run its
// own admission attempt created, not one another node is already executing.
//
// It returns nil when the row is missing, belongs to a different job, or is no
// longer `running`: a run that reached a terminal status needs neither
// launching nor rescuing.
func (e *Evaluator) runByID(ctx context.Context, runID, jobID uuid.UUID) (*runstorage.JobRun, error) {
	var row models.JobRun
	if err := e.db.WithContext(ctx).Take(&row, "id = ?", runID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if row.JobID != jobID || row.Status != string(runstorage.StatusRunning) {
		return nil, nil
	}
	return &runstorage.JobRun{
		ID:         row.ID,
		JobID:      row.JobID,
		Status:     runstorage.Status(row.Status),
		Priority:   row.Priority,
		Params:     decodeParamsJSON(row.Params),
		Quarantine: row.Quarantine,
		StartedAt:  row.StartedAt,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}, nil
}

// activeRunForWatermarks reports an active or queued run of this job that is
// already refreshing from the SAME consumed watermarks, and its run id when it
// is a live run (a queued row has no run id yet).
//
// It coalesces on (job, consumed watermarks) — deliberately NOT on the produced
// dataset as well. A run executes the job's whole DAG, so it refreshes every
// output that job produces. Keying the decision on the dataset too meant a job
// with two stale outputs over one input watermark set started TWO full runs:
// the second evaluation saw the first run, compared _derived_from_dataset,
// decided it did not match and admitted a duplicate. Under a maxRuns policy the
// duplicate was merely refused rather than recognised as redundant work.
//
// The AUDIT stays per-dataset: the second output still gets its own
// skipped_active_run derivation row, linked to the run that is covering it.
func (e *Evaluator) activeRunForWatermarks(ctx context.Context, jobID uuid.UUID, params map[string]string) (*uuid.UUID, bool, error) {
	var running []struct {
		ID     uuid.UUID
		Params datatypes.JSON
	}
	if err := e.db.WithContext(ctx).Table("job_runs").
		Select("id", "params").
		Where("job_id = ? AND status = ? AND quarantine IS NOT TRUE", jobID, string(runstorage.StatusRunning)).
		Find(&running).Error; err != nil {
		return nil, false, err
	}
	for i := range running {
		if sameConsumedWatermarks(decodeParamsJSON(running[i].Params), params) {
			id := running[i].ID
			return &id, true, nil
		}
	}

	var queued []struct {
		Params datatypes.JSON
	}
	if err := e.db.WithContext(ctx).Table("run_queue").
		Select("params").
		Where("job_id = ?", jobID).
		Find(&queued).Error; err != nil {
		return nil, false, err
	}
	for _, row := range queued {
		if sameConsumedWatermarks(decodeParamsJSON(row.Params), params) {
			return nil, true, nil
		}
	}
	return nil, false, nil
}

// sameConsumedWatermarks compares the input view two derivations evaluated
// against. Both sides carry the canonical JSON the evaluator stamped, so this is
// a string comparison; a run with no snapshot at all (a manual or cron run of
// the same job) has an empty value and therefore never coalesces a derivation.
func sameConsumedWatermarks(a, b map[string]string) bool {
	consumed := b[freshnessConsumedWatermarksParam]
	return consumed != "" && a[freshnessConsumedWatermarksParam] == consumed
}

func (e *Evaluator) recordDerivation(ctx context.Context, decl models.DatasetDeclaration, decision, reason string, consumed map[string]string, runID *uuid.UUID) error {
	return e.recordDerivationTx(ctx, e.db, decl, decision, reason, consumed, runID)
}

// recordDerivationTx is recordDerivation bound to an explicit connection so a
// batch of skip decisions can commit atomically. It threads the *gorm.DB rather
// than reassigning e.db so a shared evaluator stays safe under concurrent ticks.
func (e *Evaluator) recordDerivationTx(ctx context.Context, db *gorm.DB, decl models.DatasetDeclaration, decision, reason string, consumed map[string]string, runID *uuid.UUID) error {
	if strings.TrimSpace(decision) == "" {
		return nil
	}
	metrics.DatasetDerivationsTotal.WithLabelValues(datasetParamName(decl.Namespace, decl.Name), decision).Inc()
	return db.WithContext(ctx).Create(&models.DatasetDerivation{
		ID:                 uuid.New(),
		Namespace:          decl.Namespace,
		Name:               decl.Name,
		Decision:           decision,
		Reason:             reason,
		ConsumedWatermarks: canonicalConsumedJSON(consumed),
		RunID:              runID,
		CreatedAt:          e.now().UTC(),
	}).Error
}

func (e *Evaluator) publishAtRiskOncePerWindow(ctx context.Context, decl models.DatasetDeclaration, status, reason string, staleness, freshness time.Duration, now time.Time) error {
	if e.bus == nil || freshness <= 0 {
		return nil
	}
	windowStart := decl.CreatedAt
	state, ok, err := e.store.Get(ctx, decl.Namespace, decl.Name)
	if err != nil {
		return err
	}
	if ok {
		if freshAt, seen := FreshAt(state); seen {
			windowStart = freshAt
		}
	}
	if windowStart.IsZero() {
		windowStart = now
	}
	if now.After(windowStart) {
		elapsed := now.Sub(windowStart)
		windows := int64(elapsed / freshness)
		windowStart = windowStart.Add(time.Duration(windows) * freshness)
	}

	var count int64
	query := e.db.WithContext(ctx).Model(&models.DatasetDerivation{}).
		Where("name = ? AND decision = ? AND created_at >= ?", decl.Name, models.DatasetDecisionSkippedUpstream, windowStart)
	if decl.Namespace == nil {
		query = query.Where("namespace IS NULL")
	} else {
		query = query.Where("namespace = ?", *decl.Namespace)
	}
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	e.publishEvent(event.TypeDatasetFreshnessAtRisk, decl, status, reason, staleness, freshness, 0, now)
	return nil
}

func (e *Evaluator) publishFreshnessViolation(decl models.DatasetDeclaration, status, reason string, staleness, freshness, maxStaleness time.Duration, now time.Time) {
	metrics.FreshnessViolationsTotal.WithLabelValues(datasetParamName(decl.Namespace, decl.Name), status).Inc()
	e.publishEvent(event.TypeFreshnessViolated, decl, status, reason, staleness, freshness, maxStaleness, now)
	e.publishEvent(event.TypeSLAMissed, decl, status, reason, staleness, freshness, maxStaleness, now)
}

func (e *Evaluator) publishEvent(t event.Type, decl models.DatasetDeclaration, status, reason string, staleness, freshness, maxStaleness time.Duration, now time.Time) {
	if e.bus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"dataset":               datasetParamName(decl.Namespace, decl.Name),
		"namespace":             nsValue(decl.Namespace),
		"name":                  decl.Name,
		"status":                status,
		"reason":                reason,
		"staleness_seconds":     staleness.Seconds(),
		"freshness_seconds":     freshness.Seconds(),
		"max_staleness_seconds": maxStaleness.Seconds(),
	})
	e.bus.Publish(event.Event{
		Type:      t,
		JobID:     decl.JobID,
		Timestamp: now,
		Payload:   payload,
	})
}

func (e *Evaluator) runTriggerDepth(ctx context.Context, runID uuid.UUID) (int, error) {
	if runID == uuid.Nil {
		return 0, nil
	}
	var row struct {
		Params datatypes.JSON
	}
	if err := e.db.WithContext(ctx).Table("job_runs").Select("params").Where("id = ?", runID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}
	params := decodeParamsJSON(row.Params)
	depth, err := strconv.Atoi(strings.TrimSpace(params[freshnessTriggerDepthParam]))
	if err != nil || depth < 0 {
		return 0, nil
	}
	return depth, nil
}

type datasetIdentity struct {
	namespace string
	name      string
}

type registrySnapshot struct {
	produced      []models.DatasetDeclaration
	consumesByJob map[uuid.UUID][]models.DatasetDeclaration
	producesByJob map[uuid.UUID][]models.DatasetDeclaration
	consumersByID map[datasetIdentity][]uuid.UUID
}

func newRegistrySnapshot(decls []models.DatasetDeclaration) registrySnapshot {
	out := registrySnapshot{
		produced:      make([]models.DatasetDeclaration, 0),
		consumesByJob: make(map[uuid.UUID][]models.DatasetDeclaration),
		producesByJob: make(map[uuid.UUID][]models.DatasetDeclaration),
		consumersByID: make(map[datasetIdentity][]uuid.UUID),
	}
	for _, decl := range decls {
		switch decl.Direction {
		case models.DatasetDirectionProduces:
			out.produced = append(out.produced, decl)
			out.producesByJob[decl.JobID] = append(out.producesByJob[decl.JobID], decl)
		case models.DatasetDirectionConsumes:
			out.consumesByJob[decl.JobID] = append(out.consumesByJob[decl.JobID], decl)
			id := declarationIdentity(decl)
			out.consumersByID[id] = append(out.consumersByID[id], decl.JobID)
		}
	}
	sort.Slice(out.produced, func(i, j int) bool {
		left, right := declarationIdentity(out.produced[i]), declarationIdentity(out.produced[j])
		if left.namespace != right.namespace {
			return left.namespace < right.namespace
		}
		if left.name != right.name {
			return left.name < right.name
		}
		return out.produced[i].JobID.String() < out.produced[j].JobID.String()
	})
	return out
}

func (g registrySnapshot) downstreamOf(start []datasetIdentity) []datasetIdentity {
	seen := make(map[datasetIdentity]struct{}, len(start))
	// Pre-seed the start nodes so a (lint-forbidden but defensively handled)
	// cyclic declared graph can never re-emit a start dataset as its own
	// downstream.
	for _, id := range start {
		seen[id] = struct{}{}
	}
	queue := append([]datasetIdentity(nil), start...)
	out := make([]datasetIdentity, 0)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, jobID := range g.consumersByID[cur] {
			for _, produced := range g.producesByJob[jobID] {
				next := declarationIdentity(produced)
				if _, ok := seen[next]; ok {
					continue
				}
				seen[next] = struct{}{}
				out = append(out, next)
				queue = append(queue, next)
			}
		}
	}
	return out
}

func declarationIdentity(decl models.DatasetDeclaration) datasetIdentity {
	return datasetIdentity{namespace: nsValue(decl.Namespace), name: strings.TrimSpace(decl.Name)}
}

func parsePositiveDuration(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
}

func parseOptionalDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return parsePositiveDuration(raw)
}

func durationSince(now, then time.Time) time.Duration {
	if then.IsZero() || now.Before(then) {
		return 0
	}
	return now.Sub(then)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

func datasetParamName(namespace *string, name string) string {
	ns := nsValue(namespace)
	if ns == "" {
		return name
	}
	return ns + "." + name
}

func decodeConsumedWatermarks(raw datatypes.JSON) map[string]string {
	return decodeParamsJSON(raw)
}

func decodeParamsJSON(raw datatypes.JSON) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]string{}
	}
	if out == nil {
		return map[string]string{}
	}
	return out
}

func canonicalConsumedJSON(consumed map[string]string) datatypes.JSON {
	if len(consumed) == 0 {
		return datatypes.JSON([]byte(`{}`))
	}
	// encoding/json already marshals map keys in sorted order, so the output is
	// canonical without an explicit sort-then-rebuild.
	blob, err := json.Marshal(consumed)
	if err != nil {
		return datatypes.JSON([]byte(`{}`))
	}
	return datatypes.JSON(blob)
}
