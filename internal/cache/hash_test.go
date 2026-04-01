package cache

import (
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseInput() HashInput {
	return HashInput{
		JobAlias: "my-job",
		TaskName: "my-task",
		Image:    "alpine:3.18",
		Command:  []string{"echo", "hello"},
		Env:      map[string]string{"FOO": "bar", "BAZ": "qux"},
		WorkDir:  "/app",
		Mounts: []container.Mount{
			{Type: container.MountTypeBind, Source: "/host", Target: "/container", ReadOnly: true},
		},
		PredecessorHashes:  []string{"abc123", "def456"},
		PredecessorOutputs: map[string]map[string]string{"step1": {"key": "val"}},
		RunParams:          map[string]string{"param1": "value1"},
		CacheVersion:       1,
	}
}

func TestCompute_Deterministic(t *testing.T) {
	h1 := baseInput().Compute()
	h2 := baseInput().Compute()
	assert.Equal(t, h1, h2, "same input should produce same hash")
	assert.Len(t, h1, 64, "SHA-256 hex digest should be 64 characters")
}

func TestCompute_DifferentJobAlias(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.JobAlias = "other-job"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentTaskName(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.TaskName = "other-task"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentImage(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Image = "ubuntu:22.04"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentCommand(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Command = []string{"echo", "world"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentEnv(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Env = map[string]string{"FOO": "changed", "BAZ": "qux"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentWorkDir(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.WorkDir = "/other"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentMounts(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Mounts = []container.Mount{
		{Type: container.MountTypeBind, Source: "/other", Target: "/container", ReadOnly: false},
	}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentPredecessorHashes(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.PredecessorHashes = []string{"abc123", "zzz999"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentRunParams(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.RunParams = map[string]string{"param1": "changed"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentCacheVersion(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.CacheVersion = 2
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_EnvOrderDoesNotMatter(t *testing.T) {
	a := HashInput{
		Env: map[string]string{"A": "1", "B": "2", "C": "3"},
	}
	b := HashInput{
		Env: map[string]string{"C": "3", "A": "1", "B": "2"},
	}
	assert.Equal(t, a.Compute(), b.Compute(), "env var order should not affect hash")
}

func TestCompute_PredecessorHashOrderDoesNotMatter(t *testing.T) {
	a := HashInput{
		PredecessorHashes: []string{"hash1", "hash2", "hash3"},
	}
	b := HashInput{
		PredecessorHashes: []string{"hash3", "hash1", "hash2"},
	}
	assert.Equal(t, a.Compute(), b.Compute(), "predecessor hash order should not affect hash")
}

func TestCompute_EmptyAndNilInputs(t *testing.T) {
	a := HashInput{}
	b := HashInput{
		Env:                nil,
		Mounts:             nil,
		PredecessorHashes:  nil,
		PredecessorOutputs: nil,
		RunParams:          nil,
		Command:            nil,
	}
	h1 := a.Compute()
	h2 := b.Compute()
	require.Equal(t, h1, h2, "empty and nil inputs should produce same hash")
	assert.Len(t, h1, 64)
}

func TestCompute_EmptyVsPopulatedDiffers(t *testing.T) {
	a := HashInput{}
	b := baseInput()
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentPredecessorOutputs(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.PredecessorOutputs = map[string]map[string]string{"step1": {"key": "different"}}
	assert.NotEqual(t, a.Compute(), b.Compute())
}
