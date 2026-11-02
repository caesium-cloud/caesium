package harness

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/stretchr/testify/require"
)

func TestEvaluateScenarioRecordsExpectedAndActual(t *testing.T) {
	scenario := ResolvedScenario{Scenario: Scenario{
		Name: "case",
		Expect: Expectation{
			RunStatus: "failed",
			Tasks: []TaskExpectation{{
				Name:   "extract",
				Status: "succeeded",
				Output: map[string]string{"n": "2"},
			}},
		},
	}}
	run := &localrun.RunResult{
		Status: "succeeded",
		Tasks: []localrun.TaskResult{{
			Name:   "extract",
			Status: "failed",
			Output: map[string]string{"n": "1"},
		}},
	}

	failures, assertions := evaluateScenario(context.Background(), nil, scenario, run, nil, nil, nil, nil)
	require.Len(t, failures, len(assertions))
	require.Contains(t, failures, "run status: expected failed, got succeeded")
	require.Contains(t, failures, `task extract output[n]: expected "2", got "1"`)

	byName := map[string]CheckedAssertion{}
	for _, assertion := range assertions {
		byName[assertion.Name] = assertion
		require.False(t, assertion.Passed)
		require.NotNil(t, assertion.Expected)
		require.NotNil(t, assertion.Actual)
	}
	require.Equal(t, "failed", byName["run status"].Expected)
	require.Equal(t, "succeeded", byName["run status"].Actual)
	require.Equal(t, "2", byName["task extract output[n]"].Expected)
	require.Equal(t, "1", byName["task extract output[n]"].Actual)
}

func TestEvaluateScenarioMetricFailuresKeepLabelSets(t *testing.T) {
	value := func(v float64) *float64 { return &v }
	scenario := ResolvedScenario{Scenario: Scenario{
		Name: "metrics",
		Expect: Expectation{
			RunStatus: "succeeded",
			Metrics: []MetricExpectation{
				{Name: "caesium_job_runs_total", Labels: map[string]string{"job_id": "a", "status": "succeeded"}, Value: value(1)},
				{Name: "caesium_job_runs_total", Labels: map[string]string{"job_id": "b", "status": "failed"}, Value: value(1)},
				{Name: "caesium_lineage_events_emitted_total", Labels: map[string]string{"event_type": "START", "status": "success"}, Delta: value(2)},
				{Name: "caesium_lineage_events_emitted_total", Labels: map[string]string{"event_type": "COMPLETE", "status": "success"}, Delta: value(2)},
			},
		},
	}}
	run := &localrun.RunResult{Status: "succeeded"}
	observations := []MetricObservation{
		{Name: "caesium_job_runs_total", Labels: map[string]string{"job_id": "a", "status": "succeeded"}, Value: 0},
		{Name: "caesium_job_runs_total", Labels: map[string]string{"job_id": "b", "status": "failed"}, Value: 3},
		{Name: "caesium_lineage_events_emitted_total", Labels: map[string]string{"event_type": "START", "status": "success"}, Delta: 0},
		{Name: "caesium_lineage_events_emitted_total", Labels: map[string]string{"event_type": "COMPLETE", "status": "success"}, Delta: 1},
	}

	_, assertions := evaluateScenario(context.Background(), nil, scenario, run, nil, observations, nil, nil)
	require.Len(t, assertions, 4)
	byName := map[string]CheckedAssertion{}
	for _, assertion := range assertions {
		_, dup := byName[assertion.Name]
		require.False(t, dup, "duplicate assertion name %s", assertion.Name)
		byName[assertion.Name] = assertion
	}
	require.Equal(t, 0.0, byName["metric caesium_job_runs_total {job_id=a, status=succeeded} value"].Actual)
	require.Equal(t, 3.0, byName["metric caesium_job_runs_total {job_id=b, status=failed} value"].Actual)
	require.Equal(t, 0.0, byName["metric caesium_lineage_events_emitted_total {event_type=START, status=success} delta"].Actual)
	require.Equal(t, 1.0, byName["metric caesium_lineage_events_emitted_total {event_type=COMPLETE, status=success} delta"].Actual)
}

func TestEvaluateScenarioImpactActualIsObservedNames(t *testing.T) {
	db := seedImpactGraph(t, harnessNamespace)
	run := &localrun.RunResult{Status: "succeeded"}
	matched := ResolvedScenario{Scenario: Scenario{
		Name: "impact",
		Expect: Expectation{
			RunStatus: "succeeded",
			Lineage: &LineageExpectation{
				Impact: []ImpactExpectation{{
					Dataset:    "harness-job.extract.output",
					Downstream: []string{"harness-job.transform.output"},
				}},
			},
		},
	}}
	failures, assertions := evaluateScenario(context.Background(), db, matched, run, nil, nil, nil, nil)
	require.Empty(t, failures)
	require.Empty(t, assertions)

	mismatched := matched
	mismatched.Scenario.Expect.Lineage = &LineageExpectation{
		Impact: []ImpactExpectation{{
			Dataset:    "harness-job.extract.output",
			Downstream: []string{"harness-job.missing.output"},
		}},
	}
	_, assertions = evaluateScenario(context.Background(), db, mismatched, run, nil, nil, nil, nil)
	require.Len(t, assertions, 1)
	require.Equal(t, "lineage impact[harness-job.extract.output]", assertions[0].Name)
	require.Equal(t, []string{"harness-job.missing.output"}, assertions[0].Expected)
	require.Equal(t, []string{"harness-job.transform.output"}, assertions[0].Actual)

	_, queryAssertions := evaluateScenario(context.Background(), nil, mismatched, run, nil, nil, nil, nil)
	require.Len(t, queryAssertions, 1)
	require.Equal(t, "lineage impact[harness-job.extract.output] query", queryAssertions[0].Name)
	require.Equal(t, "ok", queryAssertions[0].Expected)
	require.Contains(t, queryAssertions[0].Actual, "no database available")
	require.NotEqual(t, assertions[0].Actual, queryAssertions[0].Actual)
}

func TestEvaluateScenarioPassHasNoAssertions(t *testing.T) {
	scenario := ResolvedScenario{Scenario: Scenario{
		Name: "ok",
		Expect: Expectation{
			RunStatus: "succeeded",
			Tasks:     []TaskExpectation{{Name: "extract", Status: "succeeded"}},
		},
	}}
	run := &localrun.RunResult{
		Status: "succeeded",
		Tasks:  []localrun.TaskResult{{Name: "extract", Status: "succeeded"}},
	}
	failures, assertions := evaluateScenario(context.Background(), nil, scenario, run, nil, nil, nil, nil)
	require.Empty(t, failures)
	require.Empty(t, assertions)
}
