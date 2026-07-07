//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
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
	claimOwner := "cancel-race/e2e"
	s.setQueuedRunClaim(job.ID, queued.ID, claimOwner, time.Now().UTC())

	status, body := s.deleteQueuedRun(job.ID, queued.ID)
	s.Equal(http.StatusConflict, status, body)
	s.Contains(body, "queued run already started")

	s.setQueuedRunClaim(job.ID, queued.ID, claimOwner, time.Now().UTC().Add(-5*time.Minute))
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
