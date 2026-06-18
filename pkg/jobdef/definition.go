package jobdef

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	"gopkg.in/yaml.v3"
)

const (
	APIVersionV1 = "v1"
	KindJob      = "Job"

	TriggerCron = "cron"
	TriggerHTTP = "http"

	CallbackNotification = "notification"

	EngineDocker     = "docker"
	EngineKubernetes = "kubernetes"
	EnginePodman     = "podman"

	TriggerRuleAllSuccess = "all_success"
	TriggerRuleAllDone    = "all_done"
	TriggerRuleAllFailed  = "all_failed"
	TriggerRuleOneSuccess = "one_success"
	TriggerRuleAlways     = "always"

	StepTypeTask   = "task"
	StepTypeBranch = "branch"
)

var simpleJSONPathPattern = regexp.MustCompile(`^\$(?:\.[^.\s]+)*$`)

// Definition models the root job document.
type Definition struct {
	Schema     string     `yaml:"$schema,omitempty" json:"$schema,omitempty"`
	APIVersion string     `yaml:"apiVersion" json:"apiVersion"`
	Kind       string     `yaml:"kind" json:"kind"`
	Metadata   Metadata   `yaml:"metadata" json:"metadata"`
	Trigger    Trigger    `yaml:"trigger" json:"trigger"`
	Callbacks  []Callback `yaml:"callbacks,omitempty" json:"callbacks,omitempty"`
	Volumes    []Volume   `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Steps      []Step     `yaml:"steps" json:"steps"`
}

const (
	SchemaValidationDisabled = ""
	SchemaValidationWarn     = "warn"
	SchemaValidationFail     = "fail"
)

// SLAConfig defines the service-level agreement for a job.
type SLAConfig struct {
	// Duration is the maximum time a run may take before an SLA miss alert is
	// emitted, measured from the run's start time. Does not cancel execution.
	Duration time.Duration `yaml:"duration,omitempty" json:"duration,omitempty"`

	// CompletedBy is a wall-clock time of day in "HH:MM" format (UTC) by
	// which the job must have a successfully completed run. If no run has
	// completed by this time, an SLA miss alert is emitted — even if the job
	// was never triggered.
	CompletedBy string `yaml:"completedBy,omitempty" json:"completedBy,omitempty"`
}

// HasSLA returns true if any SLA constraint is configured.
func (s *SLAConfig) HasSLA() bool {
	return s != nil && (s.Duration > 0 || s.CompletedBy != "")
}

// Metadata contains descriptive data for the job.
type Metadata struct {
	Alias            string            `yaml:"alias" json:"alias"`
	Labels           map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations      map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
	MaxParallelTasks int               `yaml:"maxParallelTasks,omitempty" json:"maxParallelTasks,omitempty"`
	TaskTimeout      time.Duration     `yaml:"taskTimeout,omitempty" json:"taskTimeout,omitempty"`
	RunTimeout       time.Duration     `yaml:"runTimeout,omitempty" json:"runTimeout,omitempty"`
	// SLA defines the service-level agreement for this job. It supports two
	// modes that may be used independently or together:
	//   duration    — max run duration before an SLA miss alert (relative to
	//                 run start; does not cancel execution).
	//   completedBy — wall-clock time of day ("HH:MM", UTC) by which the job
	//                 must have a successfully completed run. Alerts even if
	//                 no run has started.
	SLA *SLAConfig `yaml:"sla,omitempty" json:"sla,omitempty"`
	// SchemaValidation controls runtime output schema validation.
	// Values: "" (disabled), "warn" (log violations), "fail" (fail task on violation).
	SchemaValidation             string            `yaml:"schemaValidation,omitempty" json:"schemaValidation,omitempty"`
	Cache                        interface{}       `yaml:"cache,omitempty" json:"cache"`
	ServiceAccountName           string            `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
	PodAnnotations               map[string]string `yaml:"podAnnotations,omitempty" json:"podAnnotations,omitempty"`
	AutomountServiceAccountToken *bool             `yaml:"automountServiceAccountToken,omitempty" json:"automountServiceAccountToken,omitempty"`
}

// Trigger defines how the job is triggered.
type Trigger struct {
	Type          string            `yaml:"type" json:"type"`
	Configuration map[string]any    `yaml:"configuration" json:"configuration"`
	DefaultParams map[string]string `yaml:"defaultParams,omitempty" json:"defaultParams,omitempty"`
}

// Callback defines a job callback (notification, etc.).
type Callback struct {
	Type          string         `yaml:"type" json:"type"`
	Configuration map[string]any `yaml:"configuration" json:"configuration"`
}

// Volume declares a named BYO storage source that steps can mount by name.
type Volume struct {
	Name       string                  `yaml:"name" json:"name"`
	Source     *VolumeSource           `yaml:"source,omitempty" json:"source,omitempty"`
	Sources    map[string]VolumeSource `yaml:"sources,omitempty" json:"sources,omitempty"`
	AccessMode string                  `yaml:"accessMode,omitempty" json:"accessMode,omitempty"`
}

// VolumeSource describes one concrete engine-specific source kind.
type VolumeSource struct {
	PVC           string         `yaml:"pvc,omitempty" json:"pvc,omitempty"`
	ClaimTemplate *ClaimTemplate `yaml:"claimTemplate,omitempty" json:"claimTemplate,omitempty"`
	VolumeSource  map[string]any `yaml:"volumeSource,omitempty" json:"volumeSource,omitempty"`
	Bind          string         `yaml:"bind,omitempty" json:"bind,omitempty"`
	Volume        string         `yaml:"volume,omitempty" json:"volume,omitempty"`
	Tmpfs         *TmpfsSource   `yaml:"tmpfs,omitempty" json:"tmpfs,omitempty"`
}

// ClaimTemplate configures a Kubernetes inline ephemeral PVC claim template.
type ClaimTemplate struct {
	StorageClass string            `yaml:"storageClass,omitempty" json:"storageClass,omitempty"`
	Size         string            `yaml:"size,omitempty" json:"size,omitempty"`
	AccessMode   string            `yaml:"accessMode,omitempty" json:"accessMode,omitempty"`
	Labels       map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Annotations  map[string]string `yaml:"annotations,omitempty" json:"annotations,omitempty"`
}

// TmpfsSource configures a Docker/Podman tmpfs source.
type TmpfsSource struct {
	SizeBytes int64 `yaml:"sizeBytes,omitempty" json:"sizeBytes,omitempty"`
	Mode      *int  `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// VolumeMount references a job-level volume from a step.
type VolumeMount struct {
	Volume   string `yaml:"volume" json:"volume"`
	Path     string `yaml:"path" json:"path"`
	ReadOnly bool   `yaml:"readOnly,omitempty" json:"readOnly,omitempty"`
	SubPath  string `yaml:"subPath,omitempty" json:"subPath,omitempty"`
}

// Step defines an execution step.
type Step struct {
	Name                         string            `yaml:"name" json:"name"`
	Type                         string            `yaml:"type,omitempty" json:"type,omitempty"`
	Engine                       string            `yaml:"engine,omitempty" json:"engine,omitempty"`
	Image                        string            `yaml:"image" json:"image"`
	Command                      []string          `yaml:"command,omitempty" json:"command,omitempty"`
	NodeSelector                 map[string]string `yaml:"nodeSelector,omitempty" json:"nodeSelector,omitempty"`
	Next                         []string          `yaml:"next,omitempty" json:"next,omitempty"`
	DependsOn                    []string          `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	Retries                      int               `yaml:"retries,omitempty" json:"retries,omitempty"`
	RetryDelay                   time.Duration     `yaml:"retryDelay,omitempty" json:"retryDelay,omitempty"`
	RetryBackoff                 bool              `yaml:"retryBackoff,omitempty" json:"retryBackoff,omitempty"`
	TriggerRule                  string            `yaml:"triggerRule,omitempty" json:"triggerRule,omitempty"`
	VolumeMounts                 []VolumeMount     `yaml:"volumeMounts,omitempty" json:"volumeMounts,omitempty"`
	ServiceAccountName           string            `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
	PodAnnotations               map[string]string `yaml:"podAnnotations,omitempty" json:"podAnnotations,omitempty"`
	AutomountServiceAccountToken *bool             `yaml:"automountServiceAccountToken,omitempty" json:"automountServiceAccountToken,omitempty"`
	// OutputSchema is a JSON Schema describing this step's expected output keys.
	OutputSchema map[string]any `yaml:"outputSchema,omitempty" json:"outputSchema,omitempty"`
	// InputSchema maps predecessor step names to JSON Schema fragments describing
	// which keys this step requires from each predecessor's output.
	InputSchema    map[string]map[string]any `yaml:"inputSchema,omitempty" json:"inputSchema,omitempty"`
	Cache          interface{}               `yaml:"cache,omitempty" json:"cache"`
	container.Spec `yaml:",inline" json:",inline"`
}

// UnmarshalYAML sets defaults while deserialising a step.
func (s *Step) UnmarshalYAML(value *yaml.Node) error {
	type rawStep struct {
		Name                         string                    `yaml:"name"`
		Type                         string                    `yaml:"type"`
		Engine                       string                    `yaml:"engine"`
		Image                        string                    `yaml:"image"`
		Command                      []string                  `yaml:"command"`
		NodeSelector                 map[string]string         `yaml:"nodeSelector"`
		Next                         interface{}               `yaml:"next"`
		DependsOn                    interface{}               `yaml:"dependsOn"`
		Retries                      int                       `yaml:"retries"`
		RetryDelay                   time.Duration             `yaml:"retryDelay"`
		RetryBackoff                 bool                      `yaml:"retryBackoff"`
		TriggerRule                  string                    `yaml:"triggerRule"`
		VolumeMounts                 []VolumeMount             `yaml:"volumeMounts"`
		ServiceAccountName           string                    `yaml:"serviceAccountName"`
		PodAnnotations               map[string]string         `yaml:"podAnnotations"`
		AutomountServiceAccountToken *bool                     `yaml:"automountServiceAccountToken"`
		OutputSchema                 map[string]any            `yaml:"outputSchema"`
		InputSchema                  map[string]map[string]any `yaml:"inputSchema"`
		Cache                        interface{}               `yaml:"cache"`
		container.Spec               `yaml:",inline"`
	}

	rs := rawStep{Engine: EngineDocker}
	if err := value.Decode(&rs); err != nil {
		return err
	}

	nextList, err := normalizeInterfaceList(rs.Next)
	if err != nil {
		return fmt.Errorf("step %s next: %w", rs.Name, err)
	}

	dependsList, err := normalizeInterfaceList(rs.DependsOn)
	if err != nil {
		return fmt.Errorf("step %s dependsOn: %w", rs.Name, err)
	}

	s.Name = rs.Name
	s.Type = rs.Type
	if s.Type == "" {
		s.Type = StepTypeTask
	}
	s.Engine = rs.Engine
	if s.Engine == "" {
		s.Engine = EngineDocker
	}
	s.Image = rs.Image
	s.Command = rs.Command
	s.NodeSelector = rs.NodeSelector
	s.Next = nextList
	s.DependsOn = dependsList
	s.Retries = rs.Retries
	s.RetryDelay = rs.RetryDelay
	s.RetryBackoff = rs.RetryBackoff
	s.TriggerRule = rs.TriggerRule
	s.VolumeMounts = rs.VolumeMounts
	s.ServiceAccountName = rs.ServiceAccountName
	s.PodAnnotations = rs.PodAnnotations
	s.AutomountServiceAccountToken = rs.AutomountServiceAccountToken
	s.OutputSchema = rs.OutputSchema
	s.InputSchema = rs.InputSchema
	s.Cache = rs.Cache
	s.Spec = rs.Spec

	return nil
}

// UnmarshalJSON mirrors the YAML defaults so REST/UI JSON apply requests behave
// the same as YAML manifests loaded from disk.
func (s *Step) UnmarshalJSON(data []byte) error {
	type rawStep struct {
		Name                         string                    `json:"name"`
		Type                         string                    `json:"type"`
		Engine                       string                    `json:"engine"`
		Image                        string                    `json:"image"`
		Command                      []string                  `json:"command"`
		NodeSelector                 map[string]string         `json:"nodeSelector"`
		Next                         []string                  `json:"next"`
		DependsOn                    []string                  `json:"dependsOn"`
		Retries                      int                       `json:"retries"`
		RetryDelay                   time.Duration             `json:"retryDelay"`
		RetryBackoff                 bool                      `json:"retryBackoff"`
		TriggerRule                  string                    `json:"triggerRule"`
		VolumeMounts                 []VolumeMount             `json:"volumeMounts"`
		ServiceAccountName           string                    `json:"serviceAccountName"`
		PodAnnotations               map[string]string         `json:"podAnnotations"`
		AutomountServiceAccountToken *bool                     `json:"automountServiceAccountToken"`
		OutputSchema                 map[string]any            `json:"outputSchema"`
		InputSchema                  map[string]map[string]any `json:"inputSchema"`
		Cache                        interface{}               `json:"cache"`
		container.Spec               `json:",inline"`
	}

	rs := rawStep{Engine: EngineDocker}
	if err := json.Unmarshal(data, &rs); err != nil {
		return err
	}

	s.Name = rs.Name
	s.Type = rs.Type
	if s.Type == "" {
		s.Type = StepTypeTask
	}
	s.Engine = rs.Engine
	if s.Engine == "" {
		s.Engine = EngineDocker
	}
	s.Image = rs.Image
	s.Command = rs.Command
	s.NodeSelector = rs.NodeSelector
	s.Next = rs.Next
	s.DependsOn = rs.DependsOn
	s.Retries = rs.Retries
	s.RetryDelay = rs.RetryDelay
	s.RetryBackoff = rs.RetryBackoff
	s.TriggerRule = rs.TriggerRule
	s.VolumeMounts = rs.VolumeMounts
	s.ServiceAccountName = rs.ServiceAccountName
	s.PodAnnotations = rs.PodAnnotations
	s.AutomountServiceAccountToken = rs.AutomountServiceAccountToken
	s.OutputSchema = rs.OutputSchema
	s.InputSchema = rs.InputSchema
	s.Cache = rs.Cache
	s.Spec = rs.Spec

	return nil
}

// Parse parses YAML bytes into a Definition.
func Parse(data []byte) (*Definition, error) {
	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return &def, nil
}

// Validate performs semantic validation on the definition.
func (d *Definition) Validate() error {
	if d.APIVersion != APIVersionV1 {
		return fmt.Errorf("unsupported apiVersion: %s", d.APIVersion)
	}
	if d.Kind != KindJob {
		return fmt.Errorf("unsupported kind: %s", d.Kind)
	}
	if strings.TrimSpace(d.Metadata.Alias) == "" {
		return fmt.Errorf("metadata.alias is required")
	}

	switch d.Metadata.SchemaValidation {
	case SchemaValidationDisabled, SchemaValidationWarn, SchemaValidationFail:
	default:
		return fmt.Errorf("metadata.schemaValidation %q must be one of [\"\",\"%s\",\"%s\"]",
			d.Metadata.SchemaValidation, SchemaValidationWarn, SchemaValidationFail)
	}

	if err := validateTrigger(&d.Trigger); err != nil {
		return err
	}
	if err := validateCallbacks(d.Callbacks); err != nil {
		return err
	}
	volumes, err := validateVolumes(d.Volumes)
	if err != nil {
		return err
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("steps must contain at least one entry")
	}
	if err := validateSteps(d.Steps, volumes); err != nil {
		return err
	}
	return nil
}

func validateTrigger(t *Trigger) error {
	switch t.Type {
	case TriggerCron, TriggerHTTP:
	default:
		return fmt.Errorf("trigger.type must be one of [%s,%s]", TriggerCron, TriggerHTTP)
	}
	if t.Configuration == nil {
		t.Configuration = map[string]any{}
	}
	if t.Type == TriggerHTTP {
		if err := validateHTTPTriggerConfiguration(t.Configuration); err != nil {
			return err
		}
	}
	return nil
}

func ValidateTriggerSpec(t *Trigger) error {
	return validateTrigger(t)
}

func validateHTTPTriggerConfiguration(cfg map[string]any) error {
	rawPath, ok := cfg["path"]
	if !ok {
		return fmt.Errorf("trigger.configuration.path is required for http triggers")
	}
	path, ok := rawPath.(string)
	if !ok {
		return fmt.Errorf("trigger.configuration.path must be a string")
	}
	if normalizeHTTPTriggerPath(path) == "" {
		return fmt.Errorf("trigger.configuration.path must not be empty")
	}

	if rawScheme, ok := cfg["signatureScheme"]; ok && rawScheme != nil {
		scheme, ok := rawScheme.(string)
		if !ok {
			return fmt.Errorf("trigger.configuration.signatureScheme must be a string")
		}
		switch strings.TrimSpace(scheme) {
		case "", "hmac-sha256", "hmac-sha1", "bearer", "basic":
		default:
			return fmt.Errorf("trigger.configuration.signatureScheme %q must be one of [hmac-sha256,hmac-sha1,bearer,basic]", scheme)
		}
	}

	if rawMapping, ok := cfg["paramMapping"]; ok && rawMapping != nil {
		mapping, ok := rawMapping.(map[string]any)
		if !ok {
			return fmt.Errorf("trigger.configuration.paramMapping must be a map of string keys and values")
		}
		for key, rawExpr := range mapping {
			expr, ok := rawExpr.(string)
			if !ok {
				return fmt.Errorf("trigger.configuration.paramMapping[%q] must be a string", key)
			}
			if err := validateSimpleJSONPath(expr); err != nil {
				return fmt.Errorf("trigger.configuration.paramMapping[%q]: %w", key, err)
			}
		}
	}

	return nil
}

func normalizeHTTPTriggerPath(path string) string {
	normalized := strings.TrimSpace(path)
	normalized = strings.TrimPrefix(normalized, "/")
	normalized = strings.TrimPrefix(normalized, "v1/")
	normalized = strings.TrimPrefix(normalized, "hooks/")
	return strings.Trim(normalized, "/")
}

func validateSimpleJSONPath(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return fmt.Errorf("must not be empty")
	}
	if !simpleJSONPathPattern.MatchString(expr) {
		return fmt.Errorf("must be '$' or a dot-separated path starting with '$.'")
	}
	return nil
}

func validateCallbacks(callbacks []Callback) error {
	for i, cb := range callbacks {
		if cb.Type != CallbackNotification {
			return fmt.Errorf("callbacks[%d].type must be %s", i, CallbackNotification)
		}
		if cb.Configuration == nil {
			return fmt.Errorf("callbacks[%d].configuration is required", i)
		}
	}
	return nil
}

func validateVolumes(volumes []Volume) (map[string]*Volume, error) {
	byName := make(map[string]*Volume, len(volumes))
	for i := range volumes {
		volume := &volumes[i]
		name := strings.TrimSpace(volume.Name)
		if name == "" {
			return nil, fmt.Errorf("volumes[%d].name is required", i)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("duplicate volume name %q", name)
		}
		volume.Name = name
		byName[name] = volume

		hasSource := volume.Source != nil
		hasSources := len(volume.Sources) > 0
		switch {
		case hasSource == hasSources:
			return nil, fmt.Errorf("volumes[%d] must set exactly one of source or sources", i)
		case hasSource:
			if err := validateVolumeSource(*volume.Source, "", fmt.Sprintf("volumes[%d].source", i)); err != nil {
				return nil, err
			}
		case hasSources:
			for rawEngine, source := range volume.Sources {
				engine := strings.TrimSpace(rawEngine)
				if engine != rawEngine {
					return nil, fmt.Errorf("volumes[%d].sources has invalid engine key %q", i, rawEngine)
				}
				if err := validateEngine(engine, fmt.Sprintf("volumes[%d].sources", i)); err != nil {
					return nil, err
				}
				if err := validateVolumeSource(source, engine, fmt.Sprintf("volumes[%d].sources[%q]", i, engine)); err != nil {
					return nil, err
				}
			}
		}
		if strings.TrimSpace(volume.AccessMode) != "" && !isKnownAccessMode(volume.AccessMode) {
			return nil, fmt.Errorf("volumes[%d].accessMode %q is not supported", i, volume.AccessMode)
		}
	}
	return byName, nil
}

func validateVolumeSource(source VolumeSource, engine, field string) error {
	kind, err := sourceKind(source)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if engine != "" && !sourceKindCompatible(kind, engine) {
		return fmt.Errorf("%s.%s is not valid for engine %q", field, kind, engine)
	}
	if source.ClaimTemplate != nil {
		accessMode := strings.TrimSpace(source.ClaimTemplate.AccessMode)
		if accessMode != "" && !isKnownAccessMode(accessMode) {
			return fmt.Errorf("%s.claimTemplate.accessMode %q is not supported", field, accessMode)
		}
		if strings.TrimSpace(source.ClaimTemplate.Size) == "" {
			return fmt.Errorf("%s.claimTemplate.size is required", field)
		}
	}
	if source.VolumeSource != nil && len(source.VolumeSource) == 0 {
		return fmt.Errorf("%s.volumeSource must not be empty", field)
	}
	return nil
}

func sourceKind(source VolumeSource) (string, error) {
	kinds := make([]string, 0, 6)
	if strings.TrimSpace(source.PVC) != "" {
		kinds = append(kinds, "pvc")
	}
	if source.ClaimTemplate != nil {
		kinds = append(kinds, "claimTemplate")
	}
	if source.VolumeSource != nil {
		kinds = append(kinds, "volumeSource")
	}
	if strings.TrimSpace(source.Bind) != "" {
		kinds = append(kinds, "bind")
	}
	if strings.TrimSpace(source.Volume) != "" {
		kinds = append(kinds, "volume")
	}
	if source.Tmpfs != nil {
		kinds = append(kinds, "tmpfs")
	}
	if len(kinds) != 1 {
		return "", fmt.Errorf("must set exactly one source kind, got %d", len(kinds))
	}
	return kinds[0], nil
}

func sourceKindCompatible(kind, engine string) bool {
	switch engine {
	case EngineKubernetes:
		return kind == "pvc" || kind == "claimTemplate" || kind == "volumeSource"
	case EngineDocker, EnginePodman:
		return kind == "bind" || kind == "volume" || kind == "tmpfs"
	default:
		return false
	}
}

func validateEngine(engine, field string) error {
	switch engine {
	case EngineDocker, EngineKubernetes, EnginePodman:
		return nil
	default:
		return fmt.Errorf("%s has unknown engine %q", field, engine)
	}
}

func isKnownAccessMode(value string) bool {
	switch strings.TrimSpace(value) {
	case "ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany", "ReadWriteOncePod":
		return true
	default:
		return false
	}
}

func validateSteps(steps []Step, volumes map[string]*Volume) error {
	names, adj, err := computeStepAdjacency(steps)
	if err != nil {
		return err
	}
	if err := validateStepVolumeMounts(steps, volumes); err != nil {
		return err
	}
	if err := detectCycles(adj, names); err != nil {
		return err
	}
	// Build predecessors map (reverse of adj) for schema validation.
	predecessors := make(map[string]map[string]struct{}, len(names))
	for from, targets := range adj {
		for to := range targets {
			if predecessors[to] == nil {
				predecessors[to] = make(map[string]struct{})
			}
			predecessors[to][from] = struct{}{}
		}
	}
	if err := validateSchemas(steps, names, predecessors); err != nil {
		return err
	}
	return nil
}

func validateStepVolumeMounts(steps []Step, volumes map[string]*Volume) error {
	for i := range steps {
		step := &steps[i]
		paths := make(map[string]struct{}, len(step.Mounts)+len(step.VolumeMounts))
		for _, mount := range step.Mounts {
			target := strings.TrimSpace(mount.Target)
			if target == "" {
				continue
			}
			if !path.IsAbs(target) {
				return fmt.Errorf("steps[%d].mounts target %q must be absolute", i, target)
			}
			if _, exists := paths[target]; exists {
				return fmt.Errorf("steps[%d] mounts duplicate target path %q", i, target)
			}
			paths[target] = struct{}{}
		}
		for j := range step.VolumeMounts {
			mount := &step.VolumeMounts[j]
			volumeName := strings.TrimSpace(mount.Volume)
			if volumeName == "" {
				return fmt.Errorf("steps[%d].volumeMounts[%d].volume is required", i, j)
			}
			volume, ok := volumes[volumeName]
			if !ok {
				return fmt.Errorf("steps[%d].volumeMounts[%d] references unknown volume %q", i, j, volumeName)
			}
			target := strings.TrimSpace(mount.Path)
			if target == "" {
				return fmt.Errorf("steps[%d].volumeMounts[%d].path is required", i, j)
			}
			if !path.IsAbs(target) {
				return fmt.Errorf("steps[%d].volumeMounts[%d].path %q must be absolute", i, j, target)
			}
			if _, exists := paths[target]; exists {
				return fmt.Errorf("steps[%d] mounts duplicate target path %q", i, target)
			}
			paths[target] = struct{}{}

			source, err := volume.sourceForEngine(step.Engine)
			if err != nil {
				return fmt.Errorf("steps[%d].volumeMounts[%d]: %w", i, j, err)
			}
			kind, err := sourceKind(source)
			if err != nil {
				return fmt.Errorf("steps[%d].volumeMounts[%d]: %w", i, j, err)
			}
			if !sourceKindCompatible(kind, step.Engine) {
				return fmt.Errorf("steps[%d].volumeMounts[%d]: volume %q source %q is not valid for engine %q", i, j, volumeName, kind, step.Engine)
			}
		}

		if step.Engine != EngineKubernetes {
			if strings.TrimSpace(step.ServiceAccountName) != "" {
				return fmt.Errorf("steps[%d].serviceAccountName is only supported for kubernetes steps", i)
			}
			if len(step.PodAnnotations) > 0 {
				return fmt.Errorf("steps[%d].podAnnotations is only supported for kubernetes steps", i)
			}
			if step.AutomountServiceAccountToken != nil {
				return fmt.Errorf("steps[%d].automountServiceAccountToken is only supported for kubernetes steps", i)
			}
		}
	}
	return nil
}

func (v *Volume) sourceForEngine(engine string) (VolumeSource, error) {
	if v == nil {
		return VolumeSource{}, fmt.Errorf("volume is nil")
	}
	if v.Source != nil {
		return *v.Source, nil
	}
	if len(v.Sources) == 0 {
		return VolumeSource{}, fmt.Errorf("volume %q has no sources", v.Name)
	}
	source, ok := v.Sources[engine]
	if !ok {
		return VolumeSource{}, fmt.Errorf("volume %q has no source for engine %q", v.Name, engine)
	}
	return source, nil
}

// RuntimeSpecForStep resolves definition-level fields into the container spec
// persisted on the step's atom. The returned spec contains only runtime-native
// data and does not require the worker to reload the original job definition.
func (d *Definition) RuntimeSpecForStep(step *Step) (container.Spec, error) {
	if d == nil || step == nil {
		return container.Spec{}, fmt.Errorf("definition and step are required")
	}
	volumes := make(map[string]*Volume, len(d.Volumes))
	for i := range d.Volumes {
		volume := &d.Volumes[i]
		volumes[volume.Name] = volume
	}

	spec := cloneContainerSpec(step.Spec)
	spec.ResolvedVolumeMounts = nil
	for _, mount := range step.VolumeMounts {
		volume := volumes[strings.TrimSpace(mount.Volume)]
		if volume == nil {
			return container.Spec{}, fmt.Errorf("step %s references unknown volume %q", step.Name, mount.Volume)
		}
		source, err := volume.sourceForEngine(step.Engine)
		if err != nil {
			return container.Spec{}, fmt.Errorf("step %s volume %s: %w", step.Name, volume.Name, err)
		}
		resolved, err := resolveVolumeMount(volume.Name, source, mount)
		if err != nil {
			return container.Spec{}, fmt.Errorf("step %s volume %s: %w", step.Name, volume.Name, err)
		}
		spec.ResolvedVolumeMounts = append(spec.ResolvedVolumeMounts, resolved)
	}

	if step.Engine == EngineKubernetes {
		k8sSpec := &container.KubernetesSpec{
			ServiceAccountName:           strings.TrimSpace(d.Metadata.ServiceAccountName),
			PodAnnotations:               cloneStringMap(d.Metadata.PodAnnotations),
			AutomountServiceAccountToken: cloneBoolPtr(d.Metadata.AutomountServiceAccountToken),
		}
		if strings.TrimSpace(step.ServiceAccountName) != "" {
			k8sSpec.ServiceAccountName = strings.TrimSpace(step.ServiceAccountName)
		}
		if len(step.PodAnnotations) > 0 {
			if k8sSpec.PodAnnotations == nil {
				k8sSpec.PodAnnotations = make(map[string]string, len(step.PodAnnotations))
			}
			for k, v := range step.PodAnnotations {
				k8sSpec.PodAnnotations[k] = v
			}
		}
		if step.AutomountServiceAccountToken != nil {
			k8sSpec.AutomountServiceAccountToken = cloneBoolPtr(step.AutomountServiceAccountToken)
		}
		if k8sSpec.ServiceAccountName != "" || len(k8sSpec.PodAnnotations) > 0 || k8sSpec.AutomountServiceAccountToken != nil {
			spec.Kubernetes = k8sSpec
		}
	}

	return spec, nil
}

func resolveVolumeMount(name string, source VolumeSource, mount VolumeMount) (container.VolumeMount, error) {
	kind, err := sourceKind(source)
	if err != nil {
		return container.VolumeMount{}, err
	}
	resolved := container.VolumeMount{
		Name:     strings.TrimSpace(name),
		Target:   strings.TrimSpace(mount.Path),
		ReadOnly: mount.ReadOnly,
		SubPath:  strings.TrimSpace(mount.SubPath),
	}
	switch kind {
	case "bind":
		resolved.Type = container.VolumeMountTypeBind
		resolved.Source = strings.TrimSpace(source.Bind)
	case "volume":
		resolved.Type = container.VolumeMountTypeVolume
		resolved.Source = strings.TrimSpace(source.Volume)
	case "tmpfs":
		resolved.Type = container.VolumeMountTypeTmpfs
		if source.Tmpfs != nil {
			resolved.Tmpfs = &container.TmpfsOptions{
				SizeBytes: source.Tmpfs.SizeBytes,
				Mode:      cloneIntPtr(source.Tmpfs.Mode),
			}
		}
	case "pvc":
		resolved.Type = container.VolumeMountTypePVC
		resolved.Source = strings.TrimSpace(source.PVC)
	case "claimTemplate":
		resolved.Type = container.VolumeMountTypeClaimTemplate
		resolved.ClaimTemplate = convertClaimTemplate(source.ClaimTemplate)
	case "volumeSource":
		resolved.Type = container.VolumeMountTypeVolumeSource
		resolved.VolumeSource = cloneAnyMap(source.VolumeSource)
	default:
		return container.VolumeMount{}, fmt.Errorf("unsupported source kind %q", kind)
	}
	return resolved, nil
}

func convertClaimTemplate(source *ClaimTemplate) *container.KubernetesClaimTemplate {
	if source == nil {
		return nil
	}
	accessMode := strings.TrimSpace(source.AccessMode)
	if accessMode == "" {
		accessMode = "ReadWriteOnce"
	}
	return &container.KubernetesClaimTemplate{
		StorageClass: strings.TrimSpace(source.StorageClass),
		Size:         strings.TrimSpace(source.Size),
		AccessMode:   accessMode,
		Labels:       cloneStringMap(source.Labels),
		Annotations:  cloneStringMap(source.Annotations),
	}
}

func cloneContainerSpec(spec container.Spec) container.Spec {
	out := spec
	if len(spec.Env) > 0 {
		out.Env = cloneStringMap(spec.Env)
	}
	if len(spec.Mounts) > 0 {
		out.Mounts = slices.Clone(spec.Mounts)
	}
	if len(spec.ResolvedVolumeMounts) > 0 {
		out.ResolvedVolumeMounts = slices.Clone(spec.ResolvedVolumeMounts)
	}
	if spec.Kubernetes != nil {
		out.Kubernetes = &container.KubernetesSpec{
			ServiceAccountName:           spec.Kubernetes.ServiceAccountName,
			PodAnnotations:               cloneStringMap(spec.Kubernetes.PodAnnotations),
			AutomountServiceAccountToken: cloneBoolPtr(spec.Kubernetes.AutomountServiceAccountToken),
		}
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func cloneAnyMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

// DeriveStepSuccessors builds the adjacency list for the provided steps.
// It assumes the caller has validated the definition (Validate) beforehand.
func DeriveStepSuccessors(steps []Step) (map[string][]string, error) {
	names, adj, err := computeStepAdjacency(steps)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string, len(names))
	for name := range names {
		targets := adj[name]
		if len(targets) == 0 {
			continue
		}
		list := make([]string, 0, len(targets))
		for succ := range targets {
			list = append(list, succ)
		}
		slices.Sort(list)
		result[name] = list
	}
	return result, nil
}

func computeStepAdjacency(steps []Step) (map[string]int, map[string]map[string]struct{}, error) {
	names := make(map[string]int, len(steps))
	for i := range steps {
		step := &steps[i]
		if strings.TrimSpace(step.Name) == "" {
			return nil, nil, fmt.Errorf("steps[%d].name is required", i)
		}
		if _, exists := names[step.Name]; exists {
			return nil, nil, fmt.Errorf("duplicate step name %q", step.Name)
		}
		names[step.Name] = i

		if strings.TrimSpace(step.Image) == "" {
			return nil, nil, fmt.Errorf("steps[%d].image is required", i)
		}
		switch step.Engine {
		case EngineDocker, EngineKubernetes, EnginePodman:
		default:
			return nil, nil, fmt.Errorf("steps[%d].engine must be one of [%s,%s,%s]", i, EngineDocker, EngineKubernetes, EnginePodman)
		}

		switch step.Type {
		case StepTypeTask, StepTypeBranch:
		default:
			return nil, nil, fmt.Errorf("steps[%d].type %q must be one of [%s,%s]", i, step.Type, StepTypeTask, StepTypeBranch)
		}
		if step.Type == StepTypeBranch && len(step.Next) == 0 {
			return nil, nil, fmt.Errorf("steps[%d] is a branch step and must have at least one next entry", i)
		}

		if err := ensureUnique(step.Next, fmt.Sprintf("steps[%d].next", i)); err != nil {
			return nil, nil, err
		}
		if err := ensureUnique(step.DependsOn, fmt.Sprintf("steps[%d].dependsOn", i)); err != nil {
			return nil, nil, err
		}

		if rule := strings.TrimSpace(step.TriggerRule); rule != "" {
			switch rule {
			case TriggerRuleAllSuccess, TriggerRuleAllDone, TriggerRuleAllFailed, TriggerRuleOneSuccess, TriggerRuleAlways:
			default:
				return nil, nil, fmt.Errorf("steps[%d].triggerRule %q must be one of [%s,%s,%s,%s,%s]",
					i, rule, TriggerRuleAllSuccess, TriggerRuleAllDone, TriggerRuleAllFailed, TriggerRuleOneSuccess, TriggerRuleAlways)
			}
		}
	}

	hasExplicitEdges := false

	for i := range steps {
		step := &steps[i]
		steps[i].Next = trimCopy(step.Next)
		steps[i].DependsOn = trimCopy(step.DependsOn)
		if len(steps[i].Next) > 0 || len(steps[i].DependsOn) > 0 {
			hasExplicitEdges = true
		}
	}

	adj := make(map[string]map[string]struct{}, len(steps))

	ensureAdj := func(from string) map[string]struct{} {
		targets, ok := adj[from]
		if !ok {
			targets = make(map[string]struct{})
			adj[from] = targets
		}
		return targets
	}

	for i, step := range steps {
		for _, successor := range step.Next {
			if strings.TrimSpace(successor) == "" {
				return nil, nil, fmt.Errorf("steps[%d].next contains an empty entry", i)
			}
			if _, exists := names[successor]; !exists {
				return nil, nil, fmt.Errorf("steps[%d].next references unknown step %q", i, successor)
			}
			if successor == step.Name {
				return nil, nil, fmt.Errorf("steps[%d].next cannot reference the same step %q", i, successor)
			}
			ensureAdj(step.Name)[successor] = struct{}{}
		}

		for _, dependency := range step.DependsOn {
			if strings.TrimSpace(dependency) == "" {
				return nil, nil, fmt.Errorf("steps[%d].dependsOn contains an empty entry", i)
			}
			if _, exists := names[dependency]; !exists {
				return nil, nil, fmt.Errorf("steps[%d].dependsOn references unknown step %q", i, dependency)
			}
			if dependency == step.Name {
				return nil, nil, fmt.Errorf("steps[%d].dependsOn cannot reference the same step %q", i, dependency)
			}
			ensureAdj(dependency)[step.Name] = struct{}{}
		}
	}

	if !hasExplicitEdges {
		for idx := range steps {
			if len(steps[idx].Next) > 0 {
				continue
			}
			if idx+1 >= len(steps) {
				continue
			}
			ensureAdj(steps[idx].Name)[steps[idx+1].Name] = struct{}{}
		}
	}

	return names, adj, nil
}

func trimCopy(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

func ensureUnique(entries []string, field string) error {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%s contains duplicate entry %q", field, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func normalizeInterfaceList(value interface{}) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []string{strings.TrimSpace(v)}, nil
	case []interface{}:
		result := make([]string, 0, len(v))
		for idx, item := range v {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("entry %d must be a string", idx)
			}
			str = strings.TrimSpace(str)
			if str == "" {
				return nil, fmt.Errorf("entry %d cannot be empty", idx)
			}
			result = append(result, str)
		}
		return result, nil
	case []string:
		result := make([]string, 0, len(v))
		for idx, item := range v {
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, fmt.Errorf("entry %d cannot be empty", idx)
			}
			result = append(result, item)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("expected string or list, got %T", value)
	}
}

func detectCycles(adj map[string]map[string]struct{}, names map[string]int) error {
	const (
		unvisited = iota
		visiting
		visited
	)

	state := make(map[string]int, len(names))

	var visit func(string) error
	visit = func(node string) error {
		switch state[node] {
		case visiting:
			return fmt.Errorf("cycle detected involving step %q", node)
		case visited:
			return nil
		}

		state[node] = visiting
		for succ := range adj[node] {
			if err := visit(succ); err != nil {
				return err
			}
		}
		state[node] = visited
		return nil
	}

	for name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// CacheConfig is the resolved cache configuration for a step.
type CacheConfig struct {
	Enabled bool
	TTL     time.Duration
	Version int
}

// ResolveCacheConfig resolves the cache configuration for a step,
// considering step-level config, job-level defaults, and environment settings.
func ResolveCacheConfig(stepCache, metaCache interface{}, envEnabled bool, envTTL time.Duration) CacheConfig {
	cfg := CacheConfig{Enabled: envEnabled, TTL: envTTL}

	// Apply job-level defaults
	applyCache(&cfg, metaCache)
	// Step-level overrides job-level
	applyCache(&cfg, stepCache)

	return cfg
}

func applyCache(cfg *CacheConfig, raw interface{}) {
	switch v := raw.(type) {
	case bool:
		cfg.Enabled = v
	case map[string]any:
		cfg.Enabled = true
		if ttl, ok := v["ttl"]; ok {
			if s, ok := ttl.(string); ok {
				if d, err := time.ParseDuration(s); err == nil {
					cfg.TTL = d
				}
			}
		}
		if ver, ok := v["version"]; ok {
			switch n := ver.(type) {
			case int:
				cfg.Version = n
			case float64:
				cfg.Version = int(n)
			}
		}
	}
}
