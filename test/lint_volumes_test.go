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

	// The server-side contract-enforcement gate (internal/contract/derive.go
	// ListContractJobs) unions this lint request's definitions with EVERY job
	// already applied on the server and can report a "breaking" finding for
	// any of them, unrelated to this fixture — other integration suites
	// intentionally exercise breaking-contract scenarios against this same
	// shared server. `job lint --server` therefore may legitimately exit
	// non-zero here for a reason that has nothing to do with the volume
	// warning under test. The rendered response (including our warning) is
	// still written to stdout before that unrelated gate runs, so assert on
	// stdout content directly and only tolerate that one documented failure
	// mode — anything else (e.g. the server being unreachable) still fails
	// the test loudly.
	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	if err != nil {
		s.Require().Contains(err.Error(), "breaking contract finding", "unexpected server lint failure: %v", err)
	}
	s.Contains(serverStdout, "Warnings:")
	s.Contains(serverStdout, `"shared"`)
	s.Contains(serverStdout, "writer-one")
	s.Contains(serverStdout, "writer-two")
}

// TestLintVolumesWarnsOnRootVsSubPathWriters regression-guards containment:
// a mount with no subPath exposes the ENTIRE volume, so it still conflicts
// with a sibling step that only write-mounts a subPath of it (e.g. the
// shipped docs/examples/k8s-workload-identity-volume.job.yaml shape before
// its fix) — overlap is not decided by subPath string equality.
func (s *IntegrationTestSuite) TestLintVolumesWarnsOnRootVsSubPathWriters() {
	alias := fmt.Sprintf("integration-lint-volumes-root-vs-subpath-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, rootVsSubPathVolumeManifest(alias))
	defer os.RemoveAll(dir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.Contains(stdout, "Warnings:")
	s.Contains(stdout, `"shared"`)
	s.Contains(stdout, "writer-root")
	s.Contains(stdout, "writer-reports")
}

// TestLintVolumesSilentOnDisjointSiblingSubPaths regression-guards the
// genuinely-disjoint two-writer case Open Question 2 anticipates: two
// sibling subPaths that share no ancestor/descendant relationship never
// overlap on disk and must not warn.
func (s *IntegrationTestSuite) TestLintVolumesSilentOnDisjointSiblingSubPaths() {
	alias := fmt.Sprintf("integration-lint-volumes-disjoint-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, disjointSiblingSubPathVolumeManifest(alias))
	defer os.RemoveAll(dir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(stdout, "Warnings:")
}

// TestLintVolumesWarnsOnRawMountTypeVolume covers the low-level mounts:
// [{type: volume, source: <name>}] mechanism (container.Spec.Mounts),
// which bypasses the job-level volumes:/volumeMounts: abstraction entirely.
func (s *IntegrationTestSuite) TestLintVolumesWarnsOnRawMountTypeVolume() {
	alias := fmt.Sprintf("integration-lint-volumes-raw-mount-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, rawMountTypeVolumeManifest(alias))
	defer os.RemoveAll(dir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.Contains(stdout, "Warnings:")
	s.Contains(stdout, `"caesium-lint-raw-volume-test"`)
	s.Contains(stdout, "writer-one")
	s.Contains(stdout, "writer-two")
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

func rootVsSubPathVolumeManifest(alias string) string {
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

func disjointSiblingSubPathVolumeManifest(alias string) string {
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
  - name: writer-a
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: a
  - name: writer-b
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: b
`, alias)
}

func rawMountTypeVolumeManifest(alias string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    expression: "0 * * * *"
steps:
  - name: writer-one
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    mounts:
      - type: volume
        source: caesium-lint-raw-volume-test
        target: /data
  - name: writer-two
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    mounts:
      - type: volume
        source: caesium-lint-raw-volume-test
        target: /other
`, alias)
}
