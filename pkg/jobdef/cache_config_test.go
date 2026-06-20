package jobdef

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// envDigestTTL is the default fed to ResolveCacheConfig in these tests; the
// specific value is irrelevant to the pinDigests assertions.
const envDigestTTL = 5 * time.Minute

func TestResolveCacheConfig_PinDigestsEnvDefault(t *testing.T) {
	// With no job/step cache override, the env default flows through unchanged.
	cfg := ResolveCacheConfig(nil, nil, true, time.Hour, true, envDigestTTL)
	assert.True(t, cfg.Enabled)
	assert.True(t, cfg.PinDigests, "env default pinDigests should carry when nothing overrides it")

	off := ResolveCacheConfig(nil, nil, true, time.Hour, false, envDigestTTL)
	assert.False(t, off.PinDigests)
}

func TestResolveCacheConfig_PinDigestsJobLevel(t *testing.T) {
	job := map[string]any{"pinDigests": true}
	cfg := ResolveCacheConfig(nil, job, false, time.Hour, false, envDigestTTL)
	assert.True(t, cfg.Enabled, "a cache map implies caching is enabled")
	assert.True(t, cfg.PinDigests, "job-level pinDigests should enable digest pinning")
}

func TestResolveCacheConfig_StepOverridesJob(t *testing.T) {
	job := map[string]any{"pinDigests": true}
	step := map[string]any{"pinDigests": false}
	cfg := ResolveCacheConfig(step, job, false, time.Hour, false, envDigestTTL)
	assert.False(t, cfg.PinDigests, "step-level pinDigests:false should override a job-level true")
}

func TestResolveCacheConfig_JobOverridesEnv(t *testing.T) {
	// env says pin on; job explicitly turns it off.
	job := map[string]any{"pinDigests": false}
	cfg := ResolveCacheConfig(nil, job, true, time.Hour, true, envDigestTTL)
	assert.False(t, cfg.PinDigests, "explicit job-level pinDigests:false should override env default true")
}

func TestResolveCacheConfig_BoolFormInheritsPinDigests(t *testing.T) {
	// cache: true (bool form) does not mention pinDigests, so the env/job
	// default must be preserved rather than reset.
	cfg := ResolveCacheConfig(true, nil, false, time.Hour, true, envDigestTTL)
	assert.True(t, cfg.Enabled)
	assert.True(t, cfg.PinDigests, "bool-form cache must not clear an inherited pinDigests default")
}

func TestResolveCacheConfig_StepMapWithoutPinDigestsKeepsJobDefault(t *testing.T) {
	// A step cache map that omits pinDigests should not reset the job-level
	// default to false.
	job := map[string]any{"pinDigests": true}
	step := map[string]any{"ttl": "30m"}
	cfg := ResolveCacheConfig(step, job, false, time.Hour, false, envDigestTTL)
	assert.True(t, cfg.PinDigests, "step map omitting pinDigests should inherit the job default")
	assert.Equal(t, 30*time.Minute, cfg.TTL)
}

func TestResolveCacheConfig_DigestTTLEnvDefault(t *testing.T) {
	cfg := ResolveCacheConfig(nil, nil, true, time.Hour, true, envDigestTTL)
	assert.Equal(t, envDigestTTL, cfg.DigestTTL, "env digestTTL default should carry when nothing overrides it")
}

func TestResolveCacheConfig_DigestTTLZeroOverride(t *testing.T) {
	// digestTTL: 0 (numeric) must override the env default to zero so the job
	// re-resolves on every check — the immediate moved-tag-detection mode.
	job := map[string]any{"pinDigests": true, "digestTTL": 0}
	cfg := ResolveCacheConfig(nil, job, false, time.Hour, false, envDigestTTL)
	assert.Equal(t, time.Duration(0), cfg.DigestTTL, "numeric digestTTL:0 must override to zero")
}

func TestResolveCacheConfig_DigestTTLStringOverride(t *testing.T) {
	job := map[string]any{"pinDigests": true, "digestTTL": "30s"}
	cfg := ResolveCacheConfig(nil, job, false, time.Hour, false, envDigestTTL)
	assert.Equal(t, 30*time.Second, cfg.DigestTTL, "string digestTTL should parse as a duration")
}

func TestResolveCacheConfig_StepDigestTTLOverridesJob(t *testing.T) {
	job := map[string]any{"pinDigests": true, "digestTTL": "10m"}
	step := map[string]any{"digestTTL": 0}
	cfg := ResolveCacheConfig(step, job, false, time.Hour, false, envDigestTTL)
	assert.Equal(t, time.Duration(0), cfg.DigestTTL, "step digestTTL:0 should override the job-level value")
}

func TestResolveCacheConfig_DigestTTLOmittedKeepsDefault(t *testing.T) {
	// A cache map that doesn't mention digestTTL keeps the inherited default.
	job := map[string]any{"pinDigests": true}
	cfg := ResolveCacheConfig(nil, job, false, time.Hour, false, envDigestTTL)
	assert.Equal(t, envDigestTTL, cfg.DigestTTL, "omitted digestTTL inherits the env/job default")
}
