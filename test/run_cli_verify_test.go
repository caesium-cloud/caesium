//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// TestRunCLIVerifyLoop is the agent verification loop for issue #510: apply
// prints each job id, start accepts an alias, and get/wait/logs read the run
// back through the CLI. Stdout is captured apart from stderr.
func (s *IntegrationTestSuite) TestRunCLIVerifyLoop() {
	alias := fmt.Sprintf("cli-verify-%d", time.Now().UnixNano())
	marker := "cli-verify-marker"
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
steps:
  - name: emit
    image: alpine:3.23
    command: ["sh", "-c", "echo %s"]
`, alias, marker)
	dir := s.writeJobManifest(manifest)
	defer func() { _ = os.RemoveAll(dir) }()

	stdout, stderr, err := s.runCLISeparate("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, "job apply failed: %s", stderr)
	s.Contains(stdout, "Applied 1 job definition(s)")
	s.NotContains(stderr, "Applied 1 job definition")
	jobID := ""
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == alias {
			jobID = fields[len(fields)-1]
		}
	}
	s.NotEmpty(jobID, "apply stdout did not name the job id:\n%s", stdout)
	s.Equal(jobID, s.requireJobByAlias(alias).ID)

	jsonOut, jsonErr, err := s.runCLISeparate("job", "apply", "--json", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, "job apply --json failed: %s", jsonErr)
	s.Require().True(json.Valid([]byte(jsonOut)), "apply --json stdout was not JSON:\n%s", jsonOut)
	s.NotContains(jsonOut, "Applied")
	s.NotContains(jsonOut, "Warning:")
	var applied struct {
		Applied int `json:"applied"`
		Jobs    []struct {
			Alias string `json:"alias"`
			ID    string `json:"id"`
		} `json:"jobs"`
		ContractWarnings []any `json:"contract_warnings"`
	}
	s.Require().NoError(json.Unmarshal([]byte(jsonOut), &applied))
	s.Equal(1, applied.Applied)
	s.Require().Len(applied.Jobs, 1)
	s.Equal(alias, applied.Jobs[0].Alias)
	s.Equal(jobID, applied.Jobs[0].ID)

	runOut, runErr, err := s.runCLISeparate("run", "start", "--alias", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "run start --alias failed: %s", runErr)
	runID := strings.TrimSpace(runOut)
	s.NotEmpty(runID)
	s.NotContains(runOut, "\n"+alias)
	s.Equal(runID+"\n", runOut, "start stdout must be only the run id")

	waitOut, waitErr, err := s.runCLISeparate(
		"run", "wait",
		"--run-id", runID,
		"--job-id", jobID,
		"--timeout", "90s",
		"--json",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "run wait failed: %s\nstderr: %s", waitOut, waitErr)
	s.Require().True(json.Valid([]byte(waitOut)), "run wait --json stdout was not JSON:\n%s", waitOut)
	s.NotContains(waitErr, waitOut)
	var waited struct {
		ID       string `json:"id"`
		JobID    string `json:"job_id"`
		JobAlias string `json:"job_alias"`
		Status   string `json:"status"`
		TimedOut bool   `json:"timed_out"`
	}
	s.Require().NoError(json.Unmarshal([]byte(waitOut), &waited))
	s.Equal(runID, waited.ID)
	s.Equal(jobID, waited.JobID)
	s.Equal(alias, waited.JobAlias)
	s.Equal("succeeded", waited.Status)
	s.False(waited.TimedOut)

	getOut, getErr, err := s.runCLISeparate("run", "get", "--run-id", runID, "--job-id", jobID, "--server", s.caesiumURL)
	s.Require().NoError(err, "run get failed: %s", getErr)
	s.Contains(getOut, "status: succeeded")
	s.Contains(getOut, alias)
	s.Contains(getOut, jobID)

	var logsOut, logsErr string
	deadline := time.Now().Add(15 * time.Second)
	for {
		logsOut, logsErr, err = s.runCLISeparate("run", "logs", "--run-id", runID, "--job-id", jobID, "--server", s.caesiumURL)
		s.Require().NoError(err, "run logs failed: %s", logsErr)
		if strings.Contains(logsOut, marker) {
			break
		}
		if time.Now().After(deadline) {
			s.Failf("run logs missing marker", "stdout:\n%s\nstderr:\n%s", logsOut, logsErr)
		}
		time.Sleep(500 * time.Millisecond)
	}

	logsJSON, logsJSONErr, err := s.runCLISeparate("run", "logs", "--run-id", runID, "--job-id", jobID, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "run logs --json failed: %s", logsJSONErr)
	s.Require().True(json.Valid([]byte(logsJSON)), "run logs --json stdout was not JSON:\n%s", logsJSON)
	s.Contains(logsJSON, marker)
	s.NotContains(logsJSON, "note:")
}
