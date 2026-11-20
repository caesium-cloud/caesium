//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
)

func (s *IntegrationTestSuite) TestReproduceDescriptorRetention() {
	jobID, runID, transformRow := s.createReproduceCLIFixture()
	reproduce := func(task string) (string, string, error) {
		return s.runCLISeparate("reproduce", runID, "--job-id", jobID, "--task", task,
			"--dry-run", "--json", "--server", s.caesiumURL)
	}
	requireFailure := func(task, message string) {
		s.T().Helper()
		stdout, stderr, err := reproduce(task)
		s.Require().Error(err)
		s.Equal(2, reproduceExitCode(err), stderr)
		s.Empty(stdout)
		s.Contains(stderr, message)
	}
	prune := func(task string, want int) {
		s.T().Helper()
		path := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/descriptor", s.caesiumURL, jobID, runID, task)
		resp, err := s.doRequest(http.MethodDelete, path, nil)
		s.Require().NoError(err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		s.Require().NoError(err)
		s.Require().Equal(want, resp.StatusCode, string(body))
	}
	stdout, stderr, err := reproduce("produce")
	s.Require().NoError(err, "stderr: %s", stderr)
	var envelope reproduceCLIEnvelope
	s.Require().NoError(json.Unmarshal([]byte(stdout), &envelope))
	s.Equal("produce", envelope.Task)
	requireFailure(uuid.NewString(), "fetch descriptor failed (404)")
	var prunedRow string
	if s.authAPIKey != "" {
		status, body := s.getReproduceDescriptor(jobID, runID, "produce")
		s.Require().Equal(http.StatusOK, status)
		var stored reproduceDescriptorResponse
		s.Require().NoError(json.Unmarshal(body, &stored))
		prunedRow = stored.TaskRunID
		var desc reproduceDescriptorPayload
		s.Require().NoError(json.Unmarshal(stored.Descriptor, &desc))
		s.Require().NoError(uuid.Validate(desc.Baseline.TaskID))
		prune("produce", http.StatusNoContent)
		prune(desc.Baseline.TaskID, http.StatusNoContent)
		status, body = s.getReproduceDescriptor(jobID, runID, "produce")
		s.Require().Equal(http.StatusNotFound, status)
		s.Contains(string(body), "descriptor pruned")
		requireFailure("produce", "descriptor pruned")
		requireFailure(desc.Baseline.TaskID, "descriptor pruned")
	} else {
		prune("produce", http.StatusUnauthorized)
		status, body := s.getReproduceDescriptor(jobID, runID, "produce")
		s.Require().Equal(http.StatusOK, status, string(body))
	}

	// A server creating new runs always records descriptors. Recreate the legacy
	// NULL on a different, real task row and read it through the same live CLI.
	if s.engineType != "kubernetes" {
		catalog := s.openIntegrationCatalogDB()
		defer func() { s.Require().NoError(catalog.Close()) }()
		if prunedRow != "" {
			var remaining int
			err := catalog.QueryRowContext(s.T().Context(), "SELECT COUNT(*) FROM task_runs WHERE id = ?", prunedRow).Scan(&remaining)
			s.Require().NoError(err)
			s.Zero(remaining, "prune must remove the original task run")
		}
		s.Require().NoError(clearTaskExecutionDescriptor(s.T().Context(), catalog, transformRow))
		if s.authAPIKey != "" {
			prune("transform", http.StatusNotFound)
		}
		requireFailure("transform", "descriptor unavailable")
	}
}
