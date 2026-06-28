package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

var _ trigger.Trigger = (*EventTrigger)(nil)

const (
	TriggerDepthParam      = "_trigger_depth"
	defaultMaxTriggerDepth = 10
)

var ErrTriggerChainDepthExceeded = errors.New("trigger chain depth exceeded")

type Config struct {
	Events        []EventPattern    `json:"events,omitempty"`
	ParamMapping  map[string]string `json:"paramMapping,omitempty"`
	DefaultParams map[string]string `json:"defaultParams,omitempty"`
}

type EventTrigger struct {
	id     uuid.UUID
	config Config

	listJobs        func(context.Context, string) (models.Jobs, error)
	runStoreFactory func() *runstorage.Store
	runJob          func(context.Context, *models.Job, map[string]string) error
	deferLaunch     bool
	maxTriggerDepth int
}

type Option func(*EventTrigger)

type FireOutcome struct {
	JobID      uuid.UUID `json:"job_id,omitempty"`
	RunID      uuid.UUID `json:"run_id,omitempty"`
	Skipped    bool      `json:"skipped,omitempty"`
	SkipReason string    `json:"skip_reason,omitempty"`
	Error      string    `json:"error,omitempty"`

	launch func()
}

func WithListJobs(fn func(context.Context, string) (models.Jobs, error)) Option {
	return func(t *EventTrigger) {
		if fn != nil {
			t.listJobs = fn
		}
	}
}

func WithRunStoreFactory(fn func() *runstorage.Store) Option {
	return func(t *EventTrigger) {
		if fn != nil {
			t.runStoreFactory = fn
		}
	}
}

func WithRunJob(fn func(context.Context, *models.Job, map[string]string) error) Option {
	return func(t *EventTrigger) {
		if fn != nil {
			t.runJob = fn
		}
	}
}

func WithMaxTriggerDepth(max int) Option {
	return func(t *EventTrigger) {
		t.maxTriggerDepth = max
	}
}

func withDeferredLaunch() Option {
	return func(t *EventTrigger) {
		t.deferLaunch = true
	}
}

func New(t *models.Trigger, opts ...Option) (*EventTrigger, error) {
	if t.Type != models.TriggerTypeEvent {
		return nil, fmt.Errorf("trigger is %v not %v", t.Type, models.TriggerTypeEvent)
	}

	cfg, err := parseConfig(t.Configuration)
	if err != nil {
		return nil, err
	}

	evt := &EventTrigger{
		id:              t.ID,
		config:          cfg,
		listJobs:        defaultListJobs,
		runStoreFactory: runstorage.Default,
		runJob:          defaultRunJob,
		maxTriggerDepth: configuredMaxTriggerDepth(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(evt)
		}
	}
	return evt, nil
}

func (t *EventTrigger) Listen(ctx context.Context) {
	log.Info("trigger listening", "id", t.id, "type", models.TriggerTypeEvent)
	<-ctx.Done()
}

func (t *EventTrigger) Fire(ctx context.Context) error {
	_, err := t.FireWithParams(ctx, nil)
	return err
}

func (t *EventTrigger) FireWithParams(ctx context.Context, params map[string]string) ([]FireOutcome, error) {
	log.Info("trigger firing", "id", t.id, "type", models.TriggerTypeEvent)

	mergedParams := t.config.mergedParams(params)
	if err := t.applyTriggerDepth(mergedParams); err != nil {
		return nil, err
	}

	jobs, err := t.listJobs(ctx, t.id.String())
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return []FireOutcome{{Skipped: true, SkipReason: "no jobs registered for trigger"}}, nil
	}

	outcomes := make([]FireOutcome, 0, len(jobs))
	for _, j := range jobs {
		jobModel := j
		if jobModel == nil {
			outcomes = append(outcomes, FireOutcome{Skipped: true, SkipReason: "nil job"})
			continue
		}
		if jobModel.Paused {
			log.Info("skipping paused job", "id", jobModel.ID)
			outcomes = append(outcomes, FireOutcome{JobID: jobModel.ID, Skipped: true, SkipReason: "job paused"})
			continue
		}

		runtimeParams := cloneParams(mergedParams)
		runRecord, err := t.runStoreFactory().Start(jobModel.ID, &t.id, runtimeParams)
		if err != nil {
			outcomes = append(outcomes, FireOutcome{JobID: jobModel.ID, Error: err.Error()})
			continue
		}

		outcome := FireOutcome{
			JobID: jobModel.ID,
			RunID: runRecord.ID,
		}
		outcome.launch = func() {
			go func(jobModel *models.Job, runID uuid.UUID, params map[string]string) {
				runCtx := runstorage.WithContext(context.WithoutCancel(ctx), runID)
				if err := t.runJob(runCtx, jobModel, params); err != nil {
					log.Error("job run failure", "id", jobModel.ID, "run_id", runID, "error", err)
				}
			}(jobModel, runRecord.ID, runtimeParams)
		}
		if !t.deferLaunch {
			outcome.launch()
			outcome.launch = nil
		}
		outcomes = append(outcomes, outcome)
	}

	return outcomes, nil
}

func (t *EventTrigger) applyTriggerDepth(params map[string]string) error {
	rawDepth, ok := params[TriggerDepthParam]
	if !ok {
		return nil
	}

	depth, err := strconv.Atoi(strings.TrimSpace(rawDepth))
	if err != nil || depth < 0 {
		metrics.TriggerChainRejectedTotal.Inc()
		return fmt.Errorf("%w: invalid %s %q", ErrTriggerChainDepthExceeded, TriggerDepthParam, rawDepth)
	}

	nextDepth := depth + 1
	maxDepth := t.maxTriggerDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxTriggerDepth
	}
	metrics.TriggerChainDepth.Observe(float64(nextDepth))
	if nextDepth > maxDepth {
		metrics.TriggerChainRejectedTotal.Inc()
		return fmt.Errorf("%w: depth %d exceeds max %d", ErrTriggerChainDepthExceeded, nextDepth, maxDepth)
	}

	params[TriggerDepthParam] = strconv.Itoa(nextDepth)
	return nil
}

func (t *EventTrigger) ID() uuid.UUID {
	return t.id
}

func (t *EventTrigger) Patterns() []EventPattern {
	out := make([]EventPattern, len(t.config.Events))
	copy(out, t.config.Events)
	return out
}

func (t *EventTrigger) Matches(evt *models.IngestedEvent) bool {
	for _, pattern := range t.config.Events {
		if pattern.Matches(evt) {
			return true
		}
	}
	return false
}

func (t *EventTrigger) ExtractEventParams(evt *models.IngestedEvent) map[string]string {
	if evt == nil {
		return map[string]string{}
	}
	return extractParams(evt.Data, t.config.ParamMapping)
}

func (t *EventTrigger) cloneWithOptions(opts ...Option) *EventTrigger {
	clone := *t
	for _, opt := range opts {
		if opt != nil {
			opt(&clone)
		}
	}
	return &clone
}

var (
	defaultListJobs = func(ctx context.Context, triggerID string) (models.Jobs, error) {
		req := &jsvc.ListRequest{TriggerID: triggerID}
		return jsvc.Service(ctx).List(req)
	}

	defaultRunJob = func(ctx context.Context, j *models.Job, params map[string]string) error {
		if j == nil {
			return fmt.Errorf("job is nil")
		}
		return job.New(j, job.WithParams(params)).Run(ctx)
	}
)

func configuredMaxTriggerDepth() int {
	maxDepth := env.Variables().MaxTriggerDepth
	if maxDepth <= 0 {
		return defaultMaxTriggerDepth
	}
	return maxDepth
}

func parseConfig(raw string) (Config, error) {
	cfg := Config{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cfg.withDefaults(), nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse trigger configuration: %w", err)
	}
	return cfg.withDefaults(), nil
}

func (c Config) withDefaults() Config {
	if c.Events == nil {
		c.Events = []EventPattern{}
	}
	if c.ParamMapping == nil {
		c.ParamMapping = map[string]string{}
	}
	if c.DefaultParams == nil {
		c.DefaultParams = map[string]string{}
	}
	return c
}

func (c Config) mergedParams(params map[string]string) map[string]string {
	merged := make(map[string]string, len(c.DefaultParams)+len(params))
	for k, v := range c.DefaultParams {
		merged[k] = v
	}
	for k, v := range params {
		merged[k] = v
	}
	return merged
}

func extractParams(data []byte, mapping map[string]string) map[string]string {
	if len(mapping) == 0 {
		return map[string]string{}
	}

	var payload any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return map[string]string{}
	}

	params := make(map[string]string, len(mapping))
	for name, jsonPath := range mapping {
		value, ok := resolveJSONPath(payload, jsonPath)
		if !ok {
			continue
		}
		params[name] = value
	}
	return params
}

func resolveJSONPath(payload any, jsonPath string) (string, bool) {
	if strings.TrimSpace(jsonPath) == "$" {
		return stringifyJSONValue(payload)
	}

	segments := parseJSONPath(jsonPath)
	if len(segments) == 0 {
		return "", false
	}

	current := payload
	for _, segment := range segments {
		next, ok := descendJSONPath(current, segment)
		if !ok {
			return "", false
		}
		current = next
	}

	return stringifyJSONValue(current)
}

func parseJSONPath(jsonPath string) []string {
	jsonPath = strings.TrimSpace(jsonPath)
	if jsonPath == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(jsonPath, "$."):
		jsonPath = jsonPath[2:]
	case jsonPath == "$":
		return []string{}
	case strings.HasPrefix(jsonPath, "$"):
		jsonPath = strings.TrimPrefix(jsonPath, "$")
		jsonPath = strings.TrimPrefix(jsonPath, ".")
	}
	if jsonPath == "" {
		return nil
	}

	raw := strings.Split(jsonPath, ".")
	segments := make([]string, 0, len(raw))
	for _, segment := range raw {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return nil
		}
		parsed, ok := parseJSONPathSegment(segment)
		if !ok {
			return nil
		}
		segments = append(segments, parsed...)
	}
	return segments
}

func parseJSONPathSegment(segment string) ([]string, bool) {
	if segment == "" {
		return nil, false
	}
	parts := make([]string, 0, 2)
	for len(segment) > 0 {
		open := strings.IndexByte(segment, '[')
		if open < 0 {
			parts = append(parts, segment)
			break
		}
		if open > 0 {
			parts = append(parts, segment[:open])
		}
		close := strings.IndexByte(segment[open:], ']')
		if close <= 1 {
			return nil, false
		}
		index := segment[open+1 : open+close]
		if _, err := strconv.Atoi(index); err != nil {
			return nil, false
		}
		parts = append(parts, index)
		segment = segment[open+close+1:]
	}
	return parts, len(parts) > 0
}

func descendJSONPath(current any, segment string) (any, bool) {
	switch value := current.(type) {
	case map[string]any:
		next, ok := value[segment]
		return next, ok
	case []any:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(value) {
			return nil, false
		}
		return value[index], true
	default:
		return nil, false
	}
}

func cloneParams(params map[string]string) map[string]string {
	if len(params) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	return out
}

func fireOutcomeErrors(outcomes []FireOutcome) string {
	var errs []string
	for _, outcome := range outcomes {
		if outcome.Error != "" {
			if outcome.JobID != uuid.Nil {
				errs = append(errs, fmt.Sprintf("%s: %s", outcome.JobID, outcome.Error))
				continue
			}
			errs = append(errs, outcome.Error)
		}
	}
	return strings.Join(errs, "; ")
}

func fireOutcomeSkipReason(outcomes []FireOutcome) string {
	var reasons []string
	for _, outcome := range outcomes {
		if outcome.Skipped && outcome.SkipReason != "" {
			if outcome.JobID != uuid.Nil {
				reasons = append(reasons, fmt.Sprintf("%s: %s", outcome.JobID, outcome.SkipReason))
				continue
			}
			reasons = append(reasons, outcome.SkipReason)
		}
	}
	return strings.Join(reasons, "; ")
}

func fireOutcomeRunIDs(outcomes []FireOutcome) []uuid.UUID {
	runIDs := make([]uuid.UUID, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.RunID != uuid.Nil {
			runIDs = append(runIDs, outcome.RunID)
		}
	}
	return runIDs
}

func fireOutcomesSkipped(outcomes []FireOutcome) bool {
	if len(outcomes) == 0 {
		return true
	}
	for _, outcome := range outcomes {
		if !outcome.Skipped {
			return false
		}
	}
	return true
}
