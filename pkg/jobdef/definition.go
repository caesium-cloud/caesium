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

	TriggerCron  = "cron"
	TriggerHTTP  = "http"
	TriggerEvent = "event"

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

var simpleJSONPathPattern = regexp.MustCompile(`^\$(?:\[[0-9]+\])*(?:\.[^.\s\[\]]+(?:\[[0-9]+\])*)*$`)

// kueueQueueNamePattern matches a Kubernetes DNS-1123 label, the form Kueue
// requires for a LocalQueue name (which Caesium emits verbatim as the
// `kueue.x-k8s.io/queue-name` label value). Validating it at lint time turns an
// invalid name into an upfront `caesium job lint` error rather than a pod that
// the API server rejects at apply/run time.
var kueueQueueNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

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

	PriorityHigh   = "high"
	PriorityNormal = "normal"
	PriorityLow    = "low"

	ConcurrencyStrategyQueue   = "queue"
	ConcurrencyStrategyReplace = "replace"
	ConcurrencyStrategySkip    = "skip"
	ConcurrencyStrategyFail    = "fail"
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
	Priority         string            `yaml:"priority,omitempty" json:"priority,omitempty"`
	Concurrency      *Concurrency      `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	RateLimits       []RateLimit       `yaml:"rateLimits,omitempty" json:"rateLimits,omitempty"`
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
	SchemaValidation string `yaml:"schemaValidation,omitempty" json:"schemaValidation,omitempty"`
	// ReplaySafe marks every step in this job as eligible for quarantined replay.
	// The effective per-step value is snapshotted onto TaskRun when the task runs.
	ReplaySafe                   bool              `yaml:"replaySafe,omitempty" json:"replaySafe,omitempty"`
	Cache                        interface{}       `yaml:"cache,omitempty" json:"cache"`
	ServiceAccountName           string            `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
	PodAnnotations               map[string]string `yaml:"podAnnotations,omitempty" json:"podAnnotations,omitempty"`
	AutomountServiceAccountToken *bool             `yaml:"automountServiceAccountToken,omitempty" json:"automountServiceAccountToken,omitempty"`
	// Datasets declares the external source datasets this job's steps consume.
	// It is scheduling metadata for freshness and does not affect the cache hash.
	Datasets *MetadataDatasets `yaml:"datasets,omitempty" json:"datasets,omitempty"`
	// Remediation declares this job's opt-in to autonomous incident
	// remediation (agent-in-the-loop-remediation Stream E): which
	// AgentProfile to use, which failure classes are in scope, the tiered
	// action catalog the agent (or a deterministic rule) may exercise
	// autonomously, and the escalation fallback. It is server-enforced
	// scheduling/policy metadata, not a step-execution input, and does not
	// affect the cache hash.
	Remediation *MetadataRemediation `yaml:"remediation,omitempty" json:"remediation,omitempty"`
}

// Concurrency controls admission of new runs for the same job.
type Concurrency struct {
	MaxRuns  int    `yaml:"maxRuns,omitempty" json:"maxRuns,omitempty"`
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
}

// RateLimit declares a shared resource budget for task scheduling.
type RateLimit struct {
	Resource string `yaml:"resource" json:"resource"`
	Limit    int    `yaml:"limit" json:"limit"`
	Window   string `yaml:"window" json:"window"`
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

// Kueue declares that a step delegates admission to Kueue. When set on a
// kubernetes step, Caesium stamps the `kueue.x-k8s.io/queue-name` label on the
// created pod so Kueue gates scheduling against the named LocalQueue's quota and
// admits the task only when capacity is available. Caesium never bin-packs or
// prioritizes itself — it delegates scheduling to Kueue — so this field is pure
// scheduling metadata and is excluded from the cache identity hash.
type Kueue struct {
	// QueueName is the Kueue LocalQueue (in the pod's namespace) to admit
	// through. It is the value of the `kueue.x-k8s.io/queue-name` label.
	// omitempty keeps the marshaled form symmetric with
	// container.KubernetesSpec.QueueName; an empty value is unreachable through
	// normal parsing because Validate() rejects a blank queueName.
	QueueName string `yaml:"queueName,omitempty" json:"queueName,omitempty"`
}

// StepRateLimit declares the units of a job-level rate limit a step consumes.
type StepRateLimit struct {
	Resource string `yaml:"resource" json:"resource"`
	Units    int    `yaml:"units" json:"units"`
}

// Dataset direction constants describe how a declaration relates a step (or the
// job's metadata) to a dataset. They are the canonical values persisted on the
// DatasetDeclaration registry model and read by the cross-job lint.
const (
	// DatasetDirectionProduces marks a dataset a step produces (carries the SLO).
	DatasetDirectionProduces = "produces"
	// DatasetDirectionConsumes marks a dataset a step consumes.
	DatasetDirectionConsumes = "consumes"
	// DatasetDirectionSource marks an external dataset declared under
	// metadata.datasets.sources that nobody in this instance produces.
	DatasetDirectionSource = "source"
)

// Watermark identifies the ##caesium::output key a producing step emits to
// advance its dataset. It is not a JSONPath — it names an output key on the
// existing zero-SDK output contract (echo '##caesium::output {"<key>": ...}').
type Watermark struct {
	Key string `yaml:"key,omitempty" json:"key,omitempty"`
}

// ProducedDataset declares a dataset a step produces plus the freshness SLO
// consumers care about. The SLO fields are scheduling metadata, not execution
// inputs — they never enter the cache identity hash.
type ProducedDataset struct {
	// Name is the dataset identity (keyed on name in v1; namespace is reserved).
	Name string `yaml:"name" json:"name"`
	// Freshness is the target staleness SLO as a Go duration string (e.g. "6h").
	Freshness string `yaml:"freshness,omitempty" json:"freshness,omitempty"`
	// MaxStaleness is the hard bound whose breach emits freshness_violated.
	MaxStaleness string `yaml:"maxStaleness,omitempty" json:"maxStaleness,omitempty"`
	// Watermark names the output key this step emits to advance the dataset.
	Watermark *Watermark `yaml:"watermark,omitempty" json:"watermark,omitempty"`
}

// StepDatasets is the per-step datasets surface: the datasets a step consumes
// and the datasets it produces (with their freshness SLOs).
type StepDatasets struct {
	Consumes []string          `yaml:"consumes,omitempty" json:"consumes,omitempty"`
	Produces []ProducedDataset `yaml:"produces,omitempty" json:"produces,omitempty"`
}

// ArrivalEvent is the event pattern a source dataset's arrival binds to. It
// mirrors the shipped event-trigger matcher shape (type + string filter).
type ArrivalEvent struct {
	Type   string            `yaml:"type,omitempty" json:"type,omitempty"`
	Filter map[string]string `yaml:"filter,omitempty" json:"filter,omitempty"`
}

// Arrival binds an external event to a source dataset advance: when an ingested
// event matches Event, Watermark (a JSONPath into the event payload) is
// extracted as the new watermark value.
type Arrival struct {
	Event     *ArrivalEvent `yaml:"event,omitempty" json:"event,omitempty"`
	Watermark string        `yaml:"watermark,omitempty" json:"watermark,omitempty"`
}

// SourceDataset declares an external dataset nobody in Caesium produces — the
// upstream a consuming step depends on. expectedEvery is a cadence expectation;
// a late arrival surfaces as stale-upstream rather than a failed run.
type SourceDataset struct {
	Name          string   `yaml:"name" json:"name"`
	ExpectedEvery string   `yaml:"expectedEvery,omitempty" json:"expectedEvery,omitempty"`
	Arrival       *Arrival `yaml:"arrival,omitempty" json:"arrival,omitempty"`
	// External marks the dataset as intentionally produced outside Caesium so
	// the cross-job lint does not demand a producing job.
	External bool `yaml:"external,omitempty" json:"external,omitempty"`
}

// MetadataDatasets is the job-level datasets surface: the external source
// datasets the job's steps consume.
type MetadataDatasets struct {
	Sources []SourceDataset `yaml:"sources,omitempty" json:"sources,omitempty"`
}

// Failure class names accepted by metadata.remediation.classes and the keys
// of metadata.remediation.autonomy.perClass. These must stay in sync with
// internal/incident.FailureClass (the deterministic classifier, Stream A);
// pkg/jobdef duplicates the vocabulary rather than importing internal/incident
// so offline `caesium job lint` (and this package generally) stays free of a
// dependency on the incident runtime.
const (
	RemediationClassTransientInfra  = "transient_infra"
	RemediationClassSchemaViolation = "schema_violation"
	RemediationClassSLARisk         = "sla_risk"
	RemediationClassDataUnavailable = "data_unavailable"
	RemediationClassAuthFailure     = "auth_failure"
	RemediationClassOOM             = "oom"
	RemediationClassQuota           = "quota"
	RemediationClassUnknown         = "unknown"
)

// remediationClasses is the lookup set backing isKnownRemediationClass.
var remediationClasses = map[string]struct{}{
	RemediationClassTransientInfra:  {},
	RemediationClassSchemaViolation: {},
	RemediationClassSLARisk:         {},
	RemediationClassDataUnavailable: {},
	RemediationClassAuthFailure:     {},
	RemediationClassOOM:             {},
	RemediationClassQuota:           {},
	RemediationClassUnknown:         {},
}

// Remediation action names accepted by metadata.remediation.autonomy.allow,
// .perClass[].allow, and .requireApproval. These name the typed action
// catalog docs/design-agent-in-the-loop.md defines for Stream B's executor
// (tier 1/2 autonomous actions, tier-3 approval-gated producers, plus the
// terminal `escalate`); pkg/jobdef validates job-declared policy names
// against this list without depending on Stream B's executor package.
const (
	RemediationActionAutoRetryBackoff         = "auto_retry_backoff"
	RemediationActionSnoozeUntilCron          = "snooze_until_cron"
	RemediationActionSnoozeRetry              = "snooze_retry"
	RemediationActionRetryFromFailure         = "retry_from_failure"
	RemediationActionRetryCallbacks           = "retry_callbacks"
	RemediationActionNotify                   = "notify"
	RemediationActionQuarantineReplay         = "quarantine_replay"
	RemediationActionRerunWithParams          = "rerun_with_params"
	RemediationActionPauseJob                 = "pause_job"
	RemediationActionUnpauseJob               = "unpause_job"
	RemediationActionClearCacheEntry          = "clear_cache_entry"
	RemediationActionSuppressDownstreamAlerts = "suppress_downstream_alerts"
	RemediationActionExtendSLAOnce            = "extend_sla_once"
	RemediationActionSkipTask                 = "skip_task"
	RemediationActionOverrideSchemaGate       = "override_schema_gate"
	RemediationActionApplyJobdefPatch         = "apply_jobdef_patch"
	RemediationActionEscalate                 = "escalate"
)

// remediationActions is the lookup set backing isKnownRemediationAction.
var remediationActions = map[string]struct{}{
	RemediationActionAutoRetryBackoff:         {},
	RemediationActionSnoozeUntilCron:          {},
	RemediationActionSnoozeRetry:              {},
	RemediationActionRetryFromFailure:         {},
	RemediationActionRetryCallbacks:           {},
	RemediationActionNotify:                   {},
	RemediationActionQuarantineReplay:         {},
	RemediationActionRerunWithParams:          {},
	RemediationActionPauseJob:                 {},
	RemediationActionUnpauseJob:               {},
	RemediationActionClearCacheEntry:          {},
	RemediationActionSuppressDownstreamAlerts: {},
	RemediationActionExtendSLAOnce:            {},
	RemediationActionSkipTask:                 {},
	RemediationActionOverrideSchemaGate:       {},
	RemediationActionApplyJobdefPatch:         {},
	RemediationActionEscalate:                 {},
}

func isKnownRemediationClass(name string) bool {
	_, ok := remediationClasses[name]
	return ok
}

func isKnownRemediationAction(name string) bool {
	_, ok := remediationActions[name]
	return ok
}

// RemediationClassPolicy narrows the allowed action set for one failure class,
// e.g. metadata.remediation.autonomy.perClass.auth_failure.allow.
type RemediationClassPolicy struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
}

// RemediationAutonomy is the tiered-autonomy policy within a remediation
// block: which actions may execute without a human, the whitelist of
// rerun_with_params overrides, per-class narrowing, and which actions always
// require approval regardless of tier.
type RemediationAutonomy struct {
	// Allow lists the actions this job permits to run autonomously (subject
	// to each action's own tier semantics — tier 3 always creates an
	// ApprovalRequest no matter what allow contains).
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	// ParamOverrides whitelists the rerun_with_params values allowed per
	// trigger.defaultParams key. Every key must name an existing
	// trigger.defaultParams entry.
	ParamOverrides map[string][]string `yaml:"paramOverrides,omitempty" json:"paramOverrides,omitempty"`
	// PerClass optionally narrows the allow list for a specific failure class.
	PerClass map[string]RemediationClassPolicy `yaml:"perClass,omitempty" json:"perClass,omitempty"`
	// RequireApproval lists actions that must create an ApprovalRequest for
	// this job even if the action's own default tier would otherwise permit
	// autonomous execution.
	RequireApproval []string `yaml:"requireApproval,omitempty" json:"requireApproval,omitempty"`
}

// RemediationEscalation configures the forced hand-off when remediation does
// not resolve an incident within the wall-clock cap.
type RemediationEscalation struct {
	// Channel names a NotificationChannel (server-side state; unverified by
	// offline lint, same posture as Profile).
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// After is the wall-clock cap, as a Go duration string, before the
	// incident is force-escalated to Channel.
	After string `yaml:"after,omitempty" json:"after,omitempty"`
}

// MetadataRemediation is the job-level opt-in to autonomous incident
// remediation (docs/design-agent-in-the-loop.md "Declarative policy"). It is
// policy metadata enforced server-side by the incident manager and executor
// (Streams A-D); it never participates in step-execution cache identity.
type MetadataRemediation struct {
	// Profile references an AgentProfile by name. AgentProfile is
	// server-side state (api/rest/controller/agentprofile), so offline
	// `caesium job lint` cannot verify this reference — it emits a scope
	// note instead. Server-side lint (POST /v1/jobdefs/lint) and the apply
	// transaction both verify it.
	Profile string `yaml:"profile" json:"profile"`
	// Classes lists the failure classes this policy applies to.
	Classes []string `yaml:"classes,omitempty" json:"classes,omitempty"`
	// MaxAttempts bounds how many remediation attempts an incident may take
	// before it force-escalates.
	MaxAttempts int `yaml:"maxAttempts,omitempty" json:"maxAttempts,omitempty"`
	// Autonomy declares which actions may run without a human and under what
	// constraints.
	Autonomy *RemediationAutonomy `yaml:"autonomy,omitempty" json:"autonomy,omitempty"`
	// Escalation configures the forced hand-off when remediation doesn't
	// resolve the incident within Escalation.After.
	Escalation *RemediationEscalation `yaml:"escalation,omitempty" json:"escalation,omitempty"`
}

// Step defines an execution step.
type Step struct {
	Name         string            `yaml:"name" json:"name"`
	Type         string            `yaml:"type,omitempty" json:"type,omitempty"`
	Engine       string            `yaml:"engine,omitempty" json:"engine,omitempty"`
	Image        string            `yaml:"image" json:"image"`
	Command      []string          `yaml:"command,omitempty" json:"command,omitempty"`
	NodeSelector map[string]string `yaml:"nodeSelector,omitempty" json:"nodeSelector,omitempty"`
	Next         []string          `yaml:"next,omitempty" json:"next,omitempty"`
	DependsOn    []string          `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	Retries      int               `yaml:"retries,omitempty" json:"retries,omitempty"`
	RetryDelay   time.Duration     `yaml:"retryDelay,omitempty" json:"retryDelay,omitempty"`
	RetryBackoff bool              `yaml:"retryBackoff,omitempty" json:"retryBackoff,omitempty"`
	TriggerRule  string            `yaml:"triggerRule,omitempty" json:"triggerRule,omitempty"`
	// ReplaySafe marks this step as eligible for quarantined replay. It is
	// control-plane metadata, not a runtime input or cache identity field.
	ReplaySafe                   bool              `yaml:"replaySafe,omitempty" json:"replaySafe,omitempty"`
	VolumeMounts                 []VolumeMount     `yaml:"volumeMounts,omitempty" json:"volumeMounts,omitempty"`
	ServiceAccountName           string            `yaml:"serviceAccountName,omitempty" json:"serviceAccountName,omitempty"`
	PodAnnotations               map[string]string `yaml:"podAnnotations,omitempty" json:"podAnnotations,omitempty"`
	AutomountServiceAccountToken *bool             `yaml:"automountServiceAccountToken,omitempty" json:"automountServiceAccountToken,omitempty"`
	// Kueue delegates this step's admission to a Kueue LocalQueue (kubernetes
	// engine only). It is scheduling metadata and does not affect the cache hash.
	Kueue *Kueue `yaml:"kueue,omitempty" json:"kueue,omitempty"`
	// RateLimit references a job-level shared resource budget for this step.
	// It is scheduling metadata and does not affect the cache hash.
	RateLimit *StepRateLimit `yaml:"rateLimit,omitempty" json:"rateLimit,omitempty"`
	// OutputSchema is a JSON Schema describing this step's expected output keys.
	OutputSchema map[string]any `yaml:"outputSchema,omitempty" json:"outputSchema,omitempty"`
	// InputSchema maps predecessor step names to JSON Schema fragments describing
	// which keys this step requires from each predecessor's output.
	InputSchema map[string]map[string]any `yaml:"inputSchema,omitempty" json:"inputSchema,omitempty"`
	// Datasets declares the datasets this step consumes and produces. It is
	// scheduling metadata for freshness and does not affect the cache hash.
	Datasets       *StepDatasets `yaml:"datasets,omitempty" json:"datasets,omitempty"`
	Cache          interface{}   `yaml:"cache,omitempty" json:"cache"`
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
		ReplaySafe                   bool                      `yaml:"replaySafe"`
		VolumeMounts                 []VolumeMount             `yaml:"volumeMounts"`
		ServiceAccountName           string                    `yaml:"serviceAccountName"`
		PodAnnotations               map[string]string         `yaml:"podAnnotations"`
		AutomountServiceAccountToken *bool                     `yaml:"automountServiceAccountToken"`
		Kueue                        *Kueue                    `yaml:"kueue"`
		RateLimit                    *StepRateLimit            `yaml:"rateLimit"`
		OutputSchema                 map[string]any            `yaml:"outputSchema"`
		InputSchema                  map[string]map[string]any `yaml:"inputSchema"`
		Datasets                     *StepDatasets             `yaml:"datasets"`
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
	s.ReplaySafe = rs.ReplaySafe
	s.VolumeMounts = rs.VolumeMounts
	s.ServiceAccountName = rs.ServiceAccountName
	s.PodAnnotations = rs.PodAnnotations
	s.AutomountServiceAccountToken = rs.AutomountServiceAccountToken
	s.Kueue = rs.Kueue
	s.RateLimit = rs.RateLimit
	s.OutputSchema = rs.OutputSchema
	s.InputSchema = rs.InputSchema
	s.Datasets = rs.Datasets
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
		ReplaySafe                   bool                      `json:"replaySafe"`
		VolumeMounts                 []VolumeMount             `json:"volumeMounts"`
		ServiceAccountName           string                    `json:"serviceAccountName"`
		PodAnnotations               map[string]string         `json:"podAnnotations"`
		AutomountServiceAccountToken *bool                     `json:"automountServiceAccountToken"`
		Kueue                        *Kueue                    `json:"kueue"`
		RateLimit                    *StepRateLimit            `json:"rateLimit"`
		OutputSchema                 map[string]any            `json:"outputSchema"`
		InputSchema                  map[string]map[string]any `json:"inputSchema"`
		Datasets                     *StepDatasets             `json:"datasets"`
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
	s.ReplaySafe = rs.ReplaySafe
	s.VolumeMounts = rs.VolumeMounts
	s.ServiceAccountName = rs.ServiceAccountName
	s.PodAnnotations = rs.PodAnnotations
	s.AutomountServiceAccountToken = rs.AutomountServiceAccountToken
	s.Kueue = rs.Kueue
	s.RateLimit = rs.RateLimit
	s.OutputSchema = rs.OutputSchema
	s.InputSchema = rs.InputSchema
	s.Datasets = rs.Datasets
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

	rateLimitResources, err := validateSchedulingMetadata(&d.Metadata)
	if err != nil {
		return err
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
	if err := validateSteps(d.Steps, volumes, rateLimitResources); err != nil {
		return err
	}
	if err := validateDatasets(d); err != nil {
		return err
	}
	if err := validateRemediation(d); err != nil {
		return err
	}
	return nil
}

// validateRemediation performs single-definition validation of the
// metadata.remediation block: class/action names are known, maxAttempts is
// non-negative, autonomy.paramOverrides keys exist in trigger.defaultParams,
// autonomy.perClass keys are known classes, and escalation.after (if set)
// parses as a positive Go duration. It deliberately cannot verify
// Profile or Escalation.Channel — those name server-side resources
// (AgentProfile, NotificationChannel) that do not exist at parse time; the
// offline lint scope note (cmd/job/lint.go) names this gap, and server-side
// lint (internal/jobdef.ValidateAgentProfileRefs, invoked from
// POST /v1/jobdefs/lint and the apply transaction) closes it. The block is
// policy metadata and never enters the cache identity hash.
func validateRemediation(d *Definition) error {
	r := d.Metadata.Remediation
	if r == nil {
		return nil
	}

	r.Profile = strings.TrimSpace(r.Profile)
	if r.Profile == "" {
		return fmt.Errorf("metadata.remediation.profile is required")
	}

	if len(r.Classes) == 0 {
		return fmt.Errorf("metadata.remediation.classes must contain at least one entry")
	}
	seenClasses := make(map[string]struct{}, len(r.Classes))
	for i, raw := range r.Classes {
		name := strings.TrimSpace(raw)
		if !isKnownRemediationClass(name) {
			return fmt.Errorf("metadata.remediation.classes[%d] %q is not a known failure class", i, raw)
		}
		if _, dup := seenClasses[name]; dup {
			return fmt.Errorf("metadata.remediation.classes[%d] %q duplicates another entry", i, raw)
		}
		seenClasses[name] = struct{}{}
		r.Classes[i] = name
	}

	if r.MaxAttempts < 0 {
		return fmt.Errorf("metadata.remediation.maxAttempts must be >= 0")
	}

	if r.Autonomy != nil {
		if err := validateRemediationAutonomy(r.Autonomy, d.Trigger.DefaultParams); err != nil {
			return err
		}
	}

	if r.Escalation != nil {
		r.Escalation.Channel = strings.TrimSpace(r.Escalation.Channel)
		after := strings.TrimSpace(r.Escalation.After)
		if r.Escalation.Channel == "" && after == "" {
			return fmt.Errorf("metadata.remediation.escalation requires channel and/or after")
		}
		if after != "" {
			dur, err := time.ParseDuration(after)
			if err != nil {
				return fmt.Errorf("metadata.remediation.escalation.after %q must be a valid duration: %w", after, err)
			}
			if dur <= 0 {
				return fmt.Errorf("metadata.remediation.escalation.after %q must be a positive duration", after)
			}
		}
		r.Escalation.After = after
	}

	return nil
}

func validateRemediationAutonomy(a *RemediationAutonomy, defaultParams map[string]string) error {
	if err := validateRemediationActionList("metadata.remediation.autonomy.allow", a.Allow); err != nil {
		return err
	}

	for key, values := range a.ParamOverrides {
		if _, ok := defaultParams[key]; !ok {
			return fmt.Errorf("metadata.remediation.autonomy.paramOverrides key %q does not match any trigger.defaultParams key", key)
		}
		if len(values) == 0 {
			return fmt.Errorf("metadata.remediation.autonomy.paramOverrides[%q] must list at least one allowed value", key)
		}
	}

	for class, policy := range a.PerClass {
		name := strings.TrimSpace(class)
		if !isKnownRemediationClass(name) {
			return fmt.Errorf("metadata.remediation.autonomy.perClass key %q is not a known failure class", class)
		}
		if err := validateRemediationActionList(fmt.Sprintf("metadata.remediation.autonomy.perClass[%q].allow", class), policy.Allow); err != nil {
			return err
		}
	}

	if err := validateRemediationActionList("metadata.remediation.autonomy.requireApproval", a.RequireApproval); err != nil {
		return err
	}

	return nil
}

func validateRemediationActionList(field string, actions []string) error {
	seen := make(map[string]struct{}, len(actions))
	for i, raw := range actions {
		name := strings.TrimSpace(raw)
		if !isKnownRemediationAction(name) {
			return fmt.Errorf("%s[%d] %q is not a known remediation action", field, i, raw)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("%s[%d] %q duplicates another entry", field, i, raw)
		}
		seen[name] = struct{}{}
		actions[i] = name
	}
	return nil
}

// validateDatasets performs single-definition validation of the datasets
// surface: SLO fields parse as Go durations, arrival watermarks are well-formed
// JSONPaths, produced/source names are unique within the job, and consumes
// entries are non-empty and de-duplicated. It deliberately does NOT reject a
// consumes name that resolves to no dataset in THIS definition: a step may
// consume a dataset produced by another job, and that cross-job resolution
// (produced-in-applied-set / declared-source / external:true) is the batch
// validator's job (internal/jobdef, item A3), which sees the whole applied set
// plus persisted declarations. Datasets are scheduling metadata and never enter
// the cache identity hash.
func validateDatasets(d *Definition) error {
	produced := make(map[string]struct{})
	sources := make(map[string]struct{})

	if d.Metadata.Datasets != nil {
		for i := range d.Metadata.Datasets.Sources {
			src := &d.Metadata.Datasets.Sources[i]
			name := strings.TrimSpace(src.Name)
			if name == "" {
				return fmt.Errorf("metadata.datasets.sources[%d].name is required", i)
			}
			if _, exists := sources[name]; exists {
				return fmt.Errorf("metadata.datasets.sources[%d].name %q duplicates another source", i, name)
			}
			sources[name] = struct{}{}
			if strings.TrimSpace(src.ExpectedEvery) != "" {
				dur, err := time.ParseDuration(src.ExpectedEvery)
				if err != nil {
					return fmt.Errorf("metadata.datasets.sources[%d].expectedEvery %q must be a valid duration: %w", i, src.ExpectedEvery, err)
				}
				if dur <= 0 {
					return fmt.Errorf("metadata.datasets.sources[%d].expectedEvery %q must be a positive duration", i, src.ExpectedEvery)
				}
			}
			if src.Arrival != nil {
				if src.Arrival.Event != nil && strings.TrimSpace(src.Arrival.Event.Type) == "" {
					return fmt.Errorf("metadata.datasets.sources[%d].arrival.event.type is required when event is set", i)
				}
				if wm := strings.TrimSpace(src.Arrival.Watermark); wm != "" {
					if err := validateSimpleJSONPath(wm); err != nil {
						return fmt.Errorf("metadata.datasets.sources[%d].arrival.watermark: %w", i, err)
					}
				}
			}
			src.Name = name
		}
	}

	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Datasets == nil {
			continue
		}
		for j := range step.Datasets.Produces {
			p := &step.Datasets.Produces[j]
			name := strings.TrimSpace(p.Name)
			if name == "" {
				return fmt.Errorf("steps[%d].datasets.produces[%d].name is required", i, j)
			}
			if _, exists := produced[name]; exists {
				return fmt.Errorf("steps[%d].datasets.produces[%d].name %q is produced more than once in this job", i, j, name)
			}
			if _, exists := sources[name]; exists {
				return fmt.Errorf("steps[%d].datasets.produces[%d].name %q is also declared under metadata.datasets.sources", i, j, name)
			}
			produced[name] = struct{}{}
			if strings.TrimSpace(p.Freshness) != "" {
				dur, err := time.ParseDuration(p.Freshness)
				if err != nil {
					return fmt.Errorf("steps[%d].datasets.produces[%d].freshness %q must be a valid duration: %w", i, j, p.Freshness, err)
				}
				if dur <= 0 {
					return fmt.Errorf("steps[%d].datasets.produces[%d].freshness %q must be a positive duration", i, j, p.Freshness)
				}
			}
			if strings.TrimSpace(p.MaxStaleness) != "" {
				dur, err := time.ParseDuration(p.MaxStaleness)
				if err != nil {
					return fmt.Errorf("steps[%d].datasets.produces[%d].maxStaleness %q must be a valid duration: %w", i, j, p.MaxStaleness, err)
				}
				if dur <= 0 {
					return fmt.Errorf("steps[%d].datasets.produces[%d].maxStaleness %q must be a positive duration", i, j, p.MaxStaleness)
				}
			}
			if p.Watermark != nil && strings.TrimSpace(p.Watermark.Key) == "" {
				return fmt.Errorf("steps[%d].datasets.produces[%d].watermark.key is required when watermark is set", i, j)
			}
			p.Name = name
		}
	}

	for i := range d.Steps {
		step := &d.Steps[i]
		if step.Datasets == nil {
			continue
		}
		seen := make(map[string]struct{}, len(step.Datasets.Consumes))
		for j, raw := range step.Datasets.Consumes {
			name := strings.TrimSpace(raw)
			if name == "" {
				return fmt.Errorf("steps[%d].datasets.consumes[%d] must not be empty", i, j)
			}
			if _, dup := seen[name]; dup {
				return fmt.Errorf("steps[%d].datasets.consumes contains duplicate entry %q", i, name)
			}
			seen[name] = struct{}{}
			step.Datasets.Consumes[j] = name
		}
	}

	return nil
}

func validateSchedulingMetadata(metadata *Metadata) (map[string]struct{}, error) {
	switch metadata.Priority {
	case "", PriorityHigh, PriorityNormal, PriorityLow:
	default:
		return nil, fmt.Errorf("metadata.priority %q must be one of [\"%s\",\"%s\",\"%s\"]",
			metadata.Priority, PriorityHigh, PriorityNormal, PriorityLow)
	}

	if metadata.Concurrency != nil {
		if metadata.Concurrency.MaxRuns < 0 {
			return nil, fmt.Errorf("metadata.concurrency.maxRuns must be >= 0")
		}
		if strings.TrimSpace(metadata.Concurrency.Strategy) == "" {
			metadata.Concurrency.Strategy = ConcurrencyStrategyQueue
		}
		switch metadata.Concurrency.Strategy {
		case ConcurrencyStrategyQueue, ConcurrencyStrategyReplace, ConcurrencyStrategySkip, ConcurrencyStrategyFail:
		default:
			return nil, fmt.Errorf("metadata.concurrency.strategy %q must be one of [\"%s\",\"%s\",\"%s\",\"%s\"]",
				metadata.Concurrency.Strategy,
				ConcurrencyStrategyQueue,
				ConcurrencyStrategyReplace,
				ConcurrencyStrategySkip,
				ConcurrencyStrategyFail)
		}
	}

	resources := make(map[string]struct{}, len(metadata.RateLimits))
	for i := range metadata.RateLimits {
		limit := &metadata.RateLimits[i]
		resource := strings.TrimSpace(limit.Resource)
		if resource == "" {
			return nil, fmt.Errorf("metadata.rateLimits[%d].resource is required", i)
		}
		if limit.Limit <= 0 {
			return nil, fmt.Errorf("metadata.rateLimits[%d].limit must be > 0", i)
		}
		if _, err := time.ParseDuration(limit.Window); err != nil {
			return nil, fmt.Errorf("metadata.rateLimits[%d].window %q must be a valid duration: %w", i, limit.Window, err)
		}
		if _, exists := resources[resource]; exists {
			return nil, fmt.Errorf("metadata.rateLimits[%d].resource %q duplicates another rate limit", i, resource)
		}
		limit.Resource = resource
		resources[resource] = struct{}{}
	}

	return resources, nil
}

func validateTrigger(t *Trigger) error {
	switch t.Type {
	case TriggerCron, TriggerHTTP, TriggerEvent:
	default:
		return fmt.Errorf("trigger.type must be one of [%s,%s,%s]", TriggerCron, TriggerHTTP, TriggerEvent)
	}
	if t.Configuration == nil {
		t.Configuration = map[string]any{}
	}
	switch t.Type {
	case TriggerHTTP:
		if err := validateHTTPTriggerConfiguration(t.Configuration); err != nil {
			return err
		}
	case TriggerEvent:
		if err := validateEventTriggerConfiguration(t.Configuration); err != nil {
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

	if err := validateParamMappingConfiguration(cfg); err != nil {
		return err
	}

	return nil
}

func validateEventTriggerConfiguration(cfg map[string]any) error {
	rawEvents, ok := cfg["events"]
	if !ok || rawEvents == nil {
		return fmt.Errorf("trigger.configuration.events is required for event triggers")
	}
	events, ok := rawEvents.([]any)
	if !ok {
		return fmt.Errorf("trigger.configuration.events must be a non-empty list")
	}
	if len(events) == 0 {
		return fmt.Errorf("trigger.configuration.events must contain at least one pattern")
	}
	for i, rawEvent := range events {
		pattern, ok := rawEvent.(map[string]any)
		if !ok {
			return fmt.Errorf("trigger.configuration.events[%d] must be an object", i)
		}
		rawType, ok := pattern["type"]
		if !ok {
			return fmt.Errorf("trigger.configuration.events[%d].type is required", i)
		}
		eventType, ok := rawType.(string)
		if !ok {
			return fmt.Errorf("trigger.configuration.events[%d].type must be a string", i)
		}
		if strings.TrimSpace(eventType) == "" {
			return fmt.Errorf("trigger.configuration.events[%d].type must not be empty", i)
		}
		if rawSource, ok := pattern["source"]; ok && rawSource != nil {
			if _, ok := rawSource.(string); !ok {
				return fmt.Errorf("trigger.configuration.events[%d].source must be a string", i)
			}
		}
		if rawFilter, ok := pattern["filter"]; ok && rawFilter != nil {
			filter, ok := rawFilter.(map[string]any)
			if !ok {
				return fmt.Errorf("trigger.configuration.events[%d].filter must be a map of string keys and values", i)
			}
			for key, rawValue := range filter {
				if _, ok := rawValue.(string); !ok {
					return fmt.Errorf("trigger.configuration.events[%d].filter[%q] must be a string", i, key)
				}
			}
		}
	}
	if err := validateParamMappingConfiguration(cfg); err != nil {
		return err
	}
	return validateDefaultParamsConfiguration(cfg)
}

func validateParamMappingConfiguration(cfg map[string]any) error {
	rawMapping, ok := cfg["paramMapping"]
	if !ok || rawMapping == nil {
		return nil
	}
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
	return nil
}

func validateDefaultParamsConfiguration(cfg map[string]any) error {
	rawParams, ok := cfg["defaultParams"]
	if !ok || rawParams == nil {
		return nil
	}
	params, ok := rawParams.(map[string]any)
	if !ok {
		return fmt.Errorf("trigger.configuration.defaultParams must be a map of string keys and values")
	}
	for key, rawValue := range params {
		if _, ok := rawValue.(string); !ok {
			return fmt.Errorf("trigger.configuration.defaultParams[%q] must be a string", key)
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

func validateSteps(steps []Step, volumes map[string]*Volume, rateLimitResources map[string]struct{}) error {
	names, adj, err := computeStepAdjacency(steps)
	if err != nil {
		return err
	}
	if err := validateStepVolumeMounts(steps, volumes); err != nil {
		return err
	}
	if err := validateStepRateLimits(steps, rateLimitResources); err != nil {
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

func validateStepRateLimits(steps []Step, resources map[string]struct{}) error {
	for i := range steps {
		rateLimit := steps[i].RateLimit
		if rateLimit == nil {
			continue
		}
		resource := strings.TrimSpace(rateLimit.Resource)
		if resource == "" {
			return fmt.Errorf("steps[%d].rateLimit.resource is required when rateLimit is set", i)
		}
		if rateLimit.Units < 0 {
			return fmt.Errorf("steps[%d].rateLimit.units must be >= 0", i)
		}
		if rateLimit.Units == 0 {
			rateLimit.Units = 1
		}
		rateLimit.Resource = resource
		if _, ok := resources[resource]; !ok {
			return fmt.Errorf("steps[%d].rateLimit.resource %q does not match any metadata.rateLimits[].resource", i, resource)
		}
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
			if step.Kueue != nil {
				return fmt.Errorf("steps[%d].kueue is only supported for kubernetes steps", i)
			}
		}
		if step.Kueue != nil {
			queueName := strings.TrimSpace(step.Kueue.QueueName)
			switch {
			case queueName == "":
				return fmt.Errorf("steps[%d].kueue.queueName is required when kueue is set", i)
			case len(queueName) > 63:
				return fmt.Errorf("steps[%d].kueue.queueName %q must be at most 63 characters", i, queueName)
			case !kueueQueueNamePattern.MatchString(queueName):
				return fmt.Errorf("steps[%d].kueue.queueName %q must be a valid DNS-1123 label (lowercase alphanumeric, '-' or '.', starting and ending with an alphanumeric)", i, queueName)
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

// EffectiveReplaySafeForStep returns the per-task replay safety value that must
// be snapshotted when the task run is created. A job-level mark applies to every
// step; a step-level mark applies only to that step.
func (d *Definition) EffectiveReplaySafeForStep(step *Step) bool {
	if d == nil || step == nil {
		return false
	}
	return d.Metadata.ReplaySafe || step.ReplaySafe
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
		if step.Kueue != nil {
			k8sSpec.QueueName = strings.TrimSpace(step.Kueue.QueueName)
		}
		if k8sSpec.ServiceAccountName != "" || len(k8sSpec.PodAnnotations) > 0 || k8sSpec.AutomountServiceAccountToken != nil || k8sSpec.QueueName != "" {
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
		resolved.VolumeSource, err = cloneAnyMap(source.VolumeSource)
		if err != nil {
			return container.VolumeMount{}, fmt.Errorf("clone volumeSource: %w", err)
		}
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
			QueueName:                    spec.Kubernetes.QueueName,
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

func cloneAnyMap(values map[string]any) (map[string]any, error) {
	if len(values) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
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
	// PinDigests resolves each step's image tag to its content digest and folds
	// the digest (not the mutable tag) into the cache key, so a moving :latest
	// produces a cache miss rather than a stale hit.
	PinDigests bool
	// DigestTTL bounds how long a resolved tag->digest mapping is reused before
	// re-resolution. It is a perf cache: within the window a moved tag is not
	// re-detected. 0 means "re-resolve every check" (immediate moved-tag
	// detection, at a registry round-trip per check). Defaults to
	// CAESIUM_CACHE_DIGEST_TTL; only meaningful when PinDigests is on.
	DigestTTL time.Duration
}

// ResolveCacheConfig resolves the cache configuration for a step,
// considering step-level config, job-level defaults, and environment settings.
//
// envPinDigests / envDigestTTL are the global CAESIUM_CACHE_PIN_DIGESTS and
// CAESIUM_CACHE_DIGEST_TTL defaults; a job- or step-level cache entry overrides
// them. Resolution is layered env -> job -> step so the most specific
// declaration wins for each field.
func ResolveCacheConfig(stepCache, metaCache interface{}, envEnabled bool, envTTL time.Duration, envPinDigests bool, envDigestTTL time.Duration) CacheConfig {
	cfg := CacheConfig{Enabled: envEnabled, TTL: envTTL, PinDigests: envPinDigests, DigestTTL: envDigestTTL}

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
		// pinDigests is only overridden when explicitly present at this level,
		// so a job-level default flows through to steps that omit it.
		if pin, ok := v["pinDigests"]; ok {
			if b, ok := pin.(bool); ok {
				cfg.PinDigests = b
			}
		}
		// digestTTL accepts a duration string ("30s") or a numeric 0 (the only
		// numeric value that is meaningful: "re-resolve every check"). Only
		// overridden when explicitly present so a job-level default flows
		// through to steps that omit it.
		if dt, ok := v["digestTTL"]; ok {
			if d, ok := parseDigestTTL(dt); ok {
				cfg.DigestTTL = d
			}
		}
	}
}

// parseDigestTTL interprets a cache.digestTTL value. A string is parsed as a
// Go duration; a numeric value is treated as a count of nanoseconds (so the
// idiomatic `digestTTL: 0` disables the cache). Returns false for unparseable
// values so the inherited default is kept.
func parseDigestTTL(raw any) (time.Duration, bool) {
	switch n := raw.(type) {
	case string:
		if d, err := time.ParseDuration(n); err == nil {
			return d, true
		}
	case int:
		return time.Duration(n), true
	case int64:
		return time.Duration(n), true
	case float64:
		return time.Duration(int64(n)), true
	}
	return 0, false
}
