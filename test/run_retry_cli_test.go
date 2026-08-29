//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// TestRunRetryCLIRoutesOverServer drives `caesium run retry --server` through
// its REAL surface: the CLI binary, not the internal handler or the REST
// endpoint directly.
//
// This closes issue #353. Before the fix, `cmd/run/retry.go` opened
// runstorage.Default() directly for a whole-run retry regardless of --server
// — only `--partition` went over the wire — so the whole-run path was the
// only retry transport never exercised against a live server in CI. The CLI
// process running here has no local dqlite connection to the containerized
// integration server (they are separate processes on separate storage), so
// the pre-fix in-process path would have failed outright rather than
// coincidentally working: the regression this test would have caught is a
// visible CLI error, not a silent behavior difference.
func (s *IntegrationTestSuite) TestRunRetryCLIRoutesOverServer() {
	alias := fmt.Sprintf("integration-retry-cli-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: step-ok
    image: alpine:3.23
    command: ["sh", "-c", "echo ok"]
    next: step-fail
  - name: step-fail
    image: alpine:3.23
    command: ["sh", "-c", "exit 1"]
    dependsOn: step-ok
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run: the second task fails, so the run fails.
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "run should fail because step-fail exits 1")
	taskStatuses := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatuses["step-ok"])
	s.Equal("failed", taskStatuses["step-fail"])

	// Retry the WHOLE run (no --partition) via the CLI with --server set
	// explicitly. Machine-relevant output is asserted on stdout only,
	// captured separately from stderr, per the repo's stdout-cleanliness gate
	// for CLI output.
	args := []string{
		"run", "retry",
		"--job-id", job.ID,
		"--run-id", runID,
		"--server", s.caesiumURL,
	}
	if s.authAPIKey != "" {
		args = append(args, "--api-key", s.authAPIKey)
	}
	stdout, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "run retry --server must succeed by going over the REST API")
	s.Contains(stdout, fmt.Sprintf("Retrying run %s (job %s)", runID, job.ID),
		"the server path's success line must match the in-process path's so scripts do not have to branch on transport: %s", stdout)

	// The retry must actually have reopened and re-run the SAME run through
	// the live server: it fails again (step-fail still exits 1), and the
	// already-succeeded task is preserved rather than re-executed — exactly
	// what RetryFromFailure on the server does, and something the pre-fix
	// in-process CLI path (talking to its own empty local store) could not
	// have produced.
	retried := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", retried.Status, "retry should fail again since step-fail still exits 1: %s", retried.Error)
	retriedStatuses := s.taskStatusesByName(job.ID, retried)
	s.Equal("succeeded", retriedStatuses["step-ok"], "the succeeded task must be preserved by a server-routed retry")
	s.Equal("failed", retriedStatuses["step-fail"], "the failed task must be re-attempted by a server-routed retry")
}
