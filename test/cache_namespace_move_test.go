//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// TestCacheNamespaceMove drives manifest syntax, apply/export, persisted run
// ownership and cache separation through the real CLI and REST server. It also
// runs in the distributed lane, so both executor hash-input paths are exercised.
func (s *IntegrationTestSuite) TestCacheNamespaceMove() {
	alias := fmt.Sprintf("integration-namespace-cache-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  namespace: marketing
  cache: true
  replaySafe: false
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
	s.runCLI("job", "lint", "--path", dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.assertJobNamespace(job.ID, "marketing", "")

	first := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Require().Equal("succeeded", first.Status)
	s.Equal("marketing", first.Namespace)
	s.Equal("succeeded", s.taskStatusesByName(job.ID, first)["deterministic"])
	second := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Require().Equal("succeeded", second.Status)
	s.Equal("marketing", second.Namespace)
	s.Equal("cached", s.taskStatusesByName(job.ID, second)["deterministic"], "prove a real hit before the move")
	s.Equal(1, second.CacheHits)

	// CLI stdout is captured separately: the export must be an uncontaminated,
	// parseable manifest carrying namespace, and reapplying it must round-trip.
	exported, stderr, err := s.runCLISeparate("job", "export", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "export failed: %s", stderr)
	definition, err := schema.Parse([]byte(exported))
	s.Require().NoError(err)
	s.Equal("marketing", definition.Metadata.Namespace)
	exportedDir := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(exportedDir, alias+".job.yaml"), []byte(exported), 0o644))
	s.runCLI("job", "apply", "--path", exportedDir, "--server", s.caesiumURL)
	again, stderr, err := s.runCLISeparate("job", "export", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "re-export failed: %s", stderr)
	s.Equal(exported, again)
	s.assertNoDiffForAlias(alias, definition)

	movedDefinition := *definition
	movedDefinition.Metadata.Namespace = "finance"
	movedYAML, err := yaml.Marshal(&movedDefinition)
	s.Require().NoError(err)
	// Export already carries the actual runtime engine; write it directly so
	// injectEngine cannot add a duplicate key on Podman or Kubernetes lanes.
	movedDir := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(movedDir, "job.yaml"), movedYAML, 0o644))
	s.assertNamespaceMoveDiff(alias, movedDir, "marketing", "finance")
	s.runCLI("job", "apply", "--path", movedDir, "--server", s.caesiumURL)
	s.Equal(job.ID, s.requireJobByAlias(alias).ID, "a move preserves the job identity")
	s.assertJobNamespace(job.ID, "finance", "marketing")
	for _, oldID := range []string{first.ID, second.ID} {
		var historical runResponse
		s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", job.ID, oldID), &historical)
		s.Equal("marketing", historical.Namespace, "historical ownership must stay with its original namespace")
	}
	// Moving the job requires execution in the new namespace; an unsafe
	// historical baseline must refuse rather than reuse the old owner's output.
	beforeReplay := len(s.fetchRuns(job.ID))
	replayKey := alias + "-unsafe-move"
	refused := s.postReplay(job.ID, first.ID, &replayKey, `{"set":{}}`)
	s.Require().Equal(http.StatusUnprocessableEntity, refused.status, refused.body)
	var refusal struct {
		Message string `json:"message"`
	}
	s.Require().NoError(json.Unmarshal([]byte(refused.body), &refusal))
	s.Require().Contains(refusal.Message, "not replay safe")
	s.Require().Contains(refusal.Message, `namespace changed from "marketing" to "finance"`)
	s.Require().Contains(refusal.Message, "requires re-execution")
	s.Require().Len(s.fetchRuns(job.ID), beforeReplay, "unsafe historical replay must not materialize any run")
	s.Require().Equal("marketing", s.fetchRun(job.ID, first.ID).Namespace)
	third := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Require().Equal("succeeded", third.Status)
	s.Equal("finance", third.Namespace)
	s.Equal(0, third.CacheHits, "the prior owner's cached result must not be reused")
	s.Equal("succeeded", s.taskStatusesByName(job.ID, third)["deterministic"])
	why := s.parseWhy(job.ID, third.ID, "deterministic")
	s.Require().NotNil(why.Diff)
	found := false
	for _, change := range why.Diff.Changes {
		if change.Field == "namespace" {
			found = true
		}
	}
	s.True(found, "the persisted hash decomposition must explain the namespace miss")
	fourth := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Require().Equal("succeeded", fourth.Status)
	s.Equal("finance", fourth.Namespace)
	s.Equal("cached", s.taskStatusesByName(job.ID, fourth)["deterministic"])
	s.Equal(1, fourth.CacheHits)

	// Omitting namespace is itself a move from finance to the default owner.
	defaultDefinition := movedDefinition
	defaultDefinition.Metadata.Namespace = ""
	defaultYAML, err := yaml.Marshal(&defaultDefinition)
	s.Require().NoError(err)
	s.Require().NotContains(string(defaultYAML), "namespace:")
	defaultDir := s.T().TempDir()
	s.Require().NoError(os.WriteFile(filepath.Join(defaultDir, "job.yaml"), defaultYAML, 0o644))
	s.assertNamespaceMoveDiff(alias, defaultDir, "finance", "default")
	s.runCLI("job", "apply", "--path", defaultDir, "--server", s.caesiumURL)
	s.assertJobNamespace(job.ID, "default", "finance")
	s.Require().Equal("finance", s.fetchRun(job.ID, fourth.ID).Namespace)
	s.assertNoDiffForAlias(alias, &defaultDefinition)
}

func (s *IntegrationTestSuite) assertNamespaceMoveDiff(alias, dir, from, to string) {
	s.T().Helper()
	type update struct {
		Alias string `json:"alias"`
		Diff  string `json:"diff"`
	}
	assertUpdate := func(modified []update) {
		s.Require().Len(modified, 1, "a namespace-only edit must be a visible update")
		s.Require().Equal(alias, modified[0].Alias)
		s.Require().Contains(modified[0].Diff, "Namespace")
		s.Require().Contains(modified[0].Diff, fmt.Sprintf("%q", from))
		s.Require().Contains(modified[0].Diff, fmt.Sprintf("%q", to))
	}
	stdout, stderr, err := s.runCLISeparate("job", "diff", "--path", dir, "--json", "--server", s.caesiumURL)
	s.Require().Error(err, "namespace-only diff must exit nonzero: stderr=%s stdout=%s", stderr, stdout)
	s.Require().Equal(1, jobDiffCLIExitCode(err))
	var cli struct {
		Added    []json.RawMessage `json:"added"`
		Modified []update          `json:"modified"`
		Removed  []json.RawMessage `json:"removed"`
	}
	s.Require().NoError(json.Unmarshal([]byte(stdout), &cli), "CLI stdout must be clean JSON")
	s.Require().Empty(cli.Added)
	s.Require().Empty(cli.Removed)
	assertUpdate(cli.Modified)

	contents, err := os.ReadFile(filepath.Join(dir, "job.yaml"))
	s.Require().NoError(err)
	definition, err := schema.Parse(contents)
	s.Require().NoError(err)
	payload, err := json.Marshal(map[string]any{"definitions": []*schema.Definition{definition}})
	s.Require().NoError(err)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/diff", bytes.NewReader(payload))
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	var rest struct {
		Added    []json.RawMessage `json:"added"`
		Modified []update          `json:"modified"`
	}
	s.Require().NoError(json.Unmarshal(body, &rest))
	s.Require().Empty(rest.Added)
	assertUpdate(rest.Modified)
}

func (s *IntegrationTestSuite) assertJobNamespace(jobID, namespace, latestNamespace string) {
	s.T().Helper()
	var job struct {
		Namespace string `json:"namespace"`
		LatestRun *struct {
			Namespace string `json:"namespace"`
		} `json:"latest_run"`
	}
	s.getJSON("/v1/jobs/"+jobID, &job)
	s.Equal(namespace, job.Namespace)
	if latestNamespace != "" {
		s.Require().NotNil(job.LatestRun)
		s.Equal(latestNamespace, job.LatestRun.Namespace)
	}
}

// Legacy POST and backfill creation are distinct constructors from manifest
// apply and ordinary run admission; exercise their real surface as well.
func (s *IntegrationTestSuite) TestNamespaceLegacyPostAndBackfill() {
	alias := fmt.Sprintf("integration-namespace-legacy-%d", time.Now().UnixNano())
	payload := map[string]any{
		"alias":    alias,
		"metadata": map[string]any{"namespace": "marketing"},
		"trigger":  map[string]any{"type": "cron", "configuration": map[string]any{"cron": "0 0 1 1 *", "timezone": "UTC"}},
		"tasks":    []any{map[string]any{"atom": map[string]any{"engine": s.engineType, "image": "alpine:3.23", "command": []string{"sh", "-c", "echo legacy namespace"}}}},
	}
	encoded, err := json.Marshal(payload)
	s.Require().NoError(err)
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobs", bytes.NewReader(encoded))
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusCreated, resp.StatusCode)
	var created struct {
		ID        string `json:"id"`
		Namespace string `json:"namespace"`
	}
	s.Require().NoError(json.NewDecoder(resp.Body).Decode(&created))
	s.Equal("marketing", created.Namespace)
	run := s.awaitRun(created.ID, s.triggerRun(created.ID), runTimeout)
	s.Require().Equal("succeeded", run.Status)
	s.Equal("marketing", run.Namespace)
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	backfill := s.createBackfill(created.ID, start, start.Add(time.Minute), 1, "all")
	s.Equal("marketing", backfill.Namespace)
	completed := s.awaitBackfill(created.ID, backfill.ID, runTimeout)
	s.Require().Equal("succeeded", completed.Status)
	s.Equal(1, completed.CompletedRuns)
	s.Equal("marketing", completed.Namespace)
	var backfillRunID string
	for _, historical := range s.listJobRunSummaries(created.ID) {
		if historical.BackfillID == backfill.ID {
			backfillRunID = historical.ID
			s.Equal("marketing", historical.Namespace)
		}
	}
	s.Require().NotEmpty(backfillRunID)
	moved := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata: {alias: %s, namespace: finance}
trigger: {type: cron, configuration: {cron: "0 0 1 1 *", timezone: UTC}}
steps:
  - name: legacy
    image: alpine:3.23
    command: ["sh", "-c", "echo moved namespace"]
`, alias)
	dir := s.writeJobManifest(moved)
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.assertJobNamespace(created.ID, "finance", "marketing")
	var preserved backfillResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/backfills/%s", created.ID, backfill.ID), &preserved)
	s.Equal("marketing", preserved.Namespace)
	var oldRun runResponse
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", created.ID, backfillRunID), &oldRun)
	s.Equal("marketing", oldRun.Namespace)
}

func (s *IntegrationTestSuite) TestNamespaceLegacyPostDefaultsAndRejectsInvalid() {
	for _, namespace := range []string{"", "Invalid_Name"} {
		alias := fmt.Sprintf("integration-namespace-validation-%d", time.Now().UnixNano())
		payload := map[string]any{"alias": alias, "metadata": map[string]any{"namespace": namespace}, "trigger": map[string]any{"type": "http", "configuration": map[string]any{"path": "/hooks/" + alias}}, "tasks": []any{map[string]any{"atom": map[string]any{"engine": s.engineType, "image": "alpine:3.23", "command": []string{"sh", "-c", "echo default namespace"}}}}}
		encoded, err := json.Marshal(payload)
		s.Require().NoError(err)
		resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobs", bytes.NewReader(encoded))
		s.Require().NoError(err)
		if namespace == "" {
			s.Require().Equal(http.StatusCreated, resp.StatusCode)
			var created struct {
				Namespace string `json:"namespace"`
			}
			s.Require().NoError(json.NewDecoder(resp.Body).Decode(&created))
			s.Equal("default", created.Namespace)
		} else {
			s.Equal(http.StatusBadRequest, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
