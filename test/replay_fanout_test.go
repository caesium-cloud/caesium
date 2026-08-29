//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Quarantined replay of a baseline containing a fan-out group.
//
// The scenarios are named TestFanOut* deliberately: the distributed lane's
// `-run` filter selects `TestIntegrationTestSuite/(…|TestFanOut)`, and replay's
// re-execution path only exists in distributed mode (the local server refuses a
// replay that would re-execute with 409 "distributed execution mode"). Naming
// them for the replay surface alone would leave the executor half of this
// feature with no coverage at all — which is exactly how fanned replay came to
// need this change.
// ---------------------------------------------------------------------------

// replayFanOutManifest is a producer → fanned group → fan-in job that is
// replay-safe and cacheable end to end.
//
// Every command is deterministic. A timestamp anywhere would change a cached
// VALUE without changing its key, which makes "the replay reused the baseline"
// indistinguishable from "the replay re-ran everything" at the assertion level.
func replayFanOutManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  replaySafe: true
  cache: true
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
  - name: list
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        echo '##caesium::partitions [{"key":"alpha","fingerprint":"%s"},{"key":"bravo","fingerprint":"%s"},{"key":"charlie","dependsOn":["alpha"]}]'
    next: [process]
  - name: process
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        echo "partition=$CAESIUM_PARTITION"
        echo "##caesium::output {\"ROWS\":\"$CAESIUM_PARTITION\"}"
    dependsOn: [list]
    fanOut:
      from: list
      maxPartitions: 8
    next: [publish]
  - name: publish
    image: alpine:3.23
    command:
      - sh
      - -c
      - |
        echo "count=$CAESIUM_OUTPUT_PROCESS_PARTITION_COUNT"
    dependsOn: [process]
`, alias, fingerprintA, fingerprintB)
}

// replayFanOutBaseline applies the manifest and runs it once, returning the job
// and the baseline run whose group replay must reconstruct.
func (s *IntegrationTestSuite) replayFanOutBaseline(alias string) (*jobSummary, *runResponse) {
	s.T().Helper()

	dir := s.writeJobManifest(replayFanOutManifest(alias))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	baseline := s.awaitRun(job.ID, s.triggerRun(job.ID), runTimeout)
	s.Require().Equal("succeeded", baseline.Status, "baseline run failed: %s", baseline.Error)

	parts := s.expandedPartitions(s.listPartitions(job.ID, baseline.ID, "process"))
	s.Require().Len(parts, 3, "precondition: the baseline expands three instances: %v", partitionStatusMap(parts))
	return job, baseline
}

// TestFanOutReplayReExpandsGroupFromRecordedPartitions is the headline: a
// baseline containing a fan-out group replays instead of being refused with a
// 409, its group comes back at the recorded width with the recorded identities,
// and the producer is served from cache rather than re-run to rediscover the
// list.
func (s *IntegrationTestSuite) TestFanOutReplayReExpandsGroupFromRecordedPartitions() {
	alias := fmt.Sprintf("fanout-replay-recorded-%d", time.Now().UnixNano())
	job, baseline := s.replayFanOutBaseline(alias)

	key := "fanout-replay-" + time.Now().Format("150405.000000000")
	replay := s.mustPostReplay(job.ID, baseline.ID, &key, `{"set":{}}`)
	s.Require().NotEmpty(replay.RunID)
	s.True(replay.Quarantine)

	replayRun := s.awaitRun(job.ID, replay.RunID, runTimeout)
	s.Require().Equal("succeeded", replayRun.Status, "replay run failed: %s", replayRun.Error)
	s.True(replayRun.Quarantine, "a replay run is always quarantined")

	statuses := s.taskStatusesByName(job.ID, replayRun)
	s.Equal("cached", statuses["list"],
		"the producer must be REUSED, never re-run to rediscover the partition list: %v", statuses)
	s.Equal("cached", statuses["publish"],
		"the fan-in consumer must cache-hit, which it can only do if replay rebuilt the SAME group "+
			"aggregate (outputs and identity) the live run folded into its key: %v", statuses)

	parts := s.expandedPartitions(s.listPartitions(job.ID, replay.RunID, "process"))
	s.Require().Len(parts, 3,
		"the group must be re-materialized at its recorded width; a collapsed group means replay "+
			"never re-expanded from the producer's recorded list: %v", partitionStatusMap(parts))

	byValue := partitionsByValue(parts)
	for _, value := range []string{"alpha", "bravo", "charlie"} {
		instance, ok := byValue[value]
		s.Require().True(ok, "partition %s is missing from the replayed group: %v", value, partitionStatusMap(parts))
		s.Equal("cached", instance.Status,
			"partition %s must resolve from its own per-partition cache entry, which only matches if "+
				"replay folded the recorded partition identity into the instance's hash", value)
	}
	s.Equal(fingerprintA, byValue["alpha"].Fingerprint, "alpha lost the fingerprint recorded on the producer's descriptor")
	s.Equal(fingerprintB, byValue["bravo"].Fingerprint, "bravo lost the fingerprint recorded on the producer's descriptor")
	s.Equal([]string{"alpha"}, byValue["charlie"].DependsOn, "charlie lost the in-group ordering recorded on the producer's descriptor")
	s.Equal(0, byValue["alpha"].Index)
	s.Equal(1, byValue["bravo"].Index)
	s.Equal(2, byValue["charlie"].Index)
}

// TestFanOutReplayOverrideReexecutesRecordedGroup drives the re-execution half.
//
// A param override changes every identity, so nothing can be served from cache
// and the whole DAG must actually run in quarantine — producer included. The
// group it runs is still the RECORDED one: replay never re-expands from the
// producer it just re-executed, because a replay that discovered its own
// partitions would be reproducing a different run than the one under
// investigation.
//
// Lane-dependent by design. Re-execution needs a dispatcher, so the local
// server refuses it with 409 "distributed execution mode" — and that refusal is
// itself worth asserting: reaching the dispatch-mode gate proves the fanned
// baseline was ACCEPTED for replay rather than refused for being fanned.
func (s *IntegrationTestSuite) TestFanOutReplayOverrideReexecutesRecordedGroup() {
	alias := fmt.Sprintf("fanout-replay-override-%d", time.Now().UnixNano())
	job, baseline := s.replayFanOutBaseline(alias)

	key := "fanout-replay-override-" + time.Now().Format("150405.000000000")
	if !distributedLane() {
		observed := s.postReplay(job.ID, baseline.ID, &key, `{"set":{"mode":"what-if"}}`)
		s.Require().Contains([]int{http.StatusConflict, http.StatusUnprocessableEntity}, observed.status, observed.body)
		s.Contains(observed.body, "distributed execution mode",
			"a fanned baseline must reach the dispatch-mode gate, not a fan-out refusal: %s", observed.body)
		s.NotContains(strings.ToLower(observed.body), "fan-out group",
			"the fan-out refusal must no longer fire for a baseline whose producer recorded its list")
		return
	}

	replay := s.mustPostReplay(job.ID, baseline.ID, &key, `{"set":{"mode":"what-if"}}`)
	s.Require().NotEmpty(replay.RunID)

	parts := s.awaitPartitionStatuses(job.ID, replay.RunID, "process", runTimeout, map[string]string{
		"alpha":   "succeeded",
		"bravo":   "succeeded",
		"charlie": "succeeded",
	})
	s.Require().Len(parts, 3, "the re-executed group keeps the recorded width: %v", partitionStatusMap(parts))

	replayRun := s.awaitRun(job.ID, replay.RunID, runTimeout)
	s.Require().Equal("succeeded", replayRun.Status, "replay run failed: %s", replayRun.Error)

	statuses := s.taskStatusesByName(job.ID, replayRun)
	s.Equal("succeeded", statuses["list"],
		"an override forces the producer to re-execute; if it were cached this scenario is the previous one: %v", statuses)

	byValue := partitionsByValue(parts)
	s.Equal(fingerprintA, byValue["alpha"].Fingerprint, "the re-executed group keeps the RECORDED identities")
	s.Equal([]string{"alpha"}, byValue["charlie"].DependsOn)

	// The recorded in-group ordering is restored, not flattened: charlie's
	// outstanding count was seeded from the recorded dependsOn, so it cannot
	// start until alpha is done. This is the observable consequence of
	// re-expanding from the list rather than materializing three independent
	// rows.
	alpha, charlie := byValue["alpha"], byValue["charlie"]
	s.Require().NotNil(alpha.CompletedAt, "alpha must have completed: %+v", alpha)
	s.Require().NotNil(charlie.StartedAt, "charlie must have started: %+v", charlie)
	s.False(charlie.StartedAt.Before(*alpha.CompletedAt),
		"charlie dependsOn alpha, so it must not start before alpha completes (alpha completed %s, charlie started %s)",
		alpha.CompletedAt, charlie.StartedAt)
}
