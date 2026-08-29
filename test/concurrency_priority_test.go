//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestPriorityRunStartSurfacesAndCronDefault() {
	alias := fmt.Sprintf("e2e-priority-start-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(priorityJobManifest(alias, "low", `echo priority-start`))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	lowRunID := s.startRunREST(job.ID, "low", map[string]string{"lane": "low"})
	normalRunID := s.startRunREST(job.ID, "normal", map[string]string{"lane": "normal"})

	stdout, stderr, err := s.runCLISeparate(
		"run", "start",
		"--job-id", job.ID,
		"--priority", "high",
		"--params", "lane=high",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium run start failed:\n%s", stderr)
	// stdout must be the clean, parseable run id (asserted below); stderr legitimately
	// carries the live server's logs (the integration server runs at debug level), so we
	// assert only that no WARN/ERROR diagnostics surfaced — not that stderr is empty.
	s.NotContains(stderr, `"level":"warn"`, "caesium run start should surface no warnings when none are needed")
	s.NotContains(stderr, `"level":"error"`, "caesium run start should surface no errors")
	highRunID := strings.TrimSpace(stdout)
	s.Require().NotEmpty(highRunID)
	s.NotContains(highRunID, "\n")

	highRun := s.awaitRun(job.ID, highRunID, runTimeout)
	normalRun := s.awaitRun(job.ID, normalRunID, runTimeout)
	lowRun := s.awaitRun(job.ID, lowRunID, runTimeout)

	s.Equal(3, highRun.Priority, "CLI --priority must override the job metadata baseline")
	s.Equal(2, normalRun.Priority, "REST priority=normal must override the low job metadata baseline")
	s.Equal(1, lowRun.Priority)
	s.Require().NotEmpty(highRun.Tasks)
	s.Require().NotEmpty(normalRun.Tasks)
	s.Require().NotEmpty(lowRun.Tasks)
	s.Equal(3, highRun.Tasks[0].Priority)
	s.Equal(2, normalRun.Tasks[0].Priority)
	s.Equal(1, lowRun.Tasks[0].Priority)

	s.Run("worker holds siblings pending until it has pool capacity", func() {
		// Regression coverage for #355: a node must never START, and never KEEP,
		// a task `running` without a free pool slot for it.  The distributed
		// lane runs a one-slot worker (CAESIUM_WORKER_POOL_SIZE=1), so exactly
		// one task may be executing at a time; before the fix the worker
		// admitted a further pool's worth into its inbound buffer and parked one
		// more in pool.Submit, so three rows sat `running` behind one busy slot.
		//
		// WHAT THIS ACTUALLY PROVES: the PUSH-path admission gate
		// (Worker.SubmitDispatched refusing for lack of capacity, and the
		// owner rolling the claim back).  It does NOT exercise the pull path the
		// issue names.  Every justfile lane that sets
		// CAESIUM_EXECUTION_MODE=distributed also sets
		// CAESIUM_RUN_OWNER_ENABLED=true, so every run here takes a live
		// run_leases row, and Claimer.claimNextSingleStatementTx's liveLeaseGuard
		// excludes exactly those tasks — ClaimNext never sees them.  The
		// pull-path invariant ("no claim without a reserved slot") is guarded by
		// the unit test TestWorkerDoesNotClaimWithoutPoolCapacity, which fails on
		// the pre-fix ordering.  No lane in the matrix covers the pull path
		// end-to-end; that would need a distributed lane with
		// CAESIUM_RUN_OWNER_ENABLED=false (noted as a PR follow-up).
		//
		// Cross-run start ORDER is deliberately not asserted.  Because these
		// tasks reach the worker through the owner's dispatch loop, and that loop
		// selects within a run by created_at (Store.PendingTasksForDispatch has
		// no priority term) and iterates owned runs in Go map order, cross-run
		// priority ordering cannot hold in run-owner mode at all — see the
		// "owner dispatch loop ignores task_runs.priority" follow-up in the PR
		// body.  The claimer's own priority order stays covered by the unit test
		// TestClaimerClaimNextPrefersHigherPriority.
		if !strings.EqualFold(strings.TrimSpace(os.Getenv("CAESIUM_EXECUTION_MODE")), "distributed") {
			s.T().Skip("worker pool-capacity e2e requires CAESIUM_EXECUTION_MODE=distributed")
		}

		fillerAlias := fmt.Sprintf("e2e-priority-filler-%d", time.Now().UnixNano())
		fillerDir := s.writeJobManifest(priorityJobManifest(fillerAlias, "high", `sleep 15`))
		defer os.RemoveAll(fillerDir)
		s.runCLI("job", "apply", "--path", fillerDir, "--server", s.caesiumURL)
		fillerJob := s.requireJobByAlias(fillerAlias)
		s.Require().NotNil(fillerJob)

		fillerRunID := s.triggerRun(fillerJob.ID)
		fillerRunning := s.awaitFirstTaskStatus(fillerJob.ID, fillerRunID, runTimeout, "running")
		s.Require().NotNil(fillerRunning.Tasks[0].StartedAt, "filler task must occupy the single worker slot before priority runs are queued")

		blockedLowRunID := s.startRunREST(job.ID, "low", map[string]string{"lane": "blocked-low"})
		blockedNormalRunID := s.startRunREST(job.ID, "normal", map[string]string{"lane": "blocked-normal"})
		blockedHighRunID := s.startRunREST(job.ID, "high", map[string]string{"lane": "blocked-high"})

		blocked := map[string]string{
			"high":   blockedHighRunID,
			"normal": blockedNormalRunID,
			"low":    blockedLowRunID,
		}

		// A blocked run may FLICKER into `running` for a few tens of
		// milliseconds: on the push path the owner claims the row
		// (status=running, started_at set) before it offers the task to a
		// worker, and a worker with no free slot refuses, at which point the
		// owner rolls the claim straight back to `pending`.  That flicker is
		// the capacity gate working, so the assertion is not "never running"
		// but "never SETTLES in running": every `running` sighting has to clear
		// within dispatchRollbackGrace.  Before the fix two of the three sat
		// `running` for the filler's whole 15s hold, which this catches; the
		// grace is two orders of magnitude above the observed rollback latency
		// and five times below the filler's hold, so it separates the two
		// cleanly.
		const dispatchRollbackGrace = 3 * time.Second

		// Sample repeatedly for as long as the filler still holds the slot.  A
		// single sample would pass even on the broken build, which took about
		// one dispatch interval to over-admit the siblings.
		//
		// The 6s is a budget for SAMPLING only: every second spent waiting out a
		// rollback inside awaitClaimRolledBack is credited back to the deadline.
		// Otherwise a couple of slow rollbacks on a loaded CI box would eat the
		// window and trip the minimum-samples check below, whose message would
		// then blame the filler for something it did not do.
		samples := 0
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			filler := s.fetchRun(fillerJob.ID, fillerRunID)
			s.Require().NotEmpty(filler.Tasks)
			if filler.Tasks[0].Status != "running" {
				break // the filler released the slot; the hold no longer applies
			}
			samples++

			for label, runID := range blocked {
				observed := s.fetchRun(job.ID, runID)
				if len(observed.Tasks) == 0 {
					continue // run row exists but its tasks are not materialised yet
				}
				if observed.Tasks[0].Status != "running" {
					continue
				}
				rollbackWait := time.Now()
				cleared := s.awaitClaimRolledBack(job.ID, runID, fillerJob.ID, fillerRunID, dispatchRollbackGrace)
				deadline = deadline.Add(time.Since(rollbackWait))
				s.Require().True(cleared,
					"%s run settled in `running` while the one-slot worker was busy with the filler; "+
						"a task may only be `running` on a node that holds a free pool slot for it", label)
			}
			time.Sleep(250 * time.Millisecond)
		}
		s.Require().GreaterOrEqual(samples, 8,
			"took only %d samples while the filler held the slot; the capacity assertion needs more "+
				"observations to be meaningful (either the filler stopped running early or the server "+
				"is responding too slowly to sample it)", samples)

		fillerDone := s.awaitRun(fillerJob.ID, fillerRunID, runTimeout)
		s.Equal("succeeded", fillerDone.Status)
		s.Require().NotEmpty(fillerDone.Tasks)

		// Withholding the claim must not lose the work: every blocked run drains
		// once capacity frees.
		blockedHighRun := s.awaitRun(job.ID, blockedHighRunID, runTimeout)
		blockedNormalRun := s.awaitRun(job.ID, blockedNormalRunID, runTimeout)
		blockedLowRun := s.awaitRun(job.ID, blockedLowRunID, runTimeout)
		for label, run := range map[string]*runResponse{
			"high":   blockedHighRun,
			"normal": blockedNormalRun,
			"low":    blockedLowRun,
		} {
			s.Require().NotEmpty(run.Tasks, "%s run has no tasks", label)
			s.Require().NotNil(run.Tasks[0].StartedAt, "%s run never started", label)
			s.Equal("succeeded", run.Status, "%s run should succeed: %s", label, run.Error)
		}
	})

	cronAlias := fmt.Sprintf("e2e-priority-cron-%d", time.Now().UnixNano())
	cronDir := s.writeJobManifest(priorityJobManifest(cronAlias, "high", `echo cron-priority`))
	defer os.RemoveAll(cronDir)
	s.runCLI("job", "apply", "--path", cronDir, "--server", s.caesiumURL)
	cronJob := s.requireJobByAlias(cronAlias)

	cronRunID := s.triggerRun(cronJob.ID)
	cronRun := s.awaitRun(cronJob.ID, cronRunID, runTimeout)
	s.Equal(3, cronRun.Priority, "cron-configured job runs should inherit metadata.priority")
	s.Require().NotEmpty(cronRun.Tasks)
	s.Equal(3, cronRun.Tasks[0].Priority, "cron-configured job tasks should inherit metadata.priority")
}

// awaitClaimRolledBack reports whether the run's first task drops out of
// `running` within grace, while the filler run still occupies the worker's only
// pool slot.  On the run-owner push path a dispatch claims the row before the
// worker is asked to take it, so a task the worker refuses for lack of capacity
// is briefly `running` before the owner's rollback lands — a settled `running`
// is the bug (#355), a flicker is not.  Returns true early if the filler itself
// finishes, because from that moment `running` is legitimate again.
func (s *IntegrationTestSuite) awaitClaimRolledBack(jobID, runID, fillerJobID, fillerRunID string, grace time.Duration) bool {
	s.T().Helper()

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)

		observed := s.fetchRun(jobID, runID)
		if len(observed.Tasks) == 0 || observed.Tasks[0].Status != "running" {
			return true
		}

		filler := s.fetchRun(fillerJobID, fillerRunID)
		if len(filler.Tasks) == 0 || filler.Tasks[0].Status != "running" {
			return true
		}
	}
	return false
}

func (s *IntegrationTestSuite) startRunREST(jobID, priority string, params map[string]string) string {
	s.T().Helper()
	body, err := json.Marshal(map[string]any{
		"priority": priority,
		"params":   params,
	})
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var run runResponse
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&run))
	s.Require().NotEmpty(run.ID)
	return run.ID
}

func (s *IntegrationTestSuite) awaitFirstTaskStatus(jobID, runID string, timeout time.Duration, statuses ...string) runResponse {
	s.T().Helper()
	want := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		want[status] = struct{}{}
	}

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for run %s first task to reach one of %v", runID, statuses)
		}

		run := s.fetchRun(jobID, runID)
		if len(run.Tasks) > 0 {
			if _, ok := want[run.Tasks[0].Status]; ok {
				return run
			}
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func priorityJobManifest(alias, priority, command string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  priority: %s
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
  - name: priority-step
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, priority, command)
}
