//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// runResponse matches the JSON returned by POST /v1/jobs/:id/run and
// GET /v1/jobs/:id/runs/:run_id.
type runResponse struct {
	ID     string `json:"id"`
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Tasks  []struct {
		ID     string            `json:"id"`
		Status string            `json:"status"`
		Result string            `json:"result,omitempty"`
		Output map[string]string `json:"output,omitempty"`
		Error  string            `json:"error,omitempty"`
	} `json:"tasks"`
}

// awaitRun polls GET /v1/jobs/:jobID/runs/:runID until the run reaches a
// terminal status ("succeeded" or "failed") or the timeout elapses.
func (s *IntegrationTestSuite) awaitRun(jobID, runID string, timeout time.Duration) *runResponse {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for run %s to complete", runID)
		}

		var run runResponse
		s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &run)
		if run.Status == "succeeded" || run.Status == "failed" {
			return &run
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// triggerRun starts a run via POST /v1/jobs/:id/run and returns the run ID.
func (s *IntegrationTestSuite) triggerRun(jobID string) string {
	s.T().Helper()

	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%v/v1/jobs/%s/run", s.caesiumURL, jobID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var run runResponse
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&run))
	s.Require().NotEmpty(run.ID)
	return run.ID
}

// --------------------------------------------------------------------------
// Run Timeout
// --------------------------------------------------------------------------

// TestRunTimeout applies a job with a 5 s runTimeout and a task that sleeps for
// 120 s.  The run should be marked failed with a timeout error well before the
// task would naturally finish.
func (s *IntegrationTestSuite) TestRunTimeout() {
	dir := s.writeJobManifest(runTimeoutManifest())
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias("integration-job-run-timeout")
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)

	// The run timeout is 5 s; allow generous headroom for container startup.
	run := s.awaitRun(job.ID, runID, 60*time.Second)

	s.Equal("failed", run.Status, "run should fail due to timeout")
	s.Contains(run.Error, "timed out", "run error should mention timeout")

	// All tasks should be in a terminal state (failed or skipped).
	for _, task := range run.Tasks {
		s.NotEqual("running", task.Status, "no task should still be running after timeout")
		s.NotEqual("pending", task.Status, "no task should still be pending after timeout")
	}
}

// --------------------------------------------------------------------------
// Structured Task Output Passing
// --------------------------------------------------------------------------

// TestTaskOutputPassing applies a two-step DAG where the first step emits
// structured output via the ##caesium::output marker and the second step
// receives it as CAESIUM_OUTPUT_<STEP>_<KEY> env vars.
func (s *IntegrationTestSuite) TestTaskOutputPassing() {
	dir := s.writeJobManifest(taskOutputManifest())
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias("integration-job-output")
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)

	run := s.awaitRun(job.ID, runID, 60*time.Second)

	s.Equal("succeeded", run.Status, "run should succeed")

	// Locate the producer task and verify output was captured.
	var producerFound bool
	for _, task := range run.Tasks {
		if task.Output != nil && task.Output["color"] == "blue" {
			producerFound = true
			s.Equal("42", task.Output["count"], "producer output should contain count=42")
		}
	}
	s.True(producerFound, "producer task should have captured structured output")
}

// TestTaskOutputMultipleMarkers verifies that when a task emits multiple
// ##caesium::output lines the last-write-wins semantics are applied.
func (s *IntegrationTestSuite) TestTaskOutputMultipleMarkers() {
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: integration-job-multi-output-%d
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: multi-emit
    image: alpine
    command: ["sh", "-c", "echo '##caesium::output {\"key\": \"first\"}' && echo '##caesium::output {\"key\": \"second\", \"extra\": \"val\"}'"]
`, time.Now().UnixNano())

	alias := extractAlias(manifest)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, 60*time.Second)

	s.Equal("succeeded", run.Status)

	s.Require().Len(run.Tasks, 1)
	task := run.Tasks[0]
	s.Equal("second", task.Output["key"], "last-write-wins: key should be 'second'")
	s.Equal("val", task.Output["extra"], "second marker should add 'extra' key")
}

// TestTaskOutputDAGInjection verifies the full DAG flow: producer emits output,
// consumer receives it via env vars, and the consumer itself can emit output
// confirming the injection.
func (s *IntegrationTestSuite) TestTaskOutputDAGInjection() {
	alias := fmt.Sprintf("integration-job-dag-inject-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: extract
    image: alpine
    command: ["sh", "-c", "echo '##caesium::output {\"row_count\": \"100\", \"source\": \"db\"}'"]
    next: transform
  - name: transform
    image: alpine
    command: ["sh", "-c", "echo env_row_count=$CAESIUM_OUTPUT_EXTRACT_ROW_COUNT env_source=$CAESIUM_OUTPUT_EXTRACT_SOURCE && echo '##caesium::output {\"transformed\": \"true\"}'"]
    dependsOn: extract
    next: load
  - name: load
    image: alpine
    command: ["sh", "-c", "echo upstream_transformed=$CAESIUM_OUTPUT_TRANSFORM_TRANSFORMED upstream_rows=$CAESIUM_OUTPUT_EXTRACT_ROW_COUNT"]
    dependsOn: transform
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, 60*time.Second)

	s.Equal("succeeded", run.Status, "three-step DAG should succeed")

	// The extract step should have row_count and source.
	var extractTask, transformTask *struct {
		ID     string
		Output map[string]string
	}
	for i := range run.Tasks {
		t := &run.Tasks[i]
		if t.Output != nil {
			if t.Output["row_count"] == "100" && t.Output["source"] == "db" {
				extractTask = &struct {
					ID     string
					Output map[string]string
				}{t.ID, t.Output}
			}
			if t.Output["transformed"] == "true" {
				transformTask = &struct {
					ID     string
					Output map[string]string
				}{t.ID, t.Output}
			}
		}
	}

	s.Require().NotNil(extractTask, "extract task should have output {row_count:100, source:db}")
	s.Require().NotNil(transformTask, "transform task should have output {transformed:true}")
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func runTimeoutManifest() string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: integration-job-run-timeout
  runTimeout: 5s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: slow-task
    image: alpine
    command: ["sh", "-c", "sleep 120"]
`)
}

func taskOutputManifest() string {
	return `
apiVersion: v1
kind: Job
metadata:
  alias: integration-job-output
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: producer
    image: alpine
    command: ["sh", "-c", "echo '##caesium::output {\"color\": \"blue\", \"count\": \"42\"}' && echo done"]
    next: consumer
  - name: consumer
    image: alpine
    command: ["sh", "-c", "echo received_color=$CAESIUM_OUTPUT_PRODUCER_COLOR received_count=$CAESIUM_OUTPUT_PRODUCER_COUNT"]
    dependsOn: producer
`
}

func extractAlias(manifest string) string {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "alias:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "alias:"))
		}
	}
	return ""
}
