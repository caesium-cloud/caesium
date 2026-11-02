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
