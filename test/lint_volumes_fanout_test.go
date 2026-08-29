//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// This file is issue #366's integration coverage: a fanOut step's own N
// partition instances may write a shared volume concurrently, but the pairwise
// multi-writer check (test/lint_volumes_test.go) compares different steps
// and never checks a step against itself, so a fanned writer was invisible
// to `caesium job lint` before checkFanOutSelfWriters
// (internal/jobdef/lint/volumes.go). It is a sibling to, and deliberately
// separate from, lint_volumes_test.go (owned by #362) — see that file for
// the shared writeLintVolumesManifest/injectEngine helpers this one reuses.

// fanOutSelfWriterMarker is the fan-out-specific half of the multi-writer
// warning message (distinct from the sibling file's volumeWarningMarker,
// which matches the pairwise "by steps that are not all pairwise ordered"
// phrasing). Asserting on it rather than the bare "Warnings:" header keeps
// these assertions immune to the shared server's CONTRACT warnings block,
// which reuses the same header (cmd/job/lint.go's renderServerLintResponse).
const fanOutSelfWriterMarker = "is mounted read-write by fanned step"

// TestLintVolumesFanoutWarnsOnSelfWriter drives the fan-out self-conflict
// finding through the real CLI surface — the local path and the server-side
// lint endpoint via `--server` — asserting the warning names the volume, the
// fanned step, and its multiplicity on clean stdout (runCLIStdout, never the
// stream-merging runCLIRaw).
func (s *IntegrationTestSuite) TestLintVolumesFanoutWarnsOnSelfWriter() {
	alias := fmt.Sprintf("integration-lint-volumes-fanout-writer-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, fanOutSelfWriterVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.Contains(localStdout, fanOutSelfWriterMarker)
	s.Contains(localStdout, `"shared"`)
	s.Contains(localStdout, "process")
	s.Contains(localStdout, "fanOut")
	s.Contains(localStdout, "N≤4")

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.Contains(serverStdout, fanOutSelfWriterMarker)
	s.Contains(serverStdout, `"shared"`)
	s.Contains(serverStdout, "process")
}

// TestLintVolumesFanoutSilentOnReadOnlyMount proves a readOnly: true mount on
// a fanOut step is not treated as a writer, mirroring the non-fanned readOnly
// case (TestLintVolumesWarnsOnParallelWriters's silent counterpart), on both
// the local path and the server-side lint endpoint.
func (s *IntegrationTestSuite) TestLintVolumesFanoutSilentOnReadOnlyMount() {
	alias := fmt.Sprintf("integration-lint-volumes-fanout-reader-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, fanOutReadOnlyVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(localStdout, fanOutSelfWriterMarker)

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.NotContains(serverStdout, fanOutSelfWriterMarker)
}

// TestLintVolumesFanoutSilentWhenSerialized proves fanOut.maxParallel: 1 is
// the within-run writable-mount escape hatch: every partition may write the
// shared volume, but no second instance from that run can hold it concurrently. Drive
// both lint surfaces so YAML decoding, local lint, server lint, and stdout
// rendering all agree on the absence of this specific warning.
func (s *IntegrationTestSuite) TestLintVolumesFanoutSilentWhenSerialized() {
	alias := fmt.Sprintf("integration-lint-volumes-fanout-serialized-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, fanOutSerializedWriterVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(localStdout, fanOutSelfWriterMarker)

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.NotContains(serverStdout, fanOutSelfWriterMarker)
}

// TestLintVolumesFanoutSilentOnPerInstanceScratch proves source resolution is
// part of the real lint wiring. Each lane selects private scratch storage —
// tmpfs on Docker/Podman or claimTemplate on Kubernetes — so the shared
// manifest alias does not represent shared physical bytes.
func (s *IntegrationTestSuite) TestLintVolumesFanoutSilentOnPerInstanceScratch() {
	alias := fmt.Sprintf("integration-lint-volumes-fanout-scratch-%d", time.Now().UnixNano())
	dir := s.writeLintVolumesManifest(alias, fanOutPerInstanceVolumeManifest(alias))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(localStdout, fanOutSelfWriterMarker)

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.NotContains(serverStdout, fanOutSelfWriterMarker)
}

func fanOutSelfWriterVolumeManifest(alias string) string {
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
        volume: caesium-lint-volumes-fanout-test
      podman:
        volume: caesium-lint-volumes-fanout-test
      kubernetes:
        pvc: caesium-lint-volumes-fanout-test-rwx
steps:
  - name: discover
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [discover]
    fanOut:
      from: discover
      maxPartitions: 16
      maxParallel: 4
    volumeMounts:
      - volume: shared
        path: /data
`, alias)
}

func fanOutReadOnlyVolumeManifest(alias string) string {
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
        volume: caesium-lint-volumes-fanout-test
      podman:
        volume: caesium-lint-volumes-fanout-test
      kubernetes:
        pvc: caesium-lint-volumes-fanout-test-rwx
steps:
  - name: discover
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [discover]
    fanOut:
      from: discover
      maxPartitions: 16
      maxParallel: 4
    volumeMounts:
      - volume: shared
        path: /data
        readOnly: true
`, alias)
}

func fanOutSerializedWriterVolumeManifest(alias string) string {
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
        volume: caesium-lint-volumes-fanout-test
      podman:
        volume: caesium-lint-volumes-fanout-test
      kubernetes:
        pvc: caesium-lint-volumes-fanout-test-rwx
steps:
  - name: discover
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [discover]
    fanOut:
      from: discover
      maxPartitions: 16
      maxParallel: 1
    volumeMounts:
      - volume: shared
        path: /data
`, alias)
}

func fanOutPerInstanceVolumeManifest(alias string) string {
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
  - name: scratch
    sources:
      docker:
        tmpfs: {}
      podman:
        tmpfs: {}
      kubernetes:
        claimTemplate:
          size: 1Gi
steps:
  - name: discover
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\"]'"]
    next: [process]
  - name: process
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    dependsOn: [discover]
    fanOut:
      from: discover
      maxPartitions: 16
      maxParallel: 4
    volumeMounts:
      - volume: scratch
        path: /scratch
`, alias)
}
