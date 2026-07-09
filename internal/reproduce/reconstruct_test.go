package reproduce

import (
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

	env, err := Reconstruct(desc, ReconstructOptions{
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

	env, err := Reconstruct(desc, ReconstructOptions{})
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

	env, err := Reconstruct(desc, ReconstructOptions{
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
