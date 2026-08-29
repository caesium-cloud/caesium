//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// TestDockerVolumeSubPathIsolatesWrites is the runtime regression test for
// issue #361: the Docker engine used to ignore VolumeMount.SubPath entirely,
// so every mount of a named volume exposed the volume ROOT regardless of the
// declared subPath — two steps that believed they were writing to disjoint
// regions of one volume were actually writing the same file. Podman and
// Kubernetes already honoured SubPath; this proves Docker now does too.
//
// Two root steps (write-a, write-b) write the SAME filename
// ("marker.txt") into DIFFERENT subPaths ("a" and "b") of one shared named
// volume. Two downstream readers (verify-a, verify-b) then mount the SAME
// subPath their corresponding writer used and report what they see. If
// SubPath were ignored, every mount would resolve to the volume root: both
// writers would collide on one file and both readers would observe
// whichever write happened to land last — never a consistent from-a/from-b
// split. With SubPath honoured, verify-a always reads exactly what
// write-a wrote and never anything write-b wrote, and vice versa.
func (s *IntegrationTestSuite) TestDockerVolumeSubPathIsolatesWrites() {
	if s.engineType != "docker" {
		s.T().Skipf("docker VolumeMount.SubPath regression test targets the docker engine; engine=%s", s.engineType)
	}

	alias := fmt.Sprintf("integration-docker-subpath-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
volumes:
  - name: shared
    sources:
      docker:
        volume: %[1]s-vol
steps:
  - name: write-a
    image: alpine:3.23
    command: ["sh", "-c", "echo from-a > /data/marker.txt"]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: a
  - name: write-b
    image: alpine:3.23
    command: ["sh", "-c", "echo from-b > /data/marker.txt"]
    volumeMounts:
      - volume: shared
        path: /data
        subPath: b
  - name: verify-a
    image: alpine:3.23
    dependsOn: [write-a, write-b]
    command: ["sh", "-c", 'content=$(cat /data/marker.txt 2>/dev/null || echo MISSING); echo "##caesium::output {\"content\": \"$content\"}"']
    volumeMounts:
      - volume: shared
        path: /data
        subPath: a
        readOnly: true
  - name: verify-b
    image: alpine:3.23
    dependsOn: [write-a, write-b]
    command: ["sh", "-c", 'content=$(cat /data/marker.txt 2>/dev/null || echo MISSING); echo "##caesium::output {\"content\": \"$content\"}"']
    volumeMounts:
      - volume: shared
        path: /data
        subPath: b
        readOnly: true
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	statuses := s.taskStatusesByName(job.ID, run)
	s.Require().Equal("succeeded", run.Status, "run should succeed; task statuses: %v", statuses)

	s.Equal("succeeded", statuses["write-a"])
	s.Equal("succeeded", statuses["write-b"])
	s.Equal("succeeded", statuses["verify-a"])
	s.Equal("succeeded", statuses["verify-b"])

	outputs := s.taskOutputsByName(job.ID, run)
	s.Equal("from-a", outputs["verify-a"]["content"],
		"verify-a mounts subPath \"a\"; it must see only what write-a wrote, never write-b's file")
	s.Equal("from-b", outputs["verify-b"]["content"],
		"verify-b mounts subPath \"b\"; it must see only what write-b wrote, never write-a's file")
}

// TestDockerVolumeSubPathHelperCreatedDirIsWorldWritable is the runtime
// regression test for I-1 (fix round 1 on #361/PR #370): ensureVolumeSubPath's
// helper container runs as root, so the sub-directory it creates on a fresh
// volume used to come up root:root 0755 — a non-root step writing into that
// same subPath (any image that does not already bake the mount target with
// its own ownership, e.g. a "prepare"-style step running the stock
// alpine:3.23 image, per docs/infrastructure-deployment.md's "Volume
// ownership" section) got EACCES. The fix chmods a NEWLY created
// sub-directory 0777.
//
// This uses a fresh, never-before-mounted volume (so the helper is
// guaranteed to be the sub-directory's first toucher) and BusyBox's `su` to
// drop the write itself to Alpine's built-in "nobody" account (uid 65534) —
// the exact permission check a job step declaring a non-root container user
// would hit — without needing a job-schema field pkg/jobdef does not expose
// (no per-step `user:`, matching the Kubernetes securityContext gap already
// documented for PVCs) or any package install at run time: unlike an
// arbitrary uid such as 10001, "nobody" already resolves in Alpine's
// /etc/passwd, so `su` accepts it with no extra network dependency beyond
// the alpine:3.23 pull every other scenario in this suite already needs.
func (s *IntegrationTestSuite) TestDockerVolumeSubPathHelperCreatedDirIsWorldWritable() {
	if s.engineType != "docker" {
		s.T().Skipf("docker VolumeMount.SubPath regression test targets the docker engine; engine=%s", s.engineType)
	}

	alias := fmt.Sprintf("integration-docker-subpath-nonroot-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
volumes:
  - name: fresh
    sources:
      docker:
        volume: %[1]s-vol
steps:
  - name: write-nonroot
    image: alpine:3.23
    command: ["sh", "-c", 'set -e; su nobody -s /bin/sh -c "echo from-nonroot > /data/marker.txt"; content=$(cat /data/marker.txt); echo "##caesium::output {\"content\": \"$content\"}"']
    volumeMounts:
      - volume: fresh
        path: /data
        subPath: stack-nonroot
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	statuses := s.taskStatusesByName(job.ID, run)
	s.Require().Equal("succeeded", run.Status,
		"write-nonroot must succeed writing into its subPath as uid 65534 (nobody); a helper-created "+
			"sub-directory that is not world-writable fails this with EACCES; task statuses: %v", statuses)

	outputs := s.taskOutputsByName(job.ID, run)
	s.Equal("from-nonroot", outputs["write-nonroot"]["content"])
}
