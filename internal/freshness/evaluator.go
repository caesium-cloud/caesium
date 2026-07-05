package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type RunAdmitter interface {
	AdmitRun(uuid.UUID, *uuid.UUID, ...runstorage.StartOption) (*runstorage.JobRun, bool, error)
}

type Config struct {
	DB                      *gorm.DB
	Bus                     event.Bus
	RunStore                RunAdmitter
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
	runStore              RunAdmitter
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
	for _, rt := range e.reactiveTypes {
		if t == rt {
			return true
		}
	}
	return false
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
	return e.db.WithContext(ctx).Clauses(clause.OnConflict{
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
	triggerID, ok, err := e.freshnessTriggerIDForJob(ctx, decl.JobID)
	if err != nil {
		return err
	}
	if !ok {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "job trigger is not freshness; waiting for scheduler", consumed, nil)
	}
	return e.derive(ctx, decl, triggerID, reason, consumed, triggerDepth, budget)
}

func (e *Evaluator) freshnessTriggerIDForJob(ctx context.Context, jobID uuid.UUID) (*uuid.UUID, bool, error) {
	var row struct {
		TriggerID   uuid.UUID
		TriggerType models.TriggerType
	}
	err := e.db.WithContext(ctx).Table("jobs").
		Select("jobs.trigger_id AS trigger_id, triggers.type AS trigger_type").
		Joins("JOIN triggers ON triggers.id = jobs.trigger_id").
		Where("jobs.id = ? AND jobs.deleted_at IS NULL", jobID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if row.TriggerType != models.TriggerTypeFreshness || row.TriggerID == uuid.Nil {
		return nil, false, nil
	}
	id := row.TriggerID
	return &id, true, nil
}

func (e *Evaluator) derive(ctx context.Context, decl models.DatasetDeclaration, triggerID *uuid.UUID, reason string, consumed map[string]string, triggerDepth int, budget *int) error {
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
	params := map[string]string{
		freshnessTriggerDepthParam:       strconv.Itoa(nextDepth),
		freshnessLogicalDateParam:        now.Format(time.RFC3339),
		freshnessDerivedFromDatasetParam: datasetParamName(decl.Namespace, decl.Name),
		freshnessConsumedWatermarksParam: string(consumedJSON),
	}

	active, err := e.hasActiveOrQueuedRun(ctx, decl.JobID, params)
	if err != nil {
		return err
	}
	if active {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedActiveRun, "producer already has an active or queued run for the consumed watermarks", consumed, nil)
	}

	runRecord, handled, err := e.runStore.AdmitRun(decl.JobID, triggerID, runstorage.WithStartParams(params))
	if err != nil {
		if errors.Is(err, runstorage.ErrRunSkipped) || errors.Is(err, runstorage.ErrRunQueued) || errors.Is(err, runstorage.ErrMaxConcurrentRunsReached) {
			return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, err.Error(), consumed, nil)
		}
		return err
	}
	if !handled || runRecord == nil {
		return e.recordDerivation(ctx, decl, models.DatasetDecisionSkippedAdmission, "admission declined", consumed, nil)
	}

	if budget != nil {
		*budget--
	}
	metrics.TriggerChainDepth.Observe(float64(nextDepth))
	return e.recordDerivation(ctx, decl, models.DatasetDecisionDerived, reason, consumed, &runRecord.ID)
}

func (e *Evaluator) hasActiveOrQueuedRun(ctx context.Context, jobID uuid.UUID, params map[string]string) (bool, error) {
	var running []struct {
		Params datatypes.JSON
	}
	if err := e.db.WithContext(ctx).Table("job_runs").
		Select("params").
		Where("job_id = ? AND status = ? AND quarantine IS NOT TRUE", jobID, string(runstorage.StatusRunning)).
		Find(&running).Error; err != nil {
		return false, err
	}
	for _, row := range running {
		if sameDerivationParams(decodeParamsJSON(row.Params), params) {
			return true, nil
		}
	}

	var queued []struct {
		Params datatypes.JSON
	}
	if err := e.db.WithContext(ctx).Table("run_queue").
		Select("params").
		Where("job_id = ?", jobID).
		Find(&queued).Error; err != nil {
		return false, err
	}
	for _, row := range queued {
		if sameDerivationParams(decodeParamsJSON(row.Params), params) {
			return true, nil
		}
	}
	return false, nil
}

func sameDerivationParams(a, b map[string]string) bool {
	return a[freshnessDerivedFromDatasetParam] == b[freshnessDerivedFromDatasetParam] &&
		a[freshnessConsumedWatermarksParam] == b[freshnessConsumedWatermarksParam]
}

func (e *Evaluator) recordDerivation(ctx context.Context, decl models.DatasetDeclaration, decision, reason string, consumed map[string]string, runID *uuid.UUID) error {
	if strings.TrimSpace(decision) == "" {
		return nil
	}
	metrics.DatasetDerivationsTotal.WithLabelValues(datasetParamName(decl.Namespace, decl.Name), decision).Inc()
	return e.db.WithContext(ctx).Create(&models.DatasetDerivation{
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
