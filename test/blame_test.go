//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func (s *IntegrationTestSuite) TestBlameCLIAttributionAndCommitRanges() {
	alias := fmt.Sprintf("e2e-blame-cli-%d", time.Now().UnixNano())
	const (
		commit1 = "blame-cli-commit-1"
		commit2 = "blame-cli-commit-2"
		commit3 = "blame-cli-commit-3"
		commit4 = "blame-cli-commit-4"
		commit5 = "blame-cli-commit-5"
		commit6 = "blame-cli-commit-6"
	)

	jobID := s.applyBlameManifestWithCommit(alias, blameCLIV1(alias), commit1)
	s.applyBlameManifestWithCommit(alias, blameCLIV2(alias), commit2)

	history := s.blameTopologyHistory(jobID)
	s.Require().Len(history.Snapshots, 2, "topology addition should write a second dag_snapshot")

	full := s.runBlameJSON(alias)
	s.Equal(jobID, full.JobID)
	s.Equal("topology+image+command", full.Coverage)
	s.Equal(commit1, requireBlameTask(s, full.Tasks, "extract").IntroducingCommit)
	s.Equal(commit1, requireBlameTask(s, full.Tasks, "load").IntroducingCommit)
	s.Equal(commit2, requireBlameTask(s, full.Tasks, "publish").IntroducingCommit)
	s.Equal(commit1, requireBlameEdge(s, full.Edges, "extract", "load").IntroducingCommit)
	s.Equal(commit2, requireBlameEdge(s, full.Edges, "load", "publish").IntroducingCommit)

	// Mandatory commit-range gate: exercise operator-facing --from/--to commit
	// filters through the CLI against distinct GitCommit values stamped on apply.
	laterOnly := s.runBlameJSON(alias, "--from", commit2, "--to", commit2)
	s.Equal(commit2, laterOnly.FromCommit)
	s.Equal(commit2, laterOnly.ToCommit)
	s.True(hasBlameTask(laterOnly.Tasks, "publish"), "range covering only commit2 should include the added task")
	s.False(hasBlameTask(laterOnly.Tasks, "extract"), "range covering only commit2 should exclude unchanged commit1 task")
	s.False(hasBlameTask(laterOnly.Tasks, "load"), "range covering only commit2 should exclude unchanged commit1 task")
	s.True(hasBlameEdge(laterOnly.Edges, "load", "publish"), "range covering only commit2 should include the added edge")
	s.False(hasBlameEdge(laterOnly.Edges, "extract", "load"), "range covering only commit2 should exclude unchanged commit1 edge")

	// Behavior-only edits are outside blame coverage. They update live runtime
	// behavior but must not write a dag_snapshot or silently move attribution.
	beforeBehaviorOnly := len(history.Snapshots)
	s.applyBlameManifestWithCommit(alias, blameCLIBehaviorOnly(alias), commit3)
	afterBehaviorOnly := s.blameTopologyHistory(jobID)
	s.Len(afterBehaviorOnly.Snapshots, beforeBehaviorOnly, "env-only change must not write a dag_snapshot")
	afterBehavior := s.runBlameJSON(alias)
	s.Equal("topology+image+command", afterBehavior.Coverage)
	s.Equal(commit1, requireBlameTask(s, afterBehavior.Tasks, "extract").IntroducingCommit)
	s.Equal(commit2, requireBlameTask(s, afterBehavior.Tasks, "publish").IntroducingCommit)
	table, err := s.runCLIStdout("blame", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium blame table failed:\n%s", table)
	s.Contains(table, "Coverage: topology + image + command only")
	s.Contains(table, "env/spec/retries/cache/schema/sla/triggerRules changes are not tracked")

	// Delete-and-readd: the current descriptor is blamed on the re-adding
	// snapshot, not its earliest-ever appearance.
	s.applyBlameManifestWithCommit(alias, blameCLIDeletePublish(alias), commit4)
	s.applyBlameManifestWithCommit(alias, blameCLIV2(alias), commit5)
	afterReadd := s.runBlameJSON(alias)
	s.Equal(commit5, requireBlameTask(s, afterReadd.Tasks, "publish").IntroducingCommit)
	s.Equal(commit5, requireBlameEdge(s, afterReadd.Edges, "load", "publish").IntroducingCommit)

	readdOnly := s.runBlameJSON(alias, "--from", commit5, "--to", commit5)
	s.True(hasBlameTask(readdOnly.Tasks, "publish"), "range covering only re-add commit should include publish")
	s.False(hasBlameTask(readdOnly.Tasks, "extract"), "range covering only re-add commit should exclude unchanged extract")
	s.True(hasBlameEdge(readdOnly.Edges, "load", "publish"), "range covering only re-add commit should include re-added edge")

	// Same-name descriptor mutation: changing load's image+command produces a
	// new descriptor and must be blamed on the mutating commit.
	s.applyBlameManifestWithCommit(alias, blameCLIMutatedLoad(alias), commit6)
	finalHistory := s.blameTopologyHistory(jobID)
	s.Require().Len(finalHistory.Snapshots, 5, "addition, delete, re-add, and image/command mutation should produce five snapshots total")
	mutated := s.runBlameJSON(alias)
	load := requireBlameTask(s, mutated.Tasks, "load")
	s.Equal(commit6, load.IntroducingCommit)
	s.Equal("busybox:1.36.1", load.Element.Image)
	s.Equal([]string{"sh", "-c", "echo load-mutated"}, load.Element.Command)
	s.Equal(commit1, requireBlameTask(s, mutated.Tasks, "extract").IntroducingCommit)
}

func (s *IntegrationTestSuite) applyBlameManifestWithCommit(alias, manifest, commit string) string {
	s.T().Helper()
	time.Sleep(10 * time.Millisecond)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI(
		"job", "apply",
		"--path", dir,
		"--server", s.caesiumURL,
		"--provenance-source-id", "integration-blame-cli",
		"--provenance-repo", "https://example.invalid/caesium-integration.git",
		"--provenance-ref", "refs/heads/integration",
		"--provenance-commit", commit,
		"--provenance-path", "jobs/"+alias+".job.yaml",
	)

	return s.requireJobByAlias(alias).ID
}

func (s *IntegrationTestSuite) runBlameJSON(job string, extraArgs ...string) blameRESTResponse {
	s.T().Helper()
	args := []string{"blame", job, "--json", "--server", s.caesiumURL}
	args = append(args, extraArgs...)
	out, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium blame --json failed:\n%s", out)
	s.Require().True(json.Valid([]byte(out)), "caesium blame --json stdout was not valid JSON (log contamination?):\n%s", out)
	var res blameRESTResponse
	s.Require().NoError(json.Unmarshal([]byte(out), &res))
	return res
}

func (s *IntegrationTestSuite) blameTopologyHistory(jobID string) topologyHistoryResponse {
	s.T().Helper()
	var history topologyHistoryResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/topology/history", jobID), &history)
	return history
}

func requireBlameEdge(s *IntegrationTestSuite, edges []blameEdgeResult, from, to string) blameEdgeResult {
	s.T().Helper()
	for _, edge := range edges {
		if edge.Element.From == from && edge.Element.To == to {
			return edge
		}
	}
	s.T().Fatalf("blame edge %q -> %q not found in %+v", from, to, edges)
	return blameEdgeResult{}
}

func hasBlameEdge(edges []blameEdgeResult, from, to string) bool {
	for _, edge := range edges {
		if edge.Element.From == from && edge.Element.To == to {
			return true
		}
	}
	return false
}

func blameCLIV1(alias string) string {
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

func blameCLIV2(alias string) string {
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

func blameCLIBehaviorOnly(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    env:
      BLAME_BEHAVIOR_ONLY: changed
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

func blameCLIDeletePublish(alias string) string {
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

func blameCLIMutatedLoad(alias string) string {
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
    image: busybox:1.36.1
    command: ["sh","-c","echo load-mutated"]
    dependsOn: extract
    next: publish
  - name: publish
    image: alpine:3.23
    command: ["sh","-c","echo publish"]
    dependsOn: load
`, alias)
}
