//go:build integration

package test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// caesium test
// ---------------------------------------------------------------------------

func (s *IntegrationTestSuite) TestTestCommandValidatesDefinitions() {
	out := s.runCLIOutput("test", "--path", filepath.Join("test", "definitions"))
	s.Contains(out, "PASS")
	s.Contains(out, "integration-job-one")
	s.Contains(out, "integration-job-two")
	s.Contains(out, "integration-job-dag")
}

func (s *IntegrationTestSuite) TestTestCommandVerbose() {
	out := s.runCLIOutput("test", "--verbose", "--path",
		filepath.Join("test", "definitions", "job_dag.yaml"))

	s.Contains(out, "PASS")
	s.Contains(out, "integration-job-dag")
	// Verbose mode should show root/leaf info and step details.
	s.Contains(out, "Roots:")
	s.Contains(out, "Leaves:")
	s.Contains(out, "start")
	s.Contains(out, "join")
}

func (s *IntegrationTestSuite) TestTestCommandRejectsInvalidYAML() {
	dir := s.writeInvalidJobManifest()
	defer os.RemoveAll(dir)

	out, err := s.runCLIRaw("test", "--path", dir)
	s.Error(err, "expected non-zero exit for invalid definition")
	s.Contains(out, "FAIL")
}

func (s *IntegrationTestSuite) TestTestCommandSkipsNonCaesiumYAML() {
	dir := s.writeMixedYAMLDir()
	defer os.RemoveAll(dir)

	out := s.runCLIOutput("test", "--path", dir)
	// The Helm chart YAML should be silently skipped; only the
	// Caesium definition should be validated.
	s.Contains(out, "PASS")
	s.Contains(out, "mixed-test-job")
	s.NotContains(out, "FAIL")
}

func (s *IntegrationTestSuite) TestTestCommandShowsDAGTopology() {
	out := s.runCLIOutput("test", "--path",
		filepath.Join("test", "definitions", "job_dag.yaml"))

	s.Contains(out, "PASS")
	// Should show step count and parallelism.
	s.Contains(out, "4 steps")
	s.Contains(out, "max parallelism: 2")
}

func (s *IntegrationTestSuite) TestTestCommandNoDefinitionsFound() {
	dir, err := os.MkdirTemp("", "caesium-empty-*")
	s.Require().NoError(err)
	defer os.RemoveAll(dir)

	out := s.runCLIOutput("test", "--path", dir)
	s.Contains(out, "No job definitions found")
}

// ---------------------------------------------------------------------------
// caesium job preview
// ---------------------------------------------------------------------------

func (s *IntegrationTestSuite) TestJobPreviewRendersSingleDefinition() {
	out := s.runCLIOutput("job", "preview", "--path",
		filepath.Join("test", "definitions", "job_two.yaml"))

	// Should contain box-drawing characters and step names.
	s.Contains(out, "build")
	s.Contains(out, "test")
	s.Contains(out, "package")
	// Should contain box-drawing arrow separator.
	s.Contains(out, "-->")
}

func (s *IntegrationTestSuite) TestJobPreviewRendersDAG() {
	out := s.runCLIOutput("job", "preview", "--path",
		filepath.Join("test", "definitions", "job_dag.yaml"))

	s.Contains(out, "start")
	s.Contains(out, "fanout-a")
	s.Contains(out, "fanout-b")
	s.Contains(out, "join")
}

func (s *IntegrationTestSuite) TestJobPreviewMultipleDefinitions() {
	out := s.runCLIOutput("job", "preview", "--path",
		filepath.Join("test", "definitions"))

	// With multiple definitions, headers should separate them.
	s.Contains(out, "---")
	s.Contains(out, "integration-job-one")
	s.Contains(out, "integration-job-two")
	s.Contains(out, "integration-job-dag")
}

func (s *IntegrationTestSuite) TestJobPreviewRejectsInvalidDefinition() {
	dir := s.writeInvalidJobManifest()
	defer os.RemoveAll(dir)

	_, err := s.runCLIRaw("job", "preview", "--path", dir)
	s.Error(err, "expected non-zero exit for invalid definition")
}

// ---------------------------------------------------------------------------
// caesium dev --once
// ---------------------------------------------------------------------------

func (s *IntegrationTestSuite) TestDevOnceExecutesSingleStepJob() {
	s.requireDocker()
	// Create a minimal job that just echoes — fast and deterministic.
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: dev-e2e-single
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: hello
    image: alpine
    command: ["echo", "hello-from-dev"]
`
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	out := s.runCLIOutput("dev", "--once", "--path", dir)
	s.Contains(out, "dev-e2e-single")
	s.Contains(out, "OK")
}

func (s *IntegrationTestSuite) TestDevOnceExecutesDAG() {
	s.requireDocker()
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: dev-e2e-dag
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: step-a
    image: alpine
    command: ["echo", "a"]
    next:
      - step-b
      - step-c
  - name: step-b
    image: alpine
    command: ["echo", "b"]
    dependsOn: step-a
  - name: step-c
    image: alpine
    command: ["echo", "c"]
    dependsOn: step-a
  - name: step-d
    image: alpine
    command: ["echo", "d"]
    dependsOn:
      - step-b
      - step-c
`
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	out := s.runCLIOutput("dev", "--once", "--path", dir)
	s.Contains(out, "dev-e2e-dag")
	s.Contains(out, "OK")
}

func (s *IntegrationTestSuite) TestDevOnceFailsOnBadImage() {
	s.requireDocker()
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: dev-e2e-bad-image
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: broken
    image: this-image-definitely-does-not-exist:9999
    command: ["echo", "should not run"]
`
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	out, err := s.runCLIRaw("dev", "--once", "--path", dir)
	s.Error(err, "expected non-zero exit for bad image")
	s.Contains(out, "FAIL")
}

func (s *IntegrationTestSuite) TestDevOnceWithRunTimeout() {
	s.requireDocker()
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: dev-e2e-timeout
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: sleeper
    image: alpine
    command: ["sleep", "300"]
`
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	_, err := s.runCLIRaw("dev", "--once", "--run-timeout", "3s", "--path", dir)
	s.Error(err, "expected non-zero exit when run-timeout expires")
}

func (s *IntegrationTestSuite) TestDevOnceSkipsNonCaesiumYAML() {
	s.requireDocker()
	dir := s.writeMixedYAMLDir()
	defer os.RemoveAll(dir)

	out := s.runCLIOutput("dev", "--once", "--path", dir)
	s.Contains(out, "mixed-test-job")
	s.Contains(out, "OK")
}

// ---------------------------------------------------------------------------
// Full workflow: test → preview → dev
// ---------------------------------------------------------------------------

func (s *IntegrationTestSuite) TestFullLocalDevWorkflow() {
	s.requireDocker()
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: workflow-e2e
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: extract
    image: alpine
    command: ["echo", "extracting"]
    next: transform
  - name: transform
    image: alpine
    command: ["echo", "transforming"]
    dependsOn: extract
    next: load
  - name: load
    image: alpine
    command: ["echo", "loading"]
    dependsOn: transform
`
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	// Step 1: Validate the definition.
	testOut := s.runCLIOutput("test", "--verbose", "--path", dir)
	s.Contains(testOut, "PASS")
	s.Contains(testOut, "workflow-e2e")
	s.Contains(testOut, "3 steps")
	s.Contains(testOut, "max parallelism: 1")
	s.Contains(testOut, "Roots:")
	s.Contains(testOut, "extract")

	// Step 2: Preview the DAG.
	previewOut := s.runCLIOutput("job", "preview", "--path", dir)
	s.Contains(previewOut, "extract")
	s.Contains(previewOut, "transform")
	s.Contains(previewOut, "load")
	s.Contains(previewOut, "-->")

	// Step 3: Execute the DAG locally.
	devOut := s.runCLIOutput("dev", "--once", "--path", dir)
	s.Contains(devOut, "workflow-e2e")
	s.Contains(devOut, "OK")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// requireDocker skips the test when the Docker daemon is not reachable.
func (s *IntegrationTestSuite) requireDocker() {
	s.T().Helper()
	d := net.Dialer{Timeout: 2 * 1e9}
	conn, err := d.DialContext(s.T().Context(), "unix", dockerSocket())
	if err != nil {
		s.T().Skipf("Docker not available (%v), skipping", err)
	}
	_ = conn.Close()
}

// dockerSocket returns the Docker socket path, respecting DOCKER_HOST.
func dockerSocket() string {
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return "/var/run/docker.sock"
}

// runCLIOutput runs the CLI and returns stdout+stderr, failing the test on
// non-zero exit.
func (s *IntegrationTestSuite) runCLIOutput(args ...string) string {
	s.T().Helper()
	out, err := s.runCLIRaw(args...)
	s.Require().NoError(err, "cli %v failed:\n%s", args, out)
	return out
}

// runCLIRaw runs the CLI and returns stdout+stderr with the error (if any).
// Callers can inspect the error to test expected failures.
func (s *IntegrationTestSuite) runCLIRaw(args ...string) (string, error) {
	s.T().Helper()
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// writeInvalidJobManifest creates a temp directory containing a YAML file that
// has kind=Job but is structurally invalid (missing required fields).
func (s *IntegrationTestSuite) writeInvalidJobManifest() string {
	s.T().Helper()
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: invalid-job
steps: []
`
	return s.writeJobManifest(manifest)
}

// writeMixedYAMLDir creates a temp directory containing a valid Caesium job
// and a non-Caesium YAML file (Helm chart) to test silent skipping.
func (s *IntegrationTestSuite) writeMixedYAMLDir() string {
	s.T().Helper()
	dir, err := os.MkdirTemp("", "caesium-mixed-*")
	s.Require().NoError(err)

	caesiumJob := `apiVersion: v1
kind: Job
metadata:
  alias: mixed-test-job
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: greet
    image: alpine
    command: ["echo", "hello"]
`
	helmChart := `apiVersion: v2
name: my-chart
description: A Helm chart
version: 0.1.0
`
	s.Require().NoError(os.WriteFile(
		filepath.Join(dir, "job.yaml"),
		[]byte(strings.TrimSpace(caesiumJob)), 0o644))
	s.Require().NoError(os.WriteFile(
		filepath.Join(dir, "Chart.yaml"),
		[]byte(strings.TrimSpace(helmChart)), 0o644))
	return dir
}
