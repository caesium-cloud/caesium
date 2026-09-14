//go:build integration

package test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// D1 (distributed-testing W3-gamma): extends the binary-driven developer
// journey local_dev_test.go started with the paths that file's scenarios
// leave uncovered — genuinely malformed YAML (as opposed to schema-invalid
// YAML, which local_dev_test.go already covers), paths containing spaces, an
// engine with no reachable backend, a --run-timeout cancellation that must
// actually stop the container it started, watch-mode interrupt, and the
// apply -> export "inspect" round trip. Every scenario drives the
// container-built release CLI (CAESIUM_CLI_PATH, see integration_test.go's
// SetupSuite) through its real surface in an empty temporary workspace —
// nothing here imports an internal package or hand-seeds state. All ground
// truth below (exact exit codes, which stream carries which text) was
// verified against a real built CLI binary before being asserted; see the PR
// description for the transcripts.
// ---------------------------------------------------------------------------

// malformedYAMLManifest is syntactically invalid YAML (a tab character
// violating block indentation), which is a different failure than
// local_dev_test.go's writeInvalidJobManifest (syntactically valid YAML that
// simply fails schema validation — missing trigger, empty steps). The two
// inputs exercise different code paths: internal/jobdef.CollectDefinitions
// (used by `caesium test`/`caesium dev`) and cmd/job.collectDefinitions
// (used by `caesium job lint`/`job preview`/`job apply`) both special-case a
// genuine parse failure and return it BEFORE any per-definition validation
// ever runs, so none of the commands below ever print "FAIL" for this input
// the way they do for a schema violation.
const malformedYAMLManifest = "apiVersion: v1\nkind: Job\nmetadata:\n  alias: malformed-syntax\n\tsteps:\n  - name: bad\n"

// ---------------------------------------------------------------------------
// Malformed input
// ---------------------------------------------------------------------------

// TestTestCommandRejectsMalformedYAMLSyntax proves `caesium test` fails
// closed on a YAML document that never parses at all. Verified against the
// real CLI: exit 1, stdout completely empty, stderr names the file and the
// underlying yaml.v3 error — unlike the schema-invalid case
// (TestTestCommandRejectsInvalidYAML in local_dev_test.go), which prints
// "FAIL" to stdout and exits nonzero from allOK==false instead.
func (s *IntegrationTestSuite) TestTestCommandRejectsMalformedYAMLSyntax() {
	dir := s.writeJobManifest(malformedYAMLManifest)
	defer os.RemoveAll(dir)

	stdout, stderr, err := s.runCLISeparate("test", "--path", dir)
	s.Require().Error(err, "a genuine YAML syntax error must exit nonzero")
	s.Empty(stdout, "a top-level parse failure must not print anything to stdout")
	s.Contains(stderr, "yaml:", "stderr should name the underlying YAML problem")
}

// TestJobLintRejectsMalformedYAMLSyntax is the same proof for `caesium job
// lint` (local mode, no --server), which loads definitions through a
// SEPARATE collector (cmd/job.collectDefinitions) than `caesium test` does.
func (s *IntegrationTestSuite) TestJobLintRejectsMalformedYAMLSyntax() {
	dir := s.writeJobManifest(malformedYAMLManifest)
	defer os.RemoveAll(dir)

	stdout, stderr, err := s.runCLISeparate("job", "lint", "--path", dir)
	s.Require().Error(err)
	s.Empty(stdout)
	s.Contains(stderr, "yaml:")
}

// TestJobPreviewRejectsMalformedYAMLSyntax is the same proof for `caesium job
// preview`, the ASCII DAG visualization step of the local workflow.
func (s *IntegrationTestSuite) TestJobPreviewRejectsMalformedYAMLSyntax() {
	dir := s.writeJobManifest(malformedYAMLManifest)
	defer os.RemoveAll(dir)

	stdout, stderr, err := s.runCLISeparate("job", "preview", "--path", dir)
	s.Require().Error(err)
	s.Empty(stdout)
	s.Contains(stderr, "yaml:")
}

// TestDevOnceRejectsMalformedYAMLSyntax is the same input through `caesium
// dev --once`. Unlike the read-only commands above, dev.go prints its own
// "Run failed: <err>" line to STDOUT before the error also reaches cobra's
// default stderr reporting — verified against the real CLI — so this
// scenario asserts the opposite of the empty-stdout cases above on purpose.
func (s *IntegrationTestSuite) TestDevOnceRejectsMalformedYAMLSyntax() {
	dir := s.writeJobManifest(malformedYAMLManifest)
	defer os.RemoveAll(dir)

	stdout, stderr, err := s.runCLISeparate("dev", "--once", "--path", dir)
	s.Require().Error(err)
	s.Contains(stdout, "Run failed:")
	s.Contains(stderr, "yaml:")
}

// TestDevOnceRejectsInvalidDefinition covers `caesium dev --once` on a
// schema-invalid (but syntactically valid) definition — local_dev_test.go
// asserts this rejection for `caesium test` and `caesium job preview` but
// never for `caesium dev`, the command that actually executes the DAG.
func (s *IntegrationTestSuite) TestDevOnceRejectsInvalidDefinition() {
	dir := s.writeInvalidJobManifest()
	defer os.RemoveAll(dir)

	out, err := s.runCLIRaw("dev", "--once", "--path", dir)
	s.Error(err, "dev --once must fail closed on a schema-invalid definition")
	s.Contains(out, "trigger.type")
}

// ---------------------------------------------------------------------------
// Paths with spaces
// ---------------------------------------------------------------------------

// TestDeveloperJourneyPathsWithSpaces drives lint -> preview -> dev-once
// through a workspace directory AND job filename that both contain spaces,
// proving the CLI's own path handling (never shell-quoted, since exec.Cmd
// passes args as a vector) survives a directory a developer might reasonably
// have — "My Jobs", "Caesium Pipelines", etc.
func (s *IntegrationTestSuite) TestDeveloperJourneyPathsWithSpaces() {
	s.requireDocker()

	alias := fmt.Sprintf("dev-journey-spaces-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: extract
    image: alpine:3.23
    command: ["echo", "extracting"]
    next: load
  - name: load
    image: alpine:3.23
    command: ["echo", "loading"]
    dependsOn: extract
`, alias)

	dir, _ := s.writeJobManifestWithSpaces(manifest)
	defer os.RemoveAll(dir)

	lintOut := s.runCLIOutput("job", "lint", "--path", dir)
	s.Contains(lintOut, "Validated 1 job definition")

	previewOut := s.runCLIOutput("job", "preview", "--path", dir)
	s.Contains(previewOut, "extract")
	s.Contains(previewOut, "load")

	devOut := s.runCLIOutput("dev", "--once", "--path", dir)
	s.Contains(devOut, alias)
	s.Contains(devOut, "OK")
}

// writeJobManifestWithSpaces mirrors the shared writeJobManifest helper
// (integration_test.go) but places the manifest under a workspace directory
// AND filename that both contain a literal space, and applies the same
// engine injection so non-docker lanes still run it.
func (s *IntegrationTestSuite) writeJobManifestWithSpaces(contents string) (dir string, path string) {
	s.T().Helper()

	dir, err := os.MkdirTemp("", "caesium job workspace *")
	s.Require().NoError(err)

	path = filepath.Join(dir, "job with spaces.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(contents))), 0o644))
	return dir, path
}

// ---------------------------------------------------------------------------
// Unavailable engines
// ---------------------------------------------------------------------------

// TestDevOnceUnavailableEngineFailsClosed declares a step on the kubernetes
// engine and runs it on a lane with no kubeconfig and no in-cluster service
// account, so the declared engine has no reachable backend.
//
// Verified against the real CLI: internal/atom/kubernetes.NewEngine panics
// on this input (internal/atom/kubernetes/engine.go's getKubernetesCore calls
// panic(err) when neither a kubeconfig nor in-cluster config resolves), and
// internal/job.buildLocalRunners calls the engine factory with no recover(),
// so the process exits 2 with a Go panic trace on stderr rather than a clean
// "FAIL" message. That is arguably a rough edge (out of this item's file
// scope — internal/atom/kubernetes is not owned here), so this assertion is
// deliberately engine-behavior-agnostic: it only requires a nonzero, bounded
// exit, and keeps passing if that panic is later turned into a graceful
// error.
func (s *IntegrationTestSuite) TestDevOnceUnavailableEngineFailsClosed() {
	if s.engineType == "kubernetes" {
		s.T().Skip("this lane provisions a real kubernetes cluster; the unavailable-engine premise does not hold here")
	}

	dir, err := os.MkdirTemp("", "caesium-unavailable-engine-*")
	s.Require().NoError(err)
	defer os.RemoveAll(dir)

	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: dev-journey-unavailable-engine
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: needs-cluster
    engine: kubernetes
    image: alpine:3.23
    command: ["echo", "hi"]
`
	path := filepath.Join(dir, "job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(manifest)), 0o644))

	_, err = s.runCLIRaw("dev", "--once", "--path", dir)
	s.Require().Error(err, "dev --once must not succeed when the declared engine has no reachable backend")
}

// ---------------------------------------------------------------------------
// Cancellation/timeouts and owned-resource cleanup
// ---------------------------------------------------------------------------

// TestDevOnceRunTimeoutStopsAndRemovesContainer extends
// TestDevOnceWithRunTimeout (local_dev_test.go), which only asserts the
// nonzero exit, with the owned-resource-cleanup half of the same contract:
// the container --run-timeout cancelled must actually stop, not just fail
// the CLI invocation while it keeps running on the daemon. Verified against
// the real CLI: the docker engine's Stop+Remove calls land before dev exits.
func (s *IntegrationTestSuite) TestDevOnceRunTimeoutStopsAndRemovesContainer() {
	s.requireDocker()
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the container-cleanup assertion inspects containers over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	marker := fmt.Sprintf("caesium-dev-timeout-marker-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: dev-journey-run-timeout
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: sleeper
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, fmt.Sprintf("sleep 60 # %s", marker))

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	defer s.removeContainersWithMarker(cli, marker)

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, "dev", "--once", "--run-timeout", "3s", "--path", dir)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	s.Require().NoError(cmd.Start())

	var orphanIDs []string
	s.Require().Eventually(func() bool {
		orphanIDs = s.runningContainerIDsWithMarker(cli, marker)
		return len(orphanIDs) > 0
	}, 30*time.Second, 500*time.Millisecond, "dev --once never started a container carrying %q", marker)

	waitErr := cmd.Wait()
	s.Require().Error(waitErr, "dev --once must exit nonzero when --run-timeout fires")

	s.Require().Eventually(func() bool {
		for _, id := range orphanIDs {
			if s.containerIsRunning(cli, id) {
				return false
			}
		}
		return true
	}, 30*time.Second, time.Second,
		"container(s) %v still running after --run-timeout fired; dev --once must stop what it started", orphanIDs)
}

// TestDevWatchModeRerunsOnEditAndStopsOnInterrupt drives the "watch/edit,
// interrupt" leg of the local development workflow CLAUDE.md documents
// (`caesium dev --path job.yaml` — watch mode, re-run on save, Ctrl-C to
// stop): starts dev in watch mode, waits for the first run, edits the job
// file and waits for the re-run it triggers, sends the process an interrupt
// (SIGINT, i.e. Ctrl-C) the way an operator would, and requires a graceful
// exit — then checks owned-resource cleanup: no container from either run is
// still around.
func (s *IntegrationTestSuite) TestDevWatchModeRerunsOnEditAndStopsOnInterrupt() {
	s.requireDocker()

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	alias := fmt.Sprintf("dev-journey-watch-%d", time.Now().UnixNano())
	marker := fmt.Sprintf("caesium-dev-watch-marker-%d", time.Now().UnixNano())
	version := func(v string) string {
		return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "*/5 * * * *"
steps:
  - name: greet
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, fmt.Sprintf("echo %s-%s", v, marker))
	}

	dir := s.writeJobManifest(version("v1"))
	defer os.RemoveAll(dir)
	defer s.removeContainersWithMarker(cli, marker)

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, "dev", "--path", dir)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	s.Require().NoError(cmd.Start())
	// Safety net: if an assertion below fails before the graceful shutdown
	// runs, don't leave a watch process behind for the next test. A Kill on
	// an already-exited process is a harmless no-op error.
	defer func() { _ = cmd.Process.Kill() }()

	s.Require().Eventually(func() bool {
		return strings.Contains(out.String(), "Watching")
	}, 60*time.Second, 200*time.Millisecond, "dev never announced watch mode:\n%s", out.String())
	s.Equal(1, strings.Count(out.String(), "  OK    "), "expected exactly one completed run before the edit:\n%s", out.String())

	s.rewriteJobManifest(dir, version("v2"))

	s.Require().Eventually(func() bool {
		return strings.Count(out.String(), "  OK    ") >= 2
	}, 60*time.Second, 200*time.Millisecond, "dev never re-ran after the file edit:\n%s", out.String())
	s.Contains(out.String(), "File changed, re-running...")

	s.Require().NoError(cmd.Process.Signal(syscall.SIGINT))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		s.NoError(waitErr, "a graceful Ctrl-C should exit 0:\n%s", out.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		s.Fail("dev did not exit after SIGINT", out.String())
	}
	s.Contains(out.String(), "Stopping.", "watch mode should announce a graceful stop")

	// Owned-resource cleanup: both the v1 and v2 runs' containers must be
	// gone, not merely stopped.
	s.Empty(s.containerIDsWithMarker(cli, marker),
		"no container from either watch-mode run should remain after a graceful stop")
}

// syncBuffer is an io.Writer safe for one goroutine to append to (the child
// process's stdout/stderr) while the test goroutine polls its contents.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------------------------------------------------------------------------
// Apply, and inspect
// ---------------------------------------------------------------------------

// TestJobApplyThenExportInspectsDeployedPipeline drives the last leg of the
// local development workflow CLAUDE.md documents: `caesium job apply` to
// deploy, then inspecting what actually landed. "Inspect" here is `caesium
// job export`, the CLI's own read-back verb (GET /v1/jobs/:id/manifest) —
// the same round trip job_manifest_export_test.go proves for docs/examples
// fixtures, exercised here against D1's own fixture and asserting the
// resulting DAG topology, not just that export produced SOME manifest.
func (s *IntegrationTestSuite) TestJobApplyThenExportInspectsDeployedPipeline() {
	alias, dir := s.stageDeveloperTestdata("pipeline.job.yaml")
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	s.Len(s.fetchTasks(job.ID), 3, "the deployed pipeline must register all three steps")

	// job export writes ONLY the manifest to stdout (streams captured
	// separately, per CLAUDE.md "End-to-end coverage is the gate" — a merged
	// capture would hide a log line landing on stdout).
	stdout, stderr, err := s.runCLISeparate("job", "export", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "job export failed: %s", stderr)
	s.Require().True(strings.HasPrefix(stdout, "apiVersion: v1\n"),
		"stdout must start with the manifest, got:\n%s", stdout)

	exported, err := schema.Parse([]byte(stdout))
	s.Require().NoError(err, "exported manifest must parse and validate:\n%s", stdout)
	s.Equal(alias, exported.Metadata.Alias)
	s.Equal("platform", exported.Metadata.Labels["team"])
	s.Equal([]string{"extract", "transform", "load"}, stepNames(exported.Steps))
}

// stageDeveloperTestdata reads a fixture from test/developer_testdata,
// rewrites its alias to a unique value (so repeated runs against a shared
// server never collide), and writes it to a fresh temp directory. Mirrors
// job_manifest_export_test.go's stageExampleManifest, which reads from
// docs/examples instead.
func (s *IntegrationTestSuite) stageDeveloperTestdata(fixture string) (alias string, dir string) {
	s.T().Helper()

	data, err := os.ReadFile(filepath.Join(s.projectRoot, "test", "developer_testdata", fixture))
	s.Require().NoError(err)

	var doc map[string]any
	s.Require().NoError(yaml.Unmarshal(data, &doc))

	metadata, ok := doc["metadata"].(map[string]any)
	s.Require().True(ok, "fixture %s has no metadata block", fixture)
	base, ok := metadata["alias"].(string)
	s.Require().True(ok, "fixture %s has no metadata.alias", fixture)

	alias = fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
	metadata["alias"] = alias

	encoded, err := yaml.Marshal(doc)
	s.Require().NoError(err)

	dir, err = os.MkdirTemp("", "caesium-developer-journey-*")
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(
		filepath.Join(dir, alias+".job.yaml"),
		[]byte(s.injectEngine(string(encoded))),
		0o644,
	))
	return alias, dir
}

// stepNames returns the ordered step names of a parsed definition.
func stepNames(steps []schema.Step) []string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.Name
	}
	return names
}
