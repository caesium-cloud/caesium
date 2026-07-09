package reproduce

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
)

func TestReconstructEnvLayeringAndSecretOmission(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "registry.example.com/etl/transform:1"
	desc.Runtime.ResolvedImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	desc.ContainerSpec.Env = map[string]string{
		"LITERAL_ENV":                 "fixture-literal",
		"SECRET_ENV":                  "secret://vault/db-password",
		"CAESIUM_PARAM_MODE":          "literal-param",
		"CAESIUM_PARAM_OVERRIDE":      "literal-override",
		"CAESIUM_OUTPUT_PRODUCE_ROWS": "literal-output",
	}
	desc.SecretRefs = []SecretRef{{EnvKey: "SECRET_ENV", Ref: "secret://vault/db-password"}}
	desc.Run.Params = map[string]string{
		"mode":     "recorded",
		"override": "recorded-override",
	}
	desc.DAG.Predecessors = []EdgeRef{{TaskID: "11111111-1111-1111-1111-111111111111", TaskName: "produce"}}
	desc.DAG.PredecessorOutputs = map[string]map[string]string{
		"11111111-1111-1111-1111-111111111111": {
			"rows": "7",
		},
	}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{
		SetParams: []Assignment{{Key: "mode", Value: "manual"}},
		SetEnv: []Assignment{
			{Key: "CAESIUM_PARAM_OVERRIDE", Value: "env-final"},
			{Key: "EXTRA_ENV", Value: "extra"},
		},
	})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	want := map[string]string{
		"LITERAL_ENV":                 "fixture-literal",
		"CAESIUM_PARAM_MODE":          "manual",
		"CAESIUM_PARAM_OVERRIDE":      "env-final",
		"CAESIUM_OUTPUT_PRODUCE_ROWS": "7",
		"EXTRA_ENV":                   "extra",
	}
	for key, value := range want {
		if got := env.Env[key]; got != value {
			t.Fatalf("env[%s] = %q, want %q in %#v", key, got, value, env.Env)
		}
	}
	if _, ok := env.Env["SECRET_ENV"]; ok {
		t.Fatalf("SECRET_ENV should be omitted by default: %#v", env.Env)
	}
	if len(env.OmittedSecrets) != 1 || env.OmittedSecrets[0].EnvKey != "SECRET_ENV" {
		t.Fatalf("omitted secrets = %#v, want SECRET_ENV", env.OmittedSecrets)
	}
	if !hasWarning(env.Warnings, WarningSecretOmitted) {
		t.Fatalf("warnings = %#v, want %s", env.Warnings, WarningSecretOmitted)
	}
	if env.ImagePullMode != "DIGEST" {
		t.Fatalf("ImagePullMode = %q, want DIGEST", env.ImagePullMode)
	}
}

func TestReconstructOutputRefUsesBuildOutputEnvShape(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "load"
	desc.Runtime.Image = "alpine:3.23"
	desc.DAG.Predecessors = []EdgeRef{{TaskID: "22222222-2222-2222-2222-222222222222", TaskName: "extract.step"}}
	ref := containerOutputRef("/data/out.parquet")
	desc.DAG.PredecessorOutputs = map[string]map[string]string{
		"22222222-2222-2222-2222-222222222222": {
			"frame-path": ref,
		},
	}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	if got := env.Env["CAESIUM_OUTPUT_EXTRACT_STEP_FRAME_PATH"]; got != "/data/out.parquet" {
		t.Fatalf("output ref env path = %q", got)
	}
	if got := env.Env["CAESIUM_OUTPUT_EXTRACT_STEP_FRAME_PATH_DIGEST"]; got == "" {
		t.Fatalf("output ref digest companion missing in %#v", env.Env)
	}
	if !hasWarning(env.Warnings, WarningOutputRefUnresolved) {
		t.Fatalf("warnings = %#v, want %s", env.Warnings, WarningOutputRefUnresolved)
	}
}

func TestReconstructImageOverrideMarksEnvelopeAndFidelity(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "registry.example.com/team/app:prod"
	desc.Runtime.ResolvedImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{ImageOverride: "registry.example.com/team/app:candidate"})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	if env.Image != "registry.example.com/team/app:candidate" {
		t.Fatalf("Image = %q, want override", env.Image)
	}
	if env.ImagePullMode != "OVERRIDDEN" || !env.ImageOverridden || env.ImageOverride != "registry.example.com/team/app:candidate" {
		t.Fatalf("override markers = mode %q overridden %t override %q", env.ImagePullMode, env.ImageOverridden, env.ImageOverride)
	}
	if env.RecordedImage != "registry.example.com/team/app:prod" {
		t.Fatalf("RecordedImage = %q, want original", env.RecordedImage)
	}
	if !hasWarning(env.Warnings, WarningImageOverridden) {
		t.Fatalf("warnings = %#v, want %s", env.Warnings, WarningImageOverridden)
	}
	image := assertFidelityStatus(t, env.Fidelity, "image_content", FidelityOverridden)
	details := strings.Join(image.Details, "; ")
	if !strings.Contains(details, "OVERRIDDEN") || !strings.Contains(details, "candidate") {
		t.Fatalf("image fidelity details = %#v, want override marker and ref", image.Details)
	}
}

func TestReconstructResolveSecretsInjectsLocalValueBeforeSetEnvOverride(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "alpine:3.23"
	desc.ContainerSpec.Env = map[string]string{
		"SECRET_ENV": "secret://env/DB_PASSWORD",
	}
	desc.SecretRefs = []SecretRef{{
		EnvKey:   "SECRET_ENV",
		Ref:      "secret://env/DB_PASSWORD",
		Provider: "env",
	}}
	resolver := &fakeSecretResolver{values: map[string]fakeSecretValue{
		"secret://env/DB_PASSWORD": {
			value: "local-db-password",
			identity: SecretIdentity{
				Provider: "env",
				Ref:      "secret://env/DB_PASSWORD",
			},
		},
	}}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{
		ResolveSecrets: true,
		SecretResolver: resolver,
		SetEnv:         []Assignment{{Key: "SECRET_ENV", Value: "manual-final"}},
	})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	if env.Env["SECRET_ENV"] != "manual-final" {
		t.Fatalf("SECRET_ENV = %q, want --set-env override after local resolution", env.Env["SECRET_ENV"])
	}
	if len(env.OmittedSecrets) != 0 {
		t.Fatalf("omitted secrets = %#v, want none", env.OmittedSecrets)
	}
	if len(env.ResolvedSecrets) != 1 || env.ResolvedSecrets[0].EnvKey != "SECRET_ENV" || env.ResolvedSecrets[0].Provider != "env" {
		t.Fatalf("resolved secrets = %#v, want SECRET_ENV env metadata", env.ResolvedSecrets)
	}
	assertFidelityStatus(t, env.Fidelity, "secret_values", FidelityDegraded)
	if resolver.refs[0] != "secret://env/DB_PASSWORD" {
		t.Fatalf("resolver refs = %#v", resolver.refs)
	}
	for _, warning := range env.Warnings {
		if strings.Contains(warning.Message, "local-db-password") || strings.Contains(warning.Message, "manual-final") {
			t.Fatalf("warning leaked secret value: %#v", warning)
		}
	}
}

func TestReconstructResolveSecretsOmitOnFailureOrProviderMismatch(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "alpine:3.23"
	desc.ContainerSpec.Env = map[string]string{
		"MISSING_SECRET":  "secret://env/MISSING",
		"MISMATCH_SECRET": "secret://vault/secret/data/db?field=password",
	}
	desc.SecretRefs = []SecretRef{
		{EnvKey: "MISSING_SECRET", Ref: "secret://env/MISSING", Provider: "env"},
		{EnvKey: "MISMATCH_SECRET", Ref: "secret://vault/secret/data/db?field=password", Provider: "vault"},
	}
	resolver := &fakeSecretResolver{
		values: map[string]fakeSecretValue{
			"secret://vault/secret/data/db?field=password": {
				value:    "wrong-provider-value",
				identity: SecretIdentity{Provider: "env", Ref: "secret://vault/secret/data/db?field=password"},
			},
		},
		errs: map[string]error{
			"secret://env/MISSING": errors.New("environment variable MISSING not set"),
		},
	}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{ResolveSecrets: true, SecretResolver: resolver})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	if _, ok := env.Env["MISSING_SECRET"]; ok {
		t.Fatalf("MISSING_SECRET should be omitted: %#v", env.Env)
	}
	if _, ok := env.Env["MISMATCH_SECRET"]; ok {
		t.Fatalf("MISMATCH_SECRET should be omitted on provider mismatch: %#v", env.Env)
	}
	if len(env.OmittedSecrets) != 2 {
		t.Fatalf("omitted secrets = %#v, want two", env.OmittedSecrets)
	}
	if !hasWarning(env.Warnings, WarningSecretResolveFailed) {
		t.Fatalf("warnings = %#v, want resolve failure", env.Warnings)
	}
	if !hasWarning(env.Warnings, WarningSecretProvider) {
		t.Fatalf("warnings = %#v, want provider mismatch", env.Warnings)
	}
}

func TestReconstructResolveSecretsWarnsOnComparableDrift(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "alpine:3.23"
	desc.ContainerSpec.Env = map[string]string{
		"VAULT_SECRET": "secret://vault/secret/data/db?field=password",
		"K8S_SECRET":   "secret://k8s/default/db/password",
	}
	desc.SecretRefs = []SecretRef{
		{
			EnvKey:   "VAULT_SECRET",
			Ref:      "secret://vault/secret/data/db?field=password",
			Provider: "vault",
			Identity: map[string]any{"version": "1", "hmacSha256": "server-keyed"},
		},
		{
			EnvKey:   "K8S_SECRET",
			Ref:      "secret://k8s/default/db/password",
			Provider: "k8s",
			Identity: map[string]any{"resourceVersion": "11"},
		},
	}
	resolver := &fakeSecretResolver{values: map[string]fakeSecretValue{
		"secret://vault/secret/data/db?field=password": {
			value: "vault-local-value",
			identity: SecretIdentity{
				Provider: "vault",
				Ref:      "secret://vault/secret/data/db?field=password",
				Version:  "2",
			},
		},
		"secret://k8s/default/db/password": {
			value: "k8s-local-value",
			identity: SecretIdentity{
				Provider:        "k8s",
				Ref:             "secret://k8s/default/db/password",
				ResourceVersion: "12",
			},
		},
	}}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{ResolveSecrets: true, SecretResolver: resolver})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	if env.Env["VAULT_SECRET"] != "vault-local-value" || env.Env["K8S_SECRET"] != "k8s-local-value" {
		t.Fatalf("resolved env = %#v", env.Env)
	}
	driftCount := 0
	for _, warning := range env.Warnings {
		if warning.Code == WarningSecretDrift {
			driftCount++
			if strings.Contains(warning.Message, "vault-local-value") || strings.Contains(warning.Message, "k8s-local-value") || strings.Contains(warning.Message, "server-keyed") {
				t.Fatalf("drift warning leaked secret or HMAC value: %#v", warning)
			}
		}
	}
	if driftCount != 2 {
		t.Fatalf("warnings = %#v, want two drift warnings", env.Warnings)
	}
}

func TestReconstructMountRemapAndKubernetesSkip(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "alpine:3.23"
	desc.ContainerSpec.Mounts = []container.Mount{{
		Type:   container.MountTypeBind,
		Source: "/prod/data",
		Target: "/data",
	}}
	desc.ContainerSpec.ResolvedVolumeMounts = []container.VolumeMount{
		{Type: container.VolumeMountTypeBind, Source: "/prod/work", Target: "/work"},
		{Type: container.VolumeMountTypePVC, Name: "shared", Source: "claim", Target: "/claim"},
	}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{
		Mounts: []MountRemap{
			{From: "/prod/data", To: "/local/data"},
			{From: "/prod/work", To: "/local/work"},
		},
	})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	if len(env.Mounts) != 2 {
		t.Fatalf("mounts = %#v, want two Docker-compatible mounts", env.Mounts)
	}
	if env.Mounts[0].Source != "/local/data" || env.Mounts[1].Source != "/local/work" {
		t.Fatalf("mount sources = %#v, want remapped sources", env.Mounts)
	}
	if !hasWarning(env.Warnings, WarningMountSkipped) {
		t.Fatalf("warnings = %#v, want PVC skip warning", env.Warnings)
	}
}

func TestReconstructFidelitySummaryListsBestEffortDimensions(t *testing.T) {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Runtime.Image = "registry.example.com/team/app:latest"
	desc.Runtime.Engine = "kubernetes"
	desc.Runtime.NodeSelector = map[string]string{"disk": "ssd"}
	desc.KubernetesSpec = &container.KubernetesSpec{
		ServiceAccountName: "pipeline-runner",
		QueueName:          "batch",
	}
	desc.DAG.Predecessors = []EdgeRef{{TaskID: "33333333-3333-3333-3333-333333333333", TaskName: "extract"}}
	desc.DAG.PredecessorOutputs = map[string]map[string]string{
		"33333333-3333-3333-3333-333333333333": {
			"frame": containerOutputRef("/byo/frame.parquet"),
		},
	}

	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{Platform: oppositePlatform()})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	assertFidelityStatus(t, env.Fidelity, "image_content", FidelityDegraded)
	assertFidelityStatus(t, env.Fidelity, "predecessor_outputs", FidelityDegraded)
	workload := assertFidelityStatus(t, env.Fidelity, "engine_workload_identity", FidelityListedNotApplied)
	if !strings.Contains(strings.Join(workload.Details, "; "), "serviceAccountName pipeline-runner") ||
		!strings.Contains(strings.Join(workload.Details, "; "), "Kueue queue batch") ||
		!strings.Contains(strings.Join(workload.Details, "; "), "node selector disk=ssd") {
		t.Fatalf("workload identity details = %#v, want service account, Kueue queue, and node selector", workload.Details)
	}
	assertFidelityStatus(t, env.Fidelity, "cpu_architecture", FidelityDegraded)
	assertFidelityStatus(t, env.Fidelity, "wall_clock_time", FidelityNotReproduced)
	assertFidelityStatus(t, env.Fidelity, "external_system_state", FidelityNotReproduced)
	assertFidelityStatus(t, env.Fidelity, "side_effects", FidelityNotReproduced)

	for _, code := range []string{
		WarningDegradedImagePull,
		WarningOutputRefUnresolved,
		WarningWorkloadIdentity,
		WarningCrossArchEmulation,
		WarningWallClock,
		WarningExternalState,
		WarningSideEffects,
	} {
		if !hasWarning(env.Warnings, code) {
			t.Fatalf("warnings = %#v, want %s", env.Warnings, code)
		}
	}
}

func containerOutputRef(path string) string {
	return `{"caesiumOutputRef":1,"path":"` + path + `","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
}

func hasWarning(warnings []Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func assertFidelityStatus(t *testing.T, summary *FidelitySummary, dimension, status string) FidelityDimension {
	t.Helper()
	if summary == nil {
		t.Fatalf("fidelity summary is nil")
	}
	for _, dim := range summary.Dimensions {
		if dim.Dimension == dimension {
			if dim.Status != status {
				t.Fatalf("fidelity[%s] = %s, want %s (details %#v)", dimension, dim.Status, status, dim.Details)
			}
			return dim
		}
	}
	t.Fatalf("fidelity dimension %s not found in %#v", dimension, summary.Dimensions)
	return FidelityDimension{}
}

func oppositePlatform() string {
	if runtime.GOARCH == "amd64" {
		return "linux/arm64"
	}
	return "linux/amd64"
}

type fakeSecretValue struct {
	value    string
	identity SecretIdentity
}

type fakeSecretResolver struct {
	values map[string]fakeSecretValue
	errs   map[string]error
	refs   []string
}

func (r *fakeSecretResolver) ResolveWithIdentity(_ context.Context, ref string) (string, SecretIdentity, error) {
	r.refs = append(r.refs, ref)
	if err := r.errs[ref]; err != nil {
		return "", SecretIdentity{}, err
	}
	value, ok := r.values[ref]
	if !ok {
		return "", SecretIdentity{}, errors.New("secret not found")
	}
	return value.value, value.identity, nil
}
