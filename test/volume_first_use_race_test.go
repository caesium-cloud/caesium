//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// volumeRaceConsumers is how many sibling steps first-mount the shared volume
// at the same time. Two is enough to lose the race (that is exactly what
// caesium#443 caught in CI, with propose-a/propose-b); four widens the window
// without making the scenario meaningfully slower, since the steps run
// concurrently.
const volumeRaceConsumers = 4

// TestSharedNamedVolumeConcurrentFirstUse is the runtime regression test for
// caesium#443: concurrent tasks first-mounting the same named volume.
//
// Podman creates a missing named volume inside its container-create call, and
// that create is not concurrency-safe — the existence check and the state
// write that registers the volume are not atomic. Two sibling tasks that both
// mount a volume nothing has created yet could therefore both find it absent,
// both try to create it, and the loser's whole container create would fail:
//
//	creating named volume "x": adding volume to state: name "x" is in use: volume already exists
//
// One sibling succeeded, the other failed the run, and an existing shared
// volume is not a failure — it is the outcome both tasks wanted. The engine
// now creates named volumes itself before the container, and tolerates losing
// that create to a concurrent winner once it has verified the volume is really
// there.
//
// What makes this scenario able to observe the bug at all is the FRESH volume
// name: it is unique per test run, so nothing has created it before the run
// starts and every consumer is genuinely a first-mounter. A volume whose name
// is stable across runs only races on the very first run against a given
// daemon, which is why the failure in #443 surfaced as an intermittent CI red
// rather than a reproducible one. Being a race, a single green run does not
// prove the fix — but a red one always proved the bug, and the deterministic
// half of the guard lives in internal/atom/podman/engine_test.go.
//
// It runs on docker as well as podman. Docker's daemon already treats a
// named-volume create during container create as idempotent, so the docker
// lane is a free guard that the engine-side pre-create did not break the
// shared-volume contract on the engine that never had the bug.
func (s *IntegrationTestSuite) TestSharedNamedVolumeConcurrentFirstUse() {
	if s.engineType == "kubernetes" {
		s.T().Skipf("named volumes are a docker/podman concept; the kubernetes engine binds PVCs instead (engine=%s)", s.engineType)
	}

	alias := fmt.Sprintf("integration-volume-race-%d", time.Now().UnixNano())
	// Unique per run: no prior run can have created it, so every consumer
	// below first-mounts a volume that does not exist yet.
	volume := alias + "-vol"

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
        volume: %[2]s
      podman:
        volume: %[2]s
steps:
`, alias, volume)

	// Every consumer is a DAG root with no dependsOn, so they are all
	// dispatched together — the concurrency the race needs. Each writes its
	// own marker file so the verifier can prove all of them really got the
	// same volume rather than silently getting separate storage.
	//
	// They mount the volume ROOT rather than disjoint subPaths on purpose.
	// `caesium job lint`'s CheckVolumeWriters would warn about that (parallel
	// write mounts with overlapping regions) — but the whole-volume mount IS
	// the shape under test: it is what makes every sibling a first-mounter of
	// the same volume, which is what the engine used to lose the creation race
	// on. There is no actual write conflict, since each one writes a filename
	// only it uses, and `job apply` does not run that lint.
	for i := 1; i <= volumeRaceConsumers; i++ {
		manifest += fmt.Sprintf(`  - name: consume-%[1]d
    image: alpine:3.23
    command: ["sh", "-c", "echo marker-%[1]d > /data/marker-%[1]d.txt"]
    volumeMounts:
      - volume: shared
        path: /data
    next: [verify]
`, i)
	}

	manifest += `  - name: verify
    image: alpine:3.23
    dependsOn: [`
	for i := 1; i <= volumeRaceConsumers; i++ {
		if i > 1 {
			manifest += ", "
		}
		manifest += fmt.Sprintf("consume-%d", i)
	}
	manifest += `]
    command: ["sh", "-c", 'set -e; seen=$(cat /data/marker-*.txt | sort | tr "\n" ","); echo "##caesium::output {\"seen\": \"$seen\"}"']
    volumeMounts:
      - volume: shared
        path: /data
        readOnly: true
`

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	statuses := s.taskStatusesByName(job.ID, run)

	// The load-bearing assertion: EVERY concurrent first-mounter succeeds.
	// Before the fix the loser of the volume-creation race failed here with
	// "volume already exists" while its siblings succeeded.
	for i := 1; i <= volumeRaceConsumers; i++ {
		step := fmt.Sprintf("consume-%d", i)
		s.Equal("succeeded", statuses[step],
			"%s first-mounts the shared volume concurrently with its siblings; "+
				"losing the volume-creation race must not fail the task (statuses: %v)",
			step, statuses)
	}
	s.Require().Equal("succeeded", run.Status, "run should succeed; task statuses: %v", statuses)
	s.Equal("succeeded", statuses["verify"])

	// And the volume they raced over must be ONE shared volume: the verifier
	// sees every sibling's marker, so the race was resolved by sharing the
	// winner's volume rather than by handing anyone separate storage.
	expected := ""
	for i := 1; i <= volumeRaceConsumers; i++ {
		expected += fmt.Sprintf("marker-%d,", i)
	}
	outputs := s.taskOutputsByName(job.ID, run)
	s.Equal(expected, outputs["verify"]["seen"],
		"every concurrent consumer must have written into the same shared volume")
}
