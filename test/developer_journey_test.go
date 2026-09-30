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
// Fixed for #479: internal/atom/kubernetes.NewEngine used to panic on this
// input (getKubernetesCore called panic(err) when neither a kubeconfig nor
// in-cluster config resolved) and internal/job.buildLocalRunners called the
// engine factory with no recover(), so the process exited 2 with a Go panic
// trace on stderr. NewEngine now returns an error instead, which
// buildLocalRunners surfaces as a clean task failure — this asserts the
// specific fixed contract: exit 1 (not the panic's exit 2), no panic trace on
// either stream, and a message naming the failing engine and cause.
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

	stdout, stderr, err := s.runCLISeparate("dev", "--once", "--path", dir)
	s.Require().Error(err, "dev --once must not succeed when the declared engine has no reachable backend")
	s.Equal(1, reproduceExitCode(err),
		"an unreachable kubernetes engine must be a clean exit 1, not an unrecovered panic's exit 2:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)

	combined := stdout + stderr
	s.NotContains(combined, "panic:", "must not surface a Go panic trace")
	s.NotContains(combined, "goroutine ", "must not surface a Go panic stack trace")
	s.Contains(combined, "kubernetes engine unavailable", "must name the failing engine and cause")
	// Must point at the setting this constructor actually reads
	// (CAESIUM_KUBERNETES_CONFIG), not the standard KUBECONFIG env var, which
	// has no effect here since clientcmd.BuildConfigFromFlags is given an
	// explicit path.
	s.Contains(combined, "CAESIUM_KUBERNETES_CONFIG", "must point the developer at the setting that actually works")
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

// TestDevOnceSIGINTStopsAndRemovesContainer fixes #480: `caesium dev --once`
// used to have no signal handling at all — SIGINT was a no-op, the process
// had to be SIGKILLed, and its running step container was orphaned. This
// sends the process a real SIGINT (the way Ctrl-C would) while a long-running
// step container is up, mirroring
// TestDevOnceRunTimeoutStopsAndRemovesContainer's shape (which proves the
// SAME cancel-and-cleanup path for --run-timeout) but for an operator
// interrupt: the run must be cancelled, its owned container stopped AND
// removed (not merely left running), and the process must exit nonzero
// without printing a Go panic trace.
func (s *IntegrationTestSuite) TestDevOnceSIGINTStopsAndRemovesContainer() {
	s.requireDocker()
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the container-cleanup assertion inspects containers over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	marker := fmt.Sprintf("caesium-dev-sigint-marker-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: dev-journey-sigint
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

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, "dev", "--once", "--path", dir)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	syncOut := &syncBuffer{}
	cmd.Stdout = syncOut
	cmd.Stderr = syncOut
	s.Require().NoError(cmd.Start())
	// Safety net in case an assertion below fails before the signal is sent.
	defer func() { _ = cmd.Process.Kill() }()

	var orphanIDs []string
	s.Require().Eventually(func() bool {
		orphanIDs = s.runningContainerIDsWithMarker(cli, marker)
		return len(orphanIDs) > 0
	}, 30*time.Second, 500*time.Millisecond, "dev --once never started a container carrying %q", marker)

	s.Require().NoError(cmd.Process.Signal(syscall.SIGINT))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		s.Require().Error(waitErr, "dev --once must exit nonzero after SIGINT:\n%s", syncOut.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		s.Fail("dev --once did not exit after SIGINT", syncOut.String())
	}

	s.NotContains(syncOut.String(), "panic:", "SIGINT must not surface a Go panic trace")
	s.NotContains(syncOut.String(), "goroutine ", "SIGINT must not surface a Go panic stack trace")

	s.Require().Eventually(func() bool {
		for _, id := range orphanIDs {
			if s.containerIsRunning(cli, id) {
				return false
			}
		}
		return true
	}, 30*time.Second, time.Second,
		"container(s) %v still running after SIGINT; dev --once must stop and remove what it started", orphanIDs)

	s.Empty(s.containerIDsWithMarker(cli, marker),
		"the interrupted run's container must be removed, not merely stopped")
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

// TestDevWatchModeDiscoversNewNestedDAGAndEdits fixes #515: watch mode
// discovered DAGs recursively at startup but only added fsnotify watches for
// directories that already contained YAML then, and filtered out events
// whose names weren't YAML paths — so `jobs/new/new.job.yaml`, created after
// watch mode started, was never seen, and neither were edits to it, until an
// ALREADY-watched file changed and the next full rescan happened to pick both
// up. This reproduces the issue's own repro/controls against the real CLI: an
// in-place edit and an atomic (rename) save of the ORIGINAL file both keep
// working, a brand new nested subdirectory created while watching is
// discovered without touching anything else, and an edit to that newly
// discovered file triggers its own re-run too.
func (s *IntegrationTestSuite) TestDevWatchModeDiscoversNewNestedDAGAndEdits() {
	s.requireDocker()

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	rootAlias := fmt.Sprintf("dev-journey-watch-nested-root-%d", time.Now().UnixNano())
	nestedAlias := fmt.Sprintf("dev-journey-watch-nested-child-%d", time.Now().UnixNano())
	recreatedV3Alias := fmt.Sprintf("dev-journey-watch-recreated-v3-%d", time.Now().UnixNano())
	recreatedV4Alias := fmt.Sprintf("dev-journey-watch-recreated-v4-%d", time.Now().UnixNano())
	marker := fmt.Sprintf("caesium-dev-watch-nested-marker-%d", time.Now().UnixNano())

	rootManifest := fmt.Sprintf(`
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
`, rootAlias, fmt.Sprintf("echo root-%s", marker))

	nestedManifest := func(alias, v string) string {
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
`, alias, fmt.Sprintf("echo nested-%s-%s", v, marker))
	}

	dir := s.writeJobManifest(rootManifest)
	defer os.RemoveAll(dir)
	defer s.removeContainersWithMarker(cli, marker)

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, "dev", "--path", dir)
	cmd.Dir = s.projectRoot
	cmd.Env = os.Environ()
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	s.Require().NoError(cmd.Start())
	defer func() { _ = cmd.Process.Kill() }()

	okCount := func(alias string) int {
		return strings.Count(out.String(), "  OK    "+alias)
	}

	s.Require().Eventually(func() bool {
		return strings.Contains(out.String(), "Watching")
	}, 60*time.Second, 200*time.Millisecond, "dev never announced watch mode:\n%s", out.String())
	s.Require().Eventually(func() bool {
		return okCount(rootAlias) >= 1
	}, 60*time.Second, 200*time.Millisecond, "the initial run of the root job never completed:\n%s", out.String())

	// Control 1 (issue repro step 2): an in-place edit of the ORIGINAL file
	// must still trigger a re-run.
	s.rewriteJobManifest(dir, rootManifest)
	s.Require().Eventually(func() bool {
		return okCount(rootAlias) >= 2
	}, 60*time.Second, 200*time.Millisecond, "in-place edit of the root job never re-ran:\n%s", out.String())

	// Control 2 (issue repro step 3): an atomic (rename) save of the
	// ORIGINAL file must still trigger a re-run.
	tmp := filepath.Join(dir, "job.yaml.tmp")
	s.Require().NoError(os.WriteFile(tmp, []byte(strings.TrimSpace(s.injectEngine(rootManifest))), 0o644))
	s.Require().NoError(os.Rename(tmp, filepath.Join(dir, "job.yaml")))
	s.Require().Eventually(func() bool {
		return okCount(rootAlias) >= 3
	}, 60*time.Second, 200*time.Millisecond, "atomic (rename) save of the root job never re-ran:\n%s", out.String())

	// The actual #515 repro (steps 4-5): create a brand new nested
	// subdirectory with its own DAG WHILE watch mode is running, and wait —
	// no other file is touched. Before the fix this was silently ignored
	// (no error, no re-run) until an already-watched file changed again.
	nestedDir := filepath.Join(dir, "new")
	s.Require().NoError(os.MkdirAll(nestedDir, 0o755))
	s.Require().NoError(os.WriteFile(
		filepath.Join(nestedDir, "new.job.yaml"),
		[]byte(strings.TrimSpace(s.injectEngine(nestedManifest(nestedAlias, "v1")))),
		0o644,
	))
	s.Require().Eventually(func() bool {
		return okCount(nestedAlias) >= 1
	}, 60*time.Second, 200*time.Millisecond,
		"dev never discovered the new nested DAG created while watching:\n%s", out.String())

	// An edit to the newly discovered nested file must also trigger its own
	// re-run (issue repro step 6's premise the other way around: not just
	// the next rescan of the OLD file resurrecting it).
	s.Require().NoError(os.WriteFile(
		filepath.Join(nestedDir, "new.job.yaml"),
		[]byte(strings.TrimSpace(s.injectEngine(nestedManifest(nestedAlias, "v2")))),
		0o644,
	))
	s.Require().Eventually(func() bool {
		return okCount(nestedAlias) >= 2
	}, 60*time.Second, 200*time.Millisecond,
		"dev never re-ran after editing the newly discovered nested DAG:\n%s", out.String())

	// Remove the dynamically watched subtree entirely, then recreate it. A
	// remove event must discard watches for the old inode; the parent tree must
	// notice the recreation, rescan the manifest written before the new watch
	// exists, and install a fresh watch that receives a later edit.
	s.Require().NoError(os.RemoveAll(nestedDir))
	s.Require().NoError(os.MkdirAll(nestedDir, 0o755))
	s.Require().NoError(os.WriteFile(
		filepath.Join(nestedDir, "new.job.yaml"),
		[]byte(strings.TrimSpace(s.injectEngine(nestedManifest(recreatedV3Alias, "v3")))),
		0o644,
	))
	s.Require().Eventually(func() bool {
		return okCount(recreatedV3Alias) >= 1
	}, 60*time.Second, 200*time.Millisecond,
		"dev never discovered the recreated nested DAG:\n%s", out.String())

	s.Require().NoError(os.WriteFile(
		filepath.Join(nestedDir, "new.job.yaml"),
		[]byte(strings.TrimSpace(s.injectEngine(nestedManifest(recreatedV4Alias, "v4")))),
		0o644,
	))
	s.Require().Eventually(func() bool {
		return okCount(recreatedV4Alias) >= 1
	}, 60*time.Second, 200*time.Millisecond,
		"dev never re-ran after editing the recreated nested DAG:\n%s", out.String())

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

	s.Empty(s.containerIDsWithMarker(cli, marker),
		"no container from any watch-mode run should remain after a graceful stop")
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
