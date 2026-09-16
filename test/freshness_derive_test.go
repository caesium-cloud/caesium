//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/stretchr/testify/require"
)

// freshnessFeatures decodes the freshness half of GET /v1/system/features.
type freshnessFeatures struct {
	FreshnessEnabled bool `json:"freshness_enabled"`
}

// requireFreshnessLane gates a scenario on a live server that reports the
// freshness feature enabled. Every self-server lane sets
// CAESIUM_FRESHNESS_ENABLED=true (justfile `integration-up`,
// `integration-up-distributed`, `integration-test-podman`, the helm CI values),
// so in steady state this never skips.
func (s *IntegrationTestSuite) requireFreshnessLane() {
	s.T().Helper()

	var features freshnessFeatures
	if err := s.tryGetJSON("/v1/system/features", &features); err != nil {
		s.T().Skipf("%s requires a live server reachable at GET /v1/system/features, but the request failed: %v", s.T().Name(), err)
		return
	}
	if !features.FreshnessEnabled {
		s.T().Skipf("%s requires CAESIUM_FRESHNESS_ENABLED=true on this lane's server; GET /v1/system/features reports freshness_enabled=false", s.T().Name())
	}
}

// runCLIWithFreshness runs the CLI with CAESIUM_FRESHNESS_ENABLED=true added to
// its environment.
//
// `caesium job apply` validates the manifest CLIENT-side before it posts, and
// pkg/jobdef gates `trigger.type: freshness` on that variable — so an operator
// applying such a manifest sets the flag in their own shell too. The lanes turn
// the flag on for the SERVER; the test-runner container is a separate process,
// so the scenario supplies it here (mirrors runCLIWithDataAssertions).
func (s *IntegrationTestSuite) runCLIWithFreshness(args ...string) {
	s.T().Helper()
	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = append(os.Environ(), "CAESIUM_FRESHNESS_ENABLED=true")
	output, err := cmd.CombinedOutput()
	require.NoError(s.T(), err, "cli %v failed: %s", args, string(output))
}

type datasetDerivationResponse struct {
	Name     string `json:"name"`
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	RunID    string `json:"run_id,omitempty"`
}

type datasetDerivationsTypedResponse struct {
	Derivations []datasetDerivationResponse `json:"derivations"`
	Total       int64                       `json:"total"`
}

// TestFreshnessDerivedRunExecutesWithoutConcurrencyPolicy is issue #501's first
// reproduction: a freshness-triggered job with NO metadata.concurrency block.
// The evaluator used to admit through the policy-only seam, which deliberately
// creates nothing when a job declares no concurrency policy — so the dataset
// went stale forever and every derivation read
// `skipped_admission: admission declined`.
func (s *IntegrationTestSuite) TestFreshnessDerivedRunExecutesWithoutConcurrencyPolicy() {
	s.runFreshnessDerivationScenario("nopolicy", "")
}

// TestFreshnessDerivedRunExecutesUnderConcurrencyPolicy is issue #501's second
// reproduction: with `metadata.concurrency: {maxRuns: 1, strategy: fail}` the
// policy path DID insert a job_runs row, but nothing ever built or dispatched
// its DAG — a `running` run with zero task rows, and `skipped_active_run`
// forever after.
func (s *IntegrationTestSuite) TestFreshnessDerivedRunExecutesUnderConcurrencyPolicy() {
	s.runFreshnessDerivationScenario("maxruns", "  concurrency:\n    maxRuns: 1\n    strategy: fail\n")
}

// TestFreshnessDerivedRunHonoursJobPause proves pause reaches the derivation
// path. Freshness derivation is the one scheduler route that creates AND
// executes a run without going through a trigger object — cron, HTTP, event and
// webhook each check `paused` before calling job.Run, and a manual run answers
// 409 — so without an explicit guard a paused job would execute on every
// arrival even though the Console promises pause blocks new runs.
//
// It drives the real `PUT /v1/jobs/:id/pause` route and a real arrival, asserts
// nothing runs while paused (with the derivation audit naming pause as the
// cause), then unpauses and proves the same circuit does run — so the "no run"
// half cannot pass because the scenario was simply broken.
func (s *IntegrationTestSuite) TestFreshnessDerivedRunHonoursJobPause() {
	s.requireFreshnessLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-freshness-paused-%d", suffix)
	source := fmt.Sprintf("integration.derive.src.paused.%d", suffix)
	produced := fmt.Sprintf("integration.derive.out.paused.%d", suffix)
	eventType := fmt.Sprintf("derive.integration.paused.%d", suffix)
	outputWatermark := fmt.Sprintf("out-%d", suffix)

	dir := s.writeJobManifest(freshnessDerivationManifest(alias, "", source, produced, eventType, outputWatermark))
	defer os.RemoveAll(dir)
	s.runCLIWithFreshness("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	s.Require().Equal(http.StatusOK, s.setJobPaused(job.ID, "pause"))
	s.Require().True(s.fetchJobPaused(job.ID), "the job must report paused before the arrival")

	// Age the produced dataset past its 1s SLO, then deliver the arrival that
	// would otherwise derive a run.
	time.Sleep(3 * time.Second)
	s.postEvent(fmt.Sprintf(`{
		"type":%q,
		"source":"ignored-by-arrival",
		"data":{"detail":{"kind":"orders","objects":[{"key":%q}]}}
	}`, eventType, fmt.Sprintf("vendor/orders/%d-paused.json", suffix)))

	// The evaluator must record the refusal, and it must create no run. Waiting
	// for the derivation first means the "no runs" assertion is not vacuous:
	// the evaluator has demonstrably looked at this dataset.
	s.Require().Eventually(func() bool {
		var derivations datasetDerivationsTypedResponse
		if err := s.tryGetJSON("/v1/datasets/_/"+produced+"/derivations", &derivations); err != nil {
			return false
		}
		for _, d := range derivations.Derivations {
			if d.Decision == "skipped_admission" && d.Reason == "job is paused" {
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "a paused freshness job should record a paused admission skip")

	s.Require().Never(func() bool {
		return len(s.fetchRuns(job.ID)) > 0
	}, 20*time.Second, time.Second, "a paused freshness job must not run on arrival")

	// Unpausing must restore derivation through exactly the same circuit.
	s.Require().Equal(http.StatusOK, s.setJobPaused(job.ID, "unpause"))
	s.Require().False(s.fetchJobPaused(job.ID), "the job must report unpaused")

	s.postEvent(fmt.Sprintf(`{
		"type":%q,
		"source":"ignored-by-arrival",
		"data":{"detail":{"kind":"orders","objects":[{"key":%q}]}}
	}`, eventType, fmt.Sprintf("vendor/orders/%d-unpaused.json", suffix)))

	derived := s.awaitFreshnessDerivedRun(job.ID, produced, 4*time.Minute)
	s.Equal("succeeded", derived.Status, "an unpaused freshness job must derive and execute (error: %s)", derived.Error)
	s.NotEmpty(derived.Tasks, "the unpaused derived run must have task rows")
}

// runFreshnessDerivationScenario drives the whole freshness derivation circuit
// through its real surfaces: `caesium job apply` installs a freshness-triggered
// job, POST /v1/events delivers a synthetic source arrival, and the assertions
// read GET /v1/jobs/:id/runs*, GET /v1/datasets/:ns/:name and
// GET /v1/datasets/:ns/:name/derivations.
//
// It deliberately asserts EXECUTION, not bookkeeping: a terminal `succeeded`
// run carrying real task rows with the step's emitted output, plus the produced
// dataset's output watermark advancing to that value. A derivation row or a
// `running` record alone is exactly what issue #501 reported as green-but-hollow.
func (s *IntegrationTestSuite) runFreshnessDerivationScenario(label, metadataExtra string) {
	s.requireFreshnessLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-freshness-derive-%s-%d", label, suffix)
	source := fmt.Sprintf("integration.derive.src.%s.%d", label, suffix)
	produced := fmt.Sprintf("integration.derive.out.%s.%d", label, suffix)
	eventType := fmt.Sprintf("derive.integration.%s.%d", label, suffix)
	arrivalWatermark := fmt.Sprintf("vendor/orders/%d.json", suffix)
	outputWatermark := fmt.Sprintf("out-%d", suffix)

	dir := s.writeJobManifest(freshnessDerivationManifest(alias, metadataExtra, source, produced, eventType, outputWatermark))
	defer os.RemoveAll(dir)
	s.runCLIWithFreshness("job", "apply", "--path", dir, "--server", s.caesiumURL)

	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	// Nothing may run before the source arrives: the produced dataset is stale
	// (freshness 1s) but its upstream has never been observed, so the evaluator
	// must sit at stale-upstream.
	s.Require().Empty(s.fetchRuns(job.ID), "a freshness-triggered job must not run before its source arrives")

	// Let the produced dataset age past its 1s SLO so the arrival is the only
	// thing the derivation is waiting on.
	time.Sleep(3 * time.Second)

	s.postEvent(fmt.Sprintf(`{
		"type":%q,
		"source":"ignored-by-arrival",
		"data":{"detail":{"kind":"orders","objects":[{"key":%q}]}}
	}`, eventType, arrivalWatermark))

	// The arrival advances the source, which publishes dataset_advanced and
	// wakes the evaluator; the 1-minute timer tick is the backstop.
	derived := s.awaitFreshnessDerivedRun(job.ID, produced, 4*time.Minute)

	s.Require().Equal("succeeded", derived.Status,
		"freshness-derived run %s did not succeed (error: %s)", derived.ID, derived.Error)
	s.Require().NotEmpty(derived.Tasks,
		"freshness-derived run %s has zero task rows: it was admitted but its DAG was never built or dispatched (issue #501)", derived.ID)
	s.Equal(produced, derived.Params["_derived_from_dataset"],
		"derived run should carry the freshness derivation params: %v", derived.Params)

	var emitted bool
	for _, task := range derived.Tasks {
		s.Equal("succeeded", task.Status, "derived run task %s did not succeed: %s", task.ID, task.Error)
		if task.Output["wm"] == outputWatermark {
			emitted = true
		}
	}
	s.True(emitted, "derived run task output should carry the emitted watermark %q: %+v", outputWatermark, derived.Tasks)

	// The produced dataset's output watermark must advance to the value the
	// derived run actually emitted.
	var state datasetDetailResponse
	s.Require().Eventually(func() bool {
		var detail datasetDetailResponse
		if err := s.tryGetJSON("/v1/datasets/_/"+produced, &detail); err != nil {
			return false
		}
		state = detail
		return detail.State.Watermark == outputWatermark
	}, 90*time.Second, 500*time.Millisecond,
		"produced dataset %s should advance its output watermark to %q after the derived run succeeds (observed %q)",
		produced, outputWatermark, state.State.Watermark)

	// And the audit trail must name that run as the derivation's outcome.
	var derivations datasetDerivationsTypedResponse
	s.getJSON("/v1/datasets/_/"+produced+"/derivations", &derivations)
	var linked bool
	for _, d := range derivations.Derivations {
		if d.Decision == "derived" && d.RunID == derived.ID {
			linked = true
		}
	}
	s.True(linked, "a `derived` derivation should reference the executed run %s: %+v", derived.ID, derivations.Derivations)
}

// awaitFreshnessDerivedRun polls the job's runs until one reaches a terminal
// status, then returns it with its task rows. It fails with the dataset's
// derivation audit attached when no run is ever created — the exact shape of
// issue #501's first reproduction.
func (s *IntegrationTestSuite) awaitFreshnessDerivedRun(jobID, produced string, timeout time.Duration) runResponse {
	s.T().Helper()

	deadline := time.Now().Add(timeout)
	var last runResponse
	for time.Now().Before(deadline) {
		for _, summary := range s.fetchRuns(jobID) {
			var detail runResponse
			if err := s.tryGetJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s", jobID, summary.ID), &detail); err != nil {
				continue
			}
			last = detail
			if detail.Status == "succeeded" || detail.Status == "failed" {
				return detail
			}
		}
		time.Sleep(time.Second)
	}

	var derivations datasetDerivationsTypedResponse
	_ = s.tryGetJSON("/v1/datasets/_/"+produced+"/derivations", &derivations)
	s.Require().Failf("freshness derivation never produced an executed run",
		"job %s produced no terminal run within %s (last observed run: %+v); derivations for %s: %+v",
		jobID, timeout, last, produced, derivations.Derivations)
	return last
}

// freshnessDerivationManifest declares a purely data-derived job:
// `trigger: {type: freshness}` with an arrival-bound external source (so one
// ingest POST advances the input without running anything) and a produced
// dataset whose freshness SLO is short enough to be stale the moment the input
// arrives. metadataExtra injects an optional `metadata.concurrency` block.
//
// The step's emitted watermark comes from ${CAESIUM_PARAM_OUTPUT_TOKEN}, a
// `trigger.defaultParams` entry, so every assertion on the emitted output and on
// the advanced dataset watermark is ALSO an assertion that the job's declared
// trigger defaults reached the derived run's task environment. That is not
// decoration: a missing param ref fails closed (see
// TestHTTPTriggerMissingParamRefFailsClosed), so a derivation that dropped
// defaults would fail the run outright.
func freshnessDerivationManifest(alias, metadataExtra, source, produced, eventType, outputWatermark string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
%s  datasets:
    sources:
      - name: %s
        expectedEvery: 24h
        external: true
        arrival:
          event:
            type: %s
            filter:
              detail.kind: orders
          watermark: "$.detail.objects[0].key"
trigger:
  type: freshness
  configuration: {}
  defaultParams:
    OUTPUT_TOKEN: "%s"
steps:
  - name: refresh
    image: alpine:3.23
    command: ["sh", "-c", "echo \"##caesium::output {\\\"wm\\\":\\\"$OUT_TOKEN\\\"}\""]
    env:
      OUT_TOKEN: "${CAESIUM_PARAM_OUTPUT_TOKEN}"
    datasets:
      consumes:
        - %s
      produces:
        - name: %s
          freshness: 1s
          watermark:
            key: wm
`, alias, metadataExtra, source, eventType, outputWatermark, source, produced)
}
