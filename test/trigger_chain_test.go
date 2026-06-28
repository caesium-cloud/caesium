//go:build integration

package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *IntegrationTestSuite) TestTriggerChainLifecycleBridgeFiresDownstreamJob() {
	suffix := time.Now().UnixNano()
	upstream := fmt.Sprintf("integration-chain-a-%d", suffix)
	downstream := fmt.Sprintf("integration-chain-b-%d", suffix)

	dir := s.writeTriggerChainManifests(map[string]string{
		upstream:   triggerChainCronManifest(upstream),
		downstream: triggerChainEventManifest(downstream, upstream),
	})
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	upstreamJob := s.requireJobByAlias(upstream)
	downstreamJob := s.requireJobByAlias(downstream)

	beforeDownstream := len(s.fetchRuns(downstreamJob.ID))
	upstreamRunID := s.triggerRun(upstreamJob.ID)
	s.Require().Equal("succeeded", s.awaitRun(upstreamJob.ID, upstreamRunID, runTimeout).Status)

	downstreamRun := s.awaitNewRun(downstreamJob.ID, beforeDownstream, 60*time.Second)
	completedDownstream := s.awaitRun(downstreamJob.ID, downstreamRun.ID, runTimeout)
	s.Require().Equal("succeeded", completedDownstream.Status)
	s.Equal("1", completedDownstream.Params["_trigger_depth"])
}

func (s *IntegrationTestSuite) TestTriggerChainCycleRejectedBeforePersistence() {
	suffix := time.Now().UnixNano()
	a := fmt.Sprintf("integration-chain-cycle-a-%d", suffix)
	b := fmt.Sprintf("integration-chain-cycle-b-%d", suffix)

	dir := s.writeTriggerChainManifests(map[string]string{
		a: triggerChainEventManifest(a, b),
		b: triggerChainEventManifest(b, a),
	})
	defer os.RemoveAll(dir)

	stdout, stderr, err := s.runCLISeparate("job", "lint", "--path", dir)
	s.Require().Error(err)
	s.Contains(stdout+stderr, "trigger chain cycle detected")

	output, err := s.runCLIExpectError("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().Error(err)
	s.Contains(output, "trigger chain cycle detected")
	s.False(s.jobExists(a))
	s.False(s.jobExists(b))
}

func (s *IntegrationTestSuite) TestTriggerChainDepthLimitRejectsNextHop() {
	suffix := time.Now().UnixNano()
	a := fmt.Sprintf("integration-chain-depth-a-%d", suffix)
	b := fmt.Sprintf("integration-chain-depth-b-%d", suffix)

	dir := s.writeTriggerChainManifests(map[string]string{
		a: triggerChainCronManifest(a),
		b: triggerChainEventManifest(b, a),
	})
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobA := s.requireJobByAlias(a)
	jobB := s.requireJobByAlias(b)

	runA := s.triggerRunWithParams(jobA.ID, map[string]string{"_trigger_depth": "1000000"})
	s.Require().Equal("succeeded", s.awaitRun(jobA.ID, runA, runTimeout).Status)

	s.assertNoRuns(jobB.ID, 5*time.Second)
}

func (s *IntegrationTestSuite) writeTriggerChainManifests(files map[string]string) string {
	s.T().Helper()

	dir, err := os.MkdirTemp("", "caesium-trigger-chain-*")
	s.Require().NoError(err)
	for name, contents := range files {
		path := filepath.Join(dir, name+".job.yaml")
		s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(contents))), 0o644))
	}
	return dir
}

func (s *IntegrationTestSuite) awaitNewRun(jobID string, previousCount int, timeout time.Duration) runResponse {
	s.T().Helper()

	var run runResponse
	s.Require().Eventually(func() bool {
		runs, err := s.tryFetchRuns(jobID)
		if err != nil {
			return false
		}
		if len(runs) <= previousCount {
			return false
		}
		run = runs[len(runs)-1]
		return run.ID != ""
	}, timeout, 500*time.Millisecond)
	return run
}

func (s *IntegrationTestSuite) tryFetchRuns(jobID string) ([]runResponse, error) {
	s.T().Helper()

	var runs []runResponse
	if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs", jobID), &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func (s *IntegrationTestSuite) assertNoRuns(jobID string, duration time.Duration) {
	s.T().Helper()

	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		runs := s.fetchRuns(jobID)
		if len(runs) > 0 {
			s.T().Fatalf("expected no runs for job %s, got %d", jobID, len(runs))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func triggerChainCronManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", "echo %s"]
`, alias, alias)
}

func triggerChainEventManifest(alias, upstream string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: event
  configuration:
    events:
      - type: run_completed
        source: caesium
        filter:
          job_alias: %s
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", "echo %s"]
`, alias, upstream, alias)
}
