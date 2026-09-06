//go:build integration

package test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// A cancelled run must stop its CONTAINER, not just write `cancelled` on the
// row.
//
// The concurrency `replace` admission cancels the older run in one transaction
// (cancelRunTx: every non-terminal task row goes `cancelled`, claimed_by is
// blanked, run_cancelled is published) and PR #275 stopped the orphan's
// eventual exit from resurrecting the row — but the container itself kept
// running to completion, holding a pool slot, a rate-limit token and whatever
// the step does to the outside world. Both lanes now close that:
//
//	local       — the run_cancelled event cancels the engine's registered run
//	              context (internal/job/cancel_registry.go) and the executor's
//	              taskCtx.Done() branch force-stops the atom.
//	distributed — the blanked claimed_by makes the worker's next batched
//	              RenewLeases match fewer rows; the worker cancels that task's
//	              context and monitorTask force-stops the atom.
//
// The container is identified by a unique marker embedded in the step COMMAND
// and read back from Config.Cmd: containers carry no run or task labels
// (pkg/container's Spec.Labels is caller-supplied and neither executor sets
// one) and models.TaskRun records no container id, so the command is the only
// handle the run gives us. Both runs of the job share that command, so the
// FIRST run's container ids are snapshotted before the replacement is
// triggered and the assertion is scoped to exactly those ids.
//
// The distributed lane detects the loss on the WORKER-claim renewal ticker,
// whose period is CAESIUM_WORKER_LEASE_TTL/4 — 7.5 s on this lane, which sets
// it to 30s, and 75 s on the 5 m default. (Not CAESIUM_RUN_LEASE_TTL: that
// governs run-lease ownership, a different ticker.) The liveness check runs on
// every tick regardless of whether a renewal is due, so detection is bounded by
// one tick; the deadline below is generous by design.
const cancelledContainerDeadline = 90 * time.Second

func (s *IntegrationTestSuite) TestReplaceCancelStopsOrphanedContainer() {
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the orphaned-container assertion inspects containers over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	marker := fmt.Sprintf("caesium-cancel-marker-%d", time.Now().UnixNano())
	job := s.applyConcurrencyJob("replace", fmt.Sprintf("sleep 120 # %s", marker))
	// Whatever the assertions do, do not leave a `sleep 120` container (the
	// REPLACEMENT run's, which is supposed to keep running) on a shared daemon.
	defer s.removeContainersWithMarker(cli, marker)

	firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
	s.Require().Equal(http.StatusAccepted, firstStatus)
	s.Require().NotEmpty(firstRunID)

	// The container has to EXIST before the cancel, or the scenario proves
	// nothing about reaching it.
	var orphanIDs []string
	s.Require().Eventually(func() bool {
		orphanIDs = s.runningContainerIDsWithMarker(cli, marker)
		return len(orphanIDs) > 0
	}, 60*time.Second, time.Second, "the first run never started a container carrying %q", marker)

	secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
	s.Require().Equal(http.StatusAccepted, secondStatus)
	s.Require().NotEmpty(secondRunID)
	s.Require().NotEqual(firstRunID, secondRunID)

	cancelled := s.awaitRunCancelled(job.ID, firstRunID, 30*time.Second)
	s.Equal("cancelled", cancelled.Status)

	s.Require().Eventually(func() bool {
		for _, id := range orphanIDs {
			if s.containerIsRunning(cli, id) {
				return false
			}
		}
		return true
	}, cancelledContainerDeadline, 2*time.Second,
		"the cancelled run's container(s) %v are still running; a cancelled run must reach the container, not only the row", orphanIDs)

	s.retireReplacementRun(cli, marker, orphanIDs, job.ID, secondRunID)
}

// retireReplacementRun tears down the run the `replace` admission STARTED.
//
// It is not tidiness: the replacement is running the same `sleep 120`, the
// distributed lane runs one worker slot (CAESIUM_WORKER_POOL_SIZE=1), and a
// scenario that walks away leaves the next two minutes of that lane with no
// capacity. That is not hypothetical — it is how this scenario's first run took
// TestRetryAfterApplyExecutesRegisteredCommand down with it, as a 120 s
// "timeout waiting for run to complete" that looks nothing like its cause.
//
// The replacement's container is force-removed (its task then fails, exactly as
// if the container had died) and the run is required to reach a terminal status
// before the scenario returns, so the slot is provably free for the next test.
func (s *IntegrationTestSuite) retireReplacementRun(cli *client.Client, marker string, orphanIDs []string, jobID, runID string) {
	s.T().Helper()

	orphaned := make(map[string]bool, len(orphanIDs))
	for _, id := range orphanIDs {
		orphaned[id] = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The replacement is only claimable once the cancelled run's slot frees, so
	// its container appears shortly AFTER the assertion above — poll for it.
	s.Require().Eventually(func() bool {
		removed := false
		for _, id := range s.containerIDsWithMarker(cli, marker) {
			if orphaned[id] {
				continue
			}
			if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
				s.T().Logf("retiring replacement container %s: %v", id, err)
				continue
			}
			removed = true
		}
		return removed
	}, 60*time.Second, time.Second,
		"the replacement run never started a container carrying %q, so it could not be retired", marker)

	s.awaitRunStatus(jobID, runID, 60*time.Second, "succeeded", "failed", "cancelled")
}

// runningContainerIDsWithMarker returns the ids of RUNNING containers whose
// command carries the marker.
func (s *IntegrationTestSuite) runningContainerIDsWithMarker(cli *client.Client, marker string) []string {
	s.T().Helper()
	var out []string
	for _, id := range s.containerIDsWithMarker(cli, marker) {
		if s.containerIsRunning(cli, id) {
			out = append(out, id)
		}
	}
	return out
}

// containerIDsWithMarker returns every container (running or not) whose
// Config.Cmd mentions the marker.
func (s *IntegrationTestSuite) containerIDsWithMarker(cli *client.Client, marker string) []string {
	s.T().Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		s.T().Logf("container list failed: %v", err)
		return nil
	}
	var out []string
	for _, summary := range summaries {
		info, err := cli.ContainerInspect(ctx, summary.ID)
		if err != nil {
			// Raced with removal — which is the outcome this scenario wants.
			continue
		}
		if info.Config == nil {
			continue
		}
		if strings.Contains(strings.Join(info.Config.Cmd, " "), marker) {
			out = append(out, summary.ID)
		}
	}
	return out
}

// containerIsRunning reports whether the container still exists AND is running.
// A removed container (Stop removes it — internal/atom/docker/engine.go) is not
// running.
func (s *IntegrationTestSuite) containerIsRunning(cli *client.Client, id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return false
	}
	return info.State != nil && info.State.Running
}

func (s *IntegrationTestSuite) removeContainersWithMarker(cli *client.Client, marker string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, id := range s.containerIDsWithMarker(cli, marker) {
		if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
			s.T().Logf("cleanup: failed to remove container %s: %v", id, err)
		}
	}
}

// TestReplaceCancelStopsTriggerOriginatedContainer is the same contract for a
// run nobody kicked off through a controller.
//
// The cancel registration originally lived only at the six kickoff sites, and
// the trigger paths are not among them: cron (scheduled and catch-up), http,
// event and webhook triggers all call job.New(...).Run(ctx) and let job.Run
// resolve the run id itself. Every trigger-originated run was therefore
// uncancellable — and silently, because CancelRunContexts returns 0 and its log
// line is gated on n > 0, so a cancelled cron run looked exactly like a
// cancelled manual one while its container carried on. Registration now happens
// inside job.Run, and this scenario is what proves it for a run that never
// passed through POST /v1/jobs/:id/run.
func (s *IntegrationTestSuite) TestReplaceCancelStopsTriggerOriginatedContainer() {
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the orphaned-container assertion inspects containers over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	marker := fmt.Sprintf("caesium-hook-cancel-marker-%d", time.Now().UnixNano())
	alias := fmt.Sprintf("e2e-hook-replace-%d", time.Now().UnixNano())
	hook := fmt.Sprintf("hook-replace-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  concurrency:
    maxRuns: 1
    strategy: replace
trigger:
  type: http
  configuration:
    path: "/hooks/%s"
steps:
  - name: hold
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, hook, fmt.Sprintf("sleep 120 # %s", marker))

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	defer s.removeContainersWithMarker(cli, marker)

	fire := func() {
		req, err := http.NewRequestWithContext(s.T().Context(), http.MethodPost,
			fmt.Sprintf("%s/v1/hooks/%s", s.caesiumURL, hook), strings.NewReader(`{}`))
		s.Require().NoError(err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		s.Require().NoError(err)
		defer resp.Body.Close()
		s.Require().Equal(http.StatusAccepted, resp.StatusCode)
	}

	fire()

	var firstRunID string
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(job.ID)
		if len(runs) == 0 {
			return false
		}
		firstRunID = runs[0].ID
		return firstRunID != ""
	}, 60*time.Second, 500*time.Millisecond, "the http trigger never started a run")

	var orphanIDs []string
	s.Require().Eventually(func() bool {
		orphanIDs = s.runningContainerIDsWithMarker(cli, marker)
		return len(orphanIDs) > 0
	}, 60*time.Second, time.Second, "the triggered run never started a container carrying %q", marker)

	// A second fire replaces the first run, which is the only cancel route a
	// trigger-originated run has.
	fire()

	cancelled := s.awaitRunCancelled(job.ID, firstRunID, 60*time.Second)
	s.Equal("cancelled", cancelled.Status)

	s.Require().Eventually(func() bool {
		for _, id := range orphanIDs {
			if s.containerIsRunning(cli, id) {
				return false
			}
		}
		return true
	}, cancelledContainerDeadline, 2*time.Second,
		"a cancelled TRIGGER-originated run must reach its container too; %v still running", orphanIDs)

	var secondRunID string
	s.Require().Eventually(func() bool {
		for _, r := range s.fetchRuns(job.ID) {
			if r.ID != firstRunID {
				secondRunID = r.ID
				return true
			}
		}
		return false
	}, 60*time.Second, time.Second, "the replacement run never appeared")
	s.retireReplacementRun(cli, marker, orphanIDs, job.ID, secondRunID)
}

// TestReplaceCancelWithRetriesStopsOrphanedContainer covers the case where the
// cancel used to CREATE the container it was supposed to stop.
//
// Cancelling attempt 1 makes the attempt fail, and a retry budget turned that
// failure into attempt 2: retryTask had no terminal guard, so it flipped the
// cancelled row back to pending, StartTask's own guard then saw a legitimately
// pending row, and a second container started on a run the operator had already
// cancelled. `retries: 1` is the whole point of the fixture.
func (s *IntegrationTestSuite) TestReplaceCancelWithRetriesStopsOrphanedContainer() {
	if s.engineType != "" && s.engineType != "docker" {
		s.T().Skipf("the orphaned-container assertion inspects containers over the docker SDK; engine=%s", s.engineType)
	}

	cli := s.dockerClient()
	defer func() { _ = cli.Close() }()

	marker := fmt.Sprintf("caesium-retry-cancel-marker-%d", time.Now().UnixNano())
	alias := fmt.Sprintf("e2e-retry-replace-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  concurrency:
    maxRuns: 1
    strategy: replace
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: hold
    image: alpine:3.23
    retries: 1
    command: ["sh", "-c", %q]
`, alias, fmt.Sprintf("sleep 120 # %s", marker))

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	defer s.removeContainersWithMarker(cli, marker)

	firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
	s.Require().Equal(http.StatusAccepted, firstStatus)
	s.Require().NotEmpty(firstRunID)

	var orphanIDs []string
	s.Require().Eventually(func() bool {
		orphanIDs = s.runningContainerIDsWithMarker(cli, marker)
		return len(orphanIDs) > 0
	}, 60*time.Second, time.Second, "the first run never started a container carrying %q", marker)

	secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
	s.Require().Equal(http.StatusAccepted, secondStatus)
	s.Require().NotEmpty(secondRunID)

	cancelled := s.awaitRunCancelled(job.ID, firstRunID, 60*time.Second)
	s.Equal("cancelled", cancelled.Status)

	s.Require().Eventually(func() bool {
		for _, id := range orphanIDs {
			if s.containerIsRunning(cli, id) {
				return false
			}
		}
		return true
	}, cancelledContainerDeadline, 2*time.Second,
		"the cancelled run's container(s) %v are still running", orphanIDs)

	// The retry budget must not have been spent: the cancelled run may never
	// start a SECOND container of its own. The replacement run has one, so the
	// count is scoped to containers whose name carries the cancelled run id.
	running := s.runningContainerIDsWithMarker(cli, marker)
	for _, id := range running {
		s.NotContains(s.containerName(cli, id), firstRunID,
			"a cancelled run must not spend its retry budget on a fresh container")
	}

	s.retireReplacementRun(cli, marker, orphanIDs, job.ID, secondRunID)
}

// containerName returns a container's name, or "" when it has already gone.
// Container names are "{taskID}-{runID}[-attemptN]" (internal/job/job.go
// atomName), which is what makes a run id findable in them.
func (s *IntegrationTestSuite) containerName(cli *client.Client, id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return ""
	}
	return info.Name
}
