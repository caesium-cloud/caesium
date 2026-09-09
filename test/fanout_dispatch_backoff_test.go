//go:build integration

package test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// scrapeCounter reads one Prometheus counter off GET /metrics, matching the
// metric name and an exact set of label=value pairs.  A counter that has never
// been incremented emits no sample at all, so an absent series reads as 0 —
// which is exactly what a "did this go up?" delta needs.
func (s *IntegrationTestSuite) scrapeCounter(name string, labels map[string]string) float64 {
	s.T().Helper()

	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/metrics", nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		series, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		matched := true
		for k, v := range labels {
			if !strings.Contains(series, fmt.Sprintf("%s=%q", k, v)) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		parsed, parseErr := strconv.ParseFloat(value, 64)
		s.Require().NoError(parseErr, "parse metric sample %q", line)
		return parsed
	}
	return 0
}

// awaitQuietDispatch waits until the server has stopped refusing anyone else
// for capacity, so a measurement window starts against a quiet pool.
//
// caesium_dispatch_rejected_total is process-wide: a run left behind by an
// earlier scenario that is still being refused would land inside this
// scenario's window and read as its own retries.  The reason label keeps other
// KINDS of rejection out of the count; this keeps other runs' CAPACITY
// contention out of it.  Best effort and bounded — a permanently wedged run
// must leave a visible note, not hang the suite.
func (s *IntegrationTestSuite) awaitQuietDispatch(metric string, labels map[string]string, quiet, timeout time.Duration) {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	for {
		before := s.scrapeCounter(metric, labels)
		time.Sleep(quiet)
		if s.scrapeCounter(metric, labels) == before {
			return
		}
		if time.Now().After(deadline) {
			s.T().Logf("dispatch never went quiet within %s: %s%v is still moving, so the rate below may include foreign traffic",
				timeout, metric, labels)
			return
		}
	}
}

// TestFanOutSaturatedPoolBacksOffAndSurfacesStall is the end-to-end half of
// issue #400.
//
// The owner-memory lane runs one worker slot (CAESIUM_WORKER_POOL_SIZE=1) and a
// 500ms dispatch tick, so a three-partition group is a genuinely saturated pool:
// two instances are ready and refusable for the whole time the third is running.
// The loop used to re-post each of them on EVERY tick — the spin this issue was
// filed for.  With the per-task capacity backoff, attempts follow the 250ms→5s
// schedule instead of the tick rate, so the rejection RATE is what this asserts:
// comfortably below one per second, where one post per tick per waiting instance
// would be ~4/s.
//
// The count is scoped twice, because caesium_dispatch_rejected_total is
// process-wide and a bare delta measures the whole server rather than this
// workload: it reads only reason="no_capacity" (a foreign run refused for a
// stale claim reports task_not_running and cannot contribute), and it starts
// from a quiet pool.
//
// It also drives the progress deadline, which the lane sets to 5s
// (CAESIUM_RUN_OWNER_DISPATCH_PROGRESS_DEADLINE) so the path executes in CI:
// the last partition waits longer than that for a slot, so the owner must
// surface the stall on caesium_dispatch_stalled_total — and must nonetheless
// run the task to completion, because a stall is a signal, not a new state.
func (s *IntegrationTestSuite) TestFanOutSaturatedPoolBacksOffAndSurfacesStall() {
	if !ownerInMemoryLane() {
		s.T().Skip("capacity backoff lives in the run-owner dispatch loop; only the owner-memory lane runs it with a one-slot pool and a short progress deadline")
	}

	alias := fmt.Sprintf("fanout-capacity-backoff-%d", time.Now().UnixNano())
	manifest := fanOutManifest(fanOutJob{
		Alias:       alias,
		ProducerCmd: `echo '##caesium::partitions ["alpha","bravo","charlie"]'`,
		// Long enough that the two queued instances are refused for capacity
		// across many dispatch ticks, and that the last one passes the lane's
		// 5s progress deadline before a slot frees up.
		ConsumerCmd: "sleep 8\necho done $CAESIUM_PARTITION",
	})

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)

	const rejectedMetric = "caesium_dispatch_rejected_total"
	const stalledMetric = "caesium_dispatch_stalled_total"
	capacityLabels := map[string]string{"reason": "no_capacity"}

	s.awaitQuietDispatch(rejectedMetric, capacityLabels, 2*time.Second, 30*time.Second)

	rejectedBefore := s.scrapeCounter(rejectedMetric, capacityLabels)
	stalledBefore := s.scrapeCounter(stalledMetric, capacityLabels)

	started := time.Now()
	runID := s.triggerRun(job.ID)
	parts := s.awaitPartitionStatuses(job.ID, runID, "process", runTimeout, map[string]string{
		"alpha":   "succeeded|cached",
		"bravo":   "succeeded|cached",
		"charlie": "succeeded|cached",
	})
	s.Require().Len(parts, 3, "every partition must run despite the pool being saturated: %v", partitionStatusMap(parts))

	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status,
		"backing a task off must delay it, never strand it: %s", run.Error)
	elapsed := time.Since(started).Seconds()
	s.Require().Greater(elapsed, 8.0, "the group must actually have serialised through the one-slot pool")

	rejected := s.scrapeCounter(rejectedMetric, capacityLabels) - rejectedBefore
	s.Require().GreaterOrEqual(rejected, 1.0,
		"a one-slot pool with three ready instances must be refused for capacity at least once; if this is zero the scenario is not saturating anything")
	s.Less(rejected/elapsed, 1.0,
		"capacity rejections must follow the backoff schedule, not the dispatch tick: %.0f no_capacity rejections over %.1fs (%.2f/s); one post per tick per waiting instance would be ~4/s",
		rejected, elapsed, rejected/elapsed)

	stalled := s.scrapeCounter(stalledMetric, capacityLabels) - stalledBefore
	s.GreaterOrEqual(stalled, 1.0,
		"an instance waiting longer than CAESIUM_RUN_OWNER_DISPATCH_PROGRESS_DEADLINE for a slot must surface on caesium_dispatch_stalled_total")
}
