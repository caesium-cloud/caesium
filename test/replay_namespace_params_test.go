//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const startViewParam = "_consumed_watermarks_start"

// replayDescriptorParams reads a task's execution descriptor through the
// descriptor endpoint and returns its frozen run params and baseline run.
func (s *IntegrationTestSuite) replayDescriptorParams(jobID, runID, task string) (map[string]string, string) {
	s.T().Helper()
	var response struct {
		Descriptor struct {
			Baseline struct {
				BaselineRunID string `json:"baselineRunId"`
			} `json:"baseline"`
			Run struct {
				Params map[string]string `json:"params"`
			} `json:"run"`
		} `json:"descriptor"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/descriptor", jobID, runID, task), &response)
	return response.Descriptor.Run.Params, response.Descriptor.Baseline.BaselineRunID
}

// TestReplayIntoMovedNamespaceDropsOldFreshnessViews (identity-and-access
// W7-ε; distributed lane, where a namespace change's forced re-execution can
// be dispatched): a run of a job that consumes and produces a dataset freezes
// its start view (the consumed watermarks read in its namespace) into its
// params and every task descriptor. After the job moves from marketing to
// finance, a replay runs in finance: neither the replay run's params nor the
// descriptors its TaskRuns store (served by the descriptor endpoint to the new
// namespace's readers) carry marketing's view, and the descriptors keep their
// baseline provenance.
func (s *IntegrationTestSuite) TestReplayIntoMovedNamespaceDropsOldFreshnessViews() {
	if strings.TrimSpace(os.Getenv("CAESIUM_EXECUTION_MODE")) != "distributed" {
		s.T().Skip("a re-executing replay needs the distributed lane (just integration-test-distributed)")
	}
	stamp := time.Now().UnixNano()
	alias := fmt.Sprintf("w7e-replay-view-%d", stamp)
	source := fmt.Sprintf("w7e-src-%d", stamp)
	output := fmt.Sprintf("w7e-out-%d", stamp)
	manifest := func(namespace string) string {
		return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: %s
  replaySafe: true
  datasets:
    sources:
      - name: %s
        external: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: produce
    image: alpine:3.23
    command: ["sh", "-c", "echo produced"]
    datasets:
      consumes:
        - %s
      produces:
        - name: %s
`, alias, namespace, source, source, output)
	}
	dir := s.writeJobManifest(manifest("marketing"))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	baseline := s.awaitRun(job.ID, s.triggerRun(job.ID), 120*time.Second)
	s.Require().Equal("succeeded", baseline.Status)
	s.Require().Equal("marketing", baseline.Namespace)
	s.Require().Contains(s.fetchRun(job.ID, baseline.ID).Params, startViewParam, "precondition: the run froze its start view")
	baselineDescriptor, _ := s.replayDescriptorParams(job.ID, baseline.ID, "produce")
	s.Require().Contains(baselineDescriptor, startViewParam, "precondition: the descriptor froze it too")

	movedDir := s.writeJobManifest(manifest("finance"))
	defer os.RemoveAll(movedDir)
	s.runCLI("job", "apply", "--path", movedDir, "--server", s.caesiumURL)

	key := alias + "-moved"
	replayed := s.mustPostReplay(job.ID, baseline.ID, &key, `{"set":{}}`)
	run := s.awaitRun(job.ID, replayed.RunID, 120*time.Second)
	s.Require().Equal("succeeded", run.Status)
	s.Equal("finance", run.Namespace)

	params := s.fetchRun(job.ID, replayed.RunID).Params
	s.NotContains(params, startViewParam, "marketing's view is not persisted on finance's run")
	descriptor, baselineRun := s.replayDescriptorParams(job.ID, replayed.RunID, "produce")
	s.NotContains(descriptor, startViewParam, "nor served with its descriptor")
	for key := range descriptor {
		s.False(strings.HasPrefix(key, "_consumed_watermarks"), key)
	}
	s.Equal(baseline.ID, baselineRun, "the descriptor keeps its baseline provenance")
}
