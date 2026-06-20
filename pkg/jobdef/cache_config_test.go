package jobdef

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveCacheConfig_PinDigestsEnvDefault(t *testing.T) {
	// With no job/step cache override, the env default flows through unchanged.
	cfg := ResolveCacheConfig(nil, nil, true, time.Hour, true)
	assert.True(t, cfg.Enabled)
	assert.True(t, cfg.PinDigests, "env default pinDigests should carry when nothing overrides it")

	off := ResolveCacheConfig(nil, nil, true, time.Hour, false)
	assert.False(t, off.PinDigests)
}

func TestResolveCacheConfig_PinDigestsJobLevel(t *testing.T) {
	job := map[string]any{"pinDigests": true}
	cfg := ResolveCacheConfig(nil, job, false, time.Hour, false)
	assert.True(t, cfg.Enabled, "a cache map implies caching is enabled")
	assert.True(t, cfg.PinDigests, "job-level pinDigests should enable digest pinning")
}

func TestResolveCacheConfig_StepOverridesJob(t *testing.T) {
	job := map[string]any{"pinDigests": true}
	step := map[string]any{"pinDigests": false}
	cfg := ResolveCacheConfig(step, job, false, time.Hour, false)
	assert.False(t, cfg.PinDigests, "step-level pinDigests:false should override a job-level true")
}

func TestResolveCacheConfig_JobOverridesEnv(t *testing.T) {
	// env says pin on; job explicitly turns it off.
	job := map[string]any{"pinDigests": false}
	cfg := ResolveCacheConfig(nil, job, true, time.Hour, true)
	assert.False(t, cfg.PinDigests, "explicit job-level pinDigests:false should override env default true")
}

func TestResolveCacheConfig_BoolFormInheritsPinDigests(t *testing.T) {
	// cache: true (bool form) does not mention pinDigests, so the env/job
	// default must be preserved rather than reset.
	cfg := ResolveCacheConfig(true, nil, false, time.Hour, true)
	assert.True(t, cfg.Enabled)
	assert.True(t, cfg.PinDigests, "bool-form cache must not clear an inherited pinDigests default")
}

func TestResolveCacheConfig_StepMapWithoutPinDigestsKeepsJobDefault(t *testing.T) {
	// A step cache map that omits pinDigests should not reset the job-level
	// default to false.
	job := map[string]any{"pinDigests": true}
	step := map[string]any{"ttl": "30m"}
	cfg := ResolveCacheConfig(step, job, false, time.Hour, false)
	assert.True(t, cfg.PinDigests, "step map omitting pinDigests should inherit the job default")
	assert.Equal(t, 30*time.Minute, cfg.TTL)
}
