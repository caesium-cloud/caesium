//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	secretLogCanary = "ci-user:ci-pass"
	secretLogRef    = "secret://env/CAESIUM_IT_REGISTRY_CREDS"
)

func (s *IntegrationTestSuite) TestSecretLogsLiveAndRetainedSuccessfulAndFailedTasks() {
	alias := fmt.Sprintf("secret-logs-live-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 31 2 *"
steps:
  - name: succeeds
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        printf 'secret-ref=%%s\n' '%s'
        printf 'ordinary=AKIAJ83HFKD9SLXMZ7Q2b8Xy1pQ9rT4\n'
        printf 'secret-first=%%s\n' "$QA_SECRET"
        printf '##caesium::output {"ok":"yes"}\n'
        sleep 4
        printf 'secret-last=%%s\n' "$QA_SECRET"
    env:
      QA_SECRET: %s
  - name: fails
    image: alpine:3.23
    command: ["sh", "-c", "printf 'failed-secret=%%s\\n' \"$QA_SECRET\"; exit 9"]
    env:
      QA_SECRET: %s
`, alias, secretLogRef, secretLogRef, secretLogRef)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	_ = s.awaitFirstTaskStatus(job.ID, runID, 30*time.Second, "running")

	successID := s.jobTaskIDByName(job.ID, "succeeds")
	failureID := s.jobTaskIDByName(job.ID, "fails")
	pendingResp, err := s.doRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, job.ID, runID, failureID), nil)
	s.Require().NoError(err)
	_ = pendingResp.Body.Close()
	s.Equal(http.StatusNoContent, pendingResp.StatusCode)
	s.Equal("pending", pendingResp.Header.Get("X-Caesium-Log-State"))

	liveResp, err := s.doRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, job.ID, runID, successID), nil)
	s.Require().NoError(err)
	liveBody, err := io.ReadAll(liveResp.Body)
	_ = liveResp.Body.Close()
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, liveResp.StatusCode, string(liveBody))
	s.Equal("live", liveResp.Header.Get("X-Caesium-Log-Source"),
		"a request made while the task is active must remain one safe streaming response")
	s.requireRedactedTaskLog(string(liveBody))
	s.Contains(string(liveBody), "secret-ref="+secretLogRef)
	s.Contains(string(liveBody), "ordinary=AKIAJ83HFKD9SLXMZ7Q2b8Xy1pQ9rT4",
		"unrelated high-entropy output must not be heuristically censored")
	s.Contains(string(liveBody), `##caesium::output {"ok":"yes"}`)

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)
	outputs := s.taskOutputsByName(job.ID, run)
	s.Equal("yes", outputs["succeeds"]["ok"],
		"the raw marker stream must still reach the structured output parser")

	retainedSuccess := s.getSecretTaskLog(job.ID, runID, successID, "")
	s.Equal("persisted", retainedSuccess.source)
	s.Equal(string(liveBody), retainedSuccess.body,
		"a reconnect after cleanup must replay the same sanitized snapshot")
	s.requireRedactedTaskLog(retainedSuccess.body)

	retainedFailure := s.getSecretTaskLog(job.ID, runID, failureID, "")
	s.Equal("persisted", retainedFailure.source)
	s.Contains(retainedFailure.body, "failed-secret=[REDACTED]")
	s.NotContains(retainedFailure.body, secretLogCanary)
}

func (s *IntegrationTestSuite) TestSecretLogsPartitionSnapshotsAreRedacted() {
	alias := fmt.Sprintf("secret-logs-partitions-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 31 2 *"
steps:
  - name: discover
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"east\",\"west\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "printf 'partition=%%s secret=%%s\\n' \"$CAESIUM_PARTITION\" \"$QA_SECRET\""]
    env:
      QA_SECRET: %s
    dependsOn: [discover]
    fanOut:
      from: discover
      maxPartitions: 4
`, alias, secretLogRef)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)

	processID := s.jobTaskIDByName(job.ID, "process")
	partitions := s.expandedPartitions(s.listPartitions(job.ID, runID, processID))
	s.Require().Len(partitions, 2)
	for _, partition := range partitions {
		entry := s.getSecretTaskLog(job.ID, runID, processID,
			"&task_run_id="+partition.TaskRunID)
		s.Equal("persisted", entry.source)
		s.Contains(entry.body, "partition="+partition.Value+" secret=[REDACTED]")
		s.NotContains(entry.body, secretLogCanary)
	}
}

func (s *IntegrationTestSuite) TestAuthSecretLogsViewerCanReadOnlyRedactedOutput() {
	s.requireAuthLane()

	alias := fmt.Sprintf("auth-secret-logs-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 31 2 *"
steps:
  - name: protected
    image: alpine:3.23
    command: ["sh", "-c", "printf 'viewer-secret=%%s\\n' \"$QA_SECRET\""]
    env:
      QA_SECRET: %s
`, alias, secretLogRef)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)

	viewer := s.createAPIKeyCLI("--role", "viewer", "--description", "secret log viewer")
	taskID := s.jobTaskIDByName(job.ID, "protected")
	status, body := s.requestWithKey(http.MethodGet,
		fmt.Sprintf("/v1/jobs/%s/runs/%s/logs?task_id=%s", job.ID, runID, taskID), viewer.Plaintext, nil)
	s.Equal(http.StatusOK, status, body)
	s.Contains(body, "viewer-secret=[REDACTED]")
	s.NotContains(body, secretLogCanary)
}

type secretTaskLogResponse struct {
	body   string
	source string
}

func (s *IntegrationTestSuite) getSecretTaskLog(jobID, runID, taskID, extraQuery string) secretTaskLogResponse {
	s.T().Helper()
	resp, err := s.doRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s%s", s.caesiumURL, jobID, runID, taskID, extraQuery), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	return secretTaskLogResponse{body: string(body), source: resp.Header.Get("X-Caesium-Log-Source")}
}

func (s *IntegrationTestSuite) requireRedactedTaskLog(body string) {
	s.T().Helper()
	s.NotContains(body, secretLogCanary)
	s.GreaterOrEqual(strings.Count(body, "[REDACTED]"), 2,
		"both repeated occurrences of the resolved value must be replaced")
}
