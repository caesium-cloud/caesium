//go:build integration

package test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Shared fan-out fixtures and state-based pollers.
//
// Every fan-out scenario used to carry its own inline heredoc manifest, so the
// interesting difference between two scenarios (what the producer emits, what
// an instance does, which fanOut knob is set) was buried in forty lines of
// duplicated YAML, and a schema change had to be applied N times. The builder
// below keeps the YAML in one place and makes the difference the readable part.
//
// The pollers are state-based on purpose. These scenarios run in three lanes
// (local, distributed, run-owner in-memory) whose dispatch cadences differ by
// an order of magnitude — the distributed lanes have a single worker on a 500ms
// poll — so any assertion phrased as "after N seconds, X" is a lane-specific
// flake. Assertions here wait for a state and time out loudly instead.
// ---------------------------------------------------------------------------

// Valid partition fingerprints. The marker parser rejects anything that is not
// `sha256:` + 64 hex before it ever reaches the duplicate-key check, so a
// fixture that wants to exercise fingerprint SEMANTICS (dedup, conflict,
// per-partition cache identity) must use well-formed digests.
var (
	fingerprintA       = "sha256:" + strings.Repeat("a1", 32)
	fingerprintB       = "sha256:" + strings.Repeat("b2", 32)
	fingerprintC       = "sha256:" + strings.Repeat("c3", 32)
	fingerprintBPrime  = "sha256:" + strings.Repeat("d4", 32)
	fingerprintOther   = "sha256:" + strings.Repeat("e5", 32)
	fanOutPollInterval = 250 * time.Millisecond
)

// fanOutJob describes a one-producer / one-fanned-step job. `list` is always the
// producer, `process` the fanned step, and `publish` (when PublishCmd is set)
// the downstream fan-in.
type fanOutJob struct {
	Alias string
	// JobCache sets metadata.cache: true, enabling caching for every step that
	// does not override it.
	JobCache bool
	// ProducerCacheDisabled sets `cache: false` on the producer step. A cached
	// producer completes through CacheHitTask, which carries no partitions, so a
	// scenario that needs the producer to re-emit its list every run must turn
	// its cache off explicitly.
	ProducerCacheDisabled bool
	// ProducerCmd and ConsumerCmd are shell scripts run under `sh -c`.
	ProducerCmd string
	ConsumerCmd string
	// FanOutOpts are extra `fanOut:` keys beyond from/maxPartitions, e.g.
	// "maxParallel: 1", "failurePolicy: fail_fast", "onEmpty: fail".
	FanOutOpts []string
	// PublishCmd, when set, appends a downstream `publish` step that depends on
	// the fanned group. PublishTriggerRule overrides its default all_success.
	PublishCmd         string
	PublishTriggerRule string
}

// fanOutManifest renders a fanOutJob as a job manifest.
func fanOutManifest(j fanOutJob) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\n")
	b.WriteString("kind: Job\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  alias: %s\n", j.Alias)
	if j.JobCache {
		b.WriteString("  cache: true\n")
	}
	b.WriteString("trigger:\n")
	b.WriteString("  type: cron\n")
	b.WriteString("  configuration:\n")
	// Never fires on its own; every scenario triggers manually.
	b.WriteString("    expression: \"0 0 31 2 *\"\n")
	b.WriteString("steps:\n")

	b.WriteString("  - name: list\n")
	b.WriteString("    image: alpine:3.23\n")
	b.WriteString(shellCommandBlock(j.ProducerCmd, 4))
	if j.ProducerCacheDisabled {
		b.WriteString("    cache: false\n")
	}
	b.WriteString("    next: [process]\n")

	b.WriteString("  - name: process\n")
	b.WriteString("    image: alpine:3.23\n")
	b.WriteString(shellCommandBlock(j.ConsumerCmd, 4))
	b.WriteString("    dependsOn: [list]\n")
	b.WriteString("    fanOut:\n")
	b.WriteString("      from: list\n")
	// The integration servers run with CAESIUM_FANOUT_MAX_PARTITIONS=8, so 8 is
	// the largest declaration that is not itself a cap test.
	b.WriteString("      maxPartitions: 8\n")
	for _, opt := range j.FanOutOpts {
		fmt.Fprintf(&b, "      %s\n", strings.TrimSpace(opt))
	}

	if j.PublishCmd != "" {
		b.WriteString("    next: [publish]\n")
		b.WriteString("  - name: publish\n")
		b.WriteString("    image: alpine:3.23\n")
		b.WriteString(shellCommandBlock(j.PublishCmd, 4))
		b.WriteString("    dependsOn: [process]\n")
		if j.PublishTriggerRule != "" {
			fmt.Fprintf(&b, "    triggerRule: %s\n", j.PublishTriggerRule)
		}
	}

	return b.String()
}

// shellCommandBlock renders `command: [sh, -c, <script>]` as a YAML block
// scalar, so a script may contain single quotes, JSON, and newlines without any
// escaping — the reason the inline heredocs were unreadable.
func shellCommandBlock(script string, indent int) string {
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	b.WriteString(pad + "command:\n")
	b.WriteString(pad + "  - sh\n")
	b.WriteString(pad + "  - -c\n")
	b.WriteString(pad + "  - |\n")
	for _, line := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad + "    " + line + "\n")
	}
	return b.String()
}

// distributedLane reports whether this suite run is driving a server in
// distributed execution mode (the plain distributed lane and the run-owner
// in-memory lane both set it; the local lane leaves it empty).
//
// Some surfaces are deliberately lane-dependent — per-partition retry requires
// a dispatcher to re-execute the instance, so the local server refuses it with
// 409 rather than resetting a row nothing will pick up. A test for such a
// surface must assert the RIGHT answer for its lane; branching on whichever
// answer arrived would pass just as happily if the two lanes swapped behavior.
func distributedLane() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CAESIUM_EXECUTION_MODE")), "distributed")
}

// partitionStatusMap projects instance rows onto partition value -> status.
func partitionStatusMap(rows []partitionInstance) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Value == "" {
			continue
		}
		out[r.Value] = r.Status
	}
	return out
}

// partitionsByValue indexes expanded instance rows by partition value.
func partitionsByValue(rows []partitionInstance) map[string]partitionInstance {
	out := make(map[string]partitionInstance, len(rows))
	for _, r := range rows {
		if r.Value == "" {
			continue
		}
		out[r.Value] = r
	}
	return out
}

// statusMatches reports whether got satisfies a wanted status expression. A
// want may name alternatives with "|" — "succeeded|cached" is how a scenario
// says "ran or was served from cache", which is genuinely indeterminate when a
// prior suite run seeded the cache.
func statusMatches(want, got string) bool {
	for _, w := range strings.Split(want, "|") {
		if strings.TrimSpace(w) == got {
			return true
		}
	}
	return false
}

// awaitPartitionStatuses polls the partitions endpoint until every named
// partition reaches its wanted status, and returns the final expanded rows.
//
// This is the state-based alternative to sleeping: the three lanes reach the
// same states at very different speeds, and a stall must fail with the observed
// map rather than hang the suite.
func (s *IntegrationTestSuite) awaitPartitionStatuses(
	jobID, runID, taskRef string,
	timeout time.Duration,
	want map[string]string,
) []partitionInstance {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	var observed map[string]string
	for {
		rows := s.expandedPartitions(s.listPartitions(jobID, runID, taskRef))
		observed = partitionStatusMap(rows)
		matched := len(observed) >= len(want)
		for value, wantStatus := range want {
			if !statusMatches(wantStatus, observed[value]) {
				matched = false
				break
			}
		}
		if matched {
			return rows
		}
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout waiting for partition statuses on task %s of run %s\n  want: %v\n  got:  %v",
				taskRef, runID, want, observed)
		}
		time.Sleep(fanOutPollInterval)
	}
}

// partitionSnapshot is one sampled observation of a group's instance statuses.
type partitionSnapshot struct {
	Statuses map[string]string
}

// observePartitionStates samples a fanned group every interval until its run
// reaches a terminal status, returning every snapshot taken (including the
// terminal one).
//
// Concurrency and ordering claims are asserted over these snapshots — via
// maxConcurrent/startedByLastSnapshot — rather than over wall-clock
// timestamps, because the three lanes this suite runs in (local, distributed,
// run-owner in-memory) dispatch on cadences that differ by an order of
// magnitude, so "after N seconds, X" is lane-specific. The partitions endpoint
// DOES expose per-instance started_at/completed_at (see partitionInstance);
// assertFailFastGroup reads those directly off the endpoint rather than off a
// sampled snapshot, since fail_fast's ordering claim needs the actual
// timestamps, not just which status was observed at which poll.
func (s *IntegrationTestSuite) observePartitionStates(
	jobID, runID, taskRef string,
	interval, timeout time.Duration,
) []partitionSnapshot {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	snapshots := make([]partitionSnapshot, 0, 64)
	for {
		var runState runResponse
		runErr := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, runID), &runState)

		var parts partitionListResponse
		if err := s.tryGetJSON(
			fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/partitions", jobID, runID, taskRef),
			&parts,
		); err == nil {
			snapshots = append(snapshots, partitionSnapshot{
				Statuses: partitionStatusMap(s.expandedPartitions(parts.Partitions)),
			})
		}

		if runErr == nil && (runState.Status == "succeeded" || runState.Status == "failed") {
			return snapshots
		}
		if time.Now().After(deadline) {
			s.T().Fatalf("timeout observing run %s (last status %q, %d snapshots)", runID, runState.Status, len(snapshots))
		}
		time.Sleep(interval)
	}
}

// maxConcurrent returns the highest number of instances observed in `running`
// in any single snapshot, and the snapshot index where that peak occurred.
func maxConcurrent(snapshots []partitionSnapshot) (int, int) {
	peak, at := 0, -1
	for i, snap := range snapshots {
		n := 0
		for _, status := range snap.Statuses {
			if status == "running" {
				n++
			}
		}
		if n > peak {
			peak, at = n, i
		}
	}
	return peak, at
}

// startedByLastSnapshot reports whether any instance was ever observed running,
// which is what stops a "never exceeded the cap" assertion from passing
// vacuously because every sample landed between containers.
func startedByLastSnapshot(snapshots []partitionSnapshot) bool {
	for _, snap := range snapshots {
		for _, status := range snap.Statuses {
			if status == "running" {
				return true
			}
		}
	}
	return false
}

// partitionStamp renders an instance timestamp for a failure message. An absent
// one prints as "never", which in a fail_fast assertion is a value with meaning
// — the instance was resolved without ever running — not a missing field.
func partitionStamp(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// assertFailFastGroup asserts the fail_fast contract over an expanded group in
// which the instance named failValue failed.
//
// The obvious assertion — "siblings x, y and z are skipped" — is NOT lane
// agnostic, and asserting it produced real red lanes: instance rows of a group
// share a created_at and the claim predicate orders by (priority, created_at),
// so claim order among ready instances is undefined, and a worker pool with a
// single slot runs them one at a time in whatever order it claimed. The same
// fixture legitimately produced x:skipped/y,z:succeeded on one run and
// z:skipped/x,y:succeeded on the next. Any assertion naming WHICH sibling is
// resolved is asserting the scheduler, not the policy.
//
// The lane-agnostic invariant is about STARTS, not outcomes: once the failing
// instance completes, fail_fast releases no further work of that group. A
// sibling already in flight may finish normally — Caesium cannot kill its
// container, so resolving a live row would claim a terminal state for work that
// is still executing — while a sibling that was still pending must be resolved
// and never run at all. So:
//
//   - a sibling that records a start must have started at or before the failing
//     instance completed;
//   - a sibling that records no start must be skipped/cancelled;
//   - and those two sets are complements, which is the cross-check that catches
//     a lane resolving a row it had already dispatched.
//
// Comparisons are >= (!After), never >: the endpoint's timestamps are RFC3339
// at second precision, so two events inside one second render equal.
func (s *IntegrationTestSuite) assertFailFastGroup(rows []partitionInstance, failValue string) {
	s.T().Helper()

	byValue := partitionsByValue(rows)
	statuses := partitionStatusMap(rows)

	for value, status := range statuses {
		s.True(isTerminalPartitionStatus(status),
			"instance %s left non-terminal (%s); the run finished, so nothing may still be pending: %v",
			value, status, statuses)
	}

	failed, ok := byValue[failValue]
	s.Require().True(ok, "the failing partition %q has no instance row: %v", failValue, statuses)
	s.Require().Equal("failed", failed.Status,
		"instance %q must be recorded failed; every assertion below is anchored on its completion: %v",
		failValue, statuses)
	s.Require().NotNil(failed.CompletedAt,
		"the failed instance recorded no completed_at, so there is no instant to measure the group against")

	resolved := make([]string, 0, len(rows))     // siblings fail_fast resolved
	neverStarted := make([]string, 0, len(rows)) // siblings that never ran
	var latestSiblingStart *time.Time

	for value, row := range byValue {
		if value == failValue {
			continue
		}
		isResolved := row.Status == "skipped" || row.Status == "cancelled"
		if isResolved {
			resolved = append(resolved, value)
		}

		if row.StartedAt == nil {
			neverStarted = append(neverStarted, value)
			s.True(isResolved,
				"sibling %q never started yet is %q: an instance still pending when %q failed must be resolved "+
					"by fail_fast, and these fixtures enable no cache, so there is no other way to reach a "+
					"terminal state without running: %v",
				value, row.Status, failValue, statuses)
			continue
		}

		s.False(row.StartedAt.After(*failed.CompletedAt),
			"sibling %q started at %s, AFTER %q failed at %s — fail_fast dispatched further work of the group "+
				"once it had already failed: %v",
			value, partitionStamp(row.StartedAt), failValue, partitionStamp(failed.CompletedAt), statuses)
		s.False(isResolved,
			"sibling %q is %q but records started_at %s: fail_fast resolves PENDING siblings only, so a row "+
				"that was already running must not have been resolved out from under its container: %v",
			value, row.Status, partitionStamp(row.StartedAt), statuses)

		if latestSiblingStart == nil || row.StartedAt.After(*latestSiblingStart) {
			latestSiblingStart = row.StartedAt
		}
	}

	s.Equal(len(resolved), len(neverStarted),
		"the resolved siblings and the never-started siblings must be the same set (resolved=%v, never-started=%v): %v",
		resolved, neverStarted, statuses)

	// Non-vacuity, and the case that makes this more than a tautology: nothing
	// above fails if fail_fast never fired at all but every sibling happened to
	// start before the failure. That is exactly what a single-slot worker
	// produces when it claims the failing instance LAST — a legitimate ordering,
	// and the only one in which no sibling can have been pending. So it is
	// admitted explicitly, on the evidence that the failing instance started
	// last, rather than passing silently.
	if len(resolved) == 0 {
		s.Require().NotNil(failed.StartedAt,
			"no sibling was resolved and %q records no started_at, so the group cannot be shown to have been "+
				"fully in flight when it failed: %v", failValue, statuses)
		s.Require().NotNil(latestSiblingStart,
			"no sibling was resolved and none records a start; the group has no siblings to reason about: %v", statuses)
		s.False(latestSiblingStart.After(*failed.StartedAt),
			"no sibling was resolved, yet one started at %s — after %q started at %s. Either fail_fast released "+
				"work it should have cancelled, or the group simply never had a pending sibling; only the second "+
				"is acceptable, and it requires %q to have started last: %v",
			partitionStamp(latestSiblingStart), failValue, partitionStamp(failed.StartedAt), failValue, statuses)
	}
}

// isTerminalPartitionStatus reports whether an instance status is final.
func isTerminalPartitionStatus(status string) bool {
	switch status {
	case "succeeded", "cached", "failed", "skipped", "cancelled":
		return true
	default:
		return false
	}
}

// TestFanOutManifestBuilderProducesValidJobs is a hermetic guard on the builder
// above: every fixture shape the fan-out scenarios use must parse and validate
// against the real job schema.
//
// It needs no server, and it exists because the failure it catches is
// expensive: a YAML indentation slip in the builder fails `job apply` inside
// every fan-out scenario in all three integration lanes, and reads as a
// fan-out regression rather than as a broken fixture.
func TestFanOutManifestBuilderProducesValidJobs(t *testing.T) {
	cases := map[string]fanOutJob{
		"minimal": {
			Alias:       "builder-minimal",
			ProducerCmd: `echo '##caesium::partitions ["a","b"]'`,
			ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
		},
		"cached producer disabled + job cache": {
			Alias:                 "builder-cache",
			JobCache:              true,
			ProducerCacheDisabled: true,
			ProducerCmd: fmt.Sprintf(
				`echo '##caesium::partitions [{"key":"a","fingerprint":"%s"}]'`, fingerprintA),
			ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
		},
		"cacheable producer + fingerprinted partitions": {
			Alias:    "builder-cacheable-producer",
			JobCache: true,
			// No ProducerCacheDisabled: the producer inherits metadata.cache, which
			// is the shape TestFanOutCachedProducerExpandsGroup needs. It is a
			// distinct fixture from the one above precisely because the two differ
			// only by the step-level override.
			ProducerCmd: fmt.Sprintf(
				`echo '##caesium::partitions [{"key":"a","fingerprint":"%s"},{"key":"b","fingerprint":"%s"}]'`,
				fingerprintA, fingerprintB),
			ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
		},
		"no fanOut opts + publish": {
			Alias: "builder-default-policy",
			ProducerCmd: `echo '##caesium::partitions [` +
				`{"key":"bad"},` +
				`{"key":"gate"},` +
				`{"key":"x","dependsOn":["gate"]}]'`,
			ConsumerCmd: `case "$CAESIUM_PARTITION" in
  bad) echo failing; exit 1 ;;
  gate) sleep 8; echo gate-done ;;
  *) echo ok ;;
esac`,
			PublishCmd: "echo published",
		},
		"wall-clock gated root + failurePolicy continue": {
			Alias: "builder-deadline-root",
			ProducerCmd: `echo '##caesium::partitions [` +
				`{"key":"a"},` +
				`{"key":"b","dependsOn":["a"]}]'`,
			// The shape TestFanOutOrderedGroupRetryDrivesDependents bakes a
			// deadline into: command substitution, a numeric test, and quotes,
			// all inside one block scalar. A slip here fails `job apply` in every
			// lane and reads as a retry regression.
			ConsumerCmd: fmt.Sprintf(`if [ "$CAESIUM_PARTITION" = a ] && [ "$(date +%%s)" -lt %d ]; then
  echo "root failing until epoch %d"
  exit 1
fi
echo partition=$CAESIUM_PARTITION`, 1893456000, 1893456000),
			FanOutOpts: []string{"failurePolicy: continue"},
		},
		"multi-line scripts + fanOut opts + publish": {
			Alias: "builder-full",
			ProducerCmd: `echo '##caesium::partitions [` +
				`{"key":"bad"},` +
				`{"key":"x","dependsOn":["bad"]}]'`,
			ConsumerCmd: `case "$CAESIUM_PARTITION" in
  bad) echo failing; exit 1 ;;
  *) sleep 3; echo ok ;;
esac`,
			FanOutOpts:         []string{"maxParallel: 1", "failurePolicy: fail_fast"},
			PublishCmd:         "echo published",
			PublishTriggerRule: "all_done",
		},
	}

	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := fanOutManifest(job)
			def, err := schema.Parse([]byte(manifest))
			require.NoError(t, err, "generated manifest is not a valid job:\n%s", manifest)
			require.Len(t, def.Steps, len(def.Steps))

			var fanned *schema.Step
			for i := range def.Steps {
				if def.Steps[i].Name == "process" {
					fanned = &def.Steps[i]
				}
			}
			require.NotNil(t, fanned, "builder must emit the fanned `process` step:\n%s", manifest)
			require.NotNil(t, fanned.FanOut, "fanOut must survive the round trip:\n%s", manifest)
			require.Equal(t, "list", fanned.FanOut.From)
			require.Equal(t, 8, fanned.FanOut.MaxPartitions)

			// The scripts must survive as a single `sh -c` argument, newlines and
			// quotes intact — the whole reason for the block-scalar rendering.
			require.Equal(t, []string{"sh", "-c"}, fanned.Command[:2])
			require.Contains(t, fanned.Command[2], strings.Split(job.ConsumerCmd, "\n")[0])
		})
	}
}

// TestFanOutManifestOmittedKnobsResolveToSchemaDefaults is the hermetic half of
// TestFanOutFailFastIsTheDefault: it pins that a fanOut block with no
// `failurePolicy` really is the fail_fast case, and that the builder renders no
// such key.
//
// Without this, the integration scenario proves only "this manifest behaved
// fail_fast" — if the builder ever started emitting the key, or the schema
// default flipped, that scenario would still pass while silently testing the
// explicit form a second time. The default is applied in two places (this one,
// pkg/jobdef's validateSteps, and internal/run's normalizeFanOutFailurePolicy at
// runtime), and this is the cheap one to pin.
func TestFanOutManifestOmittedKnobsResolveToSchemaDefaults(t *testing.T) {
	manifest := fanOutManifest(fanOutJob{
		Alias:       "builder-defaults",
		ProducerCmd: `echo '##caesium::partitions ["a"]'`,
		ConsumerCmd: "echo partition=$CAESIUM_PARTITION",
	})

	require.NotContains(t, manifest, "failurePolicy",
		"a fixture with no FanOutOpts must render no failurePolicy key:\n%s", manifest)
	require.NotContains(t, manifest, "onEmpty",
		"a fixture with no FanOutOpts must render no onEmpty key:\n%s", manifest)

	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err, "generated manifest is not a valid job:\n%s", manifest)

	var fanned *schema.Step
	for i := range def.Steps {
		if def.Steps[i].Name == "process" {
			fanned = &def.Steps[i]
		}
	}
	require.NotNil(t, fanned)
	require.NotNil(t, fanned.FanOut)
	require.Equal(t, schema.FanOutFailureFailFast, fanned.FanOut.FailurePolicy,
		"an omitted failurePolicy must normalize to fail_fast at parse time")
	require.Equal(t, schema.FanOutOnEmptySkip, fanned.FanOut.OnEmpty,
		"an omitted onEmpty must normalize to skip at parse time")
}

// TestFanOutManifestCacheKnobsAreIndependent pins the two producer-cache shapes
// apart, because the fan-out cache scenarios differ ONLY in this one line and
// swapping them silently turns one scenario into a duplicate of the other:
// TestFanOutPerPartitionCacheIdentity needs the producer's cache OFF (so it
// re-emits its list every run and contributes no predecessor hash), while
// TestFanOutCachedProducerExpandsGroup needs it ON (a warm producer must still
// expand its consumer's group).
func TestFanOutManifestCacheKnobsAreIndependent(t *testing.T) {
	cacheable := fanOutManifest(fanOutJob{
		Alias:       "builder-cacheable",
		JobCache:    true,
		ProducerCmd: `echo '##caesium::partitions ["a"]'`,
		ConsumerCmd: "echo ok",
	})
	require.Contains(t, cacheable, "  cache: true", "metadata.cache must be set:\n%s", cacheable)
	require.NotContains(t, cacheable, "cache: false",
		"a cacheable producer fixture must not disable the step cache:\n%s", cacheable)

	disabled := fanOutManifest(fanOutJob{
		Alias:                 "builder-cache-off",
		JobCache:              true,
		ProducerCacheDisabled: true,
		ProducerCmd:           `echo '##caesium::partitions ["a"]'`,
		ConsumerCmd:           "echo ok",
	})
	require.Contains(t, disabled, "    cache: false",
		"the override must land on the producer step, indented under it:\n%s", disabled)

	// Both must still be valid jobs — a `cache:` key in the wrong place parses
	// as a different field or fails validation, and the failure would otherwise
	// surface as `job apply` breaking inside every fan-out lane.
	for name, manifest := range map[string]string{"cacheable": cacheable, "disabled": disabled} {
		if _, err := schema.Parse([]byte(manifest)); err != nil {
			t.Fatalf("%s manifest is not a valid job: %v\n%s", name, err, manifest)
		}
	}
}
