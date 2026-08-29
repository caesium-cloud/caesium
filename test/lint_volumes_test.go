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
// the silent-case assertions immune to the CONTRACT warnings block, which
// reuses the same header (cmd/job/lint.go's renderServerContractSummary) and
// can be non-empty for any non-breaking contract finding the linted job
// participates in.
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

	// Server lint must exit clean here. Graph derivation
	// (internal/contract/derive.go ListContractJobs) still unions this
	// request's definitions with EVERY job already applied on the shared
	// server — other suites intentionally leave breaking pairs behind — but
	// the findings are now scoped to contracts the linted jobs participate in
	// (#362), so an unrelated pair can no longer fail this lint. A non-zero
	// exit is a real failure, not something to tolerate.
	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
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
	s.Require().NoError(err, serverStdout)
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

// TestLintVolumesDockerHonoursSubPath is the post-#370 contract driven
// through the real CLI (issue #366 fix round 1): internal/atom/docker's
// convertMounts now maps VolumeMount.SubPath onto
// mount.VolumeOptions.Subpath (PR #370, closes #361), so
// internal/jobdef/lint/volumes.go no longer special-cases the docker engine
// — subPath containment now follows Docker's actual adapter behavior. Two docker
// steps declaring genuinely disjoint sibling subPaths ("a" vs "b") do not
// overlap and stay silent, mirroring
// TestLintVolumesSilentOnDisjointSiblingSubPaths; two docker steps sharing
// the SAME subPath still contend and are flagged, proving this is real
// containment on docker now, not merely "docker ignores subPath" going
// silent for the wrong reason. This test replaces
// TestLintVolumesWarnsOnDockerSubPathWriters, which pinned the pre-#370
// behavior this fix removes.
func (s *IntegrationTestSuite) TestLintVolumesDockerHonoursSubPath() {
	disjointAlias := fmt.Sprintf("integration-lint-volumes-docker-subpath-disjoint-%d", time.Now().UnixNano())
	disjointDir := s.writeLintVolumesManifestVerbatim(disjointAlias, dockerSubPathVolumeManifest(disjointAlias))
	defer os.RemoveAll(disjointDir)

	disjointStdout, err := s.runCLIStdout("job", "lint", "--path", disjointDir)
	s.Require().NoError(err)
	s.NotContains(disjointStdout, "Warnings:")

	sameAlias := fmt.Sprintf("integration-lint-volumes-docker-subpath-same-%d", time.Now().UnixNano())
	sameDir := s.writeLintVolumesManifestVerbatim(sameAlias, dockerSameSubPathVolumeManifest(sameAlias))
	defer os.RemoveAll(sameDir)

	sameStdout, err := s.runCLIStdout("job", "lint", "--path", sameDir)
	s.Require().NoError(err)
	s.Contains(sameStdout, "Warnings:")
	s.Contains(sameStdout, `"shared"`)
	s.Contains(sameStdout, "writer-a")
	s.Contains(sameStdout, "writer-b")
}

// TestLintVolumesPodmanNamedVolumeHonoursSubPath drives the Podman
// named-volume branch through both lint surfaces. Podman's runtime adapter
// passes SubPath to specgen.NamedVolume, so sibling regions stay disjoint.
func (s *IntegrationTestSuite) TestLintVolumesPodmanNamedVolumeHonoursSubPath() {
	alias := fmt.Sprintf("integration-lint-volumes-podman-volume-subpath-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifestVerbatim(alias, subPathSiblingVolumeManifest(alias, "podman"))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(localStdout, volumeWarningMarker)

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.NotContains(serverStdout, volumeWarningMarker)
}

// TestLintVolumesPodmanBindSubPathsStillWarn covers the source-sensitive
// negative case. Podman's bind adapter mounts the entire source and does not
// apply VolumeMount.SubPath, so two declared sibling regions still overlap.
func (s *IntegrationTestSuite) TestLintVolumesPodmanBindSubPathsStillWarn() {
	alias := fmt.Sprintf("integration-lint-volumes-podman-bind-subpath-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifestVerbatim(alias, podmanBindSubPathVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.Contains(localStdout, volumeWarningMarker)
	s.Contains(localStdout, `"shared"`)
	s.Contains(localStdout, "writer-a")
	s.Contains(localStdout, "writer-b")

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.Contains(serverStdout, volumeWarningMarker)
	s.Contains(serverStdout, `"shared"`)
	s.Contains(serverStdout, "writer-a")
	s.Contains(serverStdout, "writer-b")
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
// the suite's engine injection. Fixtures that explicitly pin an engine must
// not have a second `engine:` spliced in — duplicate mapping keys are a YAML
// decode error.
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

// kubernetesSubPathVolumeManifest and dockerSubPathVolumeManifest pin the two
// engines independently to prove sibling subPaths use the same non-overlap
// semantics on both.
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

// dockerSameSubPathVolumeManifest is dockerSubPathVolumeManifest's
// same-region counterpart: both writers declare the identical subPath, so
// they still contend on any engine that honours subPath — proving docker's
// post-#370 subPath containment is real containment, not merely silence.
func dockerSameSubPathVolumeManifest(alias string) string {
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
    engine: docker
    command: ["sh", "-c", "true"]
    next: [writer-a, writer-b]
  - name: writer-a
    image: alpine:3.23
    engine: docker
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: shared-region
  - name: writer-b
    image: alpine:3.23
    engine: docker
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: shared-region
`, alias)
}

func podmanBindSubPathVolumeManifest(alias string) string {
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
      podman:
        bind: /tmp/caesium-lint-volumes-podman-bind
steps:
  - name: seed
    image: alpine:3.23
    engine: podman
    command: ["sh", "-c", "true"]
    next: [writer-a, writer-b]
  - name: writer-a
    image: alpine:3.23
    engine: podman
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: a
  - name: writer-b
    image: alpine:3.23
    engine: podman
    command: ["sh", "-c", "true"]
    dependsOn: [seed]
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
