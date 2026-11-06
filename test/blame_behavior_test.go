//go:build integration

package test

import (
	"fmt"
	"strings"
	"time"
)

// TestBlameCLIAttributesStepBehaviorAndRecordsRef drives #404 through the real
// CLI and REST surface: behavior-only edits (env, retries) write a snapshot and
// are attributed to the editing commit, an unchanged re-apply writes nothing,
// literal env values never reach the descriptor, snapshots carry a monotonic
// sequence, and the apply's git ref is surfaced per row.
func (s *IntegrationTestSuite) TestBlameCLIAttributesStepBehaviorAndRecordsRef() {
	alias := fmt.Sprintf("e2e-blame-behavior-%d", time.Now().UnixNano())
	const (
		commit1       = "blame-behavior-commit-1"
		commit2       = "blame-behavior-commit-2"
		commit3       = "blame-behavior-commit-3"
		literalSecret = "s3cr3t-literal-do-not-leak"
	)

	jobID := s.applyBlameManifestWithCommit(alias, blameBehaviorManifest(alias, 1, "one", literalSecret), commit1)

	// An unchanged re-apply (new commit) must not write a snapshot: the
	// behavior descriptor is deterministic through the real apply path.
	s.applyBlameManifestWithCommit(alias, blameBehaviorManifest(alias, 1, "one", literalSecret), commit2)
	s.Require().Len(s.blameTopologyHistory(jobID).Snapshots, 1, "an identical re-apply must dedup")

	// A retries-only edit.
	s.applyBlameManifestWithCommit(alias, blameBehaviorManifest(alias, 3, "one", literalSecret), commit3)
	history := s.blameTopologyHistory(jobID)
	s.Require().Len(history.Snapshots, 2, "a retries-only edit must write a snapshot")
	// newest first
	s.Greater(history.Snapshots[0].Sequence, history.Snapshots[1].Sequence, "sequence must be monotonic in insertion order")
	s.Greater(history.Snapshots[1].Sequence, int64(0))

	res := s.runBlameJSON(alias)
	s.Equal("topology+image+command+step-spec", res.Coverage)
	extract := requireBlameTask(s, res.Tasks, "extract")
	load := requireBlameTask(s, res.Tasks, "load")
	s.Equal(commit3, extract.IntroducingCommit, "a retries edit is attributed to the editing commit")
	s.Equal(commit1, load.IntroducingCommit, "an untouched task stays with its first commit")
	s.EqualValues(3, extract.Element.Behavior["retries"])
	s.Equal("refs/heads/integration", extract.Ref, "git ref of the introducing apply")
	s.Equal("refs/heads/integration", load.Ref)

	// An env-only edit is attributed too, and the literal value is digested.
	s.applyBlameManifestWithCommit(alias, blameBehaviorManifest(alias, 3, "two", literalSecret), commit2+"-env")
	res = s.runBlameJSON(alias)
	extract = requireBlameTask(s, res.Tasks, "extract")
	s.Equal(commit2+"-env", extract.IntroducingCommit)
	env, ok := extract.Element.Behavior["env"].(map[string]any)
	s.Require().True(ok, "behavior.env must be present: %+v", extract.Element.Behavior)
	s.Equal("secret://env/BLAME_TOKEN", env["REF_TOKEN"], "a secret reference is kept verbatim")
	s.True(strings.HasPrefix(fmt.Sprint(env["LITERAL_TOKEN"]), "sha256:"), "a literal env value is digested: %v", env)
	s.True(strings.HasPrefix(fmt.Sprint(env["PLAIN"]), "sha256:"), "a literal env value is digested: %v", env)

	raw, err := s.runCLIStdout("blame", alias, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.NotContains(raw, literalSecret, "a literal env value must never appear in blame output")
	s.NotContains(s.blameHistoryRaw(jobID), literalSecret, "...nor in the topology history")

	table, err := s.runCLIStdout("blame", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium blame table failed:\n%s", table)
	s.Contains(table, "step-level behavior")
	s.Contains(table, "Job-level taskTimeout/runTimeout/sla changes are not tracked")
	s.Contains(table, "SPEC")
	s.Contains(table, "refs/heads/integration")
	s.NotContains(table, literalSecret)

	// REST surface returns the same attribution and ref.
	var rest blameRESTResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/blame", jobID), &rest)
	s.Equal("topology+image+command+step-spec", rest.Coverage)
	s.Equal(commit2+"-env", requireBlameTask(s, rest.Tasks, "extract").IntroducingCommit)
	s.Equal("refs/heads/integration", requireBlameTask(s, rest.Tasks, "extract").Ref)
}

func (s *IntegrationTestSuite) blameHistoryRaw(jobID string) string {
	s.T().Helper()
	var raw map[string]any
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/topology/history", jobID), &raw)
	return fmt.Sprint(raw)
}

func blameBehaviorManifest(alias string, retries int, plain, literal string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh","-c","echo extract"]
    retries: %d
    env:
      PLAIN: %s
      LITERAL_TOKEN: %s
      REF_TOKEN: secret://env/BLAME_TOKEN
    next: load
  - name: load
    image: alpine:3.23
    command: ["sh","-c","echo load"]
    dependsOn: extract
`, alias, retries, plain, literal)
}
