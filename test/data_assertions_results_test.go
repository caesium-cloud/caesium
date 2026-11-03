//go:build integration

package test

import (
	"fmt"
	"strings"
	"time"
)

// runAssertionsBody is GET /v1/jobs/:id/runs/:run_id/assertions as the console
// reads it: the exact fields the run page and the task panel render.
type runAssertionsBody struct {
	RunID string `json:"run_id"`
	JobID string `json:"job_id"`
	Tasks []struct {
		TaskID        string         `json:"task_id"`
		Fanned        bool           `json:"fanned"`
		InstanceCount int            `json:"instance_count"`
		VerdictCounts map[string]int `json:"verdict_counts"`
		Instances     []struct {
			TaskRunID      string                `json:"task_run_id"`
			PartitionValue string                `json:"partition_value"`
			Status         string                `json:"status"`
			Attempt        int                   `json:"attempt"`
			CacheHit       bool                  `json:"cache_hit"`
			Verdict        string                `json:"verdict"`
			Results        []assertionResultBody `json:"results"`
		} `json:"instances"`
	} `json:"tasks"`
}

type assertionResultBody struct {
	Dataset         string   `json:"dataset"`
	Metric          string   `json:"metric"`
	Assertion       string   `json:"assertion"`
	Outcome         string   `json:"outcome"`
	Disposition     string   `json:"disposition"`
	Observed        *float64 `json:"observed"`
	Bound           *float64 `json:"bound"`
	BaselineMedian  *float64 `json:"baseline_median"`
	BaselineSamples int      `json:"baseline_samples"`
	Seeding         bool     `json:"seeding"`
	Reason          string   `json:"reason"`
	Message         string   `json:"message"`
}

func (s *IntegrationTestSuite) runAssertions(jobID, runID string) runAssertionsBody {
	s.T().Helper()
	var body runAssertionsBody
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/runs/%s/assertions", jobID, runID), &body)
	s.Equal(runID, body.RunID)
	s.Equal(jobID, body.JobID)
	return body
}

func resultNamed(results []assertionResultBody, metric, assertion string) (assertionResultBody, bool) {
	for _, r := range results {
		if r.Metric == metric && r.Assertion == assertion {
			return r, true
		}
	}
	return assertionResultBody{}, false
}

// TestRunAssertionsShowPassingAndViolatedResults drives the whole path: a real
// container emits two metrics against a two-assertion contract, one holds and
// one breaks (warn), and the read the console renders carries BOTH outcomes
// with their observed values and bounds — not only the violation.
func (s *IntegrationTestSuite) TestRunAssertionsShowPassingAndViolatedResults() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-results-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.results.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 1500, "dedup_ratio": 0.31})
	s.Require().NoError(err)
	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {min: 1000}\n            custom:\n              - {metric: dedup_ratio, max: 0.05}", "warn"))

	runID := s.triggerRun(job.ID)
	run := s.awaitRun(job.ID, runID, runTimeout)
	s.Require().Equal("succeeded", run.Status, "warn records the breach without failing the run")

	got := s.runAssertions(job.ID, runID)
	s.Require().Len(got.Tasks, 1)
	task := got.Tasks[0]
	s.Equal(s.jobTaskIDByName(job.ID, "load"), task.TaskID)
	s.False(task.Fanned)
	s.Equal(map[string]int{"violated": 1}, task.VerdictCounts)
	s.Require().Len(task.Instances, 1)
	instance := task.Instances[0]
	s.Equal("violated", instance.Verdict)
	s.False(instance.CacheHit)
	s.Equal(1, instance.Attempt)
	s.Require().Len(instance.Results, 2)

	passed, ok := resultNamed(instance.Results, "rowCount", "min")
	s.Require().True(ok, "the passing check is reported: %+v", instance.Results)
	s.Equal("passed", passed.Outcome)
	s.Empty(passed.Disposition)
	s.Require().NotNil(passed.Observed)
	s.InDelta(1500, *passed.Observed, 0.001)
	s.Require().NotNil(passed.Bound)
	s.InDelta(1000, *passed.Bound, 0.001)

	broken, ok := resultNamed(instance.Results, "dedup_ratio", "max")
	s.Require().True(ok)
	s.Equal("violated", broken.Outcome)
	s.Equal("warn", broken.Disposition)
	s.Require().NotNil(broken.Observed)
	s.InDelta(0.31, *broken.Observed, 0.0001)
	s.Require().NotNil(broken.Bound)
	s.InDelta(0.05, *broken.Bound, 0.0001)
	s.NotEmpty(broken.Message)

	// The read agrees with the violation row on the run payload: results are a
	// superset, never a second opinion.
	taskRow := s.requireRunTaskByName(job.ID, run, "load")
	s.Require().Len(taskRow.DataViolations, 1)
	s.Equal("dedup_ratio", taskRow.DataViolations[0].Metric)
}

// TestRunAssertionsColdStartIsNotACleanPass: the first evaluation of a delta
// check has nothing to compare against and decides nothing; the next passes
// against a baseline shorter than the cold-start floor and says so.
func (s *IntegrationTestSuite) TestRunAssertionsColdStartIsNotACleanPass() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-cold-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.cold.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 105})
	s.Require().NoError(err)
	job := s.applyAssertionsJob(alias, assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {min: 10, deltaFromBaseline: \"50%\"}", "warn"))

	first := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, first, runTimeout).Status)
	undecided := s.runAssertions(job.ID, first).Tasks[0].Instances[0]
	s.Equal("undecided", undecided.Verdict, "no history to compare against is not a pass")
	delta, ok := resultNamed(undecided.Results, "rowCount", "deltaFromBaseline")
	s.Require().True(ok)
	s.Equal("undecided", delta.Outcome)
	s.Equal("no_baseline", delta.Reason)
	min, ok := resultNamed(undecided.Results, "rowCount", "min")
	s.Require().True(ok)
	s.Equal("passed", min.Outcome, "the absolute bound is decided from run one")

	second := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, second, runTimeout).Status)
	seeded := s.runAssertions(job.ID, second).Tasks[0].Instances[0]
	s.Equal("seeding", seeded.Verdict, "a pass against %d of %d samples is not a clean pass", 1, baselineMinSamples)
	delta, ok = resultNamed(seeded.Results, "rowCount", "deltaFromBaseline")
	s.Require().True(ok)
	s.Equal("passed", delta.Outcome)
	s.True(delta.Seeding)
	s.Equal(1, delta.BaselineSamples)
	s.Require().NotNil(delta.BaselineMedian)
	s.InDelta(105, *delta.BaselineMedian, 0.001)
}

// TestRunAssertionsCacheHitRanNoAssertions: a task served from cache executes
// nothing, so the read says no assertion ran instead of reporting a pass.
func (s *IntegrationTestSuite) TestRunAssertionsCacheHitRanNoAssertions() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-cached-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.cached.%d", suffix)

	step, err := metricsProducerStepForDataset("load", dataset, map[string]any{"rowCount": 1500})
	s.Require().NoError(err)
	manifest := strings.Replace(assertionsEvaluatorManifest(alias, dataset, step,
		"            rowCount: {min: 1000}", "warn"), "cache: false", "cache: true", 1)
	job := s.applyAssertionsJob(alias, manifest)

	first := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, first, runTimeout).Status)
	executed := s.runAssertions(job.ID, first).Tasks[0].Instances[0]
	s.Equal("passed", executed.Verdict)
	s.False(executed.CacheHit)

	second := s.triggerRun(job.ID)
	run2 := s.awaitRun(job.ID, second, runTimeout)
	s.Require().Equal("cached", s.taskStatusesByName(job.ID, run2)["load"], "the second run must be a cache hit for this scenario to prove anything")

	cached := s.runAssertions(job.ID, second).Tasks[0].Instances[0]
	s.True(cached.CacheHit)
	s.Equal("not_evaluated", cached.Verdict)
	s.Empty(cached.Results, "a cache hit carries no results, not a copied pass")

	// The fact was recorded when the cache hit happened: removing the step's
	// assertions from the job afterwards does not change what that run shows.
	s.applyAssertionsJob(alias, assertionsSeederManifest(alias, dataset, step))
	again := s.runAssertions(job.ID, second)
	s.Require().Len(again.Tasks, 1, "a later edit to the job does not erase what the run recorded")
	s.Equal("not_evaluated", again.Tasks[0].Instances[0].Verdict)
}

// TestRunAssertionsFanOutVerdictsBelongToTheirInstance: a fanned producer is
// evaluated per partition, so each partition owns its verdict and the group is
// described by its mix — one bad partition neither hides among nor condemns
// its siblings.
func (s *IntegrationTestSuite) TestRunAssertionsFanOutVerdictsBelongToTheirInstance() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-fanout-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.fanout.%d", suffix)

	manifest := fmt.Sprintf(`
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
  - name: list
    image: alpine:3.23
    command: ["sh", "-c", "echo '##caesium::partitions [\"a\",\"b\",\"c\"]'"]
    next: [load]
  - name: load
    image: alpine:3.23
    dependsOn: [list]
    command:
      - sh
      - -c
      - |
        n=1500
        if [ "$CAESIUM_PARTITION" = "b" ]; then n=12; fi
        echo "##caesium::metrics {\"dataset\":\"%s\",\"rowCount\":$n}"
    fanOut:
      from: list
      maxPartitions: 8
    datasets:
      produces:
        - name: %s
          assertions:
            rowCount: {min: 1000}
          onViolation: warn
`, alias, dataset, dataset)
	job := s.applyAssertionsJob(alias, manifest)

	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status)

	got := s.runAssertions(job.ID, runID)
	s.Require().Len(got.Tasks, 1, "only the producing step has a contract")
	task := got.Tasks[0]
	s.Equal(s.jobTaskIDByName(job.ID, "load"), task.TaskID)
	s.True(task.Fanned)
	s.Equal(3, task.InstanceCount)
	s.Equal(map[string]int{"passed": 2, "violated": 1}, task.VerdictCounts)

	verdictByPartition := map[string]string{}
	for _, instance := range task.Instances {
		verdictByPartition[instance.PartitionValue] = instance.Verdict
		s.NotEmpty(instance.TaskRunID)
	}
	s.Equal(map[string]string{"a": "passed", "b": "violated", "c": "passed"}, verdictByPartition)
}

// TestRunAssertionsAbsentWithoutContract: a run whose steps declare no
// assertions has nothing to report, and addressing a run under another job's
// path is a 404.
func (s *IntegrationTestSuite) TestRunAssertionsAbsentWithoutContract() {
	s.requireDataAssertionsLane()

	suffix := time.Now().UnixNano()
	alias := fmt.Sprintf("integration-assert-none-%d", suffix)
	dataset := fmt.Sprintf("integration.assert.none.%d", suffix)

	job := s.applyAssertionsJob(alias, assertionsSeederManifest(alias, dataset, mustMetricsStep(s, "load", dataset, 1500)))
	runID := s.triggerRun(job.ID)
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status)

	got := s.runAssertions(job.ID, runID)
	s.Empty(got.Tasks, "a task with no contract records nothing, so nothing can be read as passed")

	other := s.applyAssertionsJob(alias+"-other", assertionsSeederManifest(alias+"-other", dataset+".other", mustMetricsStep(s, "load", dataset+".other", 1)))
	resp, err := s.doRequest("GET", fmt.Sprintf("%s/v1/jobs/%s/runs/%s/assertions", s.caesiumURL, other.ID, runID), nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Equal(404, resp.StatusCode)
}

func mustMetricsStep(s *IntegrationTestSuite, name, dataset string, rowCount float64) string {
	s.T().Helper()
	step, err := metricsProducerStepForDataset(name, dataset, map[string]any{"rowCount": rowCount})
	s.Require().NoError(err)
	return step
}
