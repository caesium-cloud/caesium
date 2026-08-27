package jobdef

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cache_chain_test.go covers the `cache.chain` and `cache.ttl: never` keys
// (infra-deploy A1). It sits beside cache_config_test.go, which covers the
// pinDigests/digestTTL layering the same resolver implements.

func TestResolveCacheConfig_ChainDefaultsToTransitive(t *testing.T) {
	// Nothing declares a chain anywhere: the resolved config must name the
	// transitive mode explicitly, so every hash construction site can pass it
	// through without a nil/"" special case.
	cfg := ResolveCacheConfig(nil, nil, true, time.Hour, false, envDigestTTL)
	assert.Equal(t, CacheChainTransitive, cfg.Chain)

	// A cache map that says nothing about chain keeps the default.
	step := map[string]any{"ttl": "30m"}
	assert.Equal(t, CacheChainTransitive, ResolveCacheConfig(step, nil, false, time.Hour, false, envDigestTTL).Chain)

	// So does the bool form.
	assert.Equal(t, CacheChainTransitive, ResolveCacheConfig(true, nil, false, time.Hour, false, envDigestTTL).Chain)
}

func TestResolveCacheConfig_ChainStepLevel(t *testing.T) {
	step := map[string]any{"version": 1, "chain": "values"}
	cfg := ResolveCacheConfig(step, nil, false, time.Hour, false, envDigestTTL)
	assert.True(t, cfg.Enabled, "a cache map implies caching is enabled")
	assert.Equal(t, CacheChainValues, cfg.Chain)
}

func TestResolveCacheConfig_ChainJobLevelCascades(t *testing.T) {
	// Open Question 1: chain flows through the same job -> step layering as
	// pinDigests, so metadata.cache.chain is a job-wide default.
	job := map[string]any{"chain": "values"}
	cfg := ResolveCacheConfig(nil, job, true, time.Hour, false, envDigestTTL)
	assert.Equal(t, CacheChainValues, cfg.Chain, "job-level chain should be the default for steps that omit it")

	// A step map that omits chain inherits the job default rather than resetting.
	step := map[string]any{"ttl": "5m"}
	assert.Equal(t, CacheChainValues,
		ResolveCacheConfig(step, job, true, time.Hour, false, envDigestTTL).Chain)
}

func TestResolveCacheConfig_StepChainOverridesJob(t *testing.T) {
	job := map[string]any{"chain": "values"}
	step := map[string]any{"chain": "transitive"}
	cfg := ResolveCacheConfig(step, job, false, time.Hour, false, envDigestTTL)
	assert.Equal(t, CacheChainTransitive, cfg.Chain, "an explicit step-level chain must override the job default")
}

func TestResolveCacheConfig_UnknownChainFallsBackToInherited(t *testing.T) {
	// Validate() rejects this at lint/apply time; if one ever reaches the
	// resolver it must fail SAFE — transitive is today's behaviour and can only
	// cause an extra re-run, never a stale hit.
	step := map[string]any{"chain": "nonsense"}
	assert.Equal(t, CacheChainTransitive, ResolveCacheConfig(step, nil, true, time.Hour, false, envDigestTTL).Chain)

	job := map[string]any{"chain": "values"}
	assert.Equal(t, CacheChainValues,
		ResolveCacheConfig(step, job, true, time.Hour, false, envDigestTTL).Chain,
		"an unknown step chain keeps the inherited job value rather than silently resetting it")
}

func TestResolveCacheConfig_TTLNever(t *testing.T) {
	// `ttl: never` must beat a non-zero inherited default, or an apply step keyed
	// on a fingerprint would still expire on the CAESIUM_CACHE_TTL wall clock.
	step := map[string]any{"version": 1, "ttl": "never"}
	cfg := ResolveCacheConfig(step, nil, true, 24*time.Hour, false, envDigestTTL)
	assert.True(t, cfg.TTLNever)
	assert.Equal(t, 24*time.Hour, cfg.TTL, "the inherited TTL is left untouched; TTLNever is what suppresses expiry")
}

func TestResolveCacheConfig_TTLNeverJobLevelStepOverrides(t *testing.T) {
	job := map[string]any{"ttl": "never"}
	assert.True(t, ResolveCacheConfig(nil, job, true, time.Hour, false, envDigestTTL).TTLNever)

	// An explicit step duration must clear the job-level never.
	step := map[string]any{"ttl": "15m"}
	cfg := ResolveCacheConfig(step, job, true, time.Hour, false, envDigestTTL)
	assert.False(t, cfg.TTLNever, "an explicit step duration must clear an inherited ttl: never")
	assert.Equal(t, 15*time.Minute, cfg.TTL)
}

func TestResolveCacheConfig_UnparseableTTLStillIgnored(t *testing.T) {
	// Pre-existing behaviour, deliberately unchanged: an unparseable ttl is
	// silently ignored rather than erroring, so manifests that apply today keep
	// applying. Only the literal "never" gained a meaning.
	step := map[string]any{"ttl": "12 parsecs"}
	cfg := ResolveCacheConfig(step, nil, true, time.Hour, false, envDigestTTL)
	assert.Equal(t, time.Hour, cfg.TTL)
	assert.False(t, cfg.TTLNever)
}

func TestNormalizeCacheChain(t *testing.T) {
	for _, raw := range []string{"transitive", " transitive ", "TRANSITIVE"} {
		got, ok := NormalizeCacheChain(raw)
		assert.True(t, ok, raw)
		assert.Equal(t, CacheChainTransitive, got, raw)
	}
	for _, raw := range []string{"values", "Values"} {
		got, ok := NormalizeCacheChain(raw)
		assert.True(t, ok, raw)
		assert.Equal(t, CacheChainValues, got, raw)
	}
	for _, raw := range []string{"", "value", "none", "outputs"} {
		_, ok := NormalizeCacheChain(raw)
		assert.False(t, ok, raw)
	}
}

func TestValidate_RejectsUnknownCacheChain(t *testing.T) {
	def := func(stepCache, metaCache interface{}) *Definition {
		return &Definition{
			APIVersion: APIVersionV1,
			Kind:       KindJob,
			Metadata:   Metadata{Alias: "chain-job", Cache: metaCache},
			Trigger:    Trigger{Type: TriggerCron, Configuration: map[string]any{"cron": "* * * * *"}},
			Steps: []Step{{
				Name:   "only",
				Image:  "alpine:3.23",
				Engine: EngineDocker,
				Type:   StepTypeTask,
				Cache:  stepCache,
			}},
		}
	}

	require.NoError(t, def(map[string]any{"chain": "values"}, nil).Validate())
	require.NoError(t, def(map[string]any{"chain": "transitive"}, nil).Validate())
	require.NoError(t, def(map[string]any{"ttl": "never"}, nil).Validate())
	require.NoError(t, def(true, nil).Validate())

	err := def(map[string]any{"chain": "value"}, nil).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `steps[0].cache.chain "value"`)

	err = def(nil, map[string]any{"chain": "transitively"}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.cache.chain")

	err = def(map[string]any{"chain": 7}, nil).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a string")
}

// TestParse_CacheChainRoundTrip proves the key survives the real YAML path:
// `cache` is an interface{}, so a chain declared in a manifest only reaches the
// resolver if yaml.v3 decodes the block as map[string]any.
func TestParse_CacheChainRoundTrip(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Job
metadata:
  alias: chain-yaml
  cache:
    chain: transitive
trigger:
  type: cron
  cron: "* * * * *"
steps:
  - name: only
    image: alpine:3.23
    cache:
      version: 1
      chain: values
      ttl: never
`)
	def, err := Parse(manifest)
	require.NoError(t, err)

	cfg := ResolveCacheConfig(def.Steps[0].Cache, def.Metadata.Cache, false, time.Hour, false, envDigestTTL)
	assert.Equal(t, CacheChainValues, cfg.Chain)
	assert.True(t, cfg.TTLNever)
	assert.Equal(t, 1, cfg.Version)
}
