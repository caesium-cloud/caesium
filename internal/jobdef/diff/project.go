package diff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// secretRefPrefix matches internal/jobdef/secret and internal/cache. A value
// with this prefix is a reference, not the secret, and may be shown.
const secretRefPrefix = "secret://"

// EnvValue is one environment entry in a diff. A secret:// reference is kept.
// Every other value is a sha256 digest of the raw bytes so a change is visible
// without the literal, which may be a credential, landing in diff output,
// approval payloads, or CI logs.
type EnvValue struct {
	Secret   string `json:"secret,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// RuntimeSpec is the atom spec apply stores, with env redacted. Volumes are the
// resolved mounts, not the authoring sources apply drops.
type RuntimeSpec struct {
	Env        map[string]EnvValue       `json:"env,omitempty"`
	WorkDir    string                    `json:"workdir,omitempty"`
	Mounts     []container.Mount         `json:"mounts,omitempty"`
	Volumes    []container.VolumeMount   `json:"volumes,omitempty"`
	Kubernetes *container.KubernetesSpec `json:"kubernetes,omitempty"`
}

// RateLimitSpec is the step rate-limit columns. The zero value is an absent limit.
type RateLimitSpec struct {
	Resource string `json:"resource,omitempty"`
	Units    int    `json:"units,omitempty"`
}

// EdgeSpec is one DAG edge. Edges compare as a set of step names, sorted by
// from then to, so edge-row order is not a second copy of step position.
type EdgeSpec struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DeclarationSpec is one dataset_declarations row apply writes, without the
// identity columns (id, job id, denormalized alias, namespace, timestamps).
type DeclarationSpec struct {
	StepName      string `json:"stepName,omitempty"`
	Name          string `json:"name"`
	Direction     string `json:"direction"`
	Freshness     string `json:"freshness,omitempty"`
	MaxStaleness  string `json:"maxStaleness,omitempty"`
	ExpectedEvery string `json:"expectedEvery,omitempty"`
	WatermarkKey  string `json:"watermarkKey,omitempty"`
	Schema        any    `json:"schema,omitempty"`
	SchemaFrom    string `json:"schemaFrom,omitempty"`
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	Assertions    any    `json:"assertions,omitempty"`
	OnViolation   string `json:"onViolation,omitempty"`
	Release       string `json:"release,omitempty"`
	SkipWhenFresh bool   `json:"skipWhenFresh"`
	External      bool   `json:"external,omitempty"`
	Arrival       any    `json:"arrival,omitempty"`
}

func projectDefinition(def *schema.Definition) (JobSpec, error) {
	if def == nil {
		return JobSpec{}, nil
	}
	cfg, err := triggerConfiguration(def.Trigger)
	if err != nil {
		return JobSpec{}, err
	}
	cache, err := canonicalAny(def.Metadata.Cache)
	if err != nil {
		return JobSpec{}, fmt.Errorf("metadata.cache: %w", err)
	}
	callbacks, err := projectCallbacks(def.Callbacks)
	if err != nil {
		return JobSpec{}, err
	}
	prepared := prepareSteps(def.Steps)
	steps, err := projectPreparedSteps(def, prepared)
	if err != nil {
		return JobSpec{}, err
	}
	edges, err := projectEdges(prepared)
	if err != nil {
		return JobSpec{}, err
	}
	decls, err := projectDefinitionDeclarations(def)
	if err != nil {
		return JobSpec{}, err
	}
	var sla *schema.SLAConfig
	if def.Metadata.SLA.HasSLA() {
		sla = def.Metadata.SLA
	}
	var limits []schema.RateLimit
	if len(def.Metadata.RateLimits) > 0 {
		limits = slices.Clone(def.Metadata.RateLimits)
	}
	return JobSpec{
		Alias:            def.Metadata.Alias,
		Namespace:        models.NamespaceOrDefault(def.Metadata.Namespace),
		Labels:           cloneMap(def.Metadata.Labels),
		Annotations:      cloneMap(def.Metadata.Annotations),
		Remediation:      def.Metadata.Remediation,
		OnUpstreamHold:   def.Metadata.OnUpstreamHold,
		MaxParallelTasks: def.Metadata.MaxParallelTasks,
		TaskTimeout:      def.Metadata.TaskTimeout,
		RunTimeout:       def.Metadata.RunTimeout,
		Priority:         def.Metadata.Priority,
		Concurrency:      def.Metadata.Concurrency,
		RateLimits:       limits,
		SLA:              sla,
		SchemaValidation: def.Metadata.SchemaValidation,
		ReplaySafe:       def.Metadata.ReplaySafe,
		Cache:            cache,
		Trigger: TriggerSpec{
			Type:          def.Trigger.Type,
			Configuration: cfg,
		},
		Callbacks:    callbacks,
		Steps:        steps,
		Edges:        edges,
		Declarations: decls,
	}, nil
}

// prepareSteps clones steps and fills the defaults apply stores: type task,
// engine docker, triggerRule all_success. The caller's definition is unchanged.
// DeriveStepSuccessors rejects an empty type, which YAML and JSON parsing
// already fill and which a Go-constructed step used in tests does not.
func prepareSteps(steps []schema.Step) []schema.Step {
	if len(steps) == 0 {
		return nil
	}
	out := slices.Clone(steps)
	for i := range out {
		out[i].Type = normalizeStepType(out[i].Type)
		out[i].Engine = normalizeEngine(out[i].Engine)
		out[i].TriggerRule = normalizeTriggerRule(out[i].TriggerRule)
	}
	return out
}

func normalizeStepType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return schema.StepTypeTask
	}
	return value
}

func normalizeTriggerRule(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return schema.TriggerRuleAllSuccess
	}
	return value
}

// normalizeEngine matches the importer: only an empty engine becomes docker.
func normalizeEngine(value string) string {
	if value == "" {
		return schema.EngineDocker
	}
	return value
}

func projectPreparedSteps(def *schema.Definition, steps []schema.Step) ([]StepSpec, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	out := make([]StepSpec, 0, len(steps))
	for i := range steps {
		step := &steps[i]
		runtime, err := def.RuntimeSpecForStep(step)
		if err != nil {
			return nil, fmt.Errorf("step %s: %w", step.Name, err)
		}
		projected, err := projectRuntime(runtime)
		if err != nil {
			return nil, fmt.Errorf("step %s: %w", step.Name, err)
		}
		cache, err := canonicalAny(step.Cache)
		if err != nil {
			return nil, fmt.Errorf("step %s cache: %w", step.Name, err)
		}
		output, err := canonicalAny(step.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("step %s outputSchema: %w", step.Name, err)
		}
		input, err := canonicalAny(step.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("step %s inputSchema: %w", step.Name, err)
		}
		out = append(out, StepSpec{
			Name:         step.Name,
			Type:         step.Type,
			Engine:       step.Engine,
			Image:        step.Image,
			Command:      slices.Clone(step.Command),
			NodeSelector: cloneMap(step.NodeSelector),
			Retries:      step.Retries,
			RetryDelay:   step.RetryDelay,
			RetryBackoff: step.RetryBackoff,
			TriggerRule:  step.TriggerRule,
			ReplaySafe:   def.EffectiveReplaySafeForStep(step),
			RateLimit:    rateLimitSpec(step.RateLimit),
			FanOut:       normalizeFanOut(step.FanOut),
			Cache:        cache,
			OutputSchema: output,
			InputSchema:  input,
			Runtime:      projected,
			Produces:     producedDatasetSpecs(step.Datasets),
		})
	}
	return out, nil
}

func rateLimitSpec(limit *schema.StepRateLimit) RateLimitSpec {
	if limit == nil {
		return RateLimitSpec{}
	}
	return RateLimitSpec{
		Resource: strings.TrimSpace(limit.Resource),
		Units:    limit.Units,
	}
}

func normalizeFanOut(in *schema.FanOut) *schema.FanOut {
	if in == nil {
		return nil
	}
	out := *in
	out.From = strings.TrimSpace(out.From)
	out.Env = strings.TrimSpace(out.Env)
	if out.Env == "" {
		out.Env = schema.DefaultFanOutEnv
	}
	out.OnEmpty = strings.TrimSpace(out.OnEmpty)
	if out.OnEmpty == "" {
		out.OnEmpty = schema.FanOutOnEmptySkip
	}
	out.FailurePolicy = strings.TrimSpace(out.FailurePolicy)
	if out.FailurePolicy == "" {
		out.FailurePolicy = schema.FanOutFailureFailFast
	}
	return &out
}

func projectEdges(steps []schema.Step) ([]EdgeSpec, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	// DeriveStepSuccessors trims Next and DependsOn on the slice it is given.
	successors, err := schema.DeriveStepSuccessors(steps)
	if err != nil {
		return nil, err
	}
	return normalizeEdges(successors), nil
}

func normalizeEdges(successors map[string][]string) []EdgeSpec {
	if len(successors) == 0 {
		return nil
	}
	seen := make(map[EdgeSpec]struct{})
	edges := make([]EdgeSpec, 0)
	for from, tos := range successors {
		for _, to := range tos {
			edge := EdgeSpec{From: from, To: to}
			if _, ok := seen[edge]; ok {
				continue
			}
			seen[edge] = struct{}{}
			edges = append(edges, edge)
		}
	}
	if len(edges) == 0 {
		return nil
	}
	slices.SortFunc(edges, func(a, b EdgeSpec) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		return strings.Compare(a.To, b.To)
	})
	return edges
}

func projectDefinitionDeclarations(def *schema.Definition) ([]DeclarationSpec, error) {
	rows, err := freshness.BuildDeclarations(def, uuid.Nil, def.Metadata.Alias)
	if err != nil {
		return nil, err
	}
	return projectDeclarationRows(rows)
}

func projectDeclarationRows(rows []models.DatasetDeclaration) ([]DeclarationSpec, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]DeclarationSpec, 0, len(rows))
	for i := range rows {
		spec, err := projectDeclaration(rows[i])
		if err != nil {
			return nil, fmt.Errorf("dataset declaration %s: %w", rows[i].Name, err)
		}
		out = append(out, spec)
	}
	slices.SortFunc(out, func(a, b DeclarationSpec) int {
		if c := strings.Compare(a.Direction, b.Direction); c != 0 {
			return c
		}
		if c := strings.Compare(a.StepName, b.StepName); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func projectDeclaration(row models.DatasetDeclaration) (DeclarationSpec, error) {
	schemaValue, err := decodeOptionalAny([]byte(row.SchemaJSON))
	if err != nil {
		return DeclarationSpec{}, fmt.Errorf("schema: %w", err)
	}
	assertions, err := decodeOptionalAny([]byte(row.AssertionsJSON))
	if err != nil {
		return DeclarationSpec{}, fmt.Errorf("assertions: %w", err)
	}
	arrival, err := decodeOptionalAny(row.ArrivalBinding)
	if err != nil {
		return DeclarationSpec{}, fmt.Errorf("arrival: %w", err)
	}
	skip := true
	if row.SkipWhenFresh != nil {
		skip = *row.SkipWhenFresh
	}
	return DeclarationSpec{
		StepName:      row.StepName,
		Name:          row.Name,
		Direction:     row.Direction,
		Freshness:     row.Freshness,
		MaxStaleness:  row.MaxStaleness,
		ExpectedEvery: row.ExpectedEvery,
		WatermarkKey:  row.WatermarkKey,
		Schema:        schemaValue,
		SchemaFrom:    row.SchemaFrom,
		SchemaVersion: row.SchemaVersion,
		Assertions:    assertions,
		OnViolation:   row.OnViolation,
		Release:       row.Release,
		SkipWhenFresh: skip,
		External:      row.External,
		Arrival:       arrival,
	}, nil
}

func projectCallbacks(cbs []schema.Callback) ([]CallbackSpec, error) {
	if len(cbs) == 0 {
		return nil, nil
	}
	out := make([]CallbackSpec, 0, len(cbs))
	for i := range cbs {
		cfg, err := canonicalConfigMap(cbs[i].Configuration)
		if err != nil {
			return nil, fmt.Errorf("callbacks[%d]: %w", i, err)
		}
		out = append(out, CallbackSpec{Type: cbs[i].Type, Configuration: cfg})
	}
	return out, nil
}

// triggerConfiguration folds defaultParams the way apply does, then JSON-rounds
// the map so YAML numbers and a map[string]string param block match the
// map[string]any stored on the trigger row.
func triggerConfiguration(trig schema.Trigger) (map[string]any, error) {
	cfg := make(map[string]any, len(trig.Configuration)+1)
	for key, value := range trig.Configuration {
		cfg[key] = value
	}
	if len(trig.DefaultParams) > 0 {
		params := make(map[string]string, len(trig.DefaultParams))
		for key, value := range trig.DefaultParams {
			params[key] = value
		}
		cfg["defaultParams"] = params
	}
	return canonicalConfigMap(cfg)
}

func canonicalConfigMap(in map[string]any) (map[string]any, error) {
	if in == nil {
		in = map[string]any{}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func canonicalAny(value any) (any, error) {
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
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func projectRuntime(spec container.Spec) (RuntimeSpec, error) {
	// Round-trip before redaction so YAML ints in volumeSource maps become the
	// float64 values json.Unmarshal produces from the stored atom spec. The
	// intermediate bytes are not retained on the JobSpec.
	raw, err := json.Marshal(spec)
	if err != nil {
		return RuntimeSpec{}, err
	}
	var decoded container.Spec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return RuntimeSpec{}, err
	}
	mounts := decoded.Mounts
	if len(mounts) == 0 {
		mounts = nil
	}
	volumes := decoded.ResolvedVolumeMounts
	if len(volumes) == 0 {
		volumes = nil
	}
	return RuntimeSpec{
		Env:        redactEnv(decoded.Env),
		WorkDir:    decoded.WorkDir,
		Mounts:     mounts,
		Volumes:    volumes,
		Kubernetes: normalizeKubernetes(decoded.Kubernetes),
	}, nil
}

func normalizeKubernetes(spec *container.KubernetesSpec) *container.KubernetesSpec {
	if spec == nil {
		return nil
	}
	if spec.ServiceAccountName == "" && len(spec.PodAnnotations) == 0 && spec.AutomountServiceAccountToken == nil && spec.QueueName == "" {
		return nil
	}
	if len(spec.PodAnnotations) == 0 {
		spec.PodAnnotations = nil
	}
	return spec
}

func redactEnv(env map[string]string) map[string]EnvValue {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]EnvValue, len(env))
	for key, value := range env {
		if strings.HasPrefix(value, secretRefPrefix) {
			out[key] = EnvValue{Secret: value}
			continue
		}
		sum := sha256.Sum256([]byte(value))
		out[key] = EnvValue{
			Digest:   "sha256:" + hex.EncodeToString(sum[:]),
			Redacted: true,
		}
	}
	return out
}

func applyStoredJobColumns(spec *JobSpec, job *models.Job) error {
	cache, err := decodeOptionalAny(job.CacheConfig)
	if err != nil {
		return fmt.Errorf("job %s cache: %w", job.ID, err)
	}
	concurrency, err := decodeOptional[schema.Concurrency](job.Concurrency)
	if err != nil {
		return fmt.Errorf("job %s concurrency: %w", job.ID, err)
	}
	limits, err := decodeRateLimits(job.RateLimits)
	if err != nil {
		return fmt.Errorf("job %s rate_limits: %w", job.ID, err)
	}
	sla, err := decodeOptional[schema.SLAConfig](job.SLA)
	if err != nil {
		return fmt.Errorf("job %s sla: %w", job.ID, err)
	}
	if sla != nil && !sla.HasSLA() {
		sla = nil
	}
	spec.MaxParallelTasks = job.MaxParallelTasks
	spec.TaskTimeout = job.TaskTimeout
	spec.RunTimeout = job.RunTimeout
	spec.Priority = job.Priority
	spec.Concurrency = concurrency
	spec.RateLimits = limits
	spec.SLA = sla
	spec.SchemaValidation = job.SchemaValidation
	spec.ReplaySafe = job.ReplaySafe
	spec.Cache = cache
	return nil
}

func stepFromStored(task *models.Task, atom *models.Atom, produces []ProducedDatasetSpec) (StepSpec, error) {
	command, err := decodeCommand(atom.Command)
	if err != nil {
		return StepSpec{}, fmt.Errorf("atom %s command: %w", atom.ID, err)
	}
	runtime, err := runtimeFromJSON(atom.Spec)
	if err != nil {
		return StepSpec{}, fmt.Errorf("atom %s spec: %w", atom.ID, err)
	}
	output, err := decodeOptionalAny(task.OutputSchema)
	if err != nil {
		return StepSpec{}, fmt.Errorf("task %s output_schema: %w", task.ID, err)
	}
	input, err := decodeOptionalAny(task.InputSchema)
	if err != nil {
		return StepSpec{}, fmt.Errorf("task %s input_schema: %w", task.ID, err)
	}
	cache, err := decodeOptionalAny(task.CacheConfig)
	if err != nil {
		return StepSpec{}, fmt.Errorf("task %s cache: %w", task.ID, err)
	}
	fan, err := decodeFanOut(task.FanOutConfig)
	if err != nil {
		return StepSpec{}, fmt.Errorf("task %s fan_out: %w", task.ID, err)
	}
	return StepSpec{
		Name:         task.Name,
		Type:         normalizeStepType(task.Type),
		Engine:       normalizeEngine(string(atom.Engine)),
		Image:        atom.Image,
		Command:      command,
		NodeSelector: jsonMapToStringMap(task.NodeSelector),
		Retries:      task.Retries,
		RetryDelay:   task.RetryDelay,
		RetryBackoff: task.RetryBackoff,
		TriggerRule:  normalizeTriggerRule(task.TriggerRule),
		ReplaySafe:   task.ReplaySafe,
		RateLimit: RateLimitSpec{
			Resource: strings.TrimSpace(task.RateLimitResource),
			Units:    task.RateLimitUnits,
		},
		FanOut:       fan,
		Cache:        cache,
		OutputSchema: output,
		InputSchema:  input,
		Runtime:      runtime,
		Produces:     produces,
	}, nil
}

func loadEdges(ctx context.Context, db *gorm.DB, jobID uuid.UUID, names map[uuid.UUID]string) ([]EdgeSpec, error) {
	var rows []models.TaskEdge
	if err := db.WithContext(ctx).Where("job_id = ?", jobID).Find(&rows).Error; err != nil {
		return nil, err
	}
	successors := make(map[string][]string)
	for i := range rows {
		from, ok := names[rows[i].FromTaskID]
		if !ok {
			return nil, fmt.Errorf("task edge %s: from task %s not found", rows[i].ID, rows[i].FromTaskID)
		}
		to, ok := names[rows[i].ToTaskID]
		if !ok {
			return nil, fmt.Errorf("task edge %s: to task %s not found", rows[i].ID, rows[i].ToTaskID)
		}
		successors[from] = append(successors[from], to)
	}
	return normalizeEdges(successors), nil
}

func loadDeclarations(ctx context.Context, db *gorm.DB, jobID uuid.UUID) ([]DeclarationSpec, error) {
	var rows []models.DatasetDeclaration
	if err := db.WithContext(ctx).Where("job_id = ?", jobID).Find(&rows).Error; err != nil {
		return nil, err
	}
	return projectDeclarationRows(rows)
}

func runtimeFromJSON(raw []byte) (RuntimeSpec, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return RuntimeSpec{}, nil
	}
	var spec container.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return RuntimeSpec{}, err
	}
	return projectRuntime(spec)
}

func decodeCommand(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var command []string
	if err := json.Unmarshal([]byte(trimmed), &command); err != nil {
		return nil, err
	}
	return command, nil
}

func decodeFanOut(raw []byte) (*schema.FanOut, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var fan schema.FanOut
	if err := json.Unmarshal(raw, &fan); err != nil {
		return nil, err
	}
	return normalizeFanOut(&fan), nil
}

func decodeOptionalAny(raw []byte) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeOptional[T any](raw []byte) (*T, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func decodeRateLimits(raw []byte) ([]schema.RateLimit, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var out []schema.RateLimit
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
