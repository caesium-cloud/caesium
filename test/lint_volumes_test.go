//go:build integration

package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// volumeWarningMarker is the volume-specific half of the multi-writer warning
// message. Asserting on it (rather than on the bare "Warnings:" header) keeps
// the silent-case assertions immune to the shared server's CONTRACT warnings
// block, which reuses the same header (cmd/job/lint.go's
// renderServerLintResponse) and can be non-empty because of jobs other suites
// applied to this same server.
const volumeWarningMarker = "is mounted read-write by steps"

// TestLintVolumesWarnsOnParallelWriters drives the "two read-write mounts on
// one volume" lint check (spec §8) through its real surfaces — the local CLI
// path and the server-side lint endpoint via `caesium job lint --server` —
// asserting the warning names both the volume and the offending steps on
// clean stdout (runCLIStdout, never the stream-merging runCLIRaw: a warning
// leaking to stderr instead of stdout would be exactly the kind of hollow
// wiring this check exists to catch).
//
// The two writers hang off a `seed` fan-out on purpose. The check flags
// CONCURRENT writers only, and a definition in which no step declares an
// explicit edge is auto-linked into a sequential chain by pkg/jobdef — which
// would order the pair and silence the warning for a reason that has nothing
// to do with the volume.
func (s *IntegrationTestSuite) TestLintVolumesWarnsOnParallelWriters() {
	alias := fmt.Sprintf("integration-lint-volumes-two-writer-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, parallelWriterVolumeManifest(alias))
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

// TestLintVolumesSilentOnDAGOrderedWriters is the refinement this check
// exists for, driven through both real surfaces: two steps that both write
// the whole volume but are separated by a dependsOn edge can never run at the
// same time, so the volume is a handoff (the infrastructure-deployment
// pattern's prepare → checkout and plan → apply pairs), not a race.
func (s *IntegrationTestSuite) TestLintVolumesSilentOnDAGOrderedWriters() {
	alias := fmt.Sprintf("integration-lint-volumes-ordered-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, orderedWriterVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(localStdout, "Warnings:")

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	if err != nil {
		s.Require().Contains(err.Error(), "breaking contract finding", "unexpected server lint failure: %v", err)
	}
	s.NotContains(serverStdout, volumeWarningMarker)
}

// TestLintVolumesWarnsOnRootVsSubPathWriters regression-guards containment:
// a mount with no subPath exposes the ENTIRE volume, so it still conflicts
// with a parallel step that only write-mounts a subPath of it — overlap is
// not decided by subPath string equality, on any engine.
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

// TestLintVolumesSilentOnDisjointSiblingSubPaths covers the
// genuinely-disjoint two-writer case Open Question 2 anticipates: two sibling
// subPaths that share no ancestor/descendant relationship never overlap on
// disk — on an engine that actually applies subPath. The fixture pins
// `engine: kubernetes` itself (and is written verbatim, without the suite's
// engine injection) so the assertion means the same thing on every lane.
func (s *IntegrationTestSuite) TestLintVolumesSilentOnDisjointSiblingSubPaths() {
	alias := fmt.Sprintf("integration-lint-volumes-disjoint-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifestVerbatim(alias, kubernetesSubPathVolumeManifest(alias))
	defer os.RemoveAll(dir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(stdout, "Warnings:")
}

// TestLintVolumesWarnsOnDockerSubPathWriters is the docker caveat driven
// through the real CLI: internal/atom/docker's convertMounts never sets
// VolumeOptions.Subpath, so two parallel docker steps declaring different
// subPaths of one named volume both see the whole volume and genuinely
// contend. The same fixture is silent on kubernetes (the test above), which
// is what makes this an engine-awareness assertion rather than a restatement
// of the containment rule.
func (s *IntegrationTestSuite) TestLintVolumesWarnsOnDockerSubPathWriters() {
	alias := fmt.Sprintf("integration-lint-volumes-docker-subpath-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifestVerbatim(alias, dockerSubPathVolumeManifest(alias))
	defer os.RemoveAll(dir)

	stdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.Contains(stdout, "Warnings:")
	s.Contains(stdout, `"shared"`)
	s.Contains(stdout, "writer-a")
	s.Contains(stdout, "writer-b")
	s.Contains(stdout, "docker engine ignores subPath")
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
// multi-writer volume warning. docs/examples/infra-deploy.job.yaml and
// infra-drift.job.yaml earn that the honest way — one logical volume per
// physical store, shared only between DAG-ordered writers — not by aliasing
// one store under two names to hide it from the check.
func (s *IntegrationTestSuite) TestLintVolumesExamplesProduceNoWarning() {
	stdout, err := s.runCLIStdout("job", "lint", "--path", filepath.Join(s.projectRoot, "docs", "examples"))
	s.Require().NoError(err)
	s.NotContains(stdout, "Warnings:")
}

func (s *IntegrationTestSuite) writeLintVolumesManifest(alias, manifest string) string {
	s.T().Helper()
	return s.writeLintVolumesManifestVerbatim(alias, s.injectEngine(manifest))
}

// writeLintVolumesManifestVerbatim writes a fixture exactly as given, skipping
// the suite's engine injection. Fixtures whose expected outcome depends on the
// engine (subPath is applied by kubernetes/podman and dropped by docker) pin
// their own `engine:` and must not have a second one spliced in — duplicate
// mapping keys are a YAML decode error.
func (s *IntegrationTestSuite) writeLintVolumesManifestVerbatim(alias, manifest string) string {
	s.T().Helper()

	dir, err := os.MkdirTemp("", "caesium-lint-volumes-*")
	s.Require().NoError(err)
	path := filepath.Join(dir, alias+".job.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(strings.TrimSpace(manifest)), 0o644))
	return dir
}

func parallelWriterVolumeManifest(alias string) string {
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
  - name: seed
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    next: [writer-one, writer-two]
  - name: writer-one
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
  - name: writer-two
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
`, alias)
}

func orderedWriterVolumeManifest(alias string) string {
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
  - name: prepare
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    next: [checkout]
    volumeMounts:
      - volume: shared
        path: /data
  - name: checkout
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [prepare]
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
  - name: seed
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    next: [writer-root, writer-reports]
  - name: writer-root
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
  - name: writer-reports
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: reports
`, alias)
}

// kubernetesSubPathVolumeManifest and dockerSubPathVolumeManifest are the same
// two parallel writers of two sibling subPaths, differing only in `engine:` —
// which is exactly the difference the check has to see.
func kubernetesSubPathVolumeManifest(alias string) string {
	return subPathSiblingVolumeManifest(alias, "kubernetes")
}

func dockerSubPathVolumeManifest(alias string) string {
	return subPathSiblingVolumeManifest(alias, "docker")
}

func subPathSiblingVolumeManifest(alias, engine string) string {
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
  - name: seed
    image: alpine:3.23
    engine: %s
    command: ["sh", "-c", "true"]
    next: [writer-a, writer-b]
  - name: writer-a
    image: alpine:3.23
    engine: %s
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: a
  - name: writer-b
    image: alpine:3.23
    engine: %s
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: b
`, alias, engine, engine, engine)
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
  - name: seed
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    next: [writer-one, writer-two]
  - name: writer-one
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    mounts:
      - type: volume
        source: caesium-lint-raw-volume-test
        target: /data
  - name: writer-two
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    mounts:
      - type: volume
        source: caesium-lint-raw-volume-test
        target: /other
`, alias)
}
