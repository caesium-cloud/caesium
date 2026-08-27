//go:build integration

package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TestLintVolumesWarnsOnMultipleWriters drives the "two read-write mounts on
// one volume" lint check (spec §8) through its real surfaces — the local CLI
// path and the server-side lint endpoint via `caesium job lint --server` —
// asserting the warning names both the volume and the offending steps on
// clean stdout (runCLIStdout, never the stream-merging runCLIRaw: a warning
// leaking to stderr instead of stdout would be exactly the kind of hollow
// wiring this check exists to catch).
func (s *IntegrationTestSuite) TestLintVolumesWarnsOnMultipleWriters() {
	alias := fmt.Sprintf("integration-lint-volumes-two-writer-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, twoWriterVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.Contains(localStdout, "Warnings:")
	s.Contains(localStdout, `"shared"`)
	s.Contains(localStdout, "writer-one")
	s.Contains(localStdout, "writer-two")

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.Contains(serverStdout, "Warnings:")
	s.Contains(serverStdout, `"shared"`)
	s.Contains(serverStdout, "writer-one")
	s.Contains(serverStdout, "writer-two")
}

// TestLintVolumesSilentOnDisjointSubPaths regression-guards the subPath
// carve-out: two steps writing to distinct subPaths of the same volume are a
// legitimate two-writer case (spec §11 Open Question 2) and must not warn.
func (s *IntegrationTestSuite) TestLintVolumesSilentOnDisjointSubPaths() {
	alias := fmt.Sprintf("integration-lint-volumes-disjoint-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, disjointSubPathVolumeManifest(alias))
	defer os.RemoveAll(dir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(stdout, "Warnings:")
}

// TestLintVolumesExamplesProduceNoWarning guards the reference manifests
// operators copy from docs/examples: none of them should trip the
// multi-writer volume warning.
func (s *IntegrationTestSuite) TestLintVolumesExamplesProduceNoWarning() {
	stdout, err := s.runCLIStdout("job", "lint", "--path", filepath.Join(s.projectRoot, "docs", "examples"))
	s.Require().NoError(err)
	s.NotContains(stdout, "Warnings:")
}

func (s *IntegrationTestSuite) writeLintVolumesManifest(alias, manifest string) string {
	s.T().Helper()

	dir, err := os.MkdirTemp("", "caesium-lint-volumes-*")
	s.Require().NoError(err)
	path := filepath.Join(dir, alias+".job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(s.injectEngine(manifest))), 0o644))
	return dir
}

func twoWriterVolumeManifest(alias string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 * * * *"
volumes:
  - name: shared
    sources:
      docker:
        volume: caesium-lint-volumes-test
      podman:
        volume: caesium-lint-volumes-test
      kubernetes:
        pvc: caesium-lint-volumes-test-rwx
steps:
  - name: writer-one
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    volumeMounts:
      - volume: shared
        path: /data
  - name: writer-two
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    volumeMounts:
      - volume: shared
        path: /data
`, alias)
}

func disjointSubPathVolumeManifest(alias string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 * * * *"
volumes:
  - name: shared
    sources:
      docker:
        volume: caesium-lint-volumes-test
      podman:
        volume: caesium-lint-volumes-test
      kubernetes:
        pvc: caesium-lint-volumes-test-rwx
steps:
  - name: writer-root
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    volumeMounts:
      - volume: shared
        path: /data
  - name: writer-reports
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: reports
`, alias)
}
