//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// TestHTTPTriggerInterpolatesParamRefsInStepEnv drives the real HTTP webhook
// surface: a trigger-mapped run param must reach a step env value via
// ${CAESIUM_PARAM_*}, because reagents like git-source are Go binaries with no
// shell. Asserts the container actually saw the substituted value.
func (s *IntegrationTestSuite) TestHTTPTriggerInterpolatesParamRefsInStepEnv() {
	alias := fmt.Sprintf("integration-param-env-%d", time.Now().UnixNano())
	hook := fmt.Sprintf("param-env-%d", time.Now().UnixNano())
	wantRef := "deadbeefcafebabe"
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: "/hooks/%s"
    paramMapping:
      SHA: "$.sha"
steps:
  - name: echo-ref
    image: alpine:3.23
    command: ["sh", "-c", "echo GIT_REF=$GIT_REF"]
    env:
      GIT_REF: "${CAESIUM_PARAM_SHA}"
`, alias, hook)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	req, err := http.NewRequestWithContext(
		s.T().Context(),
		http.MethodPost,
		fmt.Sprintf("%s/v1/hooks/%s", s.caesiumURL, hook),
		strings.NewReader(fmt.Sprintf(`{"sha":%q}`, wantRef)),
	)
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var runID string
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(job.ID)
		if len(runs) == 0 {
			return false
		}
		runID = runs[0].ID
		return runID != "" && runs[0].Params["SHA"] == wantRef
	}, 30*time.Second, 500*time.Millisecond, "HTTP trigger run with SHA param should start")

	finished := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", finished.Status, "run error: %s", finished.Error)

	taskID := s.jobTaskIDByName(job.ID, "echo-ref")
	logs := s.taskLog(job.ID, runID, taskID)
	s.Contains(logs, "GIT_REF="+wantRef)
	s.NotContains(logs, "${CAESIUM_PARAM_SHA}")
}

func (s *IntegrationTestSuite) TestHTTPTriggerMissingParamRefFailsClosed() {
	alias := fmt.Sprintf("integration-param-env-missing-%d", time.Now().UnixNano())
	hook := fmt.Sprintf("param-env-missing-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: "/hooks/%s"
    paramMapping:
      SHA: "$.sha"
steps:
  - name: echo-ref
    image: alpine:3.23
    command: ["sh", "-c", "echo GIT_REF=$GIT_REF"]
    env:
      GIT_REF: "${CAESIUM_PARAM_SHA}"
`, alias, hook)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	req, err := http.NewRequestWithContext(
		s.T().Context(),
		http.MethodPost,
		fmt.Sprintf("%s/v1/hooks/%s", s.caesiumURL, hook),
		strings.NewReader(`{}`),
	)
	s.Require().NoError(err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var runID string
	s.Require().Eventually(func() bool {
		runs := s.fetchRuns(job.ID)
		if len(runs) == 0 {
			return false
		}
		runID = runs[0].ID
		return runID != ""
	}, 30*time.Second, 500*time.Millisecond)

	finished := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", finished.Status)
	s.True(
		strings.Contains(finished.Error, "${CAESIUM_PARAM_SHA}") ||
			taskErrorsContain(finished, "${CAESIUM_PARAM_SHA}"),
		"failure must name the unresolved token, got run error %q tasks %#v", finished.Error, finished.Tasks,
	)
}

func taskErrorsContain(run *runResponse, needle string) bool {
	if strings.Contains(run.Error, needle) {
		return true
	}
	for _, task := range run.Tasks {
		if strings.Contains(task.Error, needle) {
			return true
		}
	}
	return false
}
