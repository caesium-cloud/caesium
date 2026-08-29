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
