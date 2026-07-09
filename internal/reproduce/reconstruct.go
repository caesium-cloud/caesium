// Package reproduce reconstructs and executes historical task descriptors for
// the caesium reproduce CLI.
package reproduce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"runtime"
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
	WarningSecretResolveFailed = "secret_resolution_failed"
	WarningSecretProvider      = "secret_provider_mismatch"
	WarningSecretDrift         = "secret_drift"
	WarningDegradedImagePull   = "degraded_image_pull"
	WarningImageOverridden     = "image_overridden"
	WarningOutputRefUnresolved = "output_ref_unresolved"
	WarningOutputMissingName   = "predecessor_output_missing_name"
	WarningMountNotRemapped    = "mount_not_remapped"
	WarningMountSkipped        = "mount_skipped"
	WarningRetryNotApplied     = "retry_policy_not_applied"
	WarningWorkloadIdentity    = "workload_identity_listed_not_applied"
	WarningCrossArchEmulation  = "cross_arch_emulation"
	WarningResourceLimits      = "resource_limits_not_reproduced"
	WarningWallClock           = "wall_clock_not_reproduced"
	WarningExternalState       = "external_state_not_reproduced"
	WarningSideEffects         = "side_effects_not_suppressed"
)

const (
	FidelityFaithful         = "faithful"
	FidelityDegraded         = "degraded"
	FidelityOverridden       = "overridden"
	FidelityNotReproduced    = "not_reproduced"
	FidelityListedNotApplied = "listed_not_applied"
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

// FidelityDimension records how faithfully one descriptor dimension can be
// reproduced on the operator's local Docker daemon.
type FidelityDimension struct {
	Dimension string   `json:"dimension"`
	Status    string   `json:"status"`
	Details   []string `json:"details,omitempty"`
}

// FidelitySummary is the structured machine-readable fidelity block emitted by
// reproduce and mirrored in compact human output.
type FidelitySummary struct {
	Dimensions []FidelityDimension `json:"dimensions"`
}

// SecretOmission records a secret env var that was deliberately not
// reconstructed.
type SecretOmission struct {
	EnvKey string `json:"env_key"`
	Ref    string `json:"ref,omitempty"`
}

// SecretResolution records a locally resolved secret without exposing its
// value outside Envelope.Env.
type SecretResolution struct {
	EnvKey   string `json:"env_key"`
	Ref      string `json:"ref,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// SecretIdentity is the provider identity subset reproduce can compare without
// importing provider implementations.
type SecretIdentity struct {
	Provider           string            `json:"provider,omitempty"`
	Ref                string            `json:"ref,omitempty"`
	Version            string            `json:"version,omitempty"`
	ResourceVersion    string            `json:"resourceVersion,omitempty"`
	Namespace          string            `json:"namespace,omitempty"`
	Name               string            `json:"name,omitempty"`
	Key                string            `json:"key,omitempty"`
	KeyID              string            `json:"keyId,omitempty"`
	HMACSHA256         string            `json:"hmacSha256,omitempty"`
	Verifiable         bool              `json:"verifiable"`
	UnverifiableReason string            `json:"unverifiableReason,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

// SecretResolver resolves local secret refs. The command layer adapts this to
// internal/jobdef/secret so this package stays independent of provider deps.
type SecretResolver interface {
	ResolveWithIdentity(ctx context.Context, ref string) (string, SecretIdentity, error)
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
	TaskName            string             `json:"task"`
	JobID               string             `json:"job_id,omitempty"`
	JobAlias            string             `json:"job_alias,omitempty"`
	BaselineRunID       string             `json:"baseline_run_id,omitempty"`
	ReplaySafe          bool               `json:"replay_safe"`
	CapturedAt          time.Time          `json:"captured_at,omitempty"`
	Image               string             `json:"image"`
	RecordedImage       string             `json:"recorded_image,omitempty"`
	ResolvedImageDigest string             `json:"resolved_image_digest,omitempty"`
	ImagePullMode       string             `json:"image_pull_mode"`
	ImageOverride       string             `json:"image_override,omitempty"`
	ImageOverridden     bool               `json:"image_overridden,omitempty"`
	Command             []string           `json:"command,omitempty"`
	WorkDir             string             `json:"workdir,omitempty"`
	Env                 map[string]string  `json:"env"`
	Mounts              []container.Mount  `json:"mounts,omitempty"`
	Timeout             string             `json:"timeout,omitempty"`
	Platform            string             `json:"platform,omitempty"`
	RecordedRetryPolicy *RetryPolicy       `json:"recorded_retry_policy,omitempty"`
	OmittedSecrets      []SecretOmission   `json:"omitted_secrets,omitempty"`
	ResolvedSecrets     []SecretResolution `json:"resolved_secrets,omitempty"`
	Fidelity            *FidelitySummary   `json:"fidelity,omitempty"`
	Warnings            []Warning          `json:"warnings,omitempty"`
}

// ReconstructOptions controls local envelope reconstruction.
type ReconstructOptions struct {
	Context        context.Context
	SetParams      []Assignment
	SetEnv         []Assignment
	Mounts         []MountRemap
	Timeout        time.Duration
	Platform       string
	ReplaySafe     bool
	ImageOverride  string
	ResolveSecrets bool
	SecretResolver SecretResolver
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
	Ref                string         `json:"ref"`
	EnvKey             string         `json:"envKey,omitempty"`
	Provider           string         `json:"provider,omitempty"`
	Identity           map[string]any `json:"identity,omitempty"`
	Verifiable         bool           `json:"verifiable"`
	UnverifiableReason string         `json:"unverifiableReason,omitempty"`
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
	resolvedSecrets := make([]SecretResolution, 0)
	secretRefs := secretRefsByEnv(desc.SecretRefs)
	processedSecretEnv := make(map[string]struct{})
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}

	for _, key := range sortedKeys(desc.ContainerSpec.Env) {
		value := desc.ContainerSpec.Env[key]
		if isSecretRef(value) {
			ref := secretRefs[key]
			ref.EnvKey = key
			ref.Ref = firstNonEmpty(ref.Ref, value)
			processedSecretEnv[key] = struct{}{}
			if resolved, resolutionWarnings, ok := resolveRecordedSecret(ctx, ref, opts); ok {
				env[key] = resolved.Value
				resolvedSecrets = appendSecretResolution(resolvedSecrets, resolved.Resolution)
				warnings = append(warnings, resolutionWarnings...)
				continue
			} else {
				warnings = append(warnings, resolutionWarnings...)
			}
			omitted = appendSecretOmission(omitted, SecretOmission{EnvKey: key, Ref: ref.Ref})
			continue
		}
		env[key] = value
	}
	for _, ref := range sortedSecretRefs(desc.SecretRefs) {
		if ref.EnvKey == "" {
			continue
		}
		if _, ok := processedSecretEnv[ref.EnvKey]; ok {
			continue
		}
		if _, ok := desc.ContainerSpec.Env[ref.EnvKey]; !ok {
			if resolved, resolutionWarnings, ok := resolveRecordedSecret(ctx, ref, opts); ok {
				env[ref.EnvKey] = resolved.Value
				resolvedSecrets = appendSecretResolution(resolvedSecrets, resolved.Resolution)
				warnings = append(warnings, resolutionWarnings...)
				continue
			} else {
				warnings = append(warnings, resolutionWarnings...)
			}
			omitted = appendSecretOmission(omitted, SecretOmission{EnvKey: ref.EnvKey, Ref: ref.Ref})
		}
	}
	if len(omitted) > 0 && !opts.ResolveSecrets {
		sort.Slice(omitted, func(i, j int) bool { return omitted[i].EnvKey < omitted[j].EnvKey })
		warnings = append(warnings, Warning{
			Code:    WarningSecretOmitted,
			Message: fmt.Sprintf("secret refs omitted by default: %s", strings.Join(secretEnvKeys(omitted), ", ")),
		})
	} else if len(omitted) > 0 {
		sort.Slice(omitted, func(i, j int) bool { return omitted[i].EnvKey < omitted[j].EnvKey })
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

	image, pullMode, imageWarnings := imageReference(desc.Runtime.Image, desc.Runtime.ResolvedImageDigest, opts.ImageOverride)
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

	envelope := &Envelope{
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
		ImageOverride:       strings.TrimSpace(opts.ImageOverride),
		ImageOverridden:     pullMode == "OVERRIDDEN",
		Command:             command,
		WorkDir:             workdir,
		Env:                 env,
		Mounts:              mounts,
		Timeout:             durationString(timeout),
		Platform:            strings.TrimSpace(opts.Platform),
		RecordedRetryPolicy: retryPolicy,
		OmittedSecrets:      omitted,
		ResolvedSecrets:     resolvedSecrets,
	}
	fidelity, fidelityWarnings := buildFidelitySummary(desc, envelope, opts, omitted, resolvedSecrets)
	warnings = append(warnings, fidelityWarnings...)
	sortWarnings(warnings)
	envelope.Fidelity = fidelity
	envelope.Warnings = warnings
	return envelope, nil
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
			Name: firstNonEmpty(desc.Baseline.TaskName, "task"),
			// Type must be set explicitly: the YAML unmarshaller defaults it,
			// but this definition is constructed in Go and localrun validates.
			Type:         pkgjobdef.StepTypeTask,
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

func imageReference(recorded, digest, override string) (string, string, []Warning) {
	recorded = strings.TrimSpace(recorded)
	digest = strings.TrimSpace(digest)
	override = strings.TrimSpace(override)
	if override != "" {
		return override, "OVERRIDDEN", []Warning{{
			Code:    WarningImageOverridden,
			Message: fmt.Sprintf("OVERRIDDEN: using image override %s instead of recorded image %s", override, recorded),
		}}
	}
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

func buildFidelitySummary(desc *Descriptor, env *Envelope, opts ReconstructOptions, omitted []SecretOmission, resolved []SecretResolution) (*FidelitySummary, []Warning) {
	var dimensions []FidelityDimension
	var warnings []Warning
	add := func(dimension, status string, details ...string) {
		dimensions = append(dimensions, FidelityDimension{
			Dimension: dimension,
			Status:    status,
			Details:   cleanDetails(details),
		})
	}

	if env.ImagePullMode == "OVERRIDDEN" {
		details := []string{fmt.Sprintf("OVERRIDDEN: local run uses image override %s instead of recorded image %s", env.Image, desc.Runtime.Image)}
		if strings.TrimSpace(desc.Runtime.ResolvedImageDigest) != "" {
			details = append(details, fmt.Sprintf("recorded digest %s is not used for the override image", desc.Runtime.ResolvedImageDigest))
		}
		add("image_content", FidelityOverridden, details...)
	} else if strings.TrimSpace(desc.Runtime.ResolvedImageDigest) == "" {
		add("image_content", FidelityDegraded, fmt.Sprintf("no resolved digest recorded for %s; local pull uses the mutable tag", desc.Runtime.Image))
	} else {
		add("image_content", FidelityFaithful, fmt.Sprintf("image pulled by recorded digest %s", desc.Runtime.ResolvedImageDigest))
	}
	add("command_argv_workdir", FidelityFaithful, "recorded command, argv, and workdir are used verbatim")
	add("literal_env_vars", FidelityFaithful, "recorded literal env vars are restored")
	add("run_params", FidelityFaithful, "recorded run params are restored as CAESIUM_PARAM_*")

	if details := outputRefFidelityDetails(desc); len(details) > 0 {
		add("predecessor_outputs", FidelityDegraded, details...)
	} else {
		add("predecessor_outputs", FidelityFaithful, "recorded scalar predecessor outputs are restored as CAESIUM_OUTPUT_*")
	}
	add("schema_config", FidelityFaithful, "recorded output schema and validation mode are applied to the local run")

	if len(omitted) > 0 {
		if opts.ResolveSecrets {
			add("secret_values", FidelityNotReproduced, fmt.Sprintf("secret refs unresolved locally and omitted: %s", strings.Join(secretEnvKeys(omitted), ", ")))
		} else {
			add("secret_values", FidelityNotReproduced, fmt.Sprintf("secret refs omitted by default: %s", strings.Join(secretEnvKeys(omitted), ", ")))
		}
	} else if len(resolved) > 0 {
		add("secret_values", FidelityDegraded, fmt.Sprintf("secret refs resolved from local providers for %s; local values may differ from the recorded baseline", strings.Join(resolvedSecretEnvKeys(resolved), ", ")))
	} else {
		add("secret_values", FidelityFaithful, "no recorded secret refs were present")
	}

	if details := mountFidelityDetails(desc.ContainerSpec, opts.Mounts); len(details) > 0 {
		add("host_mounts_volumes", FidelityNotReproduced, details...)
	} else {
		add("host_mounts_volumes", FidelityFaithful, "no recorded host mounts or volumes were present")
	}

	if details := workloadIdentityDetails(desc); len(details) > 0 {
		add("engine_workload_identity", FidelityListedNotApplied, details...)
		warnings = append(warnings, Warning{
			Code:    WarningWorkloadIdentity,
			Message: "engine and workload-identity fields have no local Docker equivalent; listed in fidelity summary and not applied",
		})
	} else {
		add("engine_workload_identity", FidelityFaithful, "recorded task has no non-Docker engine or workload-identity fields")
	}

	if requestedArch := platformArch(firstNonEmpty(opts.Platform, env.Platform)); requestedArch != "" && requestedArch != runtime.GOARCH {
		add("cpu_architecture", FidelityDegraded, fmt.Sprintf("requested platform %s differs from local architecture %s; Docker may use emulation", firstNonEmpty(opts.Platform, env.Platform), runtime.GOARCH))
		warnings = append(warnings, Warning{
			Code:    WarningCrossArchEmulation,
			Message: fmt.Sprintf("requested platform %s differs from local architecture %s; cross-arch emulation is DEGRADED", firstNonEmpty(opts.Platform, env.Platform), runtime.GOARCH),
		})
	} else if platform := strings.TrimSpace(firstNonEmpty(opts.Platform, env.Platform)); platform != "" {
		add("cpu_architecture", FidelityFaithful, fmt.Sprintf("requested platform %s matches local architecture %s", platform, runtime.GOARCH))
	} else {
		add("cpu_architecture", FidelityFaithful, "no explicit platform override requested; local Docker selects the native architecture")
	}

	add("resource_limits", FidelityNotReproduced, "descriptor schema v1 does not record resource limits")
	warnings = append(warnings, Warning{
		Code:    WarningResourceLimits,
		Message: "resource limits are not reproduced because descriptor schema v1 does not record them",
	})

	add("wall_clock_time", FidelityNotReproduced, "task observes the current local wall clock")
	warnings = append(warnings, Warning{
		Code:    WarningWallClock,
		Message: "wall clock is not reproduced; the task runs now",
	})

	add("external_system_state", FidelityNotReproduced, "databases, APIs, object stores, and other external systems are not rewound")
	warnings = append(warnings, Warning{
		Code:    WarningExternalState,
		Message: "external system state is not reproduced; the task sees whatever is reachable now",
	})

	add("side_effects", FidelityNotReproduced, "side effects are not suppressed under local reproduce")
	warnings = append(warnings, Warning{
		Code:    WarningSideEffects,
		Message: "side effects are not suppressed; the container can affect systems reachable from this machine",
	})

	return &FidelitySummary{Dimensions: dimensions}, warnings
}

func outputRefFidelityDetails(desc *Descriptor) []string {
	byName := make(map[string]map[string]string)
	used := make(map[string]struct{})
	for _, pred := range desc.DAG.Predecessors {
		if outputs := desc.DAG.PredecessorOutputs[pred.TaskID]; len(outputs) > 0 {
			byName[firstNonEmpty(pred.TaskName, pred.TaskID)] = outputs
			used[pred.TaskID] = struct{}{}
		}
	}
	for id, outputs := range desc.DAG.PredecessorOutputs {
		if len(outputs) == 0 {
			continue
		}
		if _, ok := used[id]; ok {
			continue
		}
		byName[id] = outputs
	}

	var details []string
	for stepName, outputs := range byName {
		for key, value := range outputs {
			ref, ok := pkgtask.DecodeOutputRef(value)
			if !ok {
				continue
			}
			envKey := "CAESIUM_OUTPUT_" + pkgtask.NormalizeStepName(stepName) + "_" + pkgtask.NormalizeStepName(key)
			details = append(details, fmt.Sprintf("%s points at recorded path %s with digest %s; local storage must be mounted or remapped", envKey, ref.Path, ref.Digest))
		}
	}
	sort.Strings(details)
	return details
}

func mountFidelityDetails(spec container.Spec, remaps []MountRemap) []string {
	if len(spec.Mounts) == 0 && len(spec.ResolvedVolumeMounts) == 0 {
		return nil
	}
	remapBySource := make(map[string]string, len(remaps))
	for _, remap := range remaps {
		if strings.TrimSpace(remap.From) != "" && strings.TrimSpace(remap.To) != "" {
			remapBySource[remap.From] = remap.To
		}
	}

	var details []string
	for _, mount := range spec.Mounts {
		details = append(details, mountDetail(container.Mount{
			Type:     mount.Type,
			Source:   mount.Source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		}, remapBySource))
	}
	for _, mount := range spec.ResolvedVolumeMounts {
		switch mount.Type {
		case container.VolumeMountTypeBind, container.VolumeMountTypeVolume, container.VolumeMountTypeTmpfs:
			details = append(details, mountDetail(container.Mount{
				Type:     container.MountType(mount.Type),
				Source:   mount.Source,
				Target:   mount.Target,
				ReadOnly: mount.ReadOnly,
			}, remapBySource))
		case container.VolumeMountTypePVC, container.VolumeMountTypeClaimTemplate, container.VolumeMountTypeVolumeSource:
			details = append(details, fmt.Sprintf("Kubernetes-only %s mount %s at %s is skipped under local Docker", mount.Type, firstNonEmpty(mount.Name, mount.Source), mount.Target))
		default:
			details = append(details, fmt.Sprintf("unsupported %s mount %s at %s is skipped under local Docker", mount.Type, firstNonEmpty(mount.Name, mount.Source), mount.Target))
		}
	}
	sort.Strings(details)
	return details
}

func mountDetail(mount container.Mount, remapBySource map[string]string) string {
	if mount.Type == container.MountTypeBind {
		if replacement := remapBySource[mount.Source]; replacement != "" {
			return fmt.Sprintf("bind mount %s -> %s is remapped to local source %s", mount.Source, mount.Target, replacement)
		}
		return fmt.Sprintf("bind mount %s -> %s uses the recorded host path unless remapped", mount.Source, mount.Target)
	}
	if mount.Type == container.MountTypeVolume {
		return fmt.Sprintf("volume %s -> %s uses local Docker volume contents", mount.Source, mount.Target)
	}
	if mount.Type == container.MountTypeTmpfs {
		return fmt.Sprintf("tmpfs mount at %s is recreated locally, not restored from baseline state", mount.Target)
	}
	return fmt.Sprintf("%s mount %s -> %s is best-effort under local Docker", mount.Type, mount.Source, mount.Target)
}

func workloadIdentityDetails(desc *Descriptor) []string {
	var details []string
	if engine := strings.TrimSpace(desc.Runtime.Engine); engine != "" && engine != "docker" {
		details = append(details, fmt.Sprintf("recorded engine %q runs under local Docker", engine))
	}
	if len(desc.Runtime.NodeSelector) > 0 {
		details = append(details, "node selector "+formatStringMap(desc.Runtime.NodeSelector))
	}
	if k8s := firstKubernetesSpec(desc); k8s != nil {
		if k8s.ServiceAccountName != "" {
			details = append(details, "serviceAccountName "+k8s.ServiceAccountName)
		}
		if len(k8s.PodAnnotations) > 0 {
			details = append(details, "pod annotations "+formatStringMap(k8s.PodAnnotations))
		}
		if k8s.AutomountServiceAccountToken != nil {
			details = append(details, fmt.Sprintf("automountServiceAccountToken %t", *k8s.AutomountServiceAccountToken))
		}
		if k8s.QueueName != "" {
			details = append(details, "Kueue queue "+k8s.QueueName)
		}
	}
	sort.Strings(details)
	return details
}

func firstKubernetesSpec(desc *Descriptor) *container.KubernetesSpec {
	if desc.KubernetesSpec != nil {
		return desc.KubernetesSpec
	}
	return desc.ContainerSpec.Kubernetes
}

func platformArch(platform string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(platform)), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	// linux/amd64 → amd64; linux/arm64/v8 → arm64; a bare "arm64" is an arch.
	arch := parts[len(parts)-1]
	if len(parts) >= 3 {
		arch = parts[1]
	}
	return normalizeArch(arch)
}

// normalizeArch maps OCI/uname spellings onto Go's runtime.GOARCH vocabulary
// so the cross-arch fidelity check compares like with like.
func normalizeArch(arch string) string {
	switch arch {
	case "x86_64", "x86-64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "i386", "i686":
		return "386"
	}
	return arch
}

func formatStringMap(values map[string]string) string {
	keys := sortedKeys(values)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, ", ")
}

type resolvedSecret struct {
	Value      string
	Resolution SecretResolution
}

func resolveRecordedSecret(ctx context.Context, ref SecretRef, opts ReconstructOptions) (resolvedSecret, []Warning, bool) {
	if !opts.ResolveSecrets {
		return resolvedSecret{}, nil, false
	}
	if opts.SecretResolver == nil {
		return resolvedSecret{}, []Warning{secretResolutionWarning(ref, "local secret resolver is not configured")}, false
	}
	value, identity, err := opts.SecretResolver.ResolveWithIdentity(ctx, ref.Ref)
	if err != nil {
		return resolvedSecret{}, []Warning{secretResolutionWarning(ref, err.Error())}, false
	}

	recordedProvider := recordedSecretProvider(ref)
	localProvider := normalizeSecretProvider(identity.Provider)
	if localProvider == "" || (recordedProvider != "" && recordedProvider != localProvider) {
		return resolvedSecret{}, []Warning{{
			Code: WarningSecretProvider,
			Message: fmt.Sprintf(
				"secret ref %s for %s resolved from local provider %q but recorded provider was %q; omitting env var",
				ref.Ref,
				ref.EnvKey,
				firstNonEmpty(localProvider, "unknown"),
				firstNonEmpty(recordedProvider, "unknown"),
			),
		}}, false
	}

	warnings := secretDriftWarnings(ref, identity)
	return resolvedSecret{
		Value: value,
		Resolution: SecretResolution{
			EnvKey:   ref.EnvKey,
			Ref:      ref.Ref,
			Provider: localProvider,
		},
	}, warnings, true
}

func secretResolutionWarning(ref SecretRef, reason string) Warning {
	return Warning{
		Code:    WarningSecretResolveFailed,
		Message: fmt.Sprintf("secret ref %s for %s could not be resolved locally; omitting env var: %s", ref.Ref, ref.EnvKey, reason),
	}
}

func secretDriftWarnings(ref SecretRef, identity SecretIdentity) []Warning {
	localProvider := normalizeSecretProvider(identity.Provider)
	if recorded := recordedSecretProvider(ref); recorded != "" && recorded != localProvider {
		return nil
	}

	var details []string
	if recordedRef := strings.TrimSpace(ref.Ref); recordedRef != "" && strings.TrimSpace(identity.Ref) != "" && recordedRef != strings.TrimSpace(identity.Ref) {
		details = append(details, fmt.Sprintf("recorded ref %s, local ref %s", recordedRef, strings.TrimSpace(identity.Ref)))
	}
	switch localProvider {
	case "vault":
		recordedVersion := secretIdentityValue(ref.Identity, "version")
		if recordedVersion != "" && strings.TrimSpace(identity.Version) != "" && recordedVersion != strings.TrimSpace(identity.Version) {
			details = append(details, fmt.Sprintf("recorded Vault version %s, local version %s", recordedVersion, strings.TrimSpace(identity.Version)))
		}
	case "k8s":
		recordedResourceVersion := secretIdentityValue(ref.Identity, "resourceVersion")
		if recordedResourceVersion != "" && strings.TrimSpace(identity.ResourceVersion) != "" && recordedResourceVersion != strings.TrimSpace(identity.ResourceVersion) {
			details = append(details, fmt.Sprintf("recorded k8s resourceVersion %s, local resourceVersion %s", recordedResourceVersion, strings.TrimSpace(identity.ResourceVersion)))
		}
	}
	if len(details) == 0 {
		return nil
	}
	return []Warning{{
		Code:    WarningSecretDrift,
		Message: fmt.Sprintf("secret ref %s for %s may have drifted: %s", ref.Ref, ref.EnvKey, strings.Join(details, "; ")),
	}}
}

func secretIdentityValue(identity map[string]any, key string) string {
	if len(identity) == 0 {
		return ""
	}
	value, ok := identity[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func recordedSecretProvider(ref SecretRef) string {
	if provider := normalizeSecretProvider(ref.Provider); provider != "" {
		return provider
	}
	return normalizeSecretProvider(secretProviderFromRef(ref.Ref))
}

func secretProviderFromRef(ref string) string {
	parsed, err := url.Parse(strings.TrimSpace(ref))
	if err != nil || parsed.Scheme != "secret" {
		return ""
	}
	return parsed.Host
}

func normalizeSecretProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "kubernetes" {
		return "k8s"
	}
	return provider
}

func cleanDetails(details []string) []string {
	out := make([]string, 0, len(details))
	for _, detail := range details {
		if strings.TrimSpace(detail) != "" {
			out = append(out, detail)
		}
	}
	return out
}

func paramEnvKey(key string) string {
	return "CAESIUM_PARAM_" + strings.ToUpper(key)
}

func isSecretRef(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "secret://")
}

func secretRefsByEnv(refs []SecretRef) map[string]SecretRef {
	out := make(map[string]SecretRef, len(refs))
	for _, ref := range refs {
		if ref.EnvKey != "" {
			out[ref.EnvKey] = ref
		}
	}
	return out
}

func sortedSecretRefs(refs []SecretRef) []SecretRef {
	out := slices.Clone(refs)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].EnvKey != out[j].EnvKey {
			return out[i].EnvKey < out[j].EnvKey
		}
		return out[i].Ref < out[j].Ref
	})
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

func appendSecretResolution(resolved []SecretResolution, next SecretResolution) []SecretResolution {
	for _, existing := range resolved {
		if existing.EnvKey == next.EnvKey {
			return resolved
		}
	}
	return append(resolved, next)
}

func secretEnvKeys(omitted []SecretOmission) []string {
	keys := make([]string, 0, len(omitted))
	for _, omission := range omitted {
		keys = append(keys, omission.EnvKey)
	}
	return keys
}

func resolvedSecretEnvKeys(resolved []SecretResolution) []string {
	keys := make([]string, 0, len(resolved))
	for _, resolution := range resolved {
		keys = append(keys, resolution.EnvKey)
	}
	sort.Strings(keys)
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
