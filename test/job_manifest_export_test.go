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
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// TestJobManifestExportRoundTrip drives issue #407's acceptance property
// through the REAL surfaces: apply a docs example with the CLI, export it back
// with `caesium job export` (stdout captured SEPARATELY from stderr), then
// prove the exported bytes are re-appliable — `caesium job lint` accepts them,
// POST /v1/jobdefs/diff (the same jobdiff.Compare `caesium job diff` runs, but
// against the live server) reports no create/modify for the alias, and
// re-applying + re-exporting yields byte-identical YAML.
//
// The fixtures are deliberately diverse: named volumes with read-only mounts,
// Kubernetes workload identity + a PVC-backed volume with subPath, and a
// secret:// env pipeline with five volumes, concurrency and explicit DAG edges.
func (s *IntegrationTestSuite) TestJobManifestExportRoundTrip() {
	for _, fixture := range []string{
		"volume-artifacts.job.yaml",
		"k8s-workload-identity-volume.job.yaml",
		"infra-drift.job.yaml",
	} {
		s.Run(fixture, func() {
			s.assertManifestRoundTrip(fixture)
		})
	}
}

func (s *IntegrationTestSuite) assertManifestRoundTrip(fixture string) {
	s.T().Helper()

	alias, sourceDir := s.stageExampleManifest(fixture)
	defer os.RemoveAll(sourceDir)

	s.runCLI("job", "apply", "--path", sourceDir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// 1. `caesium job export <alias>` writes the manifest to stdout and NOTHING
	//    else, so `caesium job export x > job.yaml` is byte-exact. Streams are
	//    captured separately: a merged capture would hide a log line landing on
	//    stdout (see CLAUDE.md "End-to-end coverage is the gate").
	stdout, stderr, err := s.runCLISeparate("job", "export", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "job export failed: %s", stderr)
	s.Require().NotEmpty(stdout)
	s.Require().True(strings.HasPrefix(stdout, "apiVersion: v1\n"),
		"stdout must start with the manifest, got:\n%s", stdout)
	s.NotContains(stderr, "apiVersion")

	exported, err := schema.Parse([]byte(stdout))
	s.Require().NoError(err, "exported manifest must parse and validate:\n%s", stdout)
	s.Equal(alias, exported.Metadata.Alias)

	// 2. The same manifest is served by the endpoint, as application/yaml.
	body, contentType := s.getRaw(fmt.Sprintf("/v1/jobs/%s/manifest", job.ID))
	s.Equal("application/yaml", strings.SplitN(contentType, ";", 2)[0])
	s.Equal(stdout, string(body), "CLI stdout must be the endpoint body verbatim")

	// 3. ?format=json serves the same definition for API/browser callers.
	jsonBody, jsonContentType := s.getRaw(fmt.Sprintf("/v1/jobs/%s/manifest?format=json", job.ID))
	s.Contains(jsonContentType, "application/json")
	var asJSON schema.Definition
	s.Require().NoError(json.Unmarshal(jsonBody, &asJSON))
	s.Equal(alias, asJSON.Metadata.Alias)
	s.Len(asJSON.Steps, len(exported.Steps))

	// 4. --output writes the identical bytes to a file and keeps stdout empty.
	outDir, err := os.MkdirTemp("", "caesium-export-out-*")
	s.Require().NoError(err)
	defer os.RemoveAll(outDir)
	outPath := filepath.Join(outDir, alias+".job.yaml")

	outStdout, outStderr, err := s.runCLISeparate("job", "export", alias, "--output", outPath, "--server", s.caesiumURL)
	s.Require().NoError(err, "job export --output failed: %s", outStderr)
	s.Empty(outStdout, "--output must leave stdout empty")
	written, err := os.ReadFile(outPath)
	s.Require().NoError(err)
	s.Equal(stdout, string(written))

	// 5. `caesium job lint` accepts the exported file.
	lintOut, lintErr, err := s.runCLISeparate("job", "lint", "--path", outPath)
	s.Require().NoError(err, "job lint rejected the exported manifest: %s\n%s", lintOut, lintErr)
	s.Contains(lintOut, "Validated 1 job definition(s)")

	// 6. Diffing the exported manifest against the server it came from reports
	//    no change for this alias. (Deletes are ignored: the server holds every
	//    other test's job, and a single-file diff necessarily lists those.)
	s.assertNoDiffForAlias(alias, exported)

	// 7. It genuinely re-applies: apply the exported file, export again, and the
	//    bytes must be identical — no field drifting in or out on a round trip.
	s.runCLI("job", "apply", "--path", outPath, "--server", s.caesiumURL)
	second, secondErr, err := s.runCLISeparate("job", "export", alias, "--server", s.caesiumURL)
	s.Require().NoError(err, "re-export failed: %s", secondErr)
	s.Equal(stdout, second, "export must be idempotent across a re-apply")
}

// stageExampleManifest copies a docs example into a temp dir under a unique
// alias (and a unique HTTP trigger path, when it has one) so the scenario never
// collides with `just hydrate` or another test's jobs.
func (s *IntegrationTestSuite) stageExampleManifest(fixture string) (string, string) {
	s.T().Helper()

	data, err := os.ReadFile(filepath.Join(s.projectRoot, "docs", "examples", fixture))
	s.Require().NoError(err)

	var doc map[string]any
	s.Require().NoError(yaml.Unmarshal(data, &doc))

	metadata, ok := doc["metadata"].(map[string]any)
	s.Require().True(ok, "fixture %s has no metadata block", fixture)
	base, ok := metadata["alias"].(string)
	s.Require().True(ok, "fixture %s has no metadata.alias", fixture)

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("%s-export-%d", base, suffix)
	metadata["alias"] = alias

	if trigger, ok := doc["trigger"].(map[string]any); ok {
		if cfg, ok := trigger["configuration"].(map[string]any); ok {
			if _, hasPath := cfg["path"]; hasPath {
				cfg["path"] = fmt.Sprintf("/hooks/export-round-trip/%d", suffix)
			}
		}
	}

	encoded, err := yaml.Marshal(doc)
	s.Require().NoError(err)

	dir, err := os.MkdirTemp("", "caesium-export-src-*")
	s.Require().NoError(err)
	s.Require().NoError(os.WriteFile(filepath.Join(dir, alias+".job.yaml"), encoded, 0o644))
	return alias, dir
}

// assertNoDiffForAlias posts the exported definition to POST /v1/jobdefs/diff —
// the server-side arm of `caesium job diff` (both call jobdiff.Compare over
// jobdiff.LoadDatabaseSpecs) — and requires the alias to be neither added nor
// modified.
func (s *IntegrationTestSuite) assertNoDiffForAlias(alias string, def *schema.Definition) {
	s.T().Helper()

	payload, err := json.Marshal(map[string]any{"definitions": []*schema.Definition{def}})
	s.Require().NoError(err)

	// doJSONRequest, not doRequest: echo's Bind needs the Content-Type, and
	// without it the controller answers 400 "bad request" before it ever reads
	// the definitions.
	resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/diff", bytes.NewReader(payload))
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	var diff struct {
		Added []struct {
			Alias string `json:"alias"`
		} `json:"added"`
		Modified []struct {
			Alias string `json:"alias"`
			Diff  string `json:"diff"`
		} `json:"modified"`
	}
	s.Require().NoError(json.Unmarshal(body, &diff))

	for _, entry := range diff.Added {
		s.Failf("exported manifest reported as a create", "alias %s", entry.Alias)
	}
	for _, entry := range diff.Modified {
		if entry.Alias == alias {
			s.Failf("exported manifest reported as an update", "alias %s:\n%s", entry.Alias, entry.Diff)
		}
	}
}

// getRaw fetches a path and returns the body plus its Content-Type, for
// endpoints that do not answer JSON.
func (s *IntegrationTestSuite) getRaw(path string) ([]byte, string) {
	s.T().Helper()

	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))
	return body, resp.Header.Get("Content-Type")
}

// TestJobManifestExportRejectsUnknownFormat pins the endpoint's input
// validation: only yaml and json are served.
func (s *IntegrationTestSuite) TestJobManifestExportRejectsUnknownFormat() {
	alias, dir := s.stageExampleManifest("minimal.job.yaml")
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	resp, err := s.doRequest(http.MethodGet, fmt.Sprintf("%s/v1/jobs/%s/manifest?format=toml", s.caesiumURL, job.ID), nil)
	s.Require().NoError(err)
	defer func() { _ = resp.Body.Close() }()
	s.Equal(http.StatusBadRequest, resp.StatusCode)
}
