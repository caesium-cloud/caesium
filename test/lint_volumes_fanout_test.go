//go:build integration

package test

import (
	"fmt"
	"os"
	"testing"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
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
	dir := s.writeLintVolumesManifest(alias, fanOutVolumeManifest(alias, fanOutVolumeOptions{
		volumeName:       "shared",
		dockerSource:     "volume: caesium-lint-volumes-fanout-test",
		podmanSource:     "volume: caesium-lint-volumes-fanout-test",
		kubernetesSource: "pvc: caesium-lint-volumes-fanout-test-rwx",
		mountPath:        "/data",
		maxParallel:      4,
	}))
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
	dir := s.writeLintVolumesManifest(alias, fanOutVolumeManifest(alias, fanOutVolumeOptions{
		volumeName:       "shared",
		dockerSource:     "volume: caesium-lint-volumes-fanout-test",
		podmanSource:     "volume: caesium-lint-volumes-fanout-test",
		kubernetesSource: "pvc: caesium-lint-volumes-fanout-test-rwx",
		mountPath:        "/data",
		readOnly:         true,
		maxParallel:      4,
	}))
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
	dir := s.writeLintVolumesManifest(alias, fanOutVolumeManifest(alias, fanOutVolumeOptions{
		volumeName:       "shared",
		dockerSource:     "volume: caesium-lint-volumes-fanout-test",
		podmanSource:     "volume: caesium-lint-volumes-fanout-test",
		kubernetesSource: "pvc: caesium-lint-volumes-fanout-test-rwx",
		mountPath:        "/data",
		maxParallel:      1,
	}))
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
	dir := s.writeLintVolumesManifest(alias, fanOutVolumeManifest(alias, fanOutVolumeOptions{
		volumeName:       "scratch",
		dockerSource:     "tmpfs: {}",
		podmanSource:     "tmpfs: {}",
		kubernetesSource: "claimTemplate:\n          size: 1Gi",
		mountPath:        "/scratch",
		maxParallel:      4,
	}))
	defer os.RemoveAll(dir)

	localStdout, err := s.runCLIStdout("job", "lint", "--path", dir)
	s.Require().NoError(err)
	s.NotContains(localStdout, fanOutSelfWriterMarker)

	serverStdout, err := s.runCLIStdout("job", "lint", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err, serverStdout)
	s.NotContains(serverStdout, fanOutSelfWriterMarker)
}

type fanOutVolumeOptions struct {
	volumeName       string
	dockerSource     string
	podmanSource     string
	kubernetesSource string
	mountPath        string
	readOnly         bool
	maxParallel      int
}

func fanOutVolumeManifest(alias string, opts fanOutVolumeOptions) string {
	readOnly := ""
	if opts.readOnly {
		readOnly = "        readOnly: true\n"
	}
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
  - name: %s
    sources:
      docker:
        %s
      podman:
        %s
      kubernetes:
        %s
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
      maxParallel: %d
    volumeMounts:
      - volume: %s
        path: %s
%s`, alias, opts.volumeName, opts.dockerSource, opts.podmanSource,
		opts.kubernetesSource, opts.maxParallel, opts.volumeName, opts.mountPath, readOnly)
}

func TestFanOutVolumeManifestOptions(t *testing.T) {
	tests := []struct {
		name            string
		opts            fanOutVolumeOptions
		wantDocker      schema.VolumeSource
		wantPodman      schema.VolumeSource
		wantKubernetes  schema.VolumeSource
		wantReadOnly    bool
		wantMaxParallel int
	}{
		{
			name: "shared writer",
			opts: fanOutVolumeOptions{
				volumeName: "shared", dockerSource: "volume: caesium-lint-volumes-fanout-test",
				podmanSource: "volume: caesium-lint-volumes-fanout-test", kubernetesSource: "pvc: caesium-lint-volumes-fanout-test-rwx",
				mountPath: "/data", maxParallel: 4,
			},
			wantDocker:      schema.VolumeSource{Volume: "caesium-lint-volumes-fanout-test"},
			wantPodman:      schema.VolumeSource{Volume: "caesium-lint-volumes-fanout-test"},
			wantKubernetes:  schema.VolumeSource{PVC: "caesium-lint-volumes-fanout-test-rwx"},
			wantMaxParallel: 4,
		},
		{
			name: "read-only shared reader",
			opts: fanOutVolumeOptions{
				volumeName: "shared", dockerSource: "volume: caesium-lint-volumes-fanout-test",
				podmanSource: "volume: caesium-lint-volumes-fanout-test", kubernetesSource: "pvc: caesium-lint-volumes-fanout-test-rwx",
				mountPath: "/data", readOnly: true, maxParallel: 4,
			},
			wantDocker:     schema.VolumeSource{Volume: "caesium-lint-volumes-fanout-test"},
			wantPodman:     schema.VolumeSource{Volume: "caesium-lint-volumes-fanout-test"},
			wantKubernetes: schema.VolumeSource{PVC: "caesium-lint-volumes-fanout-test-rwx"},
			wantReadOnly:   true, wantMaxParallel: 4,
		},
		{
			name: "serialized shared writer",
			opts: fanOutVolumeOptions{
				volumeName: "shared", dockerSource: "volume: caesium-lint-volumes-fanout-test",
				podmanSource: "volume: caesium-lint-volumes-fanout-test", kubernetesSource: "pvc: caesium-lint-volumes-fanout-test-rwx",
				mountPath: "/data", maxParallel: 1,
			},
			wantDocker:      schema.VolumeSource{Volume: "caesium-lint-volumes-fanout-test"},
			wantPodman:      schema.VolumeSource{Volume: "caesium-lint-volumes-fanout-test"},
			wantKubernetes:  schema.VolumeSource{PVC: "caesium-lint-volumes-fanout-test-rwx"},
			wantMaxParallel: 1,
		},
		{
			name: "per-instance scratch",
			opts: fanOutVolumeOptions{
				volumeName: "scratch", dockerSource: "tmpfs: {}", podmanSource: "tmpfs: {}",
				kubernetesSource: "claimTemplate:\n          size: 1Gi", mountPath: "/scratch", maxParallel: 4,
			},
			wantDocker:      schema.VolumeSource{Tmpfs: &schema.TmpfsSource{}},
			wantPodman:      schema.VolumeSource{Tmpfs: &schema.TmpfsSource{}},
			wantKubernetes:  schema.VolumeSource{ClaimTemplate: &schema.ClaimTemplate{Size: "1Gi"}},
			wantMaxParallel: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, err := schema.Parse([]byte(fanOutVolumeManifest("volume-options", tt.opts)))
			require.NoError(t, err)
			require.Len(t, def.Volumes, 1)
			volume := def.Volumes[0]
			require.Equal(t, tt.opts.volumeName, volume.Name)
			require.Equal(t, tt.wantDocker, volume.Sources[schema.EngineDocker])
			require.Equal(t, tt.wantPodman, volume.Sources[schema.EnginePodman])
			require.Equal(t, tt.wantKubernetes, volume.Sources[schema.EngineKubernetes])
			require.Len(t, def.Steps, 2)
			process := def.Steps[1]
			require.Equal(t, "process", process.Name)
			require.NotNil(t, process.FanOut)
			require.Equal(t, 16, process.FanOut.MaxPartitions)
			require.Equal(t, tt.wantMaxParallel, process.FanOut.MaxParallel)
			require.Len(t, process.VolumeMounts, 1)
			require.Equal(t, tt.opts.volumeName, process.VolumeMounts[0].Volume)
			require.Equal(t, tt.opts.mountPath, process.VolumeMounts[0].Path)
			require.Equal(t, tt.wantReadOnly, process.VolumeMounts[0].ReadOnly)
		})
	}
}
