//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestJobQueueCancelEndpointDeletesUnclaimedRowOnly() {
	job := s.applyConcurrencyJob("queue", `sleep 6`)
	firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
	s.Equal(http.StatusAccepted, firstStatus)
	s.Require().NotEmpty(firstRunID)

	secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
	s.Equal(http.StatusAccepted, secondStatus)
	s.Empty(secondRunID, "queued admission should not create a run synchronously")

	queued := s.requireQueuedRun(job.ID)

	status, body := s.deleteQueuedRun(job.ID, queued.ID)
	s.Equal(http.StatusNoContent, status, body)

	s.requireQueueEmpty(job.ID)
	runs := s.fetchRuns(job.ID)
	s.Require().Len(runs, 1, "cancelled queued row must not create a JobRun")
	s.Equal(firstRunID, runs[0].ID)

	s.Equal("succeeded", s.awaitRunStatus(job.ID, firstRunID, runTimeout, "succeeded").Status)
	runs = s.fetchRuns(job.ID)
	s.Require().Len(runs, 1, "cancelled queued row must not start after the active run drains")
	s.Equal(firstRunID, runs[0].ID)
}

func (s *IntegrationTestSuite) TestJobQueueCancelEndpointConflictsWithClaimedRow() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("claimed run_queue race setup writes run_queue directly; dqlite binds to POD_IP under CAESIUM_TEST_ENGINE=%s and is not port-forward-reachable; covered on the docker + podman lanes", s.engineType)
	}

	job := s.applyConcurrencyJob("queue", `sleep 6`)
	firstStatus, firstRunID := s.postConcurrencyRun(job.ID)
	s.Equal(http.StatusAccepted, firstStatus)
	s.Require().NotEmpty(firstRunID)

	secondStatus, secondRunID := s.postConcurrencyRun(job.ID)
	s.Equal(http.StatusAccepted, secondStatus)
	s.Empty(secondRunID, "queued admission should not create a run synchronously")

	queued := s.requireQueuedRun(job.ID)
	s.Equal("pending", queued.ClaimState, "an unclaimed queued run reads as pending")
	claimOwner := "cancel-race/e2e"
	s.setQueuedRunClaim(job.ID, queued.ID, claimOwner, time.Now().UTC())

	// A claimed row must stay VISIBLE in the queue view, annotated with who
	// holds it — not vanish until the claim clears. The queue view used to
	// filter on `claimed_by = ''`, so this row was invisible for as long as the
	// claim stood, which is exactly the window in which an operator needs to
	// see it. The first run holds the only slot for its whole `sleep 6`, so the
	// dequeuer cannot start or delete this row underneath the assertion, and
	// the reaper leaves a claim inside its lease alone.
	claimedRows, err := s.tryFetchQueue(job.ID)
	s.Require().NoError(err)
	s.Require().Len(claimedRows, 1, "a claimed queued run must stay visible in the queue view")
	s.Equal(queued.ID, claimedRows[0].ID)
	s.Equal("claimed", claimedRows[0].ClaimState)
	s.False(claimedRows[0].Stale, "a claim inside its lease is live, not stuck")
	s.Equal(claimOwner, claimedRows[0].ClaimedBy)

	// The CLI reads the same annotation.
	stdout, stderr, cliErr := s.runCLISeparate("job", "queue", job.Alias, "--server", s.caesiumURL)
	s.Require().NoError(cliErr, "caesium job queue failed:\nstdout=%s\nstderr=%s", stdout, stderr)
	cliLines := nonEmptyLines(stdout)
	s.Require().GreaterOrEqual(len(cliLines), 2, "queue table should have a header and row:\n%s", stdout)
	s.Equal([]string{"POSITION", "PRIORITY", "STATE", "ENQUEUED_AT", "PARAMS"}, strings.Fields(cliLines[0]))
	s.Equal("claimed", strings.Fields(cliLines[1])[2])

	status, body := s.deleteQueuedRun(job.ID, queued.ID)
	s.Equal(http.StatusConflict, status, body)
	s.Contains(body, "queued run already started")

	// Backdate the claim past its lease. The row is now `stale` to the queue
	// view and, by the same cutoff, reclaimable by the leader's reaper — which
	// runs every dequeue interval and so releases it within a few hundred
	// milliseconds. That makes the `stale` READING itself unobservable without
	// racing the reaper, so it is pinned by unit tests that share the reaper's
	// cutoff (TestQueueAnnotatesClaimStateInsteadOfHidingClaimedRows and
	// TestReclaimStaleClaimsMatchesTheViewsClaimState); what this e2e pins is
	// the consequence: the row returns to `pending` and drains.
	s.setQueuedRunClaim(job.ID, queued.ID, claimOwner, time.Now().UTC().Add(-5*time.Minute))
	s.Require().Eventually(func() bool {
		rows, err := s.tryFetchQueue(job.ID)
		if err != nil || len(rows) == 0 {
			return len(rows) == 0 && err == nil // already dequeued
		}
		return rows[0].ClaimState == "pending" && !rows[0].Stale && rows[0].ClaimedBy == ""
	}, 30*time.Second, 250*time.Millisecond, "the reaper must release the expired claim it and the queue view agree is stale")

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
	}, 30*time.Second, time.Second, "claimed row must remain available for the dequeuer after cancel returns 409")
	s.Require().NotEmpty(queuedRunID)
	s.Equal("succeeded", s.awaitRunStatus(job.ID, queuedRunID, runTimeout, "succeeded").Status)
}

func (s *IntegrationTestSuite) requireQueuedRun(jobID string) queueCLIItem {
	s.T().Helper()

	var (
		rows []queueCLIItem
		err  error
	)
	s.Require().Eventually(func() bool {
		rows, err = s.tryFetchQueue(jobID)
		return err == nil && len(rows) > 0
	}, 10*time.Second, 250*time.Millisecond, "expected queued run for job %s", jobID)
	s.Require().NoError(err)
	return rows[0]
}

func (s *IntegrationTestSuite) requireQueueEmpty(jobID string) {
	s.T().Helper()

	var (
		rows []queueCLIItem
		err  error
	)
	s.Require().Eventually(func() bool {
		rows, err = s.tryFetchQueue(jobID)
		return err == nil && len(rows) == 0
	}, 10*time.Second, 250*time.Millisecond, "expected queue to be empty for job %s", jobID)
	s.Require().NoError(err)
}

func (s *IntegrationTestSuite) tryFetchQueue(jobID string) ([]queueCLIItem, error) {
	s.T().Helper()

	var rows []queueCLIItem
	err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/queue", jobID), &rows)
	return rows, err
}

func (s *IntegrationTestSuite) deleteQueuedRun(jobID, queueID string) (int, string) {
	s.T().Helper()

	resp, err := s.doRequest(http.MethodDelete, fmt.Sprintf("%s/v1/jobs/%s/queue/%s", s.caesiumURL, jobID, queueID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(body)
}

func (s *IntegrationTestSuite) setQueuedRunClaim(jobID, queueID, claimedBy string, claimedAt time.Time) {
	s.T().Helper()

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	result, err := catalogDB.ExecContext(s.T().Context(), `
UPDATE run_queue
SET claimed_by = ?, claimed_at = ?
WHERE id = ? AND job_id = ?
`, claimedBy, claimedAt, queueID, jobID)
	s.Require().NoError(err)
	affected, err := result.RowsAffected()
	s.Require().NoError(err)
	s.Equal(int64(1), affected)
}
