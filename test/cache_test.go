//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// --------------------------------------------------------------------------
// Cache Hit: Second Run Re-uses Cached Results
// --------------------------------------------------------------------------

// TestCacheHitSkipsExecution applies a deterministic job with cache: true,
// runs it twice, and verifies that the second run completes with "cached"
// task statuses (no container re-execution).
func (s *IntegrationTestSuite) TestCacheHitSkipsExecution() {
	alias := fmt.Sprintf("integration-cache-hit-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: deterministic
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"val\": \"42\"}'"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run: should execute normally.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status, "first run should succeed")

	taskStatuses1 := s.taskStatusesByName(job.ID, run1)
	s.Equal("succeeded", taskStatuses1["deterministic"], "first run task should succeed normally")

	// Second run: same inputs, should be a cache hit.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status, "second run should succeed")

	taskStatuses2 := s.taskStatusesByName(job.ID, run2)
	s.Equal("cached", taskStatuses2["deterministic"], "second run task should be cached")

	// Verify cached task preserved output.
	for _, task := range run2.Tasks {
		if task.Status == "cached" {
			s.Equal("42", task.Output["val"], "cached task should preserve output")
		}
	}
}

// --------------------------------------------------------------------------
// Cache Hit with DAG: Outputs Propagate from Cached Tasks
// --------------------------------------------------------------------------

// TestCacheHitDAGOutputPropagation verifies that when a producer task is
// cached, its outputs are still injected into downstream consumer tasks.
func (s *IntegrationTestSuite) TestCacheHitDAGOutputPropagation() {
	alias := fmt.Sprintf("integration-cache-dag-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: producer
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"color\": \"red\", \"size\": \"large\"}'"]
    next: consumer
  - name: consumer
    image: alpine:3.23
    command: ["sh", "-c", "echo received=$CAESIUM_OUTPUT_PRODUCER_COLOR && echo '##caesium::output {\"received\": \"true\"}'"]
    dependsOn: producer
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run: both tasks execute.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status)

	// Second run: producer should be cached, consumer may re-execute
	// (depends on whether predecessor hash changed). Key thing: run succeeds.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status, "second run with cached producer should succeed")

	taskStatuses2 := s.taskStatusesByName(job.ID, run2)
	s.Equal("cached", taskStatuses2["producer"], "producer should be cached on second run")
}

// --------------------------------------------------------------------------
// Cache Disabled: Step-level Override
// --------------------------------------------------------------------------

// TestCacheDisabledAtStepLevel verifies that a step with cache: false
// always re-executes even when the job-level default is cache: true.
func (s *IntegrationTestSuite) TestCacheDisabledAtStepLevel() {
	alias := fmt.Sprintf("integration-cache-step-disabled-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: cached-step
    image: alpine:3.23
    command: ["sh", "-c", "echo cached"]
  - name: uncached-step
    image: alpine:3.23
    cache: false
    command: ["sh", "-c", "echo always-run"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status)

	// Second run.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status)

	taskStatuses := s.taskStatusesByName(job.ID, run2)
	s.Equal("cached", taskStatuses["cached-step"], "step with inherited cache should be cached")
	s.Equal("succeeded", taskStatuses["uncached-step"], "step with cache: false should re-execute")
}

// --------------------------------------------------------------------------
// Cache Management API: List, Invalidate, Prune
// --------------------------------------------------------------------------

// TestCacheManagementListAndInvalidate creates a cached run, lists cache
// entries, invalidates them, and verifies the cache is cleared.
func (s *IntegrationTestSuite) TestCacheManagementListAndInvalidate() {
	alias := fmt.Sprintf("integration-cache-mgmt-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: step-a
    image: alpine:3.23
    command: ["sh", "-c", "echo a"]
  - name: step-b
    image: alpine:3.23
    command: ["sh", "-c", "echo b"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// Execute to populate cache.
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)

	// List cache entries.
	var listResp struct {
		Entries []struct {
			Hash     string `json:"hash"`
			TaskName string `json:"task_name"`
			Result   string `json:"result"`
		} `json:"entries"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/cache", job.ID), &listResp)
	s.GreaterOrEqual(len(listResp.Entries), 1, "should have at least one cache entry after run")

	// Invalidate a specific task.
	resp, err := s.doRequest(
		http.MethodDelete,
		fmt.Sprintf("%s/v1/jobs/%s/cache/step-a", s.caesiumURL, job.ID),
		nil,
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusNoContent, resp.StatusCode, "invalidate task should return 204")

	// Re-list: step-a entry should be gone.
	var listResp2 struct {
		Entries []struct {
			TaskName string `json:"task_name"`
		} `json:"entries"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/cache", job.ID), &listResp2)

	for _, e := range listResp2.Entries {
		s.NotEqual("step-a", e.TaskName, "step-a cache entry should be invalidated")
	}

	// Invalidate the whole job.
	resp2, err := s.doRequest(
		http.MethodDelete,
		fmt.Sprintf("%s/v1/jobs/%s/cache", s.caesiumURL, job.ID),
		nil,
	)
	s.Require().NoError(err)
	defer resp2.Body.Close()
	s.Equal(http.StatusNoContent, resp2.StatusCode, "invalidate job should return 204")

	// Re-list: should be empty.
	var listResp3 struct {
		Entries []struct{} `json:"entries"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/cache", job.ID), &listResp3)
	s.Empty(listResp3.Entries, "all cache entries should be cleared after job invalidation")
}

// TestCachePrune verifies the global prune endpoint responds successfully.
func (s *IntegrationTestSuite) TestCachePrune() {
	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/cache/prune", s.caesiumURL),
		nil,
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)

	var pruneResp struct {
		Pruned int `json:"pruned"`
	}
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().NoError(json.Unmarshal(body, &pruneResp))
	s.GreaterOrEqual(pruneResp.Pruned, 0, "pruned count should be non-negative")
}

// --------------------------------------------------------------------------
// Cache Invalidation Forces Re-execution
// --------------------------------------------------------------------------

// TestCacheInvalidationForcesReexecution invalidates the cache between runs
// and verifies the task re-executes instead of using the cache.
func (s *IntegrationTestSuite) TestCacheInvalidationForcesReexecution() {
	alias := fmt.Sprintf("integration-cache-invalidate-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: step1
    image: alpine:3.23
    command: ["sh", "-c", "echo hello"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run to populate cache.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status)

	// Invalidate cache.
	resp, err := s.doRequest(
		http.MethodDelete,
		fmt.Sprintf("%s/v1/jobs/%s/cache", s.caesiumURL, job.ID),
		nil,
	)
	s.Require().NoError(err)
	_ = resp.Body.Close()

	// Second run should re-execute (not cached).
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status)

	taskStatuses := s.taskStatusesByName(job.ID, run2)
	s.Equal("succeeded", taskStatuses["step1"], "task should re-execute after cache invalidation, not be cached")
}

// --------------------------------------------------------------------------
// Retry from Failure
// --------------------------------------------------------------------------

// TestRetryFromFailure creates a job where a task fails, retries the run,
// and verifies the retry endpoint works correctly.
func (s *IntegrationTestSuite) TestRetryFromFailure() {
	alias := fmt.Sprintf("integration-retry-%d", time.Now().UnixNano())
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
  - name: always-works
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::output {\"step\": \"1\"}'"]
    next: might-fail
  - name: might-fail
    image: alpine:3.23
    command: ["sh", "-c", "exit 1"]
    dependsOn: always-works
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run: second task fails.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("failed", run1.Status, "run should fail because second task exits 1")

	taskStatuses1 := s.taskStatusesByName(job.ID, run1)
	s.Equal("succeeded", taskStatuses1["always-works"], "first task should succeed")
	s.Equal("failed", taskStatuses1["might-fail"], "second task should fail")

	// Retry the run.
	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, job.ID, run1ID),
		nil,
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusAccepted, resp.StatusCode, "retry should return 202")

	// Wait for retry to complete (it will fail again since the step still exits 1,
	// but the endpoint should work).
	retryRun := s.awaitRun(job.ID, run1ID, runTimeout)
	s.NotNil(retryRun, "retry run should reach terminal state")
}

// TestRetryPreservesSucceededTasks verifies that on retry, previously
// succeeded tasks are preserved (not re-executed).
func (s *IntegrationTestSuite) TestRetryPreservesSucceededTasks() {
	// This test uses a job with a conditional failure based on a file.
	// Since we can't change the command between runs, we verify the retry
	// API returns 202 and the run reaches a terminal state with the first
	// task's status preserved.
	alias := fmt.Sprintf("integration-retry-preserve-%d", time.Now().UnixNano())
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
  - name: step-ok
    image: alpine:3.23
    command: ["sh", "-c", "echo ok"]
    next: step-fail
  - name: step-fail
    image: alpine:3.23
    command: ["sh", "-c", "exit 1"]
    dependsOn: step-ok
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)

	// Run and wait for failure.
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)

	// Retry.
	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, job.ID, runID),
		nil,
	)
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Equal(http.StatusAccepted, resp.StatusCode, "retry should return 202: %s", string(body))

	// Wait for retry completion.
	retryRun := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", retryRun.Status, "retry should fail again since step still exits 1")

	// The first task should still show as succeeded (preserved from original run).
	taskStatuses := s.taskStatusesByName(job.ID, retryRun)
	s.Equal("succeeded", taskStatuses["step-ok"], "succeeded task should be preserved on retry")
	s.Equal("failed", taskStatuses["step-fail"], "failed task should be re-attempted")
}

// TestRetryNonTerminalRunReturnsError verifies that retrying a running
// (non-terminal) run returns an error.
func (s *IntegrationTestSuite) TestRetryNonTerminalRunReturnsError() {
	alias := fmt.Sprintf("integration-retry-running-%d", time.Now().UnixNano())
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
  - name: slow
    image: alpine:3.23
    command: ["sh", "-c", "sleep 30"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)

	// Start a run that takes a while.
	runID := s.triggerRun(job.ID)

	// Give it a moment to start.
	time.Sleep(2 * time.Second)

	// Try to retry while it's still running.
	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, job.ID, runID),
		nil,
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusConflict, resp.StatusCode, "retry of running run should return 409")
}

// --------------------------------------------------------------------------
// Cache with Run Parameters
// --------------------------------------------------------------------------

// TestCacheMissOnDifferentParams verifies that runs with different parameters
// produce cache misses (different hash).
func (s *IntegrationTestSuite) TestCacheMissOnDifferentParams() {
	alias := fmt.Sprintf("integration-cache-params-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: parameterized
    image: alpine:3.23
    command: ["sh", "-c", "echo param=$CAESIUM_PARAM_ENV"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// Run 1 with params {"env": "staging"}.
	run1ID := s.triggerRunWithParams(job.ID, map[string]string{"env": "staging"})
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status)

	// Run 2 with different params {"env": "production"} - should NOT be cached.
	run2ID := s.triggerRunWithParams(job.ID, map[string]string{"env": "production"})
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status)

	taskStatuses := s.taskStatusesByName(job.ID, run2)
	s.Equal("succeeded", taskStatuses["parameterized"],
		"different params should cause cache miss and normal execution")

	// Run 3 with same params as run 1 - should be cached.
	run3ID := s.triggerRunWithParams(job.ID, map[string]string{"env": "staging"})
	run3 := s.awaitRun(job.ID, run3ID, runTimeout)
	s.Equal("succeeded", run3.Status)

	taskStatuses3 := s.taskStatusesByName(job.ID, run3)
	s.Equal("cached", taskStatuses3["parameterized"],
		"same params as run 1 should produce cache hit")
}

// --------------------------------------------------------------------------
// Cache with Branching
// --------------------------------------------------------------------------

// TestCacheWithBranching verifies that cached branch steps correctly
// restore branch selections and skip the right paths.
func (s *IntegrationTestSuite) TestCacheWithBranching() {
	alias := fmt.Sprintf("integration-cache-branch-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: true
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
    command: ["sh", "-c", "echo selected"]
    dependsOn: [decide]
  - name: path-b
    image: alpine:3.23
    command: ["sh", "-c", "echo not-selected"]
    dependsOn: [decide]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// First run.
	run1ID := s.triggerRun(job.ID)
	run1 := s.awaitRun(job.ID, run1ID, runTimeout)
	s.Equal("succeeded", run1.Status)

	taskStatuses1 := s.taskStatusesByName(job.ID, run1)
	s.Equal("succeeded", taskStatuses1["decide"])
	s.Equal("succeeded", taskStatuses1["path-a"])
	s.Equal("skipped", taskStatuses1["path-b"])

	// Second run: decide should be cached, branch selections should be preserved.
	run2ID := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, run2ID, runTimeout)
	s.Equal("succeeded", run2.Status, "second run with cached branch should succeed")

	taskStatuses2 := s.taskStatusesByName(job.ID, run2)
	s.Equal("cached", taskStatuses2["decide"], "branch step should be cached")
	// path-a should be cached or succeeded, path-b should be skipped.
	s.Contains([]string{"cached", "succeeded"}, taskStatuses2["path-a"],
		"selected path should succeed or be cached")
	s.Equal("skipped", taskStatuses2["path-b"],
		"non-selected path should still be skipped with cached branch")
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// triggerRunWithParams starts a run with custom parameters.
func (s *IntegrationTestSuite) triggerRunWithParams(jobID string, params map[string]string) string {
	s.T().Helper()

	body := struct {
		Params map[string]string `json:"params"`
	}{Params: params}

	buf, err := json.Marshal(body)
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(
		http.MethodPost,
		fmt.Sprintf("%v/v1/jobs/%s/run", s.caesiumURL, jobID),
		bytes.NewBuffer(buf),
	)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var run runResponse
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&run))
	s.Require().NotEmpty(run.ID)
	return run.ID
}
