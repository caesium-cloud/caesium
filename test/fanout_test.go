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
)

type partitionListResponse struct {
	Partitions []partitionInstance `json:"partitions"`
}

type partitionInstance struct {
	Value       string   `json:"value"`
	Index       int      `json:"index"`
	Status      string   `json:"status"`
	Attempt     int      `json:"attempt"`
	CacheHit    bool     `json:"cache_hit"`
	Duration    string   `json:"duration,omitempty"`
	Error       string   `json:"error,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	TaskRunID   string   `json:"task_run_id"`

	// The endpoint emits these as RFC3339 strings with SECOND precision, and
	// omits them entirely when the instance never started (pending, skipped,
	// cancelled) or never finished. Decoded as *time.Time — the same shape
	// runTaskResponse.StartedAt uses — so "absent" is nil rather than a zero
	// time that compares as the year 1.
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (s *IntegrationTestSuite) jobTaskIDByName(jobID, name string) string {
	s.T().Helper()
	var raw []map[string]any
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &raw)
	for _, t := range raw {
		n, _ := t["name"].(string)
		if n == "" {
			n, _ = t["Name"].(string)
		}
		if n != name {
			continue
		}
		if id, ok := t["ID"].(string); ok && id != "" {
			return id
		}
		if id, ok := t["id"].(string); ok && id != "" {
			return id
		}
	}
	s.T().Fatalf("task %q not found on job %s", name, jobID)
	return ""
}

func (s *IntegrationTestSuite) expandedPartitions(rows []partitionInstance) []partitionInstance {
	out := make([]partitionInstance, 0, len(rows))
	for _, p := range rows {
		if p.Value != "" || p.Index > 0 {
			out = append(out, p)
		}
	}
	return out
}

func (s *IntegrationTestSuite) listPartitions(jobID, runID, taskRef string) []partitionInstance {
	s.T().Helper()
	var out partitionListResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions", jobID, runID, taskRef), &out)
	return out.Partitions
}

func (s *IntegrationTestSuite) TestFanOutMaterializesNInstances() {
	alias := fmt.Sprintf("fanout-n-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\",\"c\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo partition=$CAESIUM_PARTITION && echo json=$CAESIUM_PARTITION_JSON && echo '##caesium::output {\"saw\": \"'$CAESIUM_PARTITION'\"}'"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      onEmpty: fail
    next: [publish]
  - name: publish
    image: alpine:3.23
    command: ["sh", "-c", "echo done"]
    dependsOn: [process]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)

	processID := s.jobTaskIDByName(job.ID, "process")
	var processGroup *runTaskResponse
	for i := range run.Tasks {
		if run.Tasks[i].ID == processID || run.Tasks[i].PartitionCount >= 3 {
			processGroup = &run.Tasks[i]
			break
		}
	}
	s.Require().NotNil(processGroup, "collapsed run payload should include the process group")
	s.GreaterOrEqual(processGroup.PartitionCount, 3, "group partition_count should be N")

	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID))
	s.Len(parts, 3)
	seen := map[string]bool{}
	for _, p := range parts {
		s.Contains([]string{"succeeded", "cached"}, p.Status)
		seen[p.Value] = true
	}
	s.True(seen["a"] && seen["b"] && seen["c"], "expected partitions a,b,c got %v", seen)

	byName := s.listPartitions(job.ID, run.ID, "process")
	s.Len(byName, 3)

	stdout1, err := s.runCLIStdout("run", "partitions", run.ID, "--job-id", job.ID, "--task", "process", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.True(json.Valid([]byte(stdout1)), "caesium run partitions --json stdout must be valid JSON: %s", stdout1)
	var cli1 partitionListResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout1), &cli1))
	s.Len(cli1.Partitions, 3)

	stdout2, err := s.runCLIStdout("run", "partitions", run.ID, "--job-id", job.ID, "--task", "process", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.Equal(stdout1, stdout2)

	var http1, http2 partitionListResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions", job.ID, run.ID, processID), &http1)
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions", job.ID, run.ID, processID), &http2)
	s.Len(http1.Partitions, 3)
	s.Equal(http1.Partitions, http2.Partitions)
}

func (s *IntegrationTestSuite) TestFanOutOnEmptySkip() {
	alias := fmt.Sprintf("fanout-empty-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo no-partitions"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo should-not-run"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      onEmpty: skip
    next: [publish]
  - name: publish
    image: alpine:3.23
    command: ["sh", "-c", "echo still-ran"]
    dependsOn: [process]
    triggerRule: all_done
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)
}

func (s *IntegrationTestSuite) TestFanOutCapFailsProducer() {
	alias := fmt.Sprintf("fanout-cap-%d", time.Now().UnixNano())
	parts := make([]string, 0, 9)
	for i := 0; i < 9; i++ {
		parts = append(parts, fmt.Sprintf(`\"p%d\"`, i))
	}
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [%s]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo x"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
`, alias, strings.Join(parts, ","))

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)
	processID := s.jobTaskIDByName(job.ID, "process")
	s.Empty(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)), "cap failure must leave zero instance rows")
}

func (s *IntegrationTestSuite) TestFanOutCycleFailsProducer() {
	alias := fmt.Sprintf("fanout-cycle-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        echo '##caesium::partitions [{"key":"a","dependsOn":["b"]},{"key":"b","dependsOn":["a"]}]'
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo x"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)
	processID := s.jobTaskIDByName(job.ID, "process")
	s.Empty(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)))
	_ = json.RawMessage(nil)
}

func (s *IntegrationTestSuite) TestFanOutOnEmptyFail() {
	alias := fmt.Sprintf("fanout-empty-fail-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo no-partitions"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo should-not-run"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      onEmpty: fail
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)
}

func (s *IntegrationTestSuite) TestFanOutOrderedChain() {
	alias := fmt.Sprintf("fanout-ord-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        echo '##caesium::partitions [{"key":"a"},{"key":"b","dependsOn":["a"]},{"key":"c","dependsOn":["b"]}]'
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo partition=$CAESIUM_PARTITION"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      maxParallel: 1
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status)
	processID := s.jobTaskIDByName(job.ID, "process")
	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID))
	s.Len(parts, 3)
	for _, p := range parts {
		s.Contains([]string{"succeeded", "cached"}, p.Status)
	}
}

func (s *IntegrationTestSuite) TestFanOutContinueSkipCascade() {
	alias := fmt.Sprintf("fanout-cont-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        echo '##caesium::partitions [{"key":"ok"},{"key":"bad"},{"key":"dep","dependsOn":["bad"]}]'
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "if [ \"$CAESIUM_PARTITION\" = bad ]; then exit 1; fi; echo ok"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      failurePolicy: continue
  - name: publish
    image: alpine:3.23
    command: ["sh", "-c", "echo done"]
    dependsOn: [process]
    triggerRule: all_done
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Contains([]string{"succeeded", "failed"}, run.Status)
	processID := s.jobTaskIDByName(job.ID, "process")
	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID))
	s.Require().Len(parts, 3)
	byVal := map[string]string{}
	for _, p := range parts {
		byVal[p.Value] = p.Status
	}
	s.Equal("succeeded", byVal["ok"])
	s.Equal("failed", byVal["bad"])
	s.Equal("skipped", byVal["dep"])
}

// TestFanOutConflictingKeyFailsProducer covers the duplicate-key CONFLICT: one
// key emitted twice with two different payloads is unresolvable — the two
// emissions describe different units of work under one identity — so it fails
// the producer and materializes nothing.
//
// The fixture uses two well-formed sha256 digests deliberately. An earlier
// version emitted fingerprint:"one"/"two", which the marker parser rejects as a
// malformed digest BEFORE the accumulator's duplicate-key check ever runs, so
// the test passed on the wrong error and the conflicting-key class had zero
// coverage. The error assertion below pins that: it must name the key and the
// conflict, not the digest format.
func (s *IntegrationTestSuite) TestFanOutConflictingKeyFailsProducer() {
	alias := fmt.Sprintf("fanout-dup-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias: alias,
		ProducerCmd: fmt.Sprintf(
			`echo '##caesium::partitions [{"key":"dup-key","fingerprint":"%s"},{"key":"dup-key","fingerprint":"%s"}]'`,
			fingerprintA, fingerprintB),
		ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("failed", run.Status)

	// The cause must be legible to an operator: the producer's row carries the
	// conflicting key. A generic "task failed" here would leave the author of the
	// producer with nothing to act on.
	failure := run.Error
	for _, t := range run.Tasks {
		if t.Error != "" {
			failure += "\n" + t.Error
		}
	}
	s.Contains(failure, "dup-key", "the failure must name the conflicting partition key:\n%s", failure)
	s.Contains(failure, "conflicting", "the failure must say the payloads conflict, not that a digest is malformed:\n%s", failure)

	processID := s.jobTaskIDByName(job.ID, "process")
	s.Empty(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)))
}

// TestFanOutHTTPRetryPartition drives the per-instance retry ENDPOINT, whose
// contract is deliberately lane-dependent: retrying one instance means
// re-executing it, and only a distributed lane runs the dispatcher that can, so
// the local server answers 409 instead of resetting a row nothing would pick up.
// The endpoint also accepts a `failed` instance only — retrying a success would
// discard a good result and re-run the work for nothing — so the fixture must
// hand it a genuinely failed one, which the assertion below pins rather than
// assumes.
func (s *IntegrationTestSuite) TestFanOutHTTPRetryPartition() {
	alias := fmt.Sprintf("fanout-retry-%d", time.Now().UnixNano())
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"keep\",\"fail\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "if [ \"$CAESIUM_PARTITION\" = fail ]; then exit 1; fi; echo ok"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      failurePolicy: continue
    next: [publish]
  - name: publish
    image: alpine:3.23
    command: ["sh", "-c", "echo done"]
    dependsOn: [process]
    triggerRule: all_done
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	processID := s.jobTaskIDByName(job.ID, "process")
	before := partitionsByValue(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)))
	s.Require().Len(before, 2, "both partitions must materialize: %v", before)
	failRow, ok := before["fail"]
	s.Require().True(ok, "the fixture must produce a `fail` instance: %v", before)
	s.Require().Equal("failed", failRow.Status,
		"the endpoint accepts a FAILED instance only, so the fixture has to hand it one (got %q)", failRow.Status)
	keepRow := before["keep"]

	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry", s.caesiumURL, job.ID, run.ID, processID, failRow.Index), nil)
	s.Require().NoError(err)
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().NoError(readErr)

	if !distributedLane() {
		s.Require().Equal(http.StatusConflict, resp.StatusCode,
			"local mode must refuse per-partition retry rather than reset an instance nothing re-dispatches: %s", body)
		s.Contains(string(body), "distributed execution mode",
			"the refusal must name the condition an operator can act on: %s", body)

		after := partitionsByValue(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)))
		s.Equal("failed", after["fail"].Status,
			"a refused retry must leave the instance exactly as it was, never half-reset")
		s.Equal(failRow.TaskRunID, after["fail"].TaskRunID, "a refused retry must not re-key the instance")
		return
	}

	s.Require().Equal(http.StatusOK, resp.StatusCode,
		"retrying a failed instance in a distributed lane must be accepted: %s", body)

	// The reset happens inside the request, so this half is deterministic in
	// every distributed configuration regardless of dispatch cadence.
	after := partitionsByValue(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)))
	s.NotEqual("failed", after["fail"].Status,
		"a 200 retry must reset the instance for another attempt, got %q", after["fail"].Status)
	s.Equal(failRow.TaskRunID, after["fail"].TaskRunID,
		"a retry reuses the instance row; a new row would orphan its logs and receipts")
	s.Equal(keepRow.Status, after["keep"].Status, "retrying `fail` must not disturb its sibling")
	s.Equal(keepRow.TaskRunID, after["keep"].TaskRunID, "retrying `fail` must not re-key its sibling")

	// Re-dispatch: a 200 claims the instance will run again, so the reopened run
	// must drain rather than leave it non-terminal forever. WHICH terminal state
	// is deliberately not asserted — this fixture's command still fails for
	// `fail`, so failing a second time proves the re-execution just as well.
	redispatchDeadline := time.Now().Add(60 * time.Second)
	for {
		current := partitionsByValue(s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)))
		if isTerminalPartitionStatus(current["fail"].Status) {
			break
		}
		if time.Now().After(redispatchDeadline) {
			s.Failf("a reset instance never ran again",
				"partition `fail` is still %q 60s after a 200 retry: the endpoint accepted a retry that "+
					"nothing performed, so the reopened run is not being dispatched", current["fail"].Status)
			break
		}
		time.Sleep(fanOutPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Read surfaces over a fanned group (Stream E3 / G6): why, receipt, and logs.
//
// Each of these used to resolve (job_run_id, task_id) with a `.First()` and so
// answered for an ARBITRARY sibling of the group. These scenarios drive the real
// CLI/HTTP surfaces — not the internal functions — because the specific failure
// they guard against (an answer that is well-formed but about the wrong
// instance) is invisible to a unit test that hand-picks the row.
// ---------------------------------------------------------------------------

// whyGroupResponse mirrors the fan-out fields of `caesium why --json`.
type whyGroupResponse struct {
	TaskName  string `json:"taskName"`
	TaskRunID string `json:"taskRunId"`
	Partition string `json:"partition"`
	Status    string `json:"status"`
	Verdict   string `json:"verdict"`
	Summary   string `json:"summary"`
	Baseline  struct {
		Kind string `json:"kind"`
	} `json:"baseline"`
	Group *struct {
		PartitionCount int            `json:"partitionCount"`
		StatusCounts   map[string]int `json:"statusCounts"`
		Partitions     []string       `json:"partitions"`
		FirstFailure   *struct {
			Partition string `json:"partition"`
			Error     string `json:"error"`
		} `json:"firstFailure"`
	} `json:"group"`
}

// fanOutReadSurfaceManifest is a 3-partition job whose instances each print
// their partition into the log, so a log-streaming assertion can prove WHICH
// container it got.
func fanOutReadSurfaceManifest(alias string) string {
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\",\"c\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "echo partition=$CAESIUM_PARTITION"]
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
      onEmpty: fail
`, alias)
}

// TestFanOutWhyGroupSummaryAndPartitionSelector drives `caesium why` over a
// fanned step: the default answer is the GROUP (which is the only honest answer
// when --task names N instances), and --partition selects one.
func (s *IntegrationTestSuite) TestFanOutWhyGroupSummaryAndPartitionSelector() {
	alias := fmt.Sprintf("fanout-why-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(fanOutReadSurfaceManifest(alias))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status)

	// (a) No selector -> group summary.
	out, err := s.runCLIStdout("why", run.ID, "--job-id", job.ID, "--task", "process", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium why failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)),
		"caesium why --json stdout was not valid JSON (log contamination?):\n%s", out)

	var group whyGroupResponse
	s.Require().NoError(json.Unmarshal([]byte(out), &group))
	s.Require().NotNil(group.Group, "a fanned step must answer with a group summary, not one sibling:\n%s", out)
	s.Equal(3, group.Group.PartitionCount)
	s.ElementsMatch([]string{"a", "b", "c"}, group.Group.Partitions)
	s.Equal(3, group.Group.StatusCounts["succeeded"]+group.Group.StatusCounts["cached"],
		"status histogram must account for every instance: %+v", group.Group.StatusCounts)
	s.Empty(group.TaskRunID, "a group summary names no single task run")
	s.Equal("per_partition", group.Baseline.Kind)
	s.Contains(group.Summary, "FANNED GROUP")

	// (b) --partition selects one instance and restores the single-task answer.
	outB, err := s.runCLIStdout("why", run.ID, "--job-id", job.ID, "--task", "process",
		"--partition", "b", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium why --partition failed:\n%s", outB)
	s.Require().True(json.Valid([]byte(outB)), "why --partition stdout was not valid JSON:\n%s", outB)

	var instance whyGroupResponse
	s.Require().NoError(json.Unmarshal([]byte(outB), &instance))
	s.Nil(instance.Group, "an explicit selection is a single-instance explanation")
	s.Equal("b", instance.Partition)
	s.NotEmpty(instance.TaskRunID, "the selected instance must name its task run id")

	// The task run id must belong to partition b — the whole point of the fix.
	processID := s.jobTaskIDByName(job.ID, "process")
	var wantTaskRunID string
	for _, p := range s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID)) {
		if p.Value == "b" {
			wantTaskRunID = p.TaskRunID
		}
	}
	s.Require().NotEmpty(wantTaskRunID)
	s.Equal(wantTaskRunID, instance.TaskRunID, "why --partition b must explain partition b's row")

	// (c) An unknown partition fails with the available values, not a bare 404.
	_, errUnknown := s.runCLIStdout("why", run.ID, "--job-id", job.ID, "--task", "process",
		"--partition", "zz", "--json", "--server", s.caesiumURL)
	s.Require().Error(errUnknown)
	s.Contains(errUnknown.Error(), "available partitions")
}

// receiptResponse mirrors the fields of `caesium receipt get` this scenario
// asserts on.
type receiptResponse struct {
	Tasks []struct {
		TaskName     string `json:"task_name"`
		Partition    string `json:"partition"`
		IdentityHash string `json:"identity_hash"`
	} `json:"tasks"`
	ReceiptDigest string `json:"receipt_digest"`
}

// TestFanOutReceiptAttestsEveryPartition drives `caesium receipt get` over a
// fanned run. The receipt builder collapsed instances by task id, so a
// 3-partition step contributed ONE entry: the receipt attested one arbitrary
// partition and silently omitted the rest of the work it claims to describe.
func (s *IntegrationTestSuite) TestFanOutReceiptAttestsEveryPartition() {
	alias := fmt.Sprintf("fanout-receipt-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(fanOutReadSurfaceManifest(alias))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status)

	out, err := s.runCLIStdout("receipt", "get", "--job-id", job.ID, "--run-id", run.ID, "--server", s.caesiumURL)
	s.Require().NoError(err, "receipt get failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)),
		"receipt get stdout was not clean JSON (the fan-out note must go to stderr):\n%s", out)

	var receipt receiptResponse
	s.Require().NoError(json.Unmarshal([]byte(out), &receipt))

	partitions := map[string]string{}
	for _, entry := range receipt.Tasks {
		if entry.TaskName == "process" {
			partitions[entry.Partition] = entry.IdentityHash
		}
	}
	s.Len(partitions, 3, "one attested entry per partition, got %+v", receipt.Tasks)
	for _, value := range []string{"a", "b", "c"} {
		s.Contains(partitions, value)
	}
	s.NotEqual(partitions["a"], partitions["b"],
		"each partition is a distinct unit of work with its own identity hash")
	s.NotEmpty(receipt.ReceiptDigest)
}

// TestFanOutLogsSelectInstance drives the logs endpoint over a fanned group:
// unselected is a 400 enumerating the instances, and a task_run_id selection
// returns THAT instance's container output (each partition prints its own key,
// so a wrong-container answer is detectable).
func (s *IntegrationTestSuite) TestFanOutLogsSelectInstance() {
	alias := fmt.Sprintf("fanout-logs-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(fanOutReadSurfaceManifest(alias))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status)

	processID := s.jobTaskIDByName(job.ID, "process")
	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, processID))
	s.Require().Len(parts, 3)

	byValue := map[string]partitionInstance{}
	for _, p := range parts {
		byValue[p.Value] = p
	}

	// (a) No selector on a fanned task -> 400 listing the selectable instances.
	unselected, err := s.doJSONRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/logs?task_id=%s", s.caesiumURL, job.ID, run.ID, processID), nil)
	s.Require().NoError(err)
	defer unselected.Body.Close()
	s.Require().Equal(http.StatusBadRequest, unselected.StatusCode,
		"a fanned task without a selector must fail loudly, not stream instance 0")
	var problem struct {
		Message        string `json:"message"`
		PartitionCount int    `json:"partition_count"`
		Instances      []struct {
			TaskRunID string `json:"task_run_id"`
			Partition string `json:"partition"`
		} `json:"instances"`
	}
	body, readErr := io.ReadAll(unselected.Body)
	s.Require().NoError(readErr)
	s.Require().NoError(json.Unmarshal(body, &problem), "400 body was not the instance list: %s", body)
	s.Equal(3, problem.PartitionCount)
	s.Len(problem.Instances, 3, "the error must carry the list the client retries with")

	// (b) task_run_id selects that instance's log.
	for _, value := range []string{"a", "b", "c"} {
		instance := byValue[value]
		s.Require().NotEmpty(instance.TaskRunID)

		resp, logErr := s.doJSONRequest(http.MethodGet, fmt.Sprintf(
			"%s/v1/jobs/%s/runs/%s/logs?task_id=%s&task_run_id=%s",
			s.caesiumURL, job.ID, run.ID, processID, instance.TaskRunID), nil)
		s.Require().NoError(logErr)
		text, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		s.Require().Equal(http.StatusOK, resp.StatusCode, "logs for partition %s: %s", value, text)
		s.Equal(instance.TaskRunID, resp.Header.Get("X-Caesium-Task-Run-ID"),
			"the response must name the instance it streamed")
		s.Equal(value, resp.Header.Get("X-Caesium-Partition"))
		s.Contains(string(text), "partition="+value,
			"streamed the wrong container's log for partition %s: %s", value, text)
	}

	// (c) partition=<value> is the value-addressed equivalent.
	resp, err := s.doJSONRequest(http.MethodGet, fmt.Sprintf(
		"%s/v1/jobs/%s/runs/%s/logs?task_id=%s&partition=c",
		s.caesiumURL, job.ID, run.ID, processID), nil)
	s.Require().NoError(err)
	text, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(text))
	s.Contains(string(text), "partition=c")

	// (d) An unknown partition is a 404 that enumerates the real ones.
	missing, err := s.doJSONRequest(http.MethodGet, fmt.Sprintf(
		"%s/v1/jobs/%s/runs/%s/logs?task_id=%s&partition=zz",
		s.caesiumURL, job.ID, run.ID, processID), nil)
	s.Require().NoError(err)
	defer missing.Body.Close()
	s.Equal(http.StatusNotFound, missing.StatusCode)
}

// ---------------------------------------------------------------------------
// Partition-list semantics, failure policy, in-flight cap, ordered retry, and
// ordering — driven end to end against the live server in every lane
// (local, distributed, run-owner in-memory).
//
// These scenarios use the shared builder in fanout_helpers_test.go and assert
// on STATE (the partitions endpoint) rather than on elapsed time: the
// distributed lanes run a single worker on a 500 ms poll, so any wall-clock
// assertion is a lane-specific flake.
// ---------------------------------------------------------------------------

// TestFanOutIdenticalDuplicatePartitionDedups is the other half of the
// duplicate-key contract: the same key emitted twice with the SAME payload is
// one unit of work, not a conflict and not two instances. A producer that
// paginates a listing and re-emits an overlap page must not fan out twice over
// the same partition.
func (s *IntegrationTestSuite) TestFanOutIdenticalDuplicatePartitionDedups() {
	alias := fmt.Sprintf("fanout-dedup-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias: alias,
		ProducerCmd: fmt.Sprintf(
			`echo '##caesium::partitions [{"key":"same","fingerprint":"%s"},{"key":"same","fingerprint":"%s"},{"key":"other","fingerprint":"%s"}]'`,
			fingerprintA, fingerprintA, fingerprintOther),
		ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status, "an identical re-emission is a dedup, not a failure: %s", run.Error)

	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, "process"))
	s.Require().Len(parts, 2, "the duplicate must collapse into one instance, got %v", partitionStatusMap(parts))
	statuses := partitionStatusMap(parts)
	for _, value := range []string{"same", "other"} {
		s.Contains([]string{"succeeded", "cached"}, statuses[value], "partition %s: %v", value, statuses)
	}

	// The surviving instance keeps the emitted fingerprint — dedup preserves the
	// payload rather than blanking it.
	byValue := partitionsByValue(parts)
	s.Equal(fingerprintA, byValue["same"].Fingerprint)
}

// TestFanOutPerPartitionCacheIdentity is acceptance criterion 3's cache half:
// the partition key and fingerprint fold into cache.HashInput, so an unchanged
// unit of work cache-hits on the next run and a unit whose CONTENT changed —
// same key, new fingerprint — is the only one that re-executes.
//
// The fixture pins the producer with `cache: false` on purpose. Two independent
// reasons:
//
//  1. A cached producer completes through CacheHitTask, which carries no
//     partitions, so its group would not expand at all.
//  2. A cache-enabled predecessor contributes its identity hash to every
//     downstream key (Store.PredecessorHashes). With the producer's cache off
//     it records no hash, so re-applying the producer with a different emitted
//     list does NOT re-key the consumer — which is exactly what makes
//     "only the changed partition misses" observable instead of "everything
//     misses because the producer changed".
//
// The producer-cache-ON composition — `cache.chain: values` on the consumer,
// only the changed fingerprints miss across a producer re-run — is
// TestFanOutValuesChainPerPartitionSkip.
func (s *IntegrationTestSuite) TestFanOutPerPartitionCacheIdentity() {
	alias := fmt.Sprintf("fanout-cache-identity-%d", time.Now().UnixNano())
	producer := func(fingerprintForB string) string {
		return fmt.Sprintf(
			`echo '##caesium::partitions [{"key":"a","fingerprint":"%s"},{"key":"b","fingerprint":"%s"},{"key":"c","fingerprint":"%s"}]'`,
			fingerprintA, fingerprintForB, fingerprintC)
	}
	job := fanOutJob{
		Alias:                 alias,
		JobCache:              true,
		ProducerCacheDisabled: true,
		ProducerCmd:           producer(fingerprintB),
		// Deterministic output only: a timestamp here would change the cached
		// value without changing the key and make the assertions meaningless.
		ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
	}

	dir := s.writeJobManifest(fanOutManifest(job))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobEntry := s.requireJobByAlias(alias)

	// Run 1 — cold: every instance executes.
	run1 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run1.Status, "run 1 failed: %s", run1.Error)
	statuses1 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run1.ID, "process")))
	s.Require().Len(statuses1, 3)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("succeeded", statuses1[value], "run 1 partition %s should execute cold: %v", value, statuses1)
	}

	// Run 2 — identical inputs: every instance is a hit. This is the per-unit
	// key working: one cache entry per partition, keyed by its own identity.
	run2 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run2.Status, "run 2 failed: %s", run2.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run2)["list"],
		"the producer must re-execute (cache: false) or its group cannot expand")
	statuses2 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run2.ID, "process")))
	s.Require().Len(statuses2, 3)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("cached", statuses2[value], "run 2 partition %s should be a cache hit: %v", value, statuses2)
	}

	// Run 3 — one partition's content changed. Its fingerprint is part of its
	// key, so it and only it misses.
	s.overwriteJobManifest(dir, fanOutManifest(func() fanOutJob {
		changed := job
		changed.ProducerCmd = producer(fingerprintBPrime)
		return changed
	}()))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run3 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run3.Status, "run 3 failed: %s", run3.Error)
	parts3 := s.expandedPartitions(s.listPartitions(jobEntry.ID, run3.ID, "process"))
	statuses3 := partitionStatusMap(parts3)
	s.Require().Len(statuses3, 3)
	s.Equal("succeeded", statuses3["b"], "the re-fingerprinted partition must re-execute: %v", statuses3)
	s.Equal("cached", statuses3["a"], "an unchanged partition must stay a hit: %v", statuses3)
	s.Equal("cached", statuses3["c"], "an unchanged partition must stay a hit: %v", statuses3)
	s.Equal(fingerprintBPrime, partitionsByValue(parts3)["b"].Fingerprint,
		"the instance row must record the fingerprint it was keyed by")
}

// valueSkipProducerCmd is the listing step for TestFanOutValuesChainPerPartitionSkip.
// `revision` is echoed (not a structured output) so it churns the producer's
// own identity without changing PredecessorOutputs. An empty token emits NO
// ##caesium::output on purpose: EquivalentPriorHash refuses to short-circuit a
// silent step, so predecessor-hash churn is real and chain: values is what
// stops it cascading into every instance. A non-empty token lets the same live
// scenario prove that predecessor outputs still invalidate every instance.
func valueSkipProducerCmd(revision, fingerprintForB, attributeForC, token string) string {
	lines := []string{fmt.Sprintf("echo warming revision=%s", revision)}
	if token != "" {
		lines = append(lines, fmt.Sprintf(`echo '##caesium::output {"token":%q}'`, token))
	}
	lines = append(lines, fmt.Sprintf(
		`echo '##caesium::partitions [{"key":"a","fingerprint":%q,"root":"root-a"},{"key":"b","fingerprint":%q,"root":"root-b"},{"key":"c","fingerprint":%q,"root":%q}]'`,
		fingerprintA, fingerprintForB, fingerprintC, attributeForC))
	return strings.Join(lines, "\n")
}

// assertPartitionExecutionLogs proves the cache status through the container's
// real observable surface. Executed instances print the current run ID and
// therefore have a persisted log snapshot; cache hits launch no container and
// have no snapshot on the new TaskRun row. A status-only assertion could stay
// green if execution happened and the row was projected as cached afterward.
func (s *IntegrationTestSuite) assertPartitionExecutionLogs(
	jobID string,
	run *runResponse,
	taskID string,
	executed map[string]bool,
) {
	s.T().Helper()
	instances := partitionsByValue(s.expandedPartitions(s.listPartitions(jobID, run.ID, taskID)))
	s.Require().Len(instances, 3)
	for _, partition := range []string{"a", "b", "c"} {
		instance, ok := instances[partition]
		s.Require().True(ok, "run %s has no partition %s", run.ID, partition)
		s.Require().NotEmpty(instance.TaskRunID)
		resp, err := s.doJSONRequest(http.MethodGet, fmt.Sprintf(
			"%s/v1/jobs/%s/runs/%s/logs?task_id=%s&task_run_id=%s",
			s.caesiumURL, jobID, run.ID, taskID, instance.TaskRunID), nil)
		s.Require().NoError(err)
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		s.Require().NoError(readErr)
		s.Equal(instance.TaskRunID, resp.Header.Get("X-Caesium-Task-Run-ID"),
			"log response must name this run's exact partition instance")
		s.Equal(partition, resp.Header.Get("X-Caesium-Partition"))

		if executed[partition] {
			s.Require().Equal(http.StatusOK, resp.StatusCode,
				"partition %s should have an execution log: %s", partition, body)
			s.Equal("persisted", resp.Header.Get("X-Caesium-Log-Source"))
			s.Contains(string(body), "executed_run="+run.ID,
				"partition %s did not expose this run's real container execution: %s", partition, body)
			continue
		}

		s.Require().Equal(http.StatusNoContent, resp.StatusCode,
			"cached partition %s unexpectedly has a container log: %s", partition, body)
		s.Equal("empty", resp.Header.Get("X-Caesium-Log-State"))
		s.Empty(body, "cached partition %s must not have executed a container", partition)
	}
}

// TestFanOutValuesChainPerPartitionSkip is issue #360's acceptance: with
// cache.chain: values on the fanned consumer, a producer re-run that changes
// only some partition fingerprints re-executes exactly those instances.
//
// Contrast TestFanOutPerPartitionCacheIdentity, which pins `cache: false` on
// the producer so it contributes no predecessor hash at all. That proves
// fingerprints enter the key; this proves the chain break makes them
// *effective* across a cache-enabled producer whose own inputs moved.
func (s *IntegrationTestSuite) TestFanOutValuesChainPerPartitionSkip() {
	alias := fmt.Sprintf("fanout-values-skip-%d", time.Now().UnixNano())
	job := fanOutJob{
		Alias:    alias,
		JobCache: true,
		// Producer cache stays ON so the producer records an identity hash.
		// Without that there is no predecessor-hash churn for values mode to
		// exclude, and a green test would not distinguish the chain break from
		// the cache-off workaround.
		ConsumerCache: "chain: values",
		ProducerCmd:   valueSkipProducerCmd("r1", fingerprintB, "root-c-v1", ""),
		ConsumerCmd:   "echo partition=$CAESIUM_PARTITION json=$CAESIUM_PARTITION_JSON executed_run=$CAESIUM_RUN_ID",
	}

	dir := s.writeJobManifest(fanOutManifest(job))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobEntry := s.requireJobByAlias(alias)

	// Run 1 — cold: producer and every instance execute.
	run1 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run1.Status, "run 1 failed: %s", run1.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run1)["list"],
		"run 1 producer must execute cold")
	statuses1 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run1.ID, "process")))
	s.Require().Len(statuses1, 3)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("succeeded", statuses1[value], "run 1 partition %s should execute cold: %v", value, statuses1)
	}
	processID := s.jobTaskIDByName(jobEntry.ID, "process")
	s.assertPartitionExecutionLogs(jobEntry.ID, run1, processID, map[string]bool{"a": true, "b": true, "c": true})

	// Run 2 — identical inputs: producer and every instance are hits.
	run2 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run2.Status, "run 2 failed: %s", run2.Error)
	s.Equal("cached", s.taskStatusesByName(jobEntry.ID, run2)["list"],
		"run 2 producer must cache-hit, or later assertions prove nothing about chain: values")
	statuses2 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run2.ID, "process")))
	s.Require().Len(statuses2, 3)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("cached", statuses2[value], "run 2 partition %s should be a cache hit: %v", value, statuses2)
	}
	s.assertPartitionExecutionLogs(jobEntry.ID, run2, processID, map[string]bool{})

	// Run 3 — producer identity churns (revision in the command); fingerprints
	// and consumed outputs do not. The producer re-executes; every instance
	// must stay cached. That is the skip chain: values exists for.
	s.overwriteJobManifest(dir, fanOutManifest(func() fanOutJob {
		changed := job
		changed.ProducerCmd = valueSkipProducerCmd("r2-edited", fingerprintB, "root-c-v1", "")
		return changed
	}()))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run3 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run3.Status, "run 3 failed: %s", run3.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run3)["list"],
		"run 3: the producer's own identity changed, so it must re-execute — "+
			"if this is cached, the scenario proves nothing")
	statuses3 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run3.ID, "process")))
	s.Require().Len(statuses3, 3)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("cached", statuses3[value],
			"run 3 partition %s must cache-hit under chain: values after producer churn: %v", value, statuses3)
	}
	s.assertPartitionExecutionLogs(jobEntry.ID, run3, processID, map[string]bool{})

	whyA := s.parseChainWhyPartition(jobEntry.ID, run3.ID, "process", "a")
	s.Equal("CACHE_HIT", whyA.Verdict)
	s.Require().NotNil(whyA.Diff)
	s.True(whyA.Diff.HashEqual, "an unchanged partition's hashes must be equal")
	s.Contains(whyA.Diff.Notes, "predecessor hashes excluded (chain: values)",
		"why --partition must name the exclusion, got %+v", whyA.Diff.Notes)

	// Run 4 — fingerprint of b changes. Fingerprints stay authoritative: b
	// re-executes even though the key and the (empty) predecessor outputs look
	// the same. a and c stay hits.
	s.overwriteJobManifest(dir, fanOutManifest(func() fanOutJob {
		changed := job
		changed.ProducerCmd = valueSkipProducerCmd("r2-edited", fingerprintBPrime, "root-c-v1", "")
		return changed
	}()))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run4 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run4.Status, "run 4 failed: %s", run4.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run4)["list"],
		"run 4: the emitted list changed, so the producer must re-execute")
	parts4 := s.expandedPartitions(s.listPartitions(jobEntry.ID, run4.ID, "process"))
	statuses4 := partitionStatusMap(parts4)
	s.Require().Len(statuses4, 3)
	s.Equal("succeeded", statuses4["b"], "the re-fingerprinted partition must re-execute: %v", statuses4)
	s.Equal("cached", statuses4["a"], "an unchanged partition must stay a hit: %v", statuses4)
	s.Equal("cached", statuses4["c"], "an unchanged partition must stay a hit: %v", statuses4)
	s.Equal(fingerprintBPrime, partitionsByValue(parts4)["b"].Fingerprint,
		"the instance row must record the fingerprint it was keyed by")
	s.assertPartitionExecutionLogs(jobEntry.ID, run4, processID, map[string]bool{"b": true})

	whyB := s.parseChainWhyPartition(jobEntry.ID, run4.ID, "process", "b")
	s.Equal("CACHE_MISS", whyB.Verdict)
	s.Require().NotNil(whyB.Diff)
	foundFingerprint := false
	for _, c := range whyB.Diff.Changes {
		if c.Field == "partitionFingerprint" {
			foundFingerprint = true
		}
	}
	s.True(foundFingerprint,
		"the miss must be attributed to the changed fingerprint, got %+v", whyB.Diff.Changes)

	// Run 5 — the producer adds a scalar output while every partition identity
	// stays fixed. values mode excludes only predecessor HASHES, never outputs:
	// every instance must execute, including in the distributed worker lanes.
	s.overwriteJobManifest(dir, fanOutManifest(func() fanOutJob {
		changed := job
		changed.ProducerCmd = valueSkipProducerCmd("r3-output", fingerprintBPrime, "root-c-v1", "token-v1")
		return changed
	}()))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run5 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run5.Status, "run 5 failed: %s", run5.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run5)["list"],
		"run 5: the producer command and scalar output changed, so it must execute")
	statuses5 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run5.ID, "process")))
	s.Require().Equal(map[string]string{
		"a": "succeeded",
		"b": "succeeded",
		"c": "succeeded",
	}, statuses5, "a changed predecessor output must invalidate every values-mode instance")
	s.assertPartitionExecutionLogs(jobEntry.ID, run5, processID, map[string]bool{"a": true, "b": true, "c": true})

	whyOutput := s.parseChainWhyPartition(jobEntry.ID, run5.ID, "process", "a")
	s.Equal("CACHE_MISS", whyOutput.Verdict)
	s.Require().NotNil(whyOutput.Diff)
	foundOutput := false
	for _, c := range whyOutput.Diff.Changes {
		if c.Field == "predecessorOutputs.list.token" {
			foundOutput = true
		}
	}
	s.True(foundOutput,
		"the miss must be attributed to the changed producer output, got %+v", whyOutput.Diff.Changes)

	// Run 6 — only c's scalar attribute changes; key, fingerprint, producer
	// output, and every other instance stay fixed. Attributes are execution
	// inputs because they are exposed through CAESIUM_PARTITION_JSON, so c alone
	// must miss. This is the live counterpart to the Greptile documentation fix.
	s.overwriteJobManifest(dir, fanOutManifest(func() fanOutJob {
		changed := job
		changed.ProducerCmd = valueSkipProducerCmd("r3-output", fingerprintBPrime, "root-c-v2", "token-v1")
		return changed
	}()))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	run6 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run6.Status, "run 6 failed: %s", run6.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run6)["list"],
		"run 6: the emitted partition attributes changed, so the producer must execute")
	statuses6 := partitionStatusMap(s.expandedPartitions(s.listPartitions(jobEntry.ID, run6.ID, "process")))
	s.Require().Equal(map[string]string{
		"a": "cached",
		"b": "cached",
		"c": "succeeded",
	}, statuses6, "exactly the attribute-changed partition must re-execute")
	s.assertPartitionExecutionLogs(jobEntry.ID, run6, processID, map[string]bool{"c": true})

	whyAttribute := s.parseChainWhyPartition(jobEntry.ID, run6.ID, "process", "c")
	s.Equal("CACHE_MISS", whyAttribute.Verdict)
	s.Require().NotNil(whyAttribute.Diff)
	foundAttribute := false
	for _, c := range whyAttribute.Diff.Changes {
		if c.Field == "partitionAttributes.root" {
			foundAttribute = true
		}
	}
	s.True(foundAttribute,
		"the miss must be attributed to the changed partition attribute, got %+v", whyAttribute.Diff.Changes)
}

// TestFanOutFailFastCancelsPendingSiblings drives failurePolicy: fail_fast —
// the contrast to TestFanOutContinueSkipCascade, which asserts the opposite
// policy on the same shape. On the first instance failure the group stops:
// pending siblings are resolved (never dispatched), and the downstream fan-in
// never runs.
//
// Determinism note: no assertion here names WHICH sibling is resolved, and an
// earlier version that did was red in the distributed lanes for good reason. A
// group's instance rows share a created_at and the claim predicate orders by
// (priority, created_at), so claim order among ready instances is undefined;
// with a single worker slot they run one at a time in claim order, and the same
// fixture produced x:skipped/y,z:succeeded on one run and z:skipped/x,y:succeeded
// on the next. Both are correct fail_fast behavior.
//
// The invariant is asserted instead by assertFailFastGroup, over the instance
// timestamps: once `bad` completes, no sibling may START. A sibling already in
// flight may finish normally (Caesium cannot kill its container), and one still
// pending must be resolved without ever running. The gate/dependsOn shape is
// kept only because it staggers readiness and so makes a genuinely pending
// sibling likely — nothing asserted depends on it.
func (s *IntegrationTestSuite) TestFanOutFailFastCancelsPendingSiblings() {
	alias := fmt.Sprintf("fanout-failfast-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias: alias,
		ProducerCmd: `echo '##caesium::partitions [` +
			`{"key":"bad"},` +
			`{"key":"gate"},` +
			`{"key":"x","dependsOn":["gate"]},` +
			`{"key":"y","dependsOn":["gate"]},` +
			`{"key":"z","dependsOn":["gate"]}]'`,
		ConsumerCmd: `case "$CAESIUM_PARTITION" in
  bad) echo failing; exit 1 ;;
  gate) sleep 8; echo gate-done ;;
  *) echo ok ;;
esac`,
		FanOutOpts: []string{"failurePolicy: fail_fast"},
		PublishCmd: "echo published",
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("failed", run.Status, "a failed instance under fail_fast must fail the run")

	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, "process"))
	s.Require().Len(parts, 5, "every emitted partition must have an instance row: %v", partitionStatusMap(parts))
	s.assertFailFastGroup(parts, "bad")

	s.NotEqual("succeeded", s.taskStatusesByName(job.ID, run)["publish"],
		"the fan-in must not run when the group failed")
}

// TestFanOutMaxParallelCapsInFlight drives fanOut.maxParallel (D2 in the claim
// predicate, D6 in the owner's ready queue, groupParallel in the local Kahn
// loop): no more than maxParallel instances of a group are in flight at once,
// and the group still drains.
//
// The cap is asserted by SAMPLING observed state, not by timing: a lane whose
// dispatch cadence differs would make any duration-based inference wrong, while
// a snapshot showing two rows in `running` is a violation in every lane. The
// "we observed something running" guard stops the assertion from passing
// vacuously when every sample lands between containers.
func (s *IntegrationTestSuite) TestFanOutMaxParallelCapsInFlight() {
	alias := fmt.Sprintf("fanout-maxparallel-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias:       alias,
		ProducerCmd: `echo '##caesium::partitions ["p1","p2","p3","p4"]'`,
		// Long enough that a violation is visible to a 250 ms sampler and short
		// enough that four serial instances finish well inside runTimeout.
		ConsumerCmd: "sleep 3\necho done=$CAESIUM_PARTITION",
		FanOutOpts:  []string{"maxParallel: 1"},
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)

	snapshots := s.observePartitionStates(job.ID, runID, "process", fanOutPollInterval, runTimeout)
	peak, at := maxConcurrent(snapshots)
	if peak > 1 {
		s.Failf("maxParallel exceeded",
			"maxParallel: 1 but %d instances were observed running at once (sample %d of %d): %v",
			peak, at, len(snapshots), snapshots[at].Statuses)
	}
	s.True(startedByLastSnapshot(snapshots),
		"no instance was ever sampled in `running`; the cap assertion would be vacuous (%d samples)", len(snapshots))

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status, "a capped group must still drain, never deadlock: %s", run.Error)
	s.awaitPartitionStatuses(job.ID, run.ID, "process", 15*time.Second, map[string]string{
		"p1": "succeeded|cached",
		"p2": "succeeded|cached",
		"p3": "succeeded|cached",
		"p4": "succeeded|cached",
	})
}

// TestFanOutOrderedChainDeeperThanMaxParallel is the deadlock-impossibility
// case D2 calls out: readiness derives from terminal siblings, never from free
// slots, so a dependency chain LONGER than maxParallel still drains. A cap
// implementation that gated readiness on slots (rather than in-flight count)
// would hang here — and hang, not fail, which is why the bounded wait matters.
func (s *IntegrationTestSuite) TestFanOutOrderedChainDeeperThanMaxParallel() {
	alias := fmt.Sprintf("fanout-chain-cap-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias: alias,
		ProducerCmd: `echo '##caesium::partitions [` +
			`{"key":"a"},` +
			`{"key":"b","dependsOn":["a"]},` +
			`{"key":"c","dependsOn":["b"]},` +
			`{"key":"d","dependsOn":["c"]}]'`,
		ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
		FanOutOpts:  []string{"maxParallel: 1"},
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)

	snapshots := s.observePartitionStates(job.ID, runID, "process", fanOutPollInterval, runTimeout)
	if peak, at := maxConcurrent(snapshots); peak > 1 {
		s.Failf("maxParallel exceeded",
			"maxParallel: 1 but %d instances ran at once (sample %d): %v", peak, at, snapshots[at].Statuses)
	}

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status,
		"a chain deeper than maxParallel must complete; a stall here is the deadlock D2 rules out: %s", run.Error)
	s.awaitPartitionStatuses(job.ID, run.ID, "process", 15*time.Second, map[string]string{
		"a": "succeeded|cached",
		"b": "succeeded|cached",
		"c": "succeeded|cached",
		"d": "succeeded|cached",
	})
}

// TestFanOutOrderedGroupRetryDrivesDependents drives E4 end to end: retrying a
// run whose ordered group failed at its ROOT must reset the root AND the
// instances the store skipped behind it, re-seed their in-group indegree, and
// run them in order — without re-expanding the group.
//
// The root fails on the first attempt and succeeds on the retry WITHOUT the job
// changing: its command carries a wall-clock deadline baked in at manifest
// generation, and it exits 1 until that epoch passes. No container-visible
// attempt counter exists (the injected env is CAESIUM_PARTITION,
// CAESIUM_PARTITION_JSON, CAESIUM_RUN_ID, CAESIUM_JOB_ALIAS and the param/output
// vars), so a clock is the only thing that distinguishes the two attempts from
// inside the container.
//
// An earlier version instead re-applied the step with a fixed command between
// the two attempts, and that made the fixture lane-dependent: the local executor
// reads a task's command from the CATALOG at execution time, so it picked the
// fix up, while a distributed worker executes the image/command SNAPSHOTTED onto
// the TaskRun row at run registration (RegisterTasks) and re-ran the original
// failing command — the retried run stayed
// map[a:failed b:skipped c:skipped solo:succeeded]. That divergence is real and
// pre-existing, has nothing to do with fan-out, and is deliberately NOT papered
// over here; this fixture simply stops depending on which of the two a lane
// does, so the assertions below are about retry semantics in every lane.
func (s *IntegrationTestSuite) TestFanOutOrderedGroupRetryDrivesDependents() {
	alias := fmt.Sprintf("fanout-retry-order-%d", time.Now().UnixNano())

	// How long the root keeps failing, measured from manifest generation. It
	// must comfortably exceed the time from here to partition a's FIRST
	// execution — `job apply`, the trigger, the producer's container, expansion,
	// then a's own dispatch on a 500 ms poll — because a window that expires
	// early makes run 1 succeed and the scenario vacuous. It is also the floor
	// on the test's wall time, so it trades directly against suite duration:
	// 30 s is several times the observed setup cost on a loaded ARM runner
	// without adding a minute to the lane.
	const rootFailureWindow = 30 * time.Second
	rootHealthyAt := time.Now().Add(rootFailureWindow)

	job := fanOutJob{
		Alias:    alias,
		JobCache: true,
		ProducerCmd: `echo '##caesium::partitions [` +
			`{"key":"a"},` +
			`{"key":"b","dependsOn":["a"]},` +
			`{"key":"c","dependsOn":["b"]},` +
			`{"key":"solo"}]'`,
		// `continue`, not fail_fast: the independent sibling must be allowed to
		// finish so the retry can prove it is NOT re-executed.
		FanOutOpts: []string{"failurePolicy: continue"},
		// `date +%s` is epoch seconds in alpine:3.23's shell, and containers
		// share the host clock, so this compares against the same clock the
		// test waits on below.
		ConsumerCmd: fmt.Sprintf(`if [ "$CAESIUM_PARTITION" = a ] && [ "$(date +%%s)" -lt %d ]; then
  echo "root failing until epoch %d"
  exit 1
fi
echo partition=$CAESIUM_PARTITION`, rootHealthyAt.Unix(), rootHealthyAt.Unix()),
	}

	dir := s.writeJobManifest(fanOutManifest(job))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobEntry := s.requireJobByAlias(alias)

	runID := s.triggerRun(jobEntry.ID)
	run := s.awaitRun(jobEntry.ID, runID, runTimeout)
	if run.Status == "succeeded" {
		// Distinguished from a product defect on purpose: the root was supposed
		// to fail and did not, which can only mean it ran late.
		s.T().Fatalf("run 1 succeeded: partition a did not execute until after its %s failure window "+
			"(deadline epoch %d, now %d). That is fixture timing, not a retry defect — widen rootFailureWindow.",
			rootFailureWindow, rootHealthyAt.Unix(), time.Now().Unix())
	}
	s.Require().Equal("failed", run.Status, "the root partition failed, so the run must fail")

	before := partitionsByValue(s.expandedPartitions(s.listPartitions(jobEntry.ID, runID, "process")))
	s.Require().Len(before, 4, "expected 4 instances, got %v", before)
	s.Equal("failed", before["a"].Status)
	s.Equal("skipped", before["b"].Status, "a dependent of a failed instance is skipped, not left pending")
	s.Equal("skipped", before["c"].Status, "the skip cascade is transitive")
	s.Contains([]string{"succeeded", "cached"}, before["solo"].Status,
		"an independent sibling under failurePolicy: continue still runs")

	// Wait out the root's failure window instead of re-applying the job: the
	// SAME command, from the catalog or from the TaskRun snapshot, now takes the
	// success branch. The extra 2 s covers the boundary second, since the
	// container compares whole epoch seconds.
	if wait := time.Until(rootHealthyAt) + 2*time.Second; wait > 0 {
		time.Sleep(wait)
	}

	resp, err := s.doJSONRequest(http.MethodPost,
		fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", s.caesiumURL, jobEntry.ID, runID), nil)
	s.Require().NoError(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode, "retry should return 202: %s", body)
	var reopened runResponse
	s.Require().NoError(json.Unmarshal(body, &reopened))
	s.NotEqual("failed", reopened.Status,
		"the retry response must show the run re-opened, or the wait below races the previous terminal state: %s", body)

	retried := s.awaitRun(jobEntry.ID, runID, runTimeout)
	s.Equal("succeeded", retried.Status, "the retried run should succeed once the root does: %s", retried.Error)

	afterRows := s.expandedPartitions(s.listPartitions(jobEntry.ID, runID, "process"))
	after := partitionsByValue(afterRows)
	afterStatuses := partitionStatusMap(afterRows)
	s.Require().Len(after, 4, "a retry reuses the recorded instances; it must not re-expand the group: %v", afterStatuses)
	for _, value := range []string{"a", "b", "c"} {
		s.Contains([]string{"succeeded", "cached"}, after[value].Status,
			"partition %s should have re-run in order after the retry: %v", value, afterStatuses)
	}
	// The independent sibling is preserved, not re-executed: it is either left
	// succeeded (RetryFromFailure keeps terminal successes) or served from cache.
	s.Contains([]string{"succeeded", "cached"}, after["solo"].Status)
	s.Equal(before["solo"].TaskRunID, after["solo"].TaskRunID,
		"the sibling's instance row must be reused, not recreated")

	// `caesium run retry --partition` is the single-instance verb over the same
	// group — and at this point every instance in it has SUCCEEDED, so what the
	// CLI must do is REFUSE. Review settled two guards, and this asserts both
	// through the operator's surface: the endpoint accepts a `failed` instance
	// only (retrying a success would discard a good result and re-run the work
	// for nothing), and the verb requires distributed execution mode. A
	// successful --partition retry of a genuinely failed instance is covered by
	// TestFanOutPartitionsPaginateAndResolveByKey.
	args := []string{
		"run", "retry",
		"--job-id", jobEntry.ID,
		"--run-id", runID,
		"--task", "process",
		"--partition", "b",
		"--server", s.caesiumURL,
	}
	if s.authAPIKey != "" {
		args = append(args, "--api-key", s.authAPIKey)
	}
	// Stdout and stderr are kept apart, and the match is on the MESSAGE: the CLI
	// prints cobra usage alongside the error, so asserting over the whole output
	// would pin the usage text rather than the refusal.
	stdout, stderr, cliErr := s.runCLISeparate(args...)
	combined := stdout + stderr
	s.Require().Error(cliErr,
		"retrying a succeeded instance must fail the command, not silently re-run it:\n%s", combined)
	if distributedLane() {
		s.Contains(combined, "only a failed partition can be retried",
			"the refusal must say WHY this instance is ineligible:\n%s", combined)
	} else {
		s.Contains(combined, "distributed execution mode",
			"in local mode the verb is refused for the lane, before instance eligibility:\n%s", combined)
	}

	// Either refusal must be inert. A retry that half-applied before rejecting
	// would leave the group inconsistent in a way the run's status never shows.
	single := partitionsByValue(s.expandedPartitions(s.listPartitions(jobEntry.ID, runID, "process")))
	for _, value := range []string{"a", "b", "c", "solo"} {
		s.Equal(after[value].Status, single[value].Status,
			"a refused --partition retry must not disturb instance %s", value)
		s.Equal(after[value].TaskRunID, single[value].TaskRunID,
			"a refused --partition retry must not re-key instance %s", value)
	}
}

// TestFanOutDiamondOrdering asserts in-group ordering over a diamond:
// a → (b, c) → d. b and c may run in parallel; d may not start until both are
// terminal, and neither may start until a is.
//
// The claim is asserted over sampled STATE rather than timestamps because the
// partition surface exposes none — the endpoint returns a duration, `why`
// returns per-instance status but no start/end, and the run payload collapses a
// group. A sample showing d running while b is still pending is an ordering
// violation in any lane, with no clock involved.
func (s *IntegrationTestSuite) TestFanOutDiamondOrdering() {
	alias := fmt.Sprintf("fanout-diamond-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias: alias,
		ProducerCmd: `echo '##caesium::partitions [` +
			`{"key":"a"},` +
			`{"key":"b","dependsOn":["a"]},` +
			`{"key":"c","dependsOn":["a"]},` +
			`{"key":"d","dependsOn":["b","c"]}]'`,
		// Long enough for the sampler to catch each instance in `running`.
		ConsumerCmd: "sleep 2\necho partition=$CAESIUM_PARTITION",
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)

	snapshots := s.observePartitionStates(job.ID, runID, "process", fanOutPollInterval, runTimeout)

	started := func(status string) bool { return status != "" && status != "pending" }
	succeeded := func(status string) bool { return status == "succeeded" || status == "cached" }

	for i, snap := range snapshots {
		st := snap.Statuses
		if len(st) < 4 {
			continue // pre-expansion sample
		}
		if started(st["b"]) || started(st["c"]) {
			s.True(succeeded(st["a"]),
				"sample %d: b/c started while the root a was %q — in-group ordering violated: %v", i, st["a"], st)
		}
		if started(st["d"]) {
			s.True(succeeded(st["b"]) && succeeded(st["c"]),
				"sample %d: d started while b=%q c=%q — a join must wait for BOTH dependencies: %v",
				i, st["b"], st["c"], st)
		}
	}
	s.True(startedByLastSnapshot(snapshots),
		"no instance was ever sampled running; the ordering assertions would be vacuous (%d samples)", len(snapshots))

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Equal("succeeded", run.Status, "the diamond should complete: %s", run.Error)
	final := partitionsByValue(s.awaitPartitionStatuses(job.ID, run.ID, "process", 15*time.Second, map[string]string{
		"a": "succeeded|cached",
		"b": "succeeded|cached",
		"c": "succeeded|cached",
		"d": "succeeded|cached",
	}))

	// The recorded timestamps close the one gap the sampler cannot: a 250 ms
	// sampler proves no violation was OBSERVED, and a violation shorter than the
	// sampling interval slips between two samples. started_at/completed_at are
	// written by the run store at the moment of each transition, so they are a
	// claim about what happened rather than about what was seen.
	//
	// Two properties keep this from being a clock-skew assertion. Every lane's
	// processes share one host kernel clock (the integration server, its worker,
	// and the owner all run as containers on the same Docker host), and the
	// endpoint formats RFC3339 at SECOND precision, which truncates rather than
	// rounds — a monotone transform, so a genuinely correct ordering can never
	// invert here. The comparisons are therefore >=, not >: two adjacent events
	// inside the same second legitimately render as the same string.
	for _, value := range []string{"a", "b", "c", "d"} {
		s.Require().NotNil(final[value].StartedAt,
			"instance %s reported no started_at (status %q); this fixture's alias is unique per test, "+
				"so every instance must run cold and record a start", value, final[value].Status)
		s.Require().NotNil(final[value].CompletedAt,
			"instance %s reported no completed_at (status %q)", value, final[value].Status)
	}

	// Fork: neither branch may start before the root finished.
	for _, value := range []string{"b", "c"} {
		s.False(final[value].StartedAt.Before(*final["a"].CompletedAt),
			"instance %s started at %s, before its dependency a completed at %s",
			value, partitionStamp(final[value].StartedAt), partitionStamp(final["a"].CompletedAt))
	}

	// Join: d waits for BOTH branches, so it may not start before the LATER of
	// the two completions — the half of the diamond a chain fixture never tests.
	joinReady := final["b"].CompletedAt
	if final["c"].CompletedAt.After(*joinReady) {
		joinReady = final["c"].CompletedAt
	}
	s.False(final["d"].StartedAt.Before(*joinReady),
		"instance d started at %s, before both dependencies had completed (b=%s, c=%s)",
		partitionStamp(final["d"].StartedAt), partitionStamp(final["b"].CompletedAt), partitionStamp(final["c"].CompletedAt))
}

// TestFanOutCachedProducerExpandsGroup is the cached-producer half of the
// fan-out cache contract, and the one shape the rest of the suite deliberately
// avoids: TestFanOutPerPartitionCacheIdentity pins `cache: false` on its
// producer precisely so the producer re-executes and re-emits its list on every
// run.
//
// A CACHEABLE producer is the ordinary shape in practice — a listing step whose
// inputs did not change — and it is the shape where a group silently collapses.
// A cache hit resolves the producer without ever running the container that
// printed the `##caesium::partitions` marker, so unless the hit carries the
// partition list recorded on the cache entry (cache.Entry.Partitions, applied
// through the store's CacheHitTask*WithPartitions path), the consumer expands to
// ZERO instances on run 2. Nothing fails: the run still reports succeeded, the
// fan-out just quietly stops happening on every run after the first. That is
// why the assertion below is a count and a status, not a run status.
//
// Run 1 is the cold baseline; run 2 is the claim.
func (s *IntegrationTestSuite) TestFanOutCachedProducerExpandsGroup() {
	alias := fmt.Sprintf("fanout-cached-producer-%d", time.Now().UnixNano())

	// Fingerprinted partitions on purpose: the fingerprint is part of each
	// instance's cache key, so "all three hit on run 2" is a statement about
	// per-unit identity surviving the producer's own cache hit, and the
	// fingerprints recorded on the run-2 rows prove the payloads round-tripped
	// through the cache entry rather than being rebuilt as bare keys.
	job := fanOutJob{
		Alias:    alias,
		JobCache: true,
		// No ProducerCacheDisabled: the producer being cacheable IS the scenario.
		ProducerCmd: fmt.Sprintf(
			`echo '##caesium::partitions [{"key":"a","fingerprint":"%s"},{"key":"b","fingerprint":"%s"},{"key":"c","fingerprint":"%s"}]'`,
			fingerprintA, fingerprintB, fingerprintC),
		// Deterministic output only. A timestamp here would change the cached
		// value without changing the key, which makes a hit indistinguishable
		// from a miss at the assertion level.
		ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
	}

	manifest := fanOutManifest(job)
	s.Require().NotContains(manifest, "cache: false",
		"this scenario is void if the builder disables the producer's cache:\n%s", manifest)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	jobEntry := s.requireJobByAlias(alias)

	// Run 1 — cold. The producer executes, emits its list, and the group expands
	// from the marker output.
	run1 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run1.Status, "run 1 failed: %s", run1.Error)
	s.Equal("succeeded", s.taskStatusesByName(jobEntry.ID, run1)["list"],
		"the alias is unique per test, so run 1's producer must execute cold")

	parts1 := s.expandedPartitions(s.listPartitions(jobEntry.ID, run1.ID, "process"))
	statuses1 := partitionStatusMap(parts1)
	s.Require().Len(parts1, 3, "run 1 must expand three instances: %v", statuses1)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("succeeded", statuses1[value], "run 1 partition %s should execute cold: %v", value, statuses1)
	}

	// Run 2 — every input unchanged, so the producer is served from cache.
	run2 := s.awaitRun(jobEntry.ID, s.triggerRun(jobEntry.ID), runTimeout)
	s.Require().Equal("succeeded", run2.Status, "run 2 failed: %s", run2.Error)
	s.Require().Equal("cached", s.taskStatusesByName(jobEntry.ID, run2)["list"],
		"run 2's producer must be a cache hit, or this scenario is just run 1 again")

	parts2 := s.expandedPartitions(s.listPartitions(jobEntry.ID, run2.ID, "process"))
	statuses2 := partitionStatusMap(parts2)
	s.Require().Len(parts2, 3,
		"a cached producer must still expand its consumer's group; %d instances means the "+
			"partition list did not survive the cache hit and the fan-out silently stopped: %v",
		len(parts2), statuses2)
	for _, value := range []string{"a", "b", "c"} {
		s.Equal("cached", statuses2[value],
			"run 2 partition %s should be served from its own per-unit cache entry: %v", value, statuses2)
	}

	// The instances must carry the fingerprints the producer originally emitted.
	// The cache entry stores the normalized partition structs, not the flattened
	// wire form, and a hit that rebuilt bare keys would still produce three rows
	// with three different (wrong) identities — which is exactly how the
	// "everything re-executes forever" regression would look.
	byValue2 := partitionsByValue(parts2)
	for value, want := range map[string]string{"a": fingerprintA, "b": fingerprintB, "c": fingerprintC} {
		s.Equal(want, byValue2[value].Fingerprint,
			"partition %s lost its fingerprint through the producer's cache hit", value)
	}
}

// TestFanOutFailFastIsTheDefault pins the SCHEMA DEFAULT: a fanned step that
// omits `failurePolicy` entirely must behave exactly like one that spells out
// `fail_fast`. TestFanOutFailFastCancelsPendingSiblings asserts the same
// contract with the key present; this one asserts nobody has to write it.
//
// The default is applied in two independent places — pkg/jobdef's validateSteps
// stamps `fail_fast` onto the stored config, and internal/run's
// normalizeFanOutFailurePolicy resolves an unset policy at runtime — and a
// manifest that omits the key is the only fixture that exercises either. A
// regression here does not fail loudly: an unset policy that fell through to
// `continue` would run every pending sibling to completion and the run would
// still reach a terminal status, so the assertion has to be about the siblings.
//
// Determinism note: the shape and the assertions are deliberately identical to
// the explicit-policy scenario — same fixture, same assertFailFastGroup call —
// so the two differ in exactly one thing, the presence of the key. Neither names
// which sibling is resolved: claim order among ready instances is undefined, and
// on a single-slot worker the resolved sibling varies run to run. The invariant
// asserted is the timestamp one: after `bad` completes no sibling may start,
// an in-flight sibling may finish, and a pending one must be resolved unrun.
func (s *IntegrationTestSuite) TestFanOutFailFastIsTheDefault() {
	alias := fmt.Sprintf("fanout-failfast-default-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias: alias,
		ProducerCmd: `echo '##caesium::partitions [` +
			`{"key":"bad"},` +
			`{"key":"gate"},` +
			`{"key":"x","dependsOn":["gate"]},` +
			`{"key":"y","dependsOn":["gate"]},` +
			`{"key":"z","dependsOn":["gate"]}]'`,
		ConsumerCmd: `case "$CAESIUM_PARTITION" in
  bad) echo failing; exit 1 ;;
  gate) sleep 8; echo gate-done ;;
  *) echo ok ;;
esac`,
		// No FanOutOpts: the ABSENT failurePolicy is the whole point.
		PublishCmd: "echo published",
	})

	// A fixture that quietly grew the key would assert nothing new, so pin it.
	s.Require().NotContains(manifest, "failurePolicy",
		"this scenario must omit failurePolicy; the explicit form is already covered "+
			"by TestFanOutFailFastCancelsPendingSiblings:\n%s", manifest)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)

	s.Equal("failed", run.Status,
		"an unset failurePolicy defaults to fail_fast, so a failed instance must fail the run")

	parts := s.expandedPartitions(s.listPartitions(job.ID, run.ID, "process"))
	s.Require().Len(parts, 5, "every emitted partition must have an instance row: %v", partitionStatusMap(parts))

	// The load-bearing check: with no policy written down, the group must obey
	// the same start-ordering invariant the explicit fail_fast fixture does. Had
	// the default regressed to `continue`, siblings would keep being dispatched
	// after `bad` failed — which is precisely what this rejects.
	s.assertFailFastGroup(parts, "bad")

	s.NotEqual("succeeded", s.taskStatusesByName(job.ID, run)["publish"],
		"the fan-in must not run when the group failed under the default policy")
}

// partitionPageResponse is the PAGINATED envelope of the partitions endpoint.
// The narrower partitionListResponse above decodes only the rows, which is why
// nothing caught the endpoint silently truncating a group at its default page:
// `total`, `limit`, `offset` and `next_offset` are the whole contract a client
// pages on, and a test that never decoded them could not observe it.
//
// NextOffset is a *int because the last page reports JSON null, and a plain int
// would decode that as 0 — "start over", which is an infinite loop, not an end.
type partitionPageResponse struct {
	Partitions   []partitionInstance `json:"partitions"`
	Total        int                 `json:"total"`
	Limit        int                 `json:"limit"`
	Offset       int                 `json:"offset"`
	NextOffset   *int                `json:"next_offset"`
	StatusCounts map[string]int      `json:"status_counts"`
}

// listPartitionsPage reads one page of the partitions endpoint with an explicit
// query string, returning the full envelope rather than only the rows.
func (s *IntegrationTestSuite) listPartitionsPage(jobID, runID, taskRef, query string) partitionPageResponse {
	s.T().Helper()
	path := fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions", jobID, runID, taskRef)
	if query != "" {
		path += "?" + query
	}
	var out partitionPageResponse
	s.getJSON(path, &out)
	return out
}

// partitionValuesOf projects instance rows onto their partition values, in the
// order the endpoint returned them (partition_index ascending).
func partitionValuesOf(rows []partitionInstance) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Value)
	}
	return out
}

// TestFanOutPartitionsPaginateAndResolveByKey drives the PAGINATION contract of
// the partitions endpoint and the two CLI consumers of it, end to end.
//
// Both consumers were reading page one and calling it the group:
//
//   - `caesium run partitions` rendered one response as the whole table, so a
//     group larger than a page was silently truncated with nothing saying so;
//   - `caesium run retry --partition <value>` listed and SCANNED for the value,
//     so any instance past page one came back `partition "x" not found` — the
//     larger the fan-out, the more of it was unreachable.
//
// Server-cap constraint on the fixture: every integration server runs with
// CAESIUM_FANOUT_MAX_PARTITIONS=8, so a group big enough to overflow the
// endpoint's real default page (100) cannot be materialized here. Paging is
// therefore exercised by shrinking the PAGE to the group instead of growing the
// group past the page — `limit=3` over five instances is the same two-page walk
// the default page size performs over 250, and it exercises the identical
// server code path (partitionPageBounds/nextPartitionOffset).
//
// `e` is the last instance by partition_index and is the one made to fail: it
// is both the row a page-one scan cannot see and the only kind of row a
// per-partition retry accepts (terminal).
func (s *IntegrationTestSuite) TestFanOutPartitionsPaginateAndResolveByKey() {
	alias := fmt.Sprintf("fanout-paging-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias:       alias,
		ProducerCmd: `echo '##caesium::partitions ["a","b","c","d","e"]'`,
		// `e` fails so it ends terminal-failed: a per-partition retry is refused
		// on a non-terminal instance, and `continue` keeps its siblings running
		// so the group really does materialize all five rows.
		ConsumerCmd: `echo "processing $CAESIUM_PARTITION"
if [ "$CAESIUM_PARTITION" = e ]; then
  echo "e is expected to fail"
  exit 1
fi`,
		FanOutOpts: []string{"failurePolicy: continue"},
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "instance `e` fails, so the run must fail under any policy")

	all := s.expandedPartitions(s.listPartitions(job.ID, run.ID, "process"))
	s.Require().Len(all, 5, "every emitted partition must have an instance row: %v", partitionStatusMap(all))
	s.Require().Equal("failed", partitionStatusMap(all)["e"])

	// ---- the endpoint's pagination contract -------------------------------

	first := s.listPartitionsPage(job.ID, run.ID, "process", "limit=3")
	s.Require().Len(first.Partitions, 3, "limit=3 must return three rows")
	s.Equal([]string{"a", "b", "c"}, partitionValuesOf(first.Partitions),
		"a page must be ordered by partition_index, not by arrival")
	s.Equal(5, first.Total, "total describes the paged set, not the page")
	s.Equal(3, first.Limit)
	s.Equal(0, first.Offset)
	s.Require().NotNil(first.NextOffset, "a truncated page must hand back a continuation cursor")
	s.Equal(3, *first.NextOffset)
	// status_counts is computed over the whole group, so a page still reports
	// the group's mix — the number an operator needs while paging.
	total := 0
	for _, n := range first.StatusCounts {
		total += n
	}
	s.Equal(5, total, "status_counts must describe the whole group, not the page: %v", first.StatusCounts)
	s.Equal(1, first.StatusCounts["failed"], "status_counts: %v", first.StatusCounts)

	second := s.listPartitionsPage(job.ID, run.ID, "process",
		fmt.Sprintf("limit=3&offset=%d", *first.NextOffset))
	s.Require().Len(second.Partitions, 2, "the tail page must carry the remaining rows")
	s.Equal([]string{"d", "e"}, partitionValuesOf(second.Partitions))
	s.Nil(second.NextOffset, "the last page must report next_offset null, not an offset past the end")

	// The keyed read: `e` lives on page two, and this is the lookup that makes
	// it addressable without walking there.
	keyed := s.listPartitionsPage(job.ID, run.ID, "process", "partition=e")
	s.Require().Len(keyed.Partitions, 1, "a keyed lookup must resolve exactly one instance")
	s.Equal("e", keyed.Partitions[0].Value)
	s.Equal(1, keyed.Total, "total must describe the filtered set for a keyed read")
	s.Nil(keyed.NextOffset)

	// ---- `caesium run partitions` -----------------------------------------

	stdout, stderr, err := s.runCLISeparate("run", "partitions", run.ID,
		"--job-id", job.ID, "--task", "process", "--server", s.caesiumURL)
	s.Require().NoError(err, "run partitions failed: %s", stderr)
	for _, value := range []string{"a", "b", "c", "d", "e"} {
		s.Contains(stdout, value,
			"the default table must page to completion, not stop at the first page:\n%s", stdout)
	}
	s.Contains(stdout, "5 partitions", "the table must state the size of the whole group:\n%s", stdout)

	// --json must carry the whole group too, on clean stdout.
	jsonOut, jsonErr, err := s.runCLISeparate("run", "partitions", run.ID,
		"--job-id", job.ID, "--task", "process", "--server", s.caesiumURL, "--json")
	s.Require().NoError(err, "run partitions --json failed: %s", jsonErr)
	var doc partitionPageResponse
	s.Require().NoError(json.Unmarshal([]byte(jsonOut), &doc),
		"--json stdout was not parseable (stderr contamination?):\n%s", jsonOut)
	s.Len(doc.Partitions, 5, "--json must emit every page's rows")
	s.Equal(5, doc.Total)
	s.ElementsMatch([]string{"a", "b", "c", "d", "e"}, partitionValuesOf(doc.Partitions))

	// An explicit --limit hands paging back to the caller: one page, and the
	// truncation note goes to STDERR so --json stays machine-readable.
	windowOut, windowErr, err := s.runCLISeparate("run", "partitions", run.ID,
		"--job-id", job.ID, "--task", "process", "--server", s.caesiumURL, "--limit", "3", "--json")
	s.Require().NoError(err, "run partitions --limit failed: %s", windowErr)
	var window partitionPageResponse
	s.Require().NoError(json.Unmarshal([]byte(windowOut), &window),
		"--limit --json stdout was not parseable:\n%s", windowOut)
	s.Len(window.Partitions, 3, "--limit must fetch exactly one page")
	s.Require().NotNil(window.NextOffset, "a windowed --json read must carry the continuation cursor")
	s.Equal(3, *window.NextOffset)
	s.Contains(windowErr, "showing 3 of 5 partitions",
		"a windowed read must say so, on stderr:\n%s", windowErr)

	// ---- `caesium run retry --partition` ----------------------------------

	// The regression: `e` is on page two of a limit-3 walk and was the row a
	// page-one scan reported as missing. The retry's OUTCOME is lane-dependent —
	// per-partition retry requires distributed execution mode, and the local
	// integration server refuses it with 409 — but "not found" is a defect in
	// EVERY lane, which is what this asserts.
	retryOut, retryErr, err := s.runCLISeparate("run", "retry",
		"--job-id", job.ID, "--run-id", run.ID, "--task", "process",
		"--partition", "e", "--server", s.caesiumURL)
	combined := retryOut + retryErr
	s.NotContains(combined, "not found",
		"--partition must resolve through the keyed lookup; a page-one scan is what made an "+
			"instance past the first page unreachable:\n%s", combined)
	if err != nil {
		s.Contains(combined, "distributed execution mode",
			"the only acceptable failure here is the documented local-mode refusal:\n%s", combined)
	} else {
		s.Contains(retryOut, `Retried partition "e" (index 4)`,
			"a successful retry must name the instance it reset:\n%s", retryOut)
	}
}
