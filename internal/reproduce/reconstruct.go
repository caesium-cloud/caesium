// Package reproduce reconstructs and executes historical task descriptors for
// the caesium reproduce CLI.
package reproduce

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	pkgjobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
)

const (
	WarningSecretOmitted       = "secret_omitted"
	WarningDegradedImagePull   = "degraded_image_pull"
	WarningOutputRefUnresolved = "output_ref_unresolved"
	WarningOutputMissingName   = "predecessor_output_missing_name"
	WarningMountNotRemapped    = "mount_not_remapped"
	WarningMountSkipped        = "mount_skipped"
	WarningRetryNotApplied     = "retry_policy_not_applied"
)

// Assignment is a user-supplied KEY=VALUE override.
type Assignment struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MountRemap maps a recorded mount source to a local source.
type MountRemap struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Warning is included in machine-readable output and mirrored to stderr by the
// command layer.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SecretOmission records a secret env var that was deliberately not
// reconstructed.
type SecretOmission struct {
	EnvKey string `json:"env_key"`
	Ref    string `json:"ref,omitempty"`
}

// RetryPolicy records the historical retry knobs. Reproduce surfaces them but
// runs a single local attempt.
type RetryPolicy struct {
	RetryCount   int    `json:"retry_count"`
	RetryDelay   string `json:"retry_delay,omitempty"`
	RetryBackoff bool   `json:"retry_backoff"`
}

// Envelope is the reconstructed local execution envelope.
type Envelope struct {
	TaskName            string            `json:"task"`
	JobID               string            `json:"job_id,omitempty"`
	JobAlias            string            `json:"job_alias,omitempty"`
	BaselineRunID       string            `json:"baseline_run_id,omitempty"`
	ReplaySafe          bool              `json:"replay_safe"`
	CapturedAt          time.Time         `json:"captured_at,omitempty"`
	Image               string            `json:"image"`
	RecordedImage       string            `json:"recorded_image,omitempty"`
	ResolvedImageDigest string            `json:"resolved_image_digest,omitempty"`
	ImagePullMode       string            `json:"image_pull_mode"`
	Command             []string          `json:"command,omitempty"`
	WorkDir             string            `json:"workdir,omitempty"`
	Env                 map[string]string `json:"env"`
	Mounts              []container.Mount `json:"mounts,omitempty"`
	Timeout             string            `json:"timeout,omitempty"`
	Platform            string            `json:"platform,omitempty"`
	RecordedRetryPolicy *RetryPolicy      `json:"recorded_retry_policy,omitempty"`
	OmittedSecrets      []SecretOmission  `json:"omitted_secrets,omitempty"`
	Warnings            []Warning         `json:"warnings,omitempty"`
}

// ReconstructOptions controls local envelope reconstruction.
type ReconstructOptions struct {
	SetParams  []Assignment
	SetEnv     []Assignment
	Mounts     []MountRemap
	Timeout    time.Duration
	Platform   string
	ReplaySafe bool
}

// Descriptor mirrors descriptor schema v1 without importing internal/models.
type Descriptor struct {
	SchemaVersion int       `json:"schemaVersion"`
	CapturedAt    time.Time `json:"capturedAt"`

	Baseline struct {
		JobID         string `json:"jobId"`
		JobAlias      string `json:"jobAlias"`
		TaskID        string `json:"taskId"`
		TaskName      string `json:"taskName"`
		BaselineRunID string `json:"baselineRunId"`
		ReplaySafe    bool   `json:"replaySafe"`
	} `json:"baseline"`
	DAG struct {
		Predecessors       []EdgeRef                    `json:"predecessors,omitempty"`
		PredecessorOutputs map[string]map[string]string `json:"predecessorOutputs,omitempty"`
	} `json:"dag"`
	Run struct {
		Params map[string]string `json:"params,omitempty"`
	} `json:"run"`
	Runtime struct {
		Engine              string            `json:"engine"`
		Image               string            `json:"image"`
		ResolvedImageDigest string            `json:"resolvedImageDigest,omitempty"`
		Command             []string          `json:"command,omitempty"`
		CommandRaw          string            `json:"commandRaw,omitempty"`
		WorkDir             string            `json:"workdir,omitempty"`
		TaskType            string            `json:"taskType,omitempty"`
		NodeSelector        map[string]string `json:"nodeSelector,omitempty"`
		RetryCount          int               `json:"retryCount"`
		RetryDelay          time.Duration     `json:"retryDelay"`
		RetryBackoff        bool              `json:"retryBackoff"`
	} `json:"runtime"`
	Timing struct {
		TaskTimeout time.Duration `json:"taskTimeout"`
		RunTimeout  time.Duration `json:"runTimeout"`
	} `json:"timing"`
	Schema struct {
		InputSchema     map[string]map[string]any `json:"inputSchema,omitempty"`
		OutputSchema    map[string]any            `json:"outputSchema,omitempty"`
		ValidationMode  string                    `json:"validationMode,omitempty"`
		RawInputSchema  json.RawMessage           `json:"-"`
		RawOutputSchema json.RawMessage           `json:"-"`
	} `json:"schema"`

	ContainerSpec  container.Spec            `json:"containerSpec"`
	KubernetesSpec *container.KubernetesSpec `json:"kubernetesSpec,omitempty"`
	SecretRefs     []SecretRef               `json:"secretRefs,omitempty"`
}

// EdgeRef identifies a DAG predecessor or successor.
type EdgeRef struct {
	TaskID   string `json:"taskId"`
	TaskName string `json:"taskName,omitempty"`
}

// SecretRef mirrors the descriptor's secret metadata relevant to local
// omission warnings.
type SecretRef struct {
	Ref    string `json:"ref"`
	EnvKey string `json:"envKey,omitempty"`
}

// DecodeDescriptor decodes raw descriptor JSON into the local mirror type.
func DecodeDescriptor(raw []byte) (*Descriptor, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("descriptor is empty")
	}
	var desc Descriptor
	if err := json.Unmarshal(raw, &desc); err != nil {
		return nil, fmt.Errorf("decode descriptor: %w", err)
	}
	if desc.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported descriptor schemaVersion %d", desc.SchemaVersion)
	}
	if strings.TrimSpace(desc.Runtime.Image) == "" {
		return nil, fmt.Errorf("descriptor runtime.image is required")
	}
	if strings.TrimSpace(desc.Baseline.TaskName) == "" {
		desc.Baseline.TaskName = "task"
	}
	return &desc, nil
}

// Reconstruct builds the local envelope from a descriptor and CLI overrides.
func Reconstruct(desc *Descriptor, opts ReconstructOptions) (*Envelope, error) {
	if desc == nil {
		return nil, fmt.Errorf("descriptor is required")
	}

	warnings := make([]Warning, 0)
	env := make(map[string]string)
	omitted := make([]SecretOmission, 0)
	secretRefs := secretRefsByEnv(desc.SecretRefs)

	for _, key := range sortedKeys(desc.ContainerSpec.Env) {
		value := desc.ContainerSpec.Env[key]
		if isSecretRef(value) {
			omitted = appendSecretOmission(omitted, SecretOmission{EnvKey: key, Ref: firstNonEmpty(secretRefs[key], value)})
			continue
		}
		env[key] = value
	}
	for _, ref := range desc.SecretRefs {
		if ref.EnvKey == "" {
			continue
		}
		if _, ok := desc.ContainerSpec.Env[ref.EnvKey]; !ok {
			omitted = appendSecretOmission(omitted, SecretOmission{EnvKey: ref.EnvKey, Ref: ref.Ref})
		}
	}
	if len(omitted) > 0 {
		sort.Slice(omitted, func(i, j int) bool { return omitted[i].EnvKey < omitted[j].EnvKey })
		warnings = append(warnings, Warning{
			Code:    WarningSecretOmitted,
			Message: fmt.Sprintf("secret refs omitted by default: %s", strings.Join(secretEnvKeys(omitted), ", ")),
		})
	}

	for _, key := range sortedKeys(desc.Run.Params) {
		env[paramEnvKey(key)] = desc.Run.Params[key]
	}

	outputEnv, outputWarnings := predecessorOutputEnv(desc)
	warnings = append(warnings, outputWarnings...)
	for _, key := range sortedKeys(outputEnv) {
		env[key] = outputEnv[key]
	}

	for _, assignment := range opts.SetParams {
		if strings.TrimSpace(assignment.Key) == "" {
			return nil, fmt.Errorf("--set key cannot be empty")
		}
		env[paramEnvKey(assignment.Key)] = assignment.Value
	}
	for _, assignment := range opts.SetEnv {
		if strings.TrimSpace(assignment.Key) == "" {
			return nil, fmt.Errorf("--set-env key cannot be empty")
		}
		env[assignment.Key] = assignment.Value
	}

	image, pullMode, imageWarnings := imageReference(desc.Runtime.Image, desc.Runtime.ResolvedImageDigest)
	warnings = append(warnings, imageWarnings...)

	mounts, mountWarnings := reconstructMounts(desc.ContainerSpec, opts.Mounts)
	warnings = append(warnings, mountWarnings...)

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = desc.Timing.TaskTimeout
	}

	command := slices.Clone(desc.Runtime.Command)
	if len(command) == 0 && strings.TrimSpace(desc.Runtime.CommandRaw) != "" {
		command = []string{desc.Runtime.CommandRaw}
	}

	workdir := desc.ContainerSpec.WorkDir
	if workdir == "" {
		workdir = desc.Runtime.WorkDir
	}

	var retryPolicy *RetryPolicy
	if desc.Runtime.RetryCount > 0 || desc.Runtime.RetryDelay > 0 || desc.Runtime.RetryBackoff {
		retryPolicy = &RetryPolicy{
			RetryCount:   desc.Runtime.RetryCount,
			RetryDelay:   durationString(desc.Runtime.RetryDelay),
			RetryBackoff: desc.Runtime.RetryBackoff,
		}
		warnings = append(warnings, Warning{
			Code:    WarningRetryNotApplied,
			Message: "recorded retry policy is surfaced but not applied; reproduce runs one local attempt",
		})
	}

	return &Envelope{
		TaskName:            desc.Baseline.TaskName,
		JobID:               desc.Baseline.JobID,
		JobAlias:            desc.Baseline.JobAlias,
		BaselineRunID:       desc.Baseline.BaselineRunID,
		ReplaySafe:          opts.ReplaySafe || desc.Baseline.ReplaySafe,
		CapturedAt:          desc.CapturedAt,
		Image:               image,
		RecordedImage:       desc.Runtime.Image,
		ResolvedImageDigest: desc.Runtime.ResolvedImageDigest,
		ImagePullMode:       pullMode,
		Command:             command,
		WorkDir:             workdir,
		Env:                 env,
		Mounts:              mounts,
		Timeout:             durationString(timeout),
		Platform:            strings.TrimSpace(opts.Platform),
		RecordedRetryPolicy: retryPolicy,
		OmittedSecrets:      omitted,
		Warnings:            warnings,
	}, nil
}

// BuildDefinition synthesizes the one-step definition consumed by localrun.
func BuildDefinition(desc *Descriptor, env *Envelope, timeout time.Duration) *pkgjobdef.Definition {
	alias := sanitizeAlias(firstNonEmpty(desc.Baseline.JobAlias, "reproduce-"+desc.Baseline.TaskName))
	validationMode := desc.Schema.ValidationMode
	if validationMode != pkgjobdef.SchemaValidationWarn && validationMode != pkgjobdef.SchemaValidationFail {
		validationMode = ""
	}
	return &pkgjobdef.Definition{
		APIVersion: pkgjobdef.APIVersionV1,
		Kind:       pkgjobdef.KindJob,
		Metadata: pkgjobdef.Metadata{
			Alias:            alias,
			TaskTimeout:      timeout,
			SchemaValidation: validationMode,
		},
		Trigger: pkgjobdef.Trigger{
			Type: pkgjobdef.TriggerCron,
			Configuration: map[string]any{
				"cron": "0 0 1 1 *",
			},
		},
		Steps: []pkgjobdef.Step{{
			Name:         firstNonEmpty(desc.Baseline.TaskName, "task"),
			Engine:       pkgjobdef.EngineDocker,
			Image:        env.Image,
			Command:      slices.Clone(env.Command),
			OutputSchema: cloneAnyMap(desc.Schema.OutputSchema),
			Spec: container.Spec{
				Env:     cloneStringMap(env.Env),
				WorkDir: env.WorkDir,
				Mounts:  slices.Clone(env.Mounts),
			},
		}},
	}
}

func predecessorOutputEnv(desc *Descriptor) (map[string]string, []Warning) {
	byName := make(map[string]map[string]string)
	used := make(map[string]struct{})
	warnings := make([]Warning, 0)
	for _, pred := range desc.DAG.Predecessors {
		outputs := desc.DAG.PredecessorOutputs[pred.TaskID]
		if len(outputs) == 0 {
			continue
		}
		name := firstNonEmpty(pred.TaskName, pred.TaskID)
		byName[name] = cloneStringMap(outputs)
		used[pred.TaskID] = struct{}{}
	}
	for id, outputs := range desc.DAG.PredecessorOutputs {
		if len(outputs) == 0 {
			continue
		}
		if _, ok := used[id]; ok {
			continue
		}
		byName[id] = cloneStringMap(outputs)
		warnings = append(warnings, Warning{
			Code:    WarningOutputMissingName,
			Message: fmt.Sprintf("predecessor output %s had no matching predecessor name; using UUID in CAESIUM_OUTPUT_* env", id),
		})
	}

	env := pkgtask.BuildOutputEnv(byName)
	for stepName, outputs := range byName {
		for key, value := range outputs {
			if ref, ok := pkgtask.DecodeOutputRef(value); ok {
				envKey := "CAESIUM_OUTPUT_" + pkgtask.NormalizeStepName(stepName) + "_" + pkgtask.NormalizeStepName(key)
				warnings = append(warnings, Warning{
					Code:    WarningOutputRefUnresolved,
					Message: fmt.Sprintf("output ref %s points at recorded path %s; ensure local storage is mounted or remapped", envKey, ref.Path),
				})
			}
		}
	}
	sortWarnings(warnings)
	return env, warnings
}

func reconstructMounts(spec container.Spec, remaps []MountRemap) ([]container.Mount, []Warning) {
	remapBySource := make(map[string]string, len(remaps))
	for _, remap := range remaps {
		if strings.TrimSpace(remap.From) == "" || strings.TrimSpace(remap.To) == "" {
			continue
		}
		remapBySource[remap.From] = remap.To
	}

	var mounts []container.Mount
	var warnings []Warning
	for _, mount := range spec.Mounts {
		mounts = append(mounts, applyMountRemap(mount, remapBySource, &warnings))
	}
	for _, mount := range spec.ResolvedVolumeMounts {
		switch mount.Type {
		case container.VolumeMountTypeBind, container.VolumeMountTypeVolume, container.VolumeMountTypeTmpfs:
			converted := container.Mount{
				Type:     container.MountType(mount.Type),
				Source:   mount.Source,
				Target:   mount.Target,
				ReadOnly: mount.ReadOnly,
			}
			mounts = append(mounts, applyMountRemap(converted, remapBySource, &warnings))
		case container.VolumeMountTypePVC, container.VolumeMountTypeClaimTemplate, container.VolumeMountTypeVolumeSource:
			warnings = append(warnings, Warning{
				Code:    WarningMountSkipped,
				Message: fmt.Sprintf("skipping Kubernetes-only volume mount %s at %s for local Docker reproduce", firstNonEmpty(mount.Name, string(mount.Type)), mount.Target),
			})
		default:
			warnings = append(warnings, Warning{
				Code:    WarningMountSkipped,
				Message: fmt.Sprintf("skipping unsupported volume mount %s at %s for local Docker reproduce", firstNonEmpty(mount.Name, string(mount.Type)), mount.Target),
			})
		}
	}
	sortWarnings(warnings)
	return mounts, warnings
}

func applyMountRemap(mount container.Mount, remapBySource map[string]string, warnings *[]Warning) container.Mount {
	if mount.Type != container.MountTypeBind || strings.TrimSpace(mount.Source) == "" {
		return mount
	}
	if replacement := remapBySource[mount.Source]; replacement != "" {
		mount.Source = replacement
		return mount
	}
	*warnings = append(*warnings, Warning{
		Code:    WarningMountNotRemapped,
		Message: fmt.Sprintf("bind mount %s -> %s was not remapped; pass --mount %s=<local-path> if the recorded source is not available", mount.Source, mount.Target, mount.Source),
	})
	return mount
}

func imageReference(recorded, digest string) (string, string, []Warning) {
	recorded = strings.TrimSpace(recorded)
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return recorded, "DEGRADED", []Warning{{
			Code:    WarningDegradedImagePull,
			Message: fmt.Sprintf("no resolved image digest recorded for %s; pulling mutable tag is DEGRADED", recorded),
		}}
	}
	base := recorded
	if before, _, ok := strings.Cut(recorded, "@"); ok {
		base = before
	}
	return base + "@" + digest, "DIGEST", nil
}

func paramEnvKey(key string) string {
	return "CAESIUM_PARAM_" + strings.ToUpper(key)
}

func isSecretRef(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "secret://")
}

func secretRefsByEnv(refs []SecretRef) map[string]string {
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if ref.EnvKey != "" {
			out[ref.EnvKey] = ref.Ref
		}
	}
	return out
}

func appendSecretOmission(omitted []SecretOmission, next SecretOmission) []SecretOmission {
	for _, existing := range omitted {
		if existing.EnvKey == next.EnvKey {
			return omitted
		}
	}
	return append(omitted, next)
}

func secretEnvKeys(omitted []SecretOmission) []string {
	keys := make([]string, 0, len(omitted))
	for _, omission := range omitted {
		keys = append(keys, omission.EnvKey)
	}
	return keys
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

func durationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func sanitizeAlias(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	alias := strings.Trim(b.String(), "-.")
	if alias == "" {
		return "reproduce-task"
	}
	if !strings.HasPrefix(alias, "reproduce-") {
		alias = "reproduce-" + alias
	}
	if len(alias) > 63 {
		alias = strings.Trim(alias[:63], "-.")
	}
	return alias
}

func sortWarnings(warnings []Warning) {
	sort.SliceStable(warnings, func(i, j int) bool {
		if warnings[i].Code != warnings[j].Code {
			return warnings[i].Code < warnings[j].Code
		}
		return warnings[i].Message < warnings[j].Message
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
