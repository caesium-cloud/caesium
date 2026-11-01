package localrun

import (
	"context"
	"sync"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestMergeRunParamsLayersDefaultsAndExplicitOverrides(t *testing.T) {
	def := &schema.Definition{
		Trigger: schema.Trigger{
			DefaultParams: map[string]string{"date": "2026-09-14", "region": "east"},
		},
	}

	merged, err := mergeRunParams(def, map[string]string{"date": "2026-09-15", "note": "a,b"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"date":   "2026-09-15",
		"region": "east",
		"note":   "a,b",
	}, merged)

	// An explicit empty string is present and wins. A nil explicit map still
	// receives defaults, matching an empty trigger POST.
	cleared, err := mergeRunParams(def, map[string]string{"date": ""})
	require.NoError(t, err)
	require.Equal(t, "", cleared["date"])
	require.Equal(t, "east", cleared["region"])

	defaultsOnly, err := mergeRunParams(def, nil)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"date": "2026-09-14", "region": "east"}, defaultsOnly)

	none, err := mergeRunParams(&schema.Definition{}, nil)
	require.NoError(t, err)
	require.Nil(t, none)
}

func TestMergeRunParamsReadsConfigurationDefaultsAndStringifies(t *testing.T) {
	def := &schema.Definition{
		Trigger: schema.Trigger{
			Configuration: map[string]any{
				"defaultParams": map[string]any{
					"date":  "2026-09-14",
					"count": 2,
					"ratio": 2.0,
				},
			},
		},
	}

	merged, err := mergeRunParams(def, map[string]string{"date": "2026-09-15"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"date":  "2026-09-15",
		"count": "2",
		"ratio": "2",
	}, merged)

	// A non-empty top-level map replaces configuration.defaultParams entirely,
	// which is what the importer persists and the trigger then reads.
	replaced := &schema.Definition{
		Trigger: schema.Trigger{
			DefaultParams: map[string]string{"date": "from-top"},
			Configuration: map[string]any{
				"defaultParams": map[string]any{"date": "from-config", "region": "east"},
			},
		},
	}
	merged, err = mergeRunParams(replaced, nil)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"date": "from-top"}, merged)

	bad := &schema.Definition{
		Trigger: schema.Trigger{
			Configuration: map[string]any{"defaultParams": "nope"},
		},
	}
	_, err = mergeRunParams(bad, nil)
	require.EqualError(t, err, "trigger.configuration.defaultParams must be a map of string keys and string values")
}

func TestLocalRunAppliesTriggerDefaultParams(t *testing.T) {
	env := runParamJob(t, paramDateManifest("2026-09-14", false), nil)
	require.Equal(t, "2026-09-14", env["DAY"])
	require.Equal(t, "2026-09-14", env["CAESIUM_PARAM_DATE"])
	require.NotContains(t, env["DAY"], "${CAESIUM_PARAM_")
}

func TestLocalRunParamOverridesDefault(t *testing.T) {
	env := runParamJob(t, paramDateManifest("2026-09-14", false), map[string]string{"date": "2026-09-15"})
	require.Equal(t, "2026-09-15", env["DAY"])
	require.Equal(t, "2026-09-15", env["CAESIUM_PARAM_DATE"])
}

func TestLocalRunConfigurationDefaultParamsAppliedWhenTopLevelEmpty(t *testing.T) {
	env := runParamJob(t, paramDateManifest("2026-09-14", true), nil)
	require.Equal(t, "2026-09-14", env["DAY"])
	require.Equal(t, "2026-09-14", env["CAESIUM_PARAM_DATE"])
	require.Equal(t, "2", env["CAESIUM_PARAM_COUNT"])
}

func TestLocalRunMissingParamStillFailsInterpolation(t *testing.T) {
	engine := &envCaptureEngine{fakeLocalEngine: newFakeLocalEngine()}
	runner := New(Config{EngineFactory: func(context.Context) atom.Engine { return engine }})

	manifest := `apiVersion: v1
kind: Job
metadata:
  alias: missing-param
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    env:
      DAY: "${CAESIUM_PARAM_DATE}"
`
	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)

	_, err = runner.RunWithResult(context.Background(), def)
	require.Error(t, err)
	require.Contains(t, err.Error(), "interpolate run params: env DAY: unresolved ${CAESIUM_PARAM_DATE}")
	require.Empty(t, engine.captured(), "container must not start when a param ref is unresolved")
}

func TestLocalRunRejectsNonMapConfigurationDefaultParams(t *testing.T) {
	engine := newFakeLocalEngine()
	runner := New(Config{EngineFactory: func(context.Context) atom.Engine { return engine }})
	def := &schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: "bad-defaults"},
		Trigger: schema.Trigger{
			Type:          "cron",
			Configuration: map[string]any{"cron": "0 2 * * *", "defaultParams": "nope"},
		},
		Steps: []schema.Step{{
			Name:    "extract",
			Type:    schema.StepTypeTask,
			Engine:  "docker",
			Image:   "alpine:3.23",
			Command: []string{"true"},
		}},
	}

	_, err := runner.RunWithResult(context.Background(), def)
	require.EqualError(t, err, "run params: trigger.configuration.defaultParams must be a map of string keys and string values")
}

// paramDateManifest is the issue's shape: DAY is interpolated from the date
// param. configurationOnly places the defaults inside trigger.configuration,
// which is where they live after import when the top-level field is empty.
func paramDateManifest(date string, configurationOnly bool) string {
	defaults := "  defaultParams:\n    date: \"" + date + "\"\n"
	configuration := "  configuration:\n    cron: \"0 2 * * *\"\n"
	if configurationOnly {
		defaults = ""
		configuration = "  configuration:\n    cron: \"0 2 * * *\"\n    defaultParams:\n      date: \"" + date + "\"\n      count: 2\n"
	}
	return `apiVersion: v1
kind: Job
metadata:
  alias: dated-extract
trigger:
  type: cron
` + configuration + defaults + `steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "true"]
    env:
      DAY: "${CAESIUM_PARAM_DATE}"
`
}

func runParamJob(t *testing.T, manifest string, params map[string]string) map[string]string {
	t.Helper()
	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)

	engine := &envCaptureEngine{fakeLocalEngine: newFakeLocalEngine()}
	runner := New(Config{
		Params:        params,
		EngineFactory: func(context.Context) atom.Engine { return engine },
	})
	result, err := runner.RunWithResult(context.Background(), def)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result.Status)

	envs := engine.captured()
	require.Len(t, envs, 1)
	return envs[0]
}

// envCaptureEngine records the container env the local executor hands to
// Create, which is after ${CAESIUM_PARAM_*} interpolation and CAESIUM_PARAM_*
// injection.
type envCaptureEngine struct {
	*fakeLocalEngine
	mu   sync.Mutex
	envs []map[string]string
}

func (e *envCaptureEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	e.mu.Lock()
	cloned := make(map[string]string, len(req.Spec.Env))
	for key, value := range req.Spec.Env {
		cloned[key] = value
	}
	e.envs = append(e.envs, cloned)
	e.mu.Unlock()
	return e.fakeLocalEngine.Create(req)
}

func (e *envCaptureEngine) captured() []map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]map[string]string, len(e.envs))
	copy(out, e.envs)
	return out
}
