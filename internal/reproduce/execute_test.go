package reproduce

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	pkgjobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
)

func TestExecutePullFailureGuidanceNamesRegistryAndLocalFallback(t *testing.T) {
	desc := basicDescriptor("registry.example.com/team/app:1")
	desc.Runtime.ResolvedImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	_, err = Execute(context.Background(), desc, env, ExecuteOptions{
		Puller: &fakePuller{err: errors.New("unauthorized: authentication required")},
		Runner: fakeRunner{},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want pull error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "registry.example.com") {
		t.Fatalf("pull error %q does not name registry", msg)
	}
	if !strings.Contains(msg, "docker login registry.example.com") {
		t.Fatalf("pull error %q does not include the docker login hint", msg)
	}
	if !strings.Contains(msg, "locally present image") {
		t.Fatalf("pull error %q does not include the local-image guidance", msg)
	}
}

type localPresentPuller struct{ pullCalled bool }

func (p *localPresentPuller) Pull(context.Context, string, string) error {
	p.pullCalled = true
	return fmt.Errorf("must not pull when the image is local")
}

func (p *localPresentPuller) ExistsLocally(context.Context, string) bool { return true }

// A locally present image short-circuits the registry pull entirely, so a
// private-registry auth failure cannot block reproducing with a local image.
func TestExecuteSkipsPullWhenImagePresentLocally(t *testing.T) {
	desc := basicDescriptor("registry.example.com/team/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}
	puller := &localPresentPuller{}
	result, err := Execute(context.Background(), desc, env, ExecuteOptions{
		Puller: puller,
		Runner: fakeRunner{result: &RunResult{Tasks: []TaskResult{{
			Name:   "transform",
			Status: "succeeded",
		}}}},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v, want success via local image", err)
	}
	if puller.pullCalled {
		t.Fatal("Pull was called despite the image existing locally")
	}
	found := false
	for _, w := range result.Envelope.Warnings {
		if w.Code == "local_image_used" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected local_image_used warning, got %+v", result.Envelope.Warnings)
	}
}

func TestExecuteUsesDegradedTagPullWhenDigestMissing(t *testing.T) {
	desc := basicDescriptor("registry.example.com/team/app:latest")
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}
	if env.ImagePullMode != "DEGRADED" {
		t.Fatalf("ImagePullMode = %q, want DEGRADED", env.ImagePullMode)
	}
	if !hasWarning(env.Warnings, WarningDegradedImagePull) {
		t.Fatalf("warnings = %#v, want degraded warning", env.Warnings)
	}

	puller := &fakePuller{}
	result, err := Execute(context.Background(), desc, env, ExecuteOptions{
		Puller: puller,
		Runner: fakeRunner{result: &RunResult{Tasks: []TaskResult{{
			Name:    "transform",
			Status:  "succeeded",
			LogText: `##caesium::output {"ok":"yes"}`,
		}}}},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if puller.imageRef != "registry.example.com/team/app:latest" {
		t.Fatalf("pulled image = %q, want mutable tag", puller.imageRef)
	}
	if result.ExitCode != 0 || result.Output["ok"] != "yes" {
		t.Fatalf("result = %#v, want parsed success output", result)
	}
}

func TestExecuteUsesImageOverrideForPullAndSynthesizedDefinition(t *testing.T) {
	desc := basicDescriptor("registry.example.com/team/app:prod")
	desc.Runtime.ResolvedImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{ImageOverride: "registry.example.com/team/app:candidate"})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}
	puller := &fakePuller{}
	runner := &capturingRunner{result: &RunResult{Tasks: []TaskResult{{
		Name:   "transform",
		Status: "succeeded",
	}}}}

	result, err := Execute(context.Background(), desc, env, ExecuteOptions{
		Puller: puller,
		Runner: runner,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if puller.imageRef != "registry.example.com/team/app:candidate" {
		t.Fatalf("pulled image = %q, want override", puller.imageRef)
	}
	if runner.def == nil || len(runner.def.Steps) != 1 || runner.def.Steps[0].Image != "registry.example.com/team/app:candidate" {
		t.Fatalf("synthesized definition = %#v, want override image", runner.def)
	}
	if result.ImagePullMode != "OVERRIDDEN" || !result.ImageOverridden {
		t.Fatalf("result override markers = mode %q overridden %t", result.ImagePullMode, result.ImageOverridden)
	}
}

func TestExecutePassesRemappedMountsToSynthesizedDefinition(t *testing.T) {
	desc := basicDescriptor("alpine:3.23")
	desc.ContainerSpec.Mounts = testBindMount("/recorded/data", "/data")
	desc.ContainerSpec.ResolvedVolumeMounts = testPVCMount("claim", "/claim")
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{
		Mounts: []MountRemap{{From: "/recorded/data", To: "/local/data"}},
	})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}
	if !hasWarning(env.Warnings, WarningMountSkipped) {
		t.Fatalf("warnings = %#v, want PVC skip warning", env.Warnings)
	}

	runner := &capturingRunner{result: &RunResult{Tasks: []TaskResult{{
		Name:   "transform",
		Status: "succeeded",
	}}}}
	_, err = Execute(context.Background(), desc, env, ExecuteOptions{
		Puller:  &fakePuller{},
		Runner:  runner,
		Timeout: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if runner.def == nil || len(runner.def.Steps) != 1 {
		t.Fatalf("runner definition = %#v", runner.def)
	}
	mounts := runner.def.Steps[0].Mounts
	if len(mounts) != 1 || mounts[0].Source != "/local/data" || mounts[0].Target != "/data" {
		t.Fatalf("synthesized mounts = %#v, want remapped bind only", mounts)
	}
	if runner.timeout != 45*time.Second {
		t.Fatalf("runner timeout = %s, want 45s", runner.timeout)
	}
}

func TestBuildShellRequestUsesDefaultShellAndClonesEnvelope(t *testing.T) {
	env := &Envelope{
		TaskName: "transform",
		Image:    "alpine:3.23",
		Env:      map[string]string{"A": "1"},
		Mounts:   testBindMount("/local/data", "/data"),
		WorkDir:  "/work",
	}

	req := BuildShellRequest(env, "linux/amd64")
	env.Env["A"] = "mutated"
	env.Mounts[0].Source = "/mutated"

	if req.Shell != DefaultShell {
		t.Fatalf("Shell = %q, want %q", req.Shell, DefaultShell)
	}
	if req.Env["A"] != "1" {
		t.Fatalf("request env was not cloned: %#v", req.Env)
	}
	if req.Mounts[0].Source != "/local/data" {
		t.Fatalf("request mounts were not cloned: %#v", req.Mounts)
	}
	if req.WorkDir != "/work" || req.Platform != "linux/amd64" {
		t.Fatalf("request = %#v, want workdir and platform", req)
	}
}

func TestExecuteShellPullsImageAndRunsShellRequest(t *testing.T) {
	desc := basicDescriptor("registry.example.com/team/app:1")
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}
	puller := &fakePuller{}
	runner := &fakeShellRunner{}

	result, err := ExecuteShell(context.Background(), env, ShellExecuteOptions{
		Puller:      puller,
		ShellRunner: runner,
		Platform:    "linux/amd64",
	})
	if err != nil {
		t.Fatalf("ExecuteShell() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if puller.imageRef != "registry.example.com/team/app:1" || puller.platform != "linux/amd64" {
		t.Fatalf("puller = %#v, want image and platform", puller)
	}
	if runner.req.Shell != DefaultShell || runner.req.Image != env.Image {
		t.Fatalf("shell request = %#v, want default shell and env image", runner.req)
	}
}

func TestExecuteShellDistrolessGuidance(t *testing.T) {
	desc := basicDescriptor("gcr.io/distroless/static:nonroot")
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	_, err = ExecuteShell(context.Background(), env, ShellExecuteOptions{
		Puller: &fakePuller{},
		ShellRunner: &fakeShellRunner{err: &ShellUnavailableError{
			Image: env.Image,
			Shell: DefaultShell,
			Err:   errors.New("exit status 127"),
		}},
	})
	if err == nil {
		t.Fatal("ExecuteShell() error = nil, want shell unavailable guidance")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--shell-image") || !strings.Contains(msg, "deferred") || !strings.Contains(msg, "distroless") {
		t.Fatalf("shell unavailable error = %q, want distroless deferred --shell-image guidance", msg)
	}
}

func TestExecuteShellReturnsInteractiveExitCode(t *testing.T) {
	desc := basicDescriptor("alpine:3.23")
	env, err := Reconstruct(context.Background(), desc, ReconstructOptions{})
	if err != nil {
		t.Fatalf("Reconstruct() error = %v", err)
	}

	result, err := ExecuteShell(context.Background(), env, ShellExecuteOptions{
		Puller: &fakePuller{},
		ShellRunner: &fakeShellRunner{err: &ShellExitError{
			Code: 7,
			Err:  errors.New("exit status 7"),
		}},
	})
	if err != nil {
		t.Fatalf("ExecuteShell() error = %v, want shell exit result", err)
	}
	if result.ExitCode != 7 || result.Error == "" {
		t.Fatalf("result = %#v, want shell exit code and error", result)
	}
}

func basicDescriptor(image string) *Descriptor {
	desc := &Descriptor{}
	desc.SchemaVersion = 1
	desc.Baseline.TaskName = "transform"
	desc.Baseline.JobAlias = "fixture"
	desc.Runtime.Image = image
	desc.Runtime.Command = []string{"sh", "-c", "echo ok"}
	return desc
}

func testBindMount(source, target string) []container.Mount {
	return []container.Mount{{Type: container.MountTypeBind, Source: source, Target: target}}
}

func testPVCMount(source, target string) []container.VolumeMount {
	return []container.VolumeMount{{Type: container.VolumeMountTypePVC, Source: source, Target: target}}
}

type fakePuller struct {
	imageRef string
	platform string
	err      error
}

func (p *fakePuller) Pull(_ context.Context, imageRef, platform string) error {
	p.imageRef = imageRef
	p.platform = platform
	return p.err
}

type fakeRunner struct {
	result *RunResult
	err    error
}

func (r fakeRunner) Run(context.Context, *pkgjobdef.Definition, time.Duration) (*RunResult, error) {
	return r.result, r.err
}

type capturingRunner struct {
	def     *pkgjobdef.Definition
	timeout time.Duration
	result  *RunResult
}

func (r *capturingRunner) Run(_ context.Context, def *pkgjobdef.Definition, timeout time.Duration) (*RunResult, error) {
	r.def = def
	r.timeout = timeout
	return r.result, nil
}

type fakeShellRunner struct {
	req ShellRequest
	err error
}

func (r *fakeShellRunner) RunShell(_ context.Context, req ShellRequest) error {
	r.req = req
	return r.err
}
