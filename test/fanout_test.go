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
	parts := s.listPartitions(job.ID, run.ID, processID)
	s.Require().GreaterOrEqual(len(parts), 2)
	failIdx := -1
	for _, p := range parts {
		if p.Value == "fail" {
			failIdx = p.Index
		}
	}
	s.Require().GreaterOrEqual(failIdx, 0)
	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry", s.caesiumURL, job.ID, run.ID, processID, failIdx), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)
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
	// group: it resets exactly one terminal instance and leaves its siblings
	// alone. Driven through the CLI (not the endpoint) because the CLI is the
	// surface an operator uses, and its stdout must stay clean.
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
	stdout, cliErr := s.runCLIStdout(args...)
	s.Require().NoError(cliErr, "caesium run retry --partition failed:\n%s", stdout)
	s.Contains(stdout, "partition", "the CLI must report what it retried: %q", stdout)
	s.Contains(stdout, `"b"`, "the CLI must name the partition it retried: %q", stdout)

	// The reset is synchronous in the request, so the sibling assertion is
	// deterministic in every lane regardless of when the instance is re-dispatched.
	single := partitionsByValue(s.expandedPartitions(s.listPartitions(jobEntry.ID, runID, "process")))
	for _, value := range []string{"a", "c", "solo"} {
		s.Equal(after[value].Status, single[value].Status,
			"retrying partition b must not disturb sibling %s", value)
		s.Equal(after[value].TaskRunID, single[value].TaskRunID,
			"retrying partition b must not re-key sibling %s", value)
	}
	s.NotContains([]string{"failed", "skipped"}, single["b"].Status,
		"the retried instance must be reset for another attempt, got %q", single["b"].Status)

	// Best-effort: where the lane has a dispatcher watching re-opened runs, the
	// reset instance re-executes and lands successful. This is asserted
	// conditionally on purpose — the reset above is the contract of the verb and
	// holds in every lane, while re-dispatch of an instance in an already-finished
	// run belongs to the dispatch loop, which only the distributed lanes run.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		current := partitionsByValue(s.expandedPartitions(s.listPartitions(jobEntry.ID, runID, "process")))
		if isTerminalPartitionStatus(current["b"].Status) {
			s.Contains([]string{"succeeded", "cached"}, current["b"].Status,
				"a re-dispatched instance must land successful, got %q", current["b"].Status)
			break
		}
		time.Sleep(fanOutPollInterval)
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
