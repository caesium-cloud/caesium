//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type runDiffRESTResponse struct {
	JobID        string               `json:"jobId"`
	LeftRunID    string               `json:"leftRunId"`
	RightRunID   string               `json:"rightRunId"`
	ParamChanges []runDiffFieldChange `json:"paramChanges,omitempty"`
	Tasks        []runDiffTask        `json:"tasks"`
}

type runDiffTask struct {
	TaskName string               `json:"taskName"`
	Verdict  string               `json:"verdict"`
	Changes  []runDiffFieldChange `json:"changes,omitempty"`
}

type runDiffFieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Added  bool   `json:"added,omitempty"`
}

type blameRESTResponse struct {
	JobID    string            `json:"job_id"`
	Coverage string            `json:"coverage"`
	Tasks    []blameTaskResult `json:"tasks"`
	Edges    []blameEdgeResult `json:"edges"`
}

type blameTaskResult struct {
	Element           blameTaskElement `json:"element"`
	IntroducingCommit string           `json:"introducing_commit"`
	SnapshotID        string           `json:"snapshot_id"`
}

type blameTaskElement struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
}

type blameEdgeResult struct {
	Element           blameEdgeElement `json:"element"`
	IntroducingCommit string           `json:"introducing_commit"`
	SnapshotID        string           `json:"snapshot_id"`
	ProvenanceCommit  string           `json:"provenance_commit,omitempty"`
}

type blameEdgeElement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type topologyHistoryResponse struct {
	Snapshots []topologySnapshot `json:"snapshots"`
}

type topologySnapshot struct {
	ID        string                 `json:"id"`
	JobID     string                 `json:"job_id"`
	GitCommit string                 `json:"git_commit"`
	Tasks     []topologySnapshotTask `json:"tasks"`
}

type topologySnapshotTask struct {
	Name string `json:"name"`
}

func (s *IntegrationTestSuite) TestRunDiffRESTEndpointCoversHTTPSurface() {
	aliasA := fmt.Sprintf("e2e-rundiff-a-%d", time.Now().UnixNano())
	dirA := s.writeJobManifest(runDiffManifest(aliasA))
	defer os.RemoveAll(dirA)
	s.runCLI("job", "apply", "--path", dirA, "--server", s.caesiumURL)
	jobA := s.requireJobByAlias(aliasA)
	s.Require().NotNil(jobA)

	runA1 := s.triggerRunWithParams(jobA.ID, map[string]string{"flavor": "vanilla"})
	s.Require().Equal("succeeded", s.awaitRun(jobA.ID, runA1, runTimeout).Status)
	runA2 := s.triggerRunWithParams(jobA.ID, map[string]string{"flavor": "chocolate"})
	s.Require().Equal("succeeded", s.awaitRun(jobA.ID, runA2, runTimeout).Status)

	diff := s.getRunDiff(jobA.ID, runA1, runA2)
	s.Equal(jobA.ID, diff.JobID)
	s.Equal(runA1, diff.LeftRunID)
	s.Equal(runA2, diff.RightRunID)
	s.True(hasRunDiffChange(diff.ParamChanges, "params.flavor", "vanilla", "chocolate"),
		"run-level param diff should include params.flavor, got %+v", diff.ParamChanges)
	task := requireRunDiffTask(s, diff.Tasks, "render")
	s.Equal("RERAN", task.Verdict)
	s.True(hasRunDiffChange(task.Changes, "runParams.flavor", "vanilla", "chocolate"),
		"task diff should include runParams.flavor, got %+v", task.Changes)

	// A 200 RunDiff body from /runs/diff proves the static route is not being
	// shadowed by /runs/:run_id treating "diff" as a run id.
	s.NotEmpty(diff.Tasks)

	s.requireGETStatus(http.StatusBadRequest, fmt.Sprintf("/v1/jobs/%s/runs/diff?right=%s", jobA.ID, runA2))
	s.requireGETStatus(http.StatusBadRequest, fmt.Sprintf("/v1/jobs/%s/runs/diff?left=%s", jobA.ID, runA1))

	aliasB := fmt.Sprintf("e2e-rundiff-b-%d", time.Now().UnixNano())
	dirB := s.writeJobManifest(runDiffManifest(aliasB))
	defer os.RemoveAll(dirB)
	s.runCLI("job", "apply", "--path", dirB, "--server", s.caesiumURL)
	jobB := s.requireJobByAlias(aliasB)
	s.Require().NotNil(jobB)

	runB := s.triggerRunWithParams(jobB.ID, map[string]string{"flavor": "foreign"})
	s.Require().Equal("succeeded", s.awaitRun(jobB.ID, runB, runTimeout).Status)

	// The auth-disabled integration server cannot exercise a scoped API-key leak
	// variant; A4 owns that auth-enabled harness. This covers the load-bearing
	// handler-level same-job check on both query-param run IDs.
	s.requireGETStatus(http.StatusNotFound, runDiffPath(jobA.ID, runB, runA1))
	s.requireGETStatus(http.StatusNotFound, runDiffPath(jobA.ID, runA1, runB))
}

func (s *IntegrationTestSuite) TestBlameRESTEndpointCoversHTTPSurface() {
	alias := fmt.Sprintf("e2e-blame-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(blameManifestV1(alias))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	time.Sleep(10 * time.Millisecond)
	s.overwriteJobManifest(dir, blameManifestV2(alias))
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	foreignAlias := fmt.Sprintf("e2e-blame-foreign-%d", time.Now().UnixNano())
	foreignDir := s.writeJobManifest(foreignBlameManifest(foreignAlias))
	defer os.RemoveAll(foreignDir)
	s.runCLI("job", "apply", "--path", foreignDir, "--server", s.caesiumURL)

	var history topologyHistoryResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/topology/history", job.ID), &history)
	s.Require().Len(history.Snapshots, 2, "topology change should write a second dag_snapshot")

	var result blameRESTResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/blame", job.ID), &result)
	s.Equal(job.ID, result.JobID)
	s.Equal("topology+image+command", result.Coverage)
	s.False(hasBlameTask(result.Tasks, "foreign-only"), "blame response should be scoped to the path job")

	publish := requireBlameTask(s, result.Tasks, "publish")
	extract := requireBlameTask(s, result.Tasks, "extract")
	s.NotEmpty(publish.SnapshotID)
	s.NotEmpty(extract.SnapshotID)
	s.NotEqual(extract.SnapshotID, publish.SnapshotID, "new task should be attributed to the later snapshot")
	s.Equal(newestSnapshotIDWithTask(history.Snapshots, "publish"), publish.SnapshotID)
	s.Equal(oldestSnapshotIDWithTask(history.Snapshots, "extract"), extract.SnapshotID)

	var filtered blameRESTResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/blame?task=publish", job.ID), &filtered)
	s.Require().Len(filtered.Tasks, 1)
	s.Equal("publish", filtered.Tasks[0].Element.Name)
	for _, edge := range filtered.Edges {
		s.True(edge.Element.From == "publish" || edge.Element.To == "publish",
			"task filter should only return adjacent edges, got %+v", edge.Element)
	}

	// Unknown task filters currently return an empty, job-scoped result; unknown
	// commit ranges are the handler's 404 path.
	var missingTask blameRESTResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/blame?task=does-not-exist", job.ID), &missingTask)
	s.Empty(missingTask.Tasks)
	s.Empty(missingTask.Edges)
	s.requireGETStatus(http.StatusNotFound, fmt.Sprintf("/v1/jobs/%s/blame?from=does-not-exist", job.ID))

	// Direct REST apply does not stamp GitCommit today, so the malformed
	// from>to range is reachable here only if a future harness adds commit
	// provenance to these applies. C4 owns the mandatory commit-stamped CLI case.
	if len(history.Snapshots) == 2 &&
		history.Snapshots[0].GitCommit != "" &&
		history.Snapshots[1].GitCommit != "" &&
		history.Snapshots[0].GitCommit != history.Snapshots[1].GitCommit {
		s.requireGETStatus(http.StatusBadRequest, fmt.Sprintf(
			"/v1/jobs/%s/blame?from=%s&to=%s",
			job.ID,
			url.QueryEscape(history.Snapshots[0].GitCommit),
			url.QueryEscape(history.Snapshots[1].GitCommit),
		))
	}
}

func runDiffManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache:
    enabled: true
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: render
    image: alpine:3.23
    command: ["sh","-c","echo flavor=$CAESIUM_PARAM_FLAVOR"]
`, alias)
}

func blameManifestV1(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh","-c","echo extract"]
    next: load
  - name: load
    image: alpine:3.23
    command: ["sh","-c","echo load"]
    dependsOn: extract
`, alias)
}

func blameManifestV2(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh","-c","echo extract"]
    next: load
  - name: load
    image: alpine:3.23
    command: ["sh","-c","echo load"]
    dependsOn: extract
    next: publish
  - name: publish
    image: alpine:3.23
    command: ["sh","-c","echo publish"]
    dependsOn: load
`, alias)
}

func foreignBlameManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: foreign-only
    image: alpine:3.23
    command: ["sh","-c","echo foreign"]
`, alias)
}

func (s *IntegrationTestSuite) overwriteJobManifest(dir, manifest string) {
	s.T().Helper()
	path := filepath.Join(dir, "job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(manifest))), 0o644))
}

func (s *IntegrationTestSuite) getRunDiff(jobID, leftRunID, rightRunID string) runDiffRESTResponse {
	s.T().Helper()
	var diff runDiffRESTResponse
	s.getJSON(runDiffPath(jobID, leftRunID, rightRunID), &diff)
	return diff
}

func runDiffPath(jobID, leftRunID, rightRunID string) string {
	q := url.Values{}
	q.Set("left", leftRunID)
	q.Set("right", rightRunID)
	return fmt.Sprintf("/v1/jobs/%s/runs/diff?%s", jobID, q.Encode())
}

func (s *IntegrationTestSuite) requireGETStatus(want int, path string) {
	s.T().Helper()
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Equal(want, resp.StatusCode, string(body))
	if resp.StatusCode == http.StatusOK {
		s.True(json.Valid(body), "200 response should be parseable JSON: %s", string(body))
	}
}

func hasRunDiffChange(changes []runDiffFieldChange, field, before, after string) bool {
	for _, change := range changes {
		if change.Field == field && change.Before == before && change.After == after {
			return true
		}
	}
	return false
}

func requireRunDiffTask(s *IntegrationTestSuite, tasks []runDiffTask, name string) runDiffTask {
	s.T().Helper()
	for _, task := range tasks {
		if task.TaskName == name {
			return task
		}
	}
	s.T().Fatalf("run diff task %q not found in %+v", name, tasks)
	return runDiffTask{}
}

func requireBlameTask(s *IntegrationTestSuite, tasks []blameTaskResult, name string) blameTaskResult {
	s.T().Helper()
	for _, task := range tasks {
		if task.Element.Name == name {
			return task
		}
	}
	s.T().Fatalf("blame task %q not found in %+v", name, tasks)
	return blameTaskResult{}
}

func hasBlameTask(tasks []blameTaskResult, name string) bool {
	for _, task := range tasks {
		if task.Element.Name == name {
			return true
		}
	}
	return false
}

func newestSnapshotIDWithTask(snaps []topologySnapshot, taskName string) string {
	for _, snap := range snaps {
		for _, task := range snap.Tasks {
			if task.Name == taskName {
				return snap.ID
			}
		}
	}
	return ""
}

func oldestSnapshotIDWithTask(snaps []topologySnapshot, taskName string) string {
	for i := len(snaps) - 1; i >= 0; i-- {
		for _, task := range snaps[i].Tasks {
			if task.Name == taskName {
				return snaps[i].ID
			}
		}
	}
	return ""
}
