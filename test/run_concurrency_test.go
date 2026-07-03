//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func (s *IntegrationTestSuite) TestRunConcurrencyStrategies() {
	s.Run("skip drops overlapping run", func() {
		job := s.applyConcurrencyJob("skip", `sleep 6`)
		firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, firstStatus)
		s.NotEmpty(firstRunID)

		secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, secondStatus)
		s.Empty(secondRunID, "skipped run should return 202 with no created run")

		s.Require().Eventually(func() bool {
			return len(s.fetchRuns(job.ID)) == 1
		}, 5*time.Second, 250*time.Millisecond)
	})

	s.Run("fail returns conflict", func() {
		job := s.applyConcurrencyJob("fail", `sleep 6`)
		firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, firstStatus)
		s.NotEmpty(firstRunID)

		secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusConflict, secondStatus)
		s.Empty(secondRunID)
	})

	s.Run("replace cancels oldest and starts fresh", func() {
		job := s.applyConcurrencyJob("replace", `sleep 8`)
		firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, firstStatus)
		s.NotEmpty(firstRunID)
		s.awaitRunHasTasks(job.ID, firstRunID, 10*time.Second)

		secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, secondStatus)
		s.NotEmpty(secondRunID)
		s.NotEqual(firstRunID, secondRunID)

		// The replace flips the run and its non-terminal tasks to "cancelled" in
		// a single transaction, and the store now refuses to resurrect a terminal
		// task (StartTask/completeTask skip an already-cancelled row), so in local
		// execution mode the orphaned container can no longer overwrite the task
		// back to running/succeeded — the cancelled state is durable. The only
		// remaining transient is a torn read under the read/write connection
		// split, where a GET can momentarily surface the cancelled run alongside a
		// task still reporting its pre-cancel "running" value. Poll until the run
		// and every task converge rather than snapshotting the instant the
		// run-level status flips.
		cancelled := s.awaitRunCancelled(job.ID, firstRunID, 15*time.Second)
		s.Equal("cancelled", cancelled.Status)
		s.NotEmpty(cancelled.Tasks, "cancelled run should still expose its tasks")
		for _, task := range cancelled.Tasks {
			s.Equal("cancelled", task.Status, "cancelled run's tasks must be unclaimable")
		}
	})

	s.Run("queue parks then dequeues", func() {
		job := s.applyConcurrencyJob("queue", `sleep 3`)
		firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, firstStatus)
		s.NotEmpty(firstRunID)

		secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, secondStatus)
		s.Empty(secondRunID, "queued admission should not create a run synchronously")

		s.Equal("succeeded", s.awaitRunStatus(job.ID, firstRunID, runTimeout, "succeeded").Status)
		var queuedRunID string
		s.Require().Eventually(func() bool {
			for _, run := range s.fetchRuns(job.ID) {
				if run.ID != firstRunID {
					queuedRunID = run.ID
					return true
				}
			}
			return false
		}, 30*time.Second, time.Second)
		s.NotEmpty(queuedRunID)
		s.Equal("succeeded", s.awaitRunStatus(job.ID, queuedRunID, runTimeout, "succeeded").Status)
	})

	s.Run("queue reclaims stale claim", func() {
		if s.engineType == "kubernetes" {
			s.T().Skipf("stale-claim reclaim setup writes run_queue directly; dqlite binds to POD_IP under CAESIUM_TEST_ENGINE=%s and is not port-forward-reachable; covered on the docker + podman lanes", s.engineType)
		}
		job := s.applyConcurrencyJob("queue", `sleep 3`)
		firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, firstStatus)
		s.NotEmpty(firstRunID)

		secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
		s.Equal(http.StatusAccepted, secondStatus)
		s.Empty(secondRunID, "queued admission should not create a run synchronously")

		s.staleClaimQueuedRun(job.ID)

		s.Equal("succeeded", s.awaitRunStatus(job.ID, firstRunID, runTimeout, "succeeded").Status)
		var reclaimedRunID string
		s.Require().Eventually(func() bool {
			for _, run := range s.fetchRuns(job.ID) {
				if run.ID != firstRunID {
					reclaimedRunID = run.ID
					return true
				}
			}
			return false
		}, 30*time.Second, time.Second)
		s.NotEmpty(reclaimedRunID)
		s.Equal("succeeded", s.awaitRunStatus(job.ID, reclaimedRunID, runTimeout, "succeeded").Status)
	})
}

func (s *IntegrationTestSuite) TestRunConcurrencyAtomicAdmissionTOCTOU() {
	job := s.applyConcurrencyJob("fail", `sleep 6`)

	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		status int
		runID  string
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, runID := s.postConcurrencyRun(job.ID)
			results <- result{status: status, runID: runID}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	admitted := 0
	conflicts := 0
	for got := range results {
		switch got.status {
		case http.StatusAccepted:
			if got.runID != "" {
				admitted++
			}
		case http.StatusConflict:
			conflicts++
		}
	}
	s.Equal(1, admitted, "two near-simultaneous maxRuns=1 admissions must create exactly one run")
	s.Equal(1, conflicts)
}

func (s *IntegrationTestSuite) applyConcurrencyJob(strategy, command string) *jobSummary {
	alias := fmt.Sprintf("e2e-concurrency-%s-%d", strategy, time.Now().UnixNano())
	dir := s.writeJobManifest(concurrencyJobManifest(alias, strategy, command))
	s.T().Cleanup(func() { _ = os.RemoveAll(dir) })
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	return s.requireJobByAlias(alias)
}

func (s *IntegrationTestSuite) postConcurrencyRun(jobID string) (int, string) {
	s.T().Helper()
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	if len(body) == 0 {
		return resp.StatusCode, ""
	}
	var run runResponse
	if err := json.Unmarshal(body, &run); err != nil {
		return resp.StatusCode, ""
	}
	return resp.StatusCode, run.ID
}

func (s *IntegrationTestSuite) staleClaimQueuedRun(jobID string) {
	s.T().Helper()
	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	ctx := s.T().Context()
	var queueID string
	s.Require().Eventually(func() bool {
		err := catalogDB.QueryRowContext(ctx, `
SELECT id
FROM run_queue
WHERE job_id = ? AND claimed_by = ''
ORDER BY created_at ASC
LIMIT 1
`, jobID).Scan(&queueID)
		return err == nil
	}, 10*time.Second, 250*time.Millisecond)
	s.Require().NotEmpty(queueID)

	staleClaimedAt := time.Now().UTC().Add(-5 * time.Minute)
	result, err := catalogDB.ExecContext(ctx, `
UPDATE run_queue
SET claimed_by = ?, claimed_at = ?
WHERE id = ? AND claimed_by = ''
`, "lost-leader/e2e", staleClaimedAt, queueID)
	s.Require().NoError(err)
	affected, err := result.RowsAffected()
	s.Require().NoError(err)
	s.Equal(int64(1), affected)
}

// awaitRunCancelled polls until the run reaches the terminal "cancelled" status
// AND every one of its tasks has also converged to "cancelled". A concurrency
// "replace" flips the run and its non-terminal tasks in a single transaction,
// but under the read/write connection split a GET can momentarily observe the
// cancelled run alongside a task still reporting its pre-cancel "running" state.
// Since the committed end state is always all-cancelled, we wait for it to
// converge instead of asserting on the first cancelled snapshot. A task that
// genuinely ended in another terminal status (e.g. "succeeded") never satisfies
// the predicate, so a real "ran to completion despite cancel" regression still
// fails here — loudly, with the observed statuses.
func (s *IntegrationTestSuite) awaitRunCancelled(jobID, runID string, timeout time.Duration) runResponse {
	s.T().Helper()
	deadline := time.Now().Add(timeout)
	for {
		run := s.fetchRun(jobID, runID)
		if run.Status == "cancelled" && allTasksHaveStatus(run.Tasks, "cancelled") {
			return run
		}
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for run %s and all tasks to converge to cancelled; last run status=%q tasks=[%s]",
				runID, run.Status, taskStatusSummary(run.Tasks))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// allTasksHaveStatus reports whether every task carries status. It returns false
// for an empty slice so callers cannot mistake "no tasks yet" for convergence.
func allTasksHaveStatus(tasks []runTaskResponse, status string) bool {
	if len(tasks) == 0 {
		return false
	}
	for _, t := range tasks {
		if t.Status != status {
			return false
		}
	}
	return true
}

// taskStatusSummary renders "id=status" pairs for a timeout failure message.
func taskStatusSummary(tasks []runTaskResponse) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		parts = append(parts, fmt.Sprintf("%s=%s", t.ID, t.Status))
	}
	return strings.Join(parts, ", ")
}

func (s *IntegrationTestSuite) awaitRunStatus(jobID, runID string, timeout time.Duration, statuses ...string) runResponse {
	s.T().Helper()
	want := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		want[status] = struct{}{}
	}
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for run %s to reach one of %v", runID, statuses)
		}
		run := s.fetchRun(jobID, runID)
		if _, ok := want[run.Status]; ok {
			return run
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *IntegrationTestSuite) awaitRunHasTasks(jobID, runID string, timeout time.Duration) runResponse {
	s.T().Helper()
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for run %s to materialize tasks", runID)
		}
		run := s.fetchRun(jobID, runID)
		if len(run.Tasks) > 0 {
			return run
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func concurrencyJobManifest(alias, strategy, command string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  concurrency:
    maxRuns: 1
    strategy: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: hold
    image: alpine:3.23
    command: ["sh", "-c", %q]
`, alias, strategy, command)
}
