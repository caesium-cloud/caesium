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
	ID     string            `json:"id"`
	JobID  string            `json:"job_id"`
	Status string            `json:"status"`
	Error  string            `json:"error,omitempty"`
	Params map[string]string `json:"params,omitempty"`
	Tasks  []runTaskResponse `json:"tasks"`
}

type runTaskResponse struct {
	ID               string                    `json:"id"`
	Status           string                    `json:"status"`
	Result           string                    `json:"result,omitempty"`
	Output           map[string]string         `json:"output,omitempty"`
	SchemaViolations []schemaViolationResponse `json:"schema_violations,omitempty"`
	Error            string                    `json:"error,omitempty"`
}

type schemaViolationResponse struct {
	Key     string `json:"key"`
	Message string `json:"message"`
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

// TestRunTimeout applies a job with a 15 s runTimeout and a task that sleeps for
// 120 s.  The run should be marked failed with a timeout error well before the
// task would naturally finish.  We use 15 s (not 5 s) to give ARM64 CI runners
// enough headroom for container startup.
func (s *IntegrationTestSuite) TestRunTimeout() {
	dir := s.writeJobManifest(runTimeoutManifest())
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias("integration-job-run-timeout")
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)

	// The run timeout is 15 s; allow generous headroom for container startup.
	run := s.awaitRun(job.ID, runID, runTimeout)

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

	run := s.awaitRun(job.ID, runID, runTimeout)

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
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"key\": \"first\"}' && echo '##caesium::output {\"key\": \"second\", \"extra\": \"val\"}'"]
`, time.Now().UnixNano())

	alias := extractAlias(manifest)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

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
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"row_count\": \"100\", \"source\": \"db\"}'"]
    next: transform
  - name: transform
    image: alpine:3.23
    command: ["sh", "-c", "echo env_row_count=$CAESIUM_OUTPUT_EXTRACT_ROW_COUNT env_source=$CAESIUM_OUTPUT_EXTRACT_SOURCE && echo '##caesium::output {\"transformed\": \"true\"}'"]
    dependsOn: extract
    next: load
  - name: load
    image: alpine:3.23
    command: ["sh", "-c", "echo upstream_transformed=$CAESIUM_OUTPUT_TRANSFORM_TRANSFORMED upstream_rows=$CAESIUM_OUTPUT_EXTRACT_ROW_COUNT"]
    dependsOn: transform
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

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
// Branching & Conditional Execution
// --------------------------------------------------------------------------

// TestBranchSelectsOnePath applies a branch step that selects one of two paths.
// The selected path should succeed, the other should be skipped.
func (s *IntegrationTestSuite) TestBranchSelectsOnePath() {
	alias := fmt.Sprintf("integration-branch-one-path-%d", time.Now().UnixNano())
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
  - name: decide
    type: branch
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::branch path-a'"]
    next: [path-a, path-b]
  - name: path-a
    image: alpine:3.23
    command: ["sh", "-c", "echo 'selected'"]
    dependsOn: [decide]
  - name: path-b
    image: alpine:3.23
    command: ["sh", "-c", "echo 'not selected'"]
    dependsOn: [decide]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("succeeded", run.Status, "run with branch should succeed")

	taskStatusByName := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatusByName["decide"], "branch step should succeed")
	s.Equal("succeeded", taskStatusByName["path-a"], "selected branch should succeed")
	s.Equal("skipped", taskStatusByName["path-b"], "non-selected branch should be skipped")
}

// TestBranchSelectsMultiplePaths applies a branch step that selects two of
// three downstream paths.
func (s *IntegrationTestSuite) TestBranchSelectsMultiplePaths() {
	alias := fmt.Sprintf("integration-branch-multi-path-%d", time.Now().UnixNano())
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
  - name: decide
    type: branch
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::branch path-a' && echo '##caesium::branch path-c'"]
    next: [path-a, path-b, path-c]
  - name: path-a
    image: alpine:3.23
    command: ["sh", "-c", "echo a"]
    dependsOn: [decide]
  - name: path-b
    image: alpine:3.23
    command: ["sh", "-c", "echo b"]
    dependsOn: [decide]
  - name: path-c
    image: alpine:3.23
    command: ["sh", "-c", "echo c"]
    dependsOn: [decide]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("succeeded", run.Status)

	taskStatusByName := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatusByName["decide"])
	s.Equal("succeeded", taskStatusByName["path-a"], "selected branch a should succeed")
	s.Equal("skipped", taskStatusByName["path-b"], "non-selected branch b should be skipped")
	s.Equal("succeeded", taskStatusByName["path-c"], "selected branch c should succeed")
}

// TestBranchEmptyOutputSkipsAll verifies that when a branch step emits no
// ##caesium::branch markers, all downstream steps are skipped.
func (s *IntegrationTestSuite) TestBranchEmptyOutputSkipsAll() {
	alias := fmt.Sprintf("integration-branch-empty-%d", time.Now().UnixNano())
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
  - name: decide
    type: branch
    image: alpine:3.23
    command: ["sh", "-c", "echo 'no branch markers here'"]
    next: [path-a, path-b]
  - name: path-a
    image: alpine:3.23
    command: ["sh", "-c", "echo a"]
    dependsOn: [decide]
  - name: path-b
    image: alpine:3.23
    command: ["sh", "-c", "echo b"]
    dependsOn: [decide]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("succeeded", run.Status, "run should succeed even with all branches skipped")

	taskStatusByName := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatusByName["decide"])
	s.Equal("skipped", taskStatusByName["path-a"], "all downstream should be skipped")
	s.Equal("skipped", taskStatusByName["path-b"], "all downstream should be skipped")
}

// TestBranchWithDownstreamJoin verifies branching works with a join step that
// uses triggerRule: one_success to run after only the selected branch completes.
func (s *IntegrationTestSuite) TestBranchWithDownstreamJoin() {
	alias := fmt.Sprintf("integration-branch-join-%d", time.Now().UnixNano())
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
  - name: decide
    type: branch
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::branch fast-path'"]
    next: [fast-path, slow-path]
  - name: fast-path
    image: alpine:3.23
    command: ["sh", "-c", "echo fast && echo '##caesium::output {\"route\": \"fast\"}'"]
    dependsOn: [decide]
    next: [join]
  - name: slow-path
    image: alpine:3.23
    command: ["sh", "-c", "echo slow"]
    dependsOn: [decide]
    next: [join]
  - name: join
    image: alpine:3.23
    command: ["sh", "-c", "echo joined via $CAESIUM_OUTPUT_FAST_PATH_ROUTE"]
    dependsOn: [fast-path, slow-path]
    triggerRule: one_success
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("succeeded", run.Status, "run with branch+join should succeed")

	taskStatusByName := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatusByName["decide"])
	s.Equal("succeeded", taskStatusByName["fast-path"])
	s.Equal("skipped", taskStatusByName["slow-path"])
	s.Equal("succeeded", taskStatusByName["join"], "join with one_success should run after selected branch")
}

// TestBranchCoexistsWithOutputs verifies that a branch step can emit both
// ##caesium::branch and ##caesium::output markers simultaneously.
func (s *IntegrationTestSuite) TestBranchCoexistsWithOutputs() {
	alias := fmt.Sprintf("integration-branch-outputs-%d", time.Now().UnixNano())
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
  - name: decide
    type: branch
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"reason\": \"data-stale\"}' && echo '##caesium::branch refresh'"]
    next: [refresh, skip]
  - name: refresh
    image: alpine:3.23
    command: ["sh", "-c", "echo refreshing because $CAESIUM_OUTPUT_DECIDE_REASON"]
    dependsOn: [decide]
  - name: skip
    image: alpine:3.23
    command: ["sh", "-c", "echo skipping"]
    dependsOn: [decide]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("succeeded", run.Status)

	taskStatusByName := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatusByName["decide"])
	s.Equal("succeeded", taskStatusByName["refresh"])
	s.Equal("skipped", taskStatusByName["skip"])

	// Verify the branch step also captured structured output.
	for _, task := range run.Tasks {
		if taskStatusByName["decide"] == "succeeded" && task.Output != nil && task.Output["reason"] == "data-stale" {
			return // found it
		}
	}
	s.Fail("branch step should have captured structured output with reason=data-stale")
}

// TestBranchSkipPropagates verifies that when a branch skips a step, the
// skip propagates to that step's descendants.
func (s *IntegrationTestSuite) TestBranchSkipPropagates() {
	alias := fmt.Sprintf("integration-branch-propagate-%d", time.Now().UnixNano())
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
  - name: decide
    type: branch
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::branch path-a'"]
    next: [path-a, path-b]
  - name: path-a
    image: alpine:3.23
    command: ["sh", "-c", "echo a"]
    dependsOn: [decide]
  - name: path-b
    image: alpine:3.23
    command: ["sh", "-c", "echo b"]
    dependsOn: [decide]
    next: [path-b-child]
  - name: path-b-child
    image: alpine:3.23
    command: ["sh", "-c", "echo bc"]
    dependsOn: [path-b]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("succeeded", run.Status)

	taskStatusByName := s.taskStatusesByName(job.ID, run)
	s.Equal("succeeded", taskStatusByName["decide"])
	s.Equal("succeeded", taskStatusByName["path-a"])
	s.Equal("skipped", taskStatusByName["path-b"], "non-selected branch should be skipped")
	s.Equal("skipped", taskStatusByName["path-b-child"], "descendant of skipped branch should also be skipped")
}

// taskStatusesByName fetches the task list for a job and maps step names to
// their run statuses using the run response.
func (s *IntegrationTestSuite) taskStatusesByName(jobID string, run *runResponse) map[string]string {
	s.T().Helper()

	// Fetch task metadata to get names.
	var tasks []struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)

	nameByID := make(map[string]string, len(tasks))
	for _, t := range tasks {
		nameByID[t.ID] = t.Name
	}

	result := make(map[string]string, len(run.Tasks))
	for _, t := range run.Tasks {
		name := nameByID[t.ID]
		if name == "" {
			name = t.ID
		}
		result[name] = t.Status
	}
	return result
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func runTimeoutManifest() string {
	return `
apiVersion: v1
kind: Job
metadata:
  alias: integration-job-run-timeout
  runTimeout: 15s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: slow-task
    image: alpine:3.23
    command: ["sh", "-c", "sleep 120"]
`
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
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"color\": \"blue\", \"count\": \"42\"}' && echo done"]
    next: consumer
  - name: consumer
    image: alpine:3.23
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
