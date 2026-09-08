//go:build integration

package test

import (
	"fmt"
	"os"
	"time"
)

// baselineMinSamples mirrors the server-side default of
// CAESIUM_BASELINE_MIN_SAMPLES (run.DefaultBaselineMinSamples). No lane
// overrides it; a lane that ever does must change it here too, and the constant
// is the one place that decision is spelled instead of a literal 5 in three
// assertions.
const baselineMinSamples = 5

// assertionsEvaluatorManifest builds a one-step job that emits
// ##caesium::metrics and declares an assertion contract over the dataset it
// produces. assertionsBlock is the YAML under `assertions:` (already indented
// to match) and onViolation is the dispatch mode under test.
func assertionsEvaluatorManifest(alias, dataset, step, assertionsBlock, onViolation string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: false
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
%s    datasets:
      produces:
        - name: %s
          assertions:
%s
          onViolation: %s
`, alias, step, dataset, assertionsBlock, onViolation)
}

// assertionsSeederManifest builds the SAME job with no assertions declared —
// pure baseline history. A scenario applies this first, triggers it as many
// times as the baseline needs, then re-applies the alias with the contract
// under test.
//
// It must be the same job (same alias): the cross-job lint refuses two jobs
// producing one dataset, so a separate seeder job cannot exist. And
// `cache: false` is load-bearing — the default lane runs with
// CAESIUM_CACHE_ENABLED=true, so N identical triggers would be one real run and
// N-1 cache hits, and a cached task emits no new sample.
func assertionsSeederManifest(alias, dataset, step string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
  cache: false
trigger:
  type: cron
  configuration:
    expression: "0 0 31 2 *"
steps:
%s    datasets:
      produces:
        - name: %s
`, alias, step, dataset)
}

// seedDatasetBaseline applies the assertion-free variant of the job under test
// and triggers it `samples` times, leaving that many clean DatasetMetric rows
// for `dataset`. It returns nothing: the caller re-applies the alias with its
// contract and re-reads the job.
func (s *IntegrationTestSuite) seedDatasetBaseline(alias, dataset string, value float64, samples int) {
	s.T().Helper()

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": value})
	s.Require().NoError(err)

	job := s.applyAssertionsJob(alias, assertionsSeederManifest(alias, dataset, step))
	for i := 0; i < samples; i++ {
		runID := s.triggerRun(job.ID)
		s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status,
			"every seeding run must succeed: only clean samples count toward a baseline")
	}
}

// applyAssertionsJob applies a manifest through the real CLI (with the feature
// flag the client-side validator demands) and returns the created job.
func (s *IntegrationTestSuite) applyAssertionsJob(alias, manifest string) *jobSummary {
	s.T().Helper()

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLIWithDataAssertions("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)
	return job
}

// TestDataAssertionsWarnRecordsViolationOnAGreenRun drives the warn
// disposition end to end: a real container emits a rowCount below the declared
// min, the run still succeeds, and the violation is visible both on the task
// read surface and on the persisted row.
func (s *IntegrationTestSuite) TestDataAssertionsWarnRecordsViolationOnAGreenRun() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-warn-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.warn.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 12})
	s.Require().NoError(err)

	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {min: 1000}", "warn"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status, "warn mode records the violation without failing the run")

	task := s.requireRunTaskByName(job.ID, run, "load")
	s.Equal("succeeded", task.Status)
	s.Require().NotEmpty(task.DataViolations, "a warn-mode violation is only visible here; the run is green")
	s.Equal(dataset, task.DataViolations[0].Dataset)
	s.Equal("rowCount", task.DataViolations[0].Metric)
	s.Equal("min", task.DataViolations[0].Assertion)
	s.Require().NotNil(task.DataViolations[0].Observed)
	s.InDelta(12, *task.DataViolations[0].Observed, 0.001)
	s.False(task.DataViolations[0].Seeding, "an absolute bound enforces from run one, cold start or not")

	// The task read surface serialises TaskRun.data_violations straight off the
	// row (internal/run.convertRunTaskModel), so the assertions above ARE the
	// persistence proof. A direct catalog read would add nothing and would turn
	// this scenario into a mid-test SKIP on the lanes without direct dqlite
	// access (podman, kubernetes), which a lane-matrix reader would misread as
	// "no coverage".
}

// TestDataAssertionsFailTurnsTheRunRed drives the fail disposition: a max
// breach fails the task exactly as schemaValidation: fail does, and the failure
// message names the dataset, the metric and the assertion so an operator can
// find the contract that broke.
func (s *IntegrationTestSuite) TestDataAssertionsFailTurnsTheRunRed() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-fail-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.fail.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"dedup_ratio": 0.31})
	s.Require().NoError(err)

	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            custom:\n              - {metric: dedup_ratio, max: 0.05}", "fail"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "fail mode escalates a data violation into a red run")

	task := s.requireRunTaskByName(job.ID, run, "load")
	s.Equal("failed", task.Status)
	s.Contains(task.Error, dataset)
	s.Contains(task.Error, "dedup_ratio")
	s.Contains(task.Error, "max")
	s.Require().NotEmpty(task.DataViolations, "the evidence outlives the failure message")
}

// TestDataAssertionsMissingMetricIsAViolation proves a step that stops emitting
// a declared metric does not silently pass: it emits a different metric
// entirely, and the declared rowCount assertion is recorded as missing.
func (s *IntegrationTestSuite) TestDataAssertionsMissingMetricIsAViolation() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-missing-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.missing.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"dedup_ratio": 0.01})
	s.Require().NoError(err)

	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {min: 1000}", "warn"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status)

	task := s.requireRunTaskByName(job.ID, run, "load")
	s.Require().NotEmpty(task.DataViolations, "a declared metric that never arrived is itself a violation")
	s.Equal("rowCount", task.DataViolations[0].Metric)
	s.Equal("missing", task.DataViolations[0].Assertion)
	s.Nil(task.DataViolations[0].Observed, "a missing metric reports no observed value")
}

// TestDataAssertionsColdStartDeltaIsSeedingOnly drives the cold start: a
// deltaFromBaseline assertion declared onViolation: fail on a dataset with a
// single prior sample must NOT fail the run — the verdict is recorded as
// seeding instead.
func (s *IntegrationTestSuite) TestDataAssertionsColdStartDeltaIsSeedingOnly() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-coldstart-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.coldstart.%d", suffix)

	// One clean sample of 10000 — real history, but a baseline of one.
	s.seedDatasetBaseline(alias, dataset, 10000, 1)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 10})
	s.Require().NoError(err)
	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {deltaFromBaseline: 50%}", "fail"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status,
		"below CAESIUM_BASELINE_MIN_SAMPLES a deltaFromBaseline verdict is warn-only, even declared onViolation: fail")

	task := s.requireRunTaskByName(job.ID, run, "load")
	s.Require().NotEmpty(task.DataViolations, "the seeding verdict is still recorded and surfaced")
	s.Equal("deltaFromBaseline", task.DataViolations[0].Assertion)
	s.True(task.DataViolations[0].Seeding, "the verdict must be marked seeding, not silently dropped")
	s.Less(task.DataViolations[0].BaselineSamples, baselineMinSamples)
}

// TestDataAssertionsSeededDeltaFailsTheRun is the other half of the cold-start
// contract: once the dataset has at least CAESIUM_BASELINE_MIN_SAMPLES clean
// samples, the same declaration DOES turn the run red.
func (s *IntegrationTestSuite) TestDataAssertionsSeededDeltaFailsTheRun() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-seeded-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.seeded.%d", suffix)

	// Seed exactly CAESIUM_BASELINE_MIN_SAMPLES clean samples so the next
	// verdict is enforced rather than seeding.
	s.seedDatasetBaseline(alias, dataset, 10000, baselineMinSamples)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 10})
	s.Require().NoError(err)
	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {deltaFromBaseline: 50%}", "fail"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("failed", run.Status, "a seeded baseline enforces deltaFromBaseline")

	task := s.requireRunTaskByName(job.ID, run, "load")
	s.Equal("failed", task.Status)
	s.Contains(task.Error, dataset)
	s.Contains(task.Error, "deltaFromBaseline")

	s.Require().NotEmpty(task.DataViolations)
	violation := task.DataViolations[0]
	s.False(violation.Seeding, "past the floor the verdict enforces")
	s.Require().NotNil(violation.BaselineMedian)
	s.InDelta(10000, *violation.BaselineMedian, 0.001, "the baseline is the seeded clean history")
	// EXACTLY the seeded samples: this run emitted a sixth, and counting it
	// would read 6 here. An inequality would pass either way, which is how a
	// self-exclusion assertion goes quietly inert.
	s.Equal(baselineMinSamples, violation.BaselineSamples,
		"the run's own sample must not count toward the baseline it is judged against")
}
