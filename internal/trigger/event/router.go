package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	eventstore "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/eventmatch"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type RouteResult struct {
	EventID         uuid.UUID            `json:"event_id"`
	EventType       string               `json:"event_type"`
	Source          string               `json:"source,omitempty"`
	MatchedTriggers []TriggerRouteResult `json:"matched_triggers"`

	// Delivered are the jobs the event was offered to (whatever their start
	// outcome) and Withheld the matching triggers' jobs the delivery scope
	// excluded (identity-and-access A6). Neither is serialised: callers decide
	// what of them a caller may see.
	Delivered []RoutedJob `json:"-"`
	Withheld  []RoutedJob `json:"-"`
}

// RoutedJob is one job a routed event reached or was withheld from.
type RoutedJob struct {
	JobID     uuid.UUID
	Namespace string
}

// RouteOption configures one Route call.
type RouteOption func(*routeConfig)

type routeConfig struct {
	scope *jobdef.TenancyScope
}

// WithDeliveryScope confines a Route call to the jobs inside scope
// (identity-and-access A6): the namespaces where the caller holds runner and,
// for a job-scoped key, its exact aliases. A matching trigger's jobs outside
// the scope are withheld; a trigger whose every job is withheld is left out of
// the result and records no match, while a trigger with no jobs at all keeps
// today's match and skip reason. Admission re-checks the namespace inside its
// transaction. A nil scope routes exactly as before.
func WithDeliveryScope(scope *jobdef.TenancyScope) RouteOption {
	return func(cfg *routeConfig) {
		cfg.scope = scope
	}
}

// DeliveryScopeOf reports the delivery scope opts carry (nil when none), so a
// caller's tests can assert what it asked Route to confine.
func DeliveryScopeOf(opts ...RouteOption) *jobdef.TenancyScope {
	var cfg routeConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg.scope
}

// afterDeliveryList is a test seam run inside the routing transaction after a
// trigger's jobs were listed and partitioned, before their runs are admitted.
var afterDeliveryList func(tx *gorm.DB)

// ErrDeliveryOutOfScope refuses the admission of a run whose job left the
// delivery scope between routing and admission.
var ErrDeliveryOutOfScope = errors.New("event delivery: the job is outside the delivery scope")

func deliveryScopeGuard(scope *jobdef.TenancyScope) func(namespace string) error {
	if scope == nil || scope.Namespaces == nil {
		return nil
	}
	return func(namespace string) error {
		if !slices.Contains(scope.Namespaces, models.NamespaceOrDefault(strings.TrimSpace(namespace))) {
			return fmt.Errorf("%w: namespace %q", ErrDeliveryOutOfScope, namespace)
		}
		return nil
	}
}

type TriggerRouteResult struct {
	TriggerID   uuid.UUID   `json:"trigger_id"`
	RunsStarted []uuid.UUID `json:"runs_started,omitempty"`
	Skipped     bool        `json:"skipped,omitempty"`
	SkipReason  string      `json:"skip_reason,omitempty"`
	Error       string      `json:"error,omitempty"`
}

type Router struct {
	db            *gorm.DB
	triggerLister func(context.Context) (models.Triggers, error)
	triggerOpts   []Option
	adoptRun      func(uuid.UUID)

	mu       sync.RWMutex
	reloadMu sync.Mutex
	triggers []*EventTrigger
}

type RouterOption func(*Router)

var (
	defaultRouter     *Router
	defaultRouterOnce sync.Once
)

var lifecycleEventTypes = []eventstore.Type{
	eventstore.TypeRunCompleted,
	eventstore.TypeRunFailed,
	eventstore.TypeRunTerminal,
}

func NewRouter(conn *gorm.DB, opts ...RouterOption) *Router {
	if conn == nil {
		panic("event trigger router requires database connection")
	}
	r := &Router{
		db:       conn,
		adoptRun: func(runID uuid.UUID) { runstorage.Default().AdoptStartedRun(runID) },
	}
	r.triggerLister = r.defaultTriggerLister
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

func (r *Router) defaultTriggerLister(ctx context.Context) (models.Triggers, error) {
	r.mu.RLock()
	conn := r.db
	r.mu.RUnlock()
	if conn == nil {
		return nil, errors.New("event trigger router has no database")
	}
	return triggersvc.ServiceWithDatabase(ctx, conn).ListByEventPattern("", "")
}

func WithTriggerLister(fn func(context.Context) (models.Triggers, error)) RouterOption {
	return func(r *Router) {
		if fn != nil {
			r.triggerLister = fn
		}
	}
}

func WithEventTriggerOptions(opts ...Option) RouterOption {
	return func(r *Router) {
		r.triggerOpts = append(r.triggerOpts, opts...)
	}
}

func withStartedRunAdopter(fn func(uuid.UUID)) RouterOption {
	return func(r *Router) {
		if fn != nil {
			r.adoptRun = fn
		}
	}
}

func DefaultRouter() *Router {
	defaultRouterOnce.Do(func() {
		defaultRouter = NewRouter(db.Connection())
	})
	return defaultRouter
}

func ConfigureDefaultRouter(conn *gorm.DB) *Router {
	r := DefaultRouter()
	r.mu.Lock()
	r.db = conn
	r.mu.Unlock()
	return r
}

func (r *Router) Reload(ctx context.Context) error {
	if r == nil {
		return errors.New("event trigger router is nil")
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	if r.triggerLister == nil {
		return errors.New("event trigger router has no trigger lister")
	}

	triggerModels, err := r.triggerLister(ctx)
	if err != nil {
		return err
	}

	triggers := make([]*EventTrigger, 0, len(triggerModels))
	for _, triggerModel := range triggerModels {
		if triggerModel == nil {
			continue
		}
		eventTrigger, err := New(triggerModel, r.triggerOpts...)
		if err != nil {
			return fmt.Errorf("load event trigger %s: %w", triggerModel.ID, err)
		}
		triggers = append(triggers, eventTrigger)
	}

	r.mu.Lock()
	r.triggers = triggers
	r.mu.Unlock()
	log.Info("event trigger router reloaded", "count", len(triggers))
	return nil
}

func (r *Router) Route(ctx context.Context, evt *models.IngestedEvent, opts ...RouteOption) (*RouteResult, error) {
	if r == nil {
		return nil, errors.New("event trigger router is nil")
	}
	if evt == nil {
		return nil, errors.New("event trigger router requires event")
	}
	var cfg routeConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	var startOpts []Option
	if guard := deliveryScopeGuard(cfg.scope); guard != nil {
		startOpts = append(startOpts, withStartOptions(runstorage.WithStartNamespaceGuard(guard)))
	}

	matches := r.matchingTriggers(evt)
	result := &RouteResult{
		EventType:       evt.Type,
		Source:          evt.Source,
		MatchedTriggers: make([]TriggerRouteResult, 0, len(matches)),
	}

	var launches []func()
	var metricMatches []TriggerRouteResult

	r.mu.RLock()
	conn := r.db
	r.mu.RUnlock()
	if conn == nil {
		return nil, errors.New("event trigger router has no database")
	}

	err := conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txStore := eventstore.NewIngestStore(tx)
		if err := txStore.CreateTx(tx, evt); err != nil {
			return err
		}
		result.EventID = evt.ID
		result.EventType = evt.Type
		result.Source = evt.Source

		matchRows := make([]*models.EventTriggerMatch, 0, len(matches))
		for _, trigger := range matches {
			params := trigger.ExtractEventParams(evt)
			params = withLifecycleTriggerDepth(evt, params)
			// The trigger's jobs are listed and partitioned by the delivery
			// scope BEFORE the trigger fires, so a trigger whose every job is
			// outside the scope leaves no trace even when firing would stop
			// early (a rejected trigger depth). A listing error surfaces from
			// the trigger exactly where its own listing would have.
			var delivered, withheld []RoutedJob
			jobs, listErr := jsvc.ServiceWithDatabase(ctx, tx).List(&jsvc.ListRequest{TriggerID: trigger.ID().String()})
			kept := make(models.Jobs, 0, len(jobs))
			for _, j := range jobs {
				if j == nil {
					kept = append(kept, j)
					continue
				}
				routed := RoutedJob{JobID: j.ID, Namespace: models.NamespaceOrDefault(j.Namespace)}
				if !cfg.scope.Allows(j.Alias, j.Namespace) {
					withheld = append(withheld, routed)
					continue
				}
				delivered = append(delivered, routed)
				kept = append(kept, j)
			}
			result.Withheld = append(result.Withheld, withheld...)
			if listErr == nil && len(withheld) > 0 && len(delivered) == 0 {
				// Every job this trigger serves is outside the caller's
				// scope: leave no trace of it (no result, no match row).
				continue
			}
			triggerOpts := append([]Option{
				WithListJobs(func(context.Context, string) (models.Jobs, error) {
					if listErr != nil {
						return nil, listErr
					}
					// Offered only once the trigger got as far as its jobs.
					result.Delivered = append(result.Delivered, delivered...)
					if afterDeliveryList != nil {
						afterDeliveryList(tx)
					}
					return kept, nil
				}),
				WithRunStoreFactory(func() *runstorage.Store {
					return runstorage.NewStore(tx)
				}),
				withDeferredLaunch(),
			}, startOpts...)
			txTrigger := trigger.cloneWithOptions(triggerOpts...)

			outcomes, fireErr := txTrigger.FireWithParams(eventstore.WithDeferredBusDispatch(ctx), params)
			for _, outcome := range outcomes {
				if outcome.launch != nil {
					launches = append(launches, outcome.launch)
				}
			}

			triggerResult := triggerRouteResult(trigger.ID(), outcomes, fireErr)
			result.MatchedTriggers = append(result.MatchedTriggers, triggerResult)
			metricMatches = append(metricMatches, triggerResult)
			matchRows = append(matchRows, eventTriggerMatchRow(evt.ID, triggerResult))
		}

		return txStore.RecordMatchesTx(tx, matchRows)
	})
	if err != nil {
		return nil, err
	}

	r.adoptStartedRuns(result)
	for _, launch := range launches {
		if launch != nil {
			launch()
		}
	}
	for _, match := range metricMatches {
		metrics.EventTriggerMatchesTotal.WithLabelValues(match.TriggerID.String()).Inc()
	}

	return result, nil
}

func (r *Router) StartLifecycleBridge(ctx context.Context, bus eventstore.Bus) error {
	events, err := r.SubscribeLifecycleBridge(ctx, bus)
	if err != nil {
		return err
	}
	return r.RunLifecycleBridge(ctx, events)
}

func (r *Router) SubscribeLifecycleBridge(ctx context.Context, bus eventstore.Bus) (<-chan eventstore.Event, error) {
	if r == nil {
		return nil, errors.New("event trigger router is nil")
	}
	if bus == nil {
		return nil, errors.New("event trigger lifecycle bridge requires bus")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	return bus.Subscribe(ctx, eventstore.Filter{Types: lifecycleEventTypes})
}

func (r *Router) RunLifecycleBridge(ctx context.Context, events <-chan eventstore.Event) error {
	if r == nil {
		return errors.New("event trigger router is nil")
	}
	if events == nil {
		return errors.New("event trigger lifecycle bridge requires event subscription")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			r.routeLifecycleEvent(ctx, evt)
		}
	}
}

func (r *Router) routeLifecycleEvent(ctx context.Context, evt eventstore.Event) {
	ingested, err := r.lifecycleIngestedEvent(ctx, evt)
	if err != nil {
		log.Error("event trigger lifecycle bridge conversion failed", "type", evt.Type, "run_id", evt.RunID, "job_id", evt.JobID, "error", err)
		return
	}
	var opts []RouteOption
	if policy.ProcessHolder().FileBacked() {
		// While a policy file is loaded, a run's lifecycle event (which
		// carries its job's alias, status and parameters) is delivered only
		// to event triggers' jobs in the run's own persisted namespace
		// (identity-and-access A6). With no policy file the fan-out is
		// exactly as before.
		namespace, err := r.lookupRunNamespace(ctx, evt.RunID)
		if err != nil {
			log.Error("event trigger lifecycle bridge: cannot resolve the run's namespace; not delivering", "type", evt.Type, "run_id", evt.RunID, "job_id", evt.JobID, "error", err)
			return
		}
		opts = append(opts, WithDeliveryScope(jobdef.NewTenancyScope([]string{namespace}, nil)))
	}
	if _, err := r.Route(ctx, ingested, opts...); err != nil {
		log.Error("event trigger lifecycle bridge routing failed", "type", evt.Type, "run_id", evt.RunID, "job_id", evt.JobID, "error", err)
	}
}

func (r *Router) lifecycleIngestedEvent(ctx context.Context, evt eventstore.Event) (*models.IngestedEvent, error) {
	data, err := lifecycleEventData(evt)
	if err != nil {
		return nil, err
	}

	jobAlias := stringValue(data["job_alias"])
	if jobAlias == "" && evt.JobID != uuid.Nil {
		jobAlias, err = r.lookupJobAlias(ctx, evt.JobID)
		if err != nil {
			return nil, err
		}
	}
	if jobAlias != "" {
		data["job_alias"] = jobAlias
	}

	if depth := lifecycleTriggerDepth(data); depth != "" {
		data[TriggerDepthParam] = depth
	}

	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &models.IngestedEvent{
		Type:   string(evt.Type),
		Source: "caesium",
		Data:   datatypes.JSON(raw),
	}, nil
}

// lookupRunNamespace returns the persisted namespace of a run. A missing run
// (or none named) is an error: the bridge then delivers nothing.
func (r *Router) lookupRunNamespace(ctx context.Context, runID uuid.UUID) (string, error) {
	if runID == uuid.Nil {
		return "", errors.New("lifecycle event names no run")
	}
	r.mu.RLock()
	conn := r.db
	r.mu.RUnlock()
	if conn == nil {
		return "", errors.New("event trigger router has no database")
	}
	var run models.JobRun
	if err := conn.WithContext(ctx).Select("namespace").First(&run, "id = ?", runID).Error; err != nil {
		return "", err
	}
	return models.NamespaceOrDefault(run.Namespace), nil
}

func (r *Router) lookupJobAlias(ctx context.Context, jobID uuid.UUID) (string, error) {
	r.mu.RLock()
	conn := r.db
	r.mu.RUnlock()
	if conn == nil {
		return "", errors.New("event trigger router has no database")
	}

	var jobModel models.Job
	err := conn.WithContext(ctx).Select("alias").First(&jobModel, "id = ?", jobID).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", nil
	case err != nil:
		return "", err
	default:
		return jobModel.Alias, nil
	}
}

func lifecycleEventData(evt eventstore.Event) (map[string]any, error) {
	data := make(map[string]any)
	if len(evt.Payload) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(evt.Payload))
		decoder.UseNumber()
		var payload any
		if err := decoder.Decode(&payload); err != nil {
			return nil, fmt.Errorf("decode lifecycle payload: %w", err)
		}
		if payloadMap, ok := payload.(map[string]any); ok {
			data = payloadMap
		} else {
			data["payload"] = payload
		}
	}

	data["event_type"] = string(evt.Type)
	if evt.Sequence != 0 {
		data["sequence"] = evt.Sequence
	}
	if evt.JobID != uuid.Nil {
		data["job_id"] = evt.JobID.String()
	}
	if evt.RunID != uuid.Nil {
		data["run_id"] = evt.RunID.String()
	}
	if evt.TaskID != uuid.Nil {
		data["task_id"] = evt.TaskID.String()
	}
	if !evt.Timestamp.IsZero() {
		data["timestamp"] = evt.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	return data, nil
}

func withLifecycleTriggerDepth(evt *models.IngestedEvent, params map[string]string) map[string]string {
	if !isCaesiumLifecycleEvent(evt) {
		return params
	}
	if params == nil {
		params = map[string]string{}
	}
	depth := "0"
	if extracted := lifecycleTriggerDepthJSON(evt.Data); extracted != "" {
		depth = extracted
	}
	params[TriggerDepthParam] = depth
	return params
}

func isCaesiumLifecycleEvent(evt *models.IngestedEvent) bool {
	if evt == nil || evt.Source != "caesium" {
		return false
	}
	for _, typ := range lifecycleEventTypes {
		if evt.Type == string(typ) {
			return true
		}
	}
	return false
}

func lifecycleTriggerDepth(data map[string]any) string {
	raw, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return lifecycleTriggerDepthJSON(raw)
}

func lifecycleTriggerDepthJSON(data []byte) string {
	if depth, ok := eventmatch.ExtractField(data, TriggerDepthParam); ok {
		return depth
	}
	if depth, ok := eventmatch.ExtractField(data, "params."+TriggerDepthParam); ok {
		return depth
	}
	return ""
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func (r *Router) adoptStartedRuns(result *RouteResult) {
	if result == nil {
		return
	}
	for _, match := range result.MatchedTriggers {
		for _, runID := range match.RunsStarted {
			if r.adoptRun != nil {
				r.adoptRun(runID)
			}
		}
	}
}

func (r *Router) matchingTriggers(evt *models.IngestedEvent) []*EventTrigger {
	r.mu.RLock()
	defer r.mu.RUnlock()

	matches := make([]*EventTrigger, 0, len(r.triggers))
	for _, trigger := range r.triggers {
		if trigger != nil && trigger.Matches(evt) {
			matches = append(matches, trigger)
		}
	}
	return matches
}

func triggerRouteResult(triggerID uuid.UUID, outcomes []FireOutcome, fireErr error) TriggerRouteResult {
	result := TriggerRouteResult{
		TriggerID:   triggerID,
		RunsStarted: fireOutcomeRunIDs(outcomes),
		Skipped:     fireOutcomesSkipped(outcomes),
		SkipReason:  fireOutcomeSkipReason(outcomes),
		Error:       fireOutcomeErrors(outcomes),
	}
	if fireErr != nil {
		result.Error = fireErr.Error()
		result.Skipped = false
	}
	if len(result.RunsStarted) > 0 {
		result.Skipped = false
	}
	return result
}

func eventTriggerMatchRow(eventID uuid.UUID, result TriggerRouteResult) *models.EventTriggerMatch {
	return &models.EventTriggerMatch{
		EventID:     eventID,
		TriggerID:   result.TriggerID,
		RunsStarted: encodeRunIDs(result.RunsStarted),
		Skipped:     result.Skipped,
		SkipReason:  result.SkipReason,
		Error:       result.Error,
	}
}

func encodeRunIDs(runIDs []uuid.UUID) datatypes.JSON {
	if len(runIDs) == 0 {
		return nil
	}
	values := make([]string, 0, len(runIDs))
	for _, runID := range runIDs {
		if runID != uuid.Nil {
			values = append(values, runID.String())
		}
	}
	if len(values) == 0 {
		return nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return datatypes.JSON(data)
}
