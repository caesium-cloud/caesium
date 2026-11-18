//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
)

// TestOperatorCancelStopsOneRunAndLeavesAnother drives cancellation through
// the operator surface: `caesium run cancel --json` against the live server.
// The target run reaches cancelled, its finished step keeps its outcome, and
// a concurrent run of a different job keeps running.
func (s *IntegrationTestSuite) TestOperatorCancelStopsOneRunAndLeavesAnother() {
	marker := fmt.Sprintf("op-cancel-%d", time.Now().UnixNano())
	targetAlias := "op-cancel-target-" + marker
	otherAlias := "op-cancel-other-" + marker

	var cli *client.Client
	if s.engineType == "" || s.engineType == "docker" {
		cli = s.dockerClient()
		defer func() { _ = cli.Close() }()
		defer s.removeContainersWithMarker(cli, marker)
	}

	targetDir := s.writeJobManifest(operatorCancelManifest(targetAlias, marker))
	defer os.RemoveAll(targetDir)
	otherDir := s.writeJobManifest(operatorCancelOtherManifest(otherAlias, marker))
	defer os.RemoveAll(otherDir)
	s.runCLI("job", "apply", "--path", targetDir, "--server", s.caesiumURL)
	s.runCLI("job", "apply", "--path", otherDir, "--server", s.caesiumURL)

	target := s.requireJobByAlias(targetAlias)
	other := s.requireJobByAlias(otherAlias)
	targetRunID := s.triggerRun(target.ID)
	otherRunID := s.triggerRun(other.ID)

	s.awaitOperatorCancelReady(target.ID, targetRunID, other.ID, otherRunID)

	var holdContainers []string
	if cli != nil {
		s.Require().Eventually(func() bool {
			holdContainers = s.runningContainerIDsWithMarker(cli, "hold-"+marker)
			return len(holdContainers) > 0
		}, 30*time.Second, time.Second, "the hold step never started a container carrying %q", marker)
	}

	args := []string{
		"run", "cancel",
		"--job-id", target.ID,
		"--run-id", targetRunID,
		"--server", s.caesiumURL,
		"--json",
	}
	if s.authAPIKey != "" {
		args = append(args, "--api-key", s.authAPIKey)
	}
	stdout, stderr, err := s.runCLISeparate(args...)
	s.Require().NoError(err, "run cancel --json failed: %s", strings.TrimSpace(stderr))
	s.Require().True(json.Valid([]byte(stdout)), "stdout was not JSON:\n%s", stdout)
	s.NotContains(stdout, "Cancellation is not a task failure")
	s.NotContains(stdout, "warning:")
	s.Contains(stderr, "Cancellation is not a task failure")

	var cancelled struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	s.Require().NoError(json.Unmarshal([]byte(stdout), &cancelled))
	s.Equal(targetRunID, cancelled.ID)
	s.Equal("cancelled", cancelled.Status)
	s.Equal("cancelled by operator", cancelled.Error)

	got := s.fetchRun(target.ID, targetRunID)
	s.Equal("cancelled", got.Status)
	statuses, outputs := s.taskStatusAndOutputByName(target.ID, &got)
	s.Equal("succeeded", statuses["step-ok"], "the finished step keeps its outcome")
	s.Equal(marker, outputs["step-ok"]["kept"])
	s.Equal("cancelled", statuses["step-hold"])

	still := s.fetchRun(other.ID, otherRunID)
	s.Equal("running", still.Status, "cancelling one run must not cancel an unrelated run")
	otherStatuses, _ := s.taskStatusAndOutputByName(other.ID, &still)
	s.Equal("running", otherStatuses["unrelated-hold"])

	if cli != nil {
		s.Require().Eventually(func() bool {
			for _, id := range holdContainers {
				if s.containerIsRunning(cli, id) {
					return false
				}
			}
			return true
		}, cancelledContainerDeadline, 2*time.Second,
			"the cancelled run's container(s) %v are still running", holdContainers)
	}

	s.assertCancelStatus(target.ID, targetRunID, http.StatusConflict, "run_not_running")
	s.assertCancelStatus(target.ID, otherRunID, http.StatusNotFound, "")
	afterWrongJob := s.fetchRun(other.ID, otherRunID)
	s.Equal("running", afterWrongJob.Status)

	okID := s.jobTaskIDByName(target.ID, "step-ok")
	logResp, logErr := s.doRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, target.ID, targetRunID, okID), nil)
	s.Require().NoError(logErr)
	defer logResp.Body.Close()
	logBody, readErr := io.ReadAll(io.LimitReader(logResp.Body, 1<<20))
	s.Require().NoError(readErr)
	s.Equal(http.StatusOK, logResp.StatusCode, "finished-step logs should still be readable: %s", logBody)
	s.Contains(string(logBody), "kept-"+marker)

	// Free the unrelated run's slot. Its cancel is the same operator path.
	s.assertCancelStatus(other.ID, otherRunID, http.StatusOK, "")
}

func (s *IntegrationTestSuite) awaitOperatorCancelReady(targetJob, targetRun, otherJob, otherRun string) {
	s.T().Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		target := s.fetchRun(targetJob, targetRun)
		other := s.fetchRun(otherJob, otherRun)
		targetStatus, _ := s.taskStatusAndOutputByName(targetJob, &target)
		otherStatus, _ := s.taskStatusAndOutputByName(otherJob, &other)
		last = fmt.Sprintf("target=%s step-ok=%s step-hold=%s other=%s unrelated=%s",
			target.Status, targetStatus["step-ok"], targetStatus["step-hold"], other.Status, otherStatus["unrelated-hold"])
		if target.Status == "succeeded" || target.Status == "failed" || target.Status == "cancelled" {
			s.FailNow("target run finished before it could be cancelled: " + last)
		}
		if targetStatus["step-ok"] == "succeeded" && targetStatus["step-hold"] == "running" && otherStatus["unrelated-hold"] == "running" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.FailNow("timed out waiting for the hold step and the unrelated run to be running: " + last)
}

func (s *IntegrationTestSuite) taskStatusAndOutputByName(jobID string, run *runResponse) (map[string]string, map[string]map[string]string) {
	s.T().Helper()
	var raw []map[string]any
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &raw)
	names := make(map[string]string, len(raw))
	for _, catalog := range raw {
		id, _ := catalog["id"].(string)
		name, _ := catalog["name"].(string)
		if id != "" && name != "" {
			names[id] = name
		}
	}
	status := make(map[string]string, len(run.Tasks))
	output := make(map[string]map[string]string, len(run.Tasks))
	for _, task := range run.Tasks {
		name := names[task.TaskID]
		if name == "" {
			name = names[task.ID]
		}
		if name == "" {
			name = task.TaskID
		}
		status[name] = task.Status
		output[name] = task.Output
	}
	return status, output
}

func (s *IntegrationTestSuite) assertCancelStatus(jobID, runID string, want int, bodySnippet string) {
	s.T().Helper()
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/cancel", s.caesiumURL, jobID, runID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	s.Require().NoError(readErr)
	s.Equal(want, resp.StatusCode, "cancel %s/%s body: %s", jobID, runID, body)
	if bodySnippet != "" {
		s.Contains(string(body), bodySnippet)
	}
}

func operatorCancelManifest(alias, marker string) string {
	return fmt.Sprintf(`
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
    command: ["sh", "-c", "echo kept-%s && echo '##caesium::output {\"kept\":\"%s\"}'"]
  - name: step-hold
    image: alpine:3.23
    command: ["sh", "-c", "sleep 120 # hold-%s"]
    dependsOn: [step-ok]
`, alias, marker, marker, marker)
}

func operatorCancelOtherManifest(alias, marker string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: unrelated-hold
    image: alpine:3.23
    command: ["sh", "-c", "sleep 120 # other-%s"]
`, alias, marker)
}
