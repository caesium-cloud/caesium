package job

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cache_chain_test.go covers the LOCAL executor's half of infra-deploy A3: the
// resolved cache.chain reaching cache.HashInput through the single construction
// site every step and every fan-out instance shares (buildTaskHashInput).
//
// The cross-run, cross-lane behaviour — a values-mode step actually reported
// `cached` by a live server after its predecessor re-ran — is asserted end to end
// in test/cache_chain_test.go against the real server and CLI.

// chainArgs is the hash input for a `mid` step consuming one `upstream`
// predecessor, parameterized by the predecessor's own identity hash and by the
// output value it published.
func chainArgs(chain, predHash, predOutput string) taskHashInputArgs {
	return taskHashInputArgs{
		JobAlias:           "chain-job",
		TaskName:           "mid",
		Image:              "alpine:3.23",
		Command:            []string{"sh", "-c", "true"},
		Env:                map[string]string{"STATIC": "1"},
		PredecessorHashes:  []string{predHash},
		PredecessorOutputs: map[string]map[string]string{"upstream": {"token": predOutput}},
		RunParams:          map[string]string{"stamp": "irrelevant-to-mid"},
		CacheVersion:       1,
		Chain:              chain,
	}
}

// TestLocalLane_ValuesChainSurvivesPredecessorRerun is the scenario the feature
// exists for: `upstream` re-ran (its own identity moved because a git ref or a
// run param moved) but published the same output. Under chain: values `mid`'s
// key is unchanged, so the second run is a cache hit.
func TestLocalLane_ValuesChainSurvivesPredecessorRerun(t *testing.T) {
	first := buildTaskHashInput(chainArgs(jobdefschema.CacheChainValues, "hash-run-1", "same-value")).Compute()
	second := buildTaskHashInput(chainArgs(jobdefschema.CacheChainValues, "hash-run-2", "same-value")).Compute()

	assert.Equal(t, first, second,
		"under chain: values a predecessor that re-ran with unchanged outputs must leave the consumer's key intact")
}

// TestLocalLane_ValuesChainStillBustsOnChangedOutput is the other half: outputs
// still chain, so a changed upstream VALUE re-runs the consumer even though its
// own definition is untouched.
func TestLocalLane_ValuesChainStillBustsOnChangedOutput(t *testing.T) {
	before := buildTaskHashInput(chainArgs(jobdefschema.CacheChainValues, "hash-run-1", "v1")).Compute()
	after := buildTaskHashInput(chainArgs(jobdefschema.CacheChainValues, "hash-run-1", "v2")).Compute()

	assert.NotEqual(t, before, after,
		"a changed predecessor OUTPUT must still invalidate a values-mode consumer")
}

// TestLocalLane_TransitiveChainStillCascades pins the default: without the new
// key, a predecessor re-run cascades exactly as it always has. This is the
// behaviour every existing pipeline depends on.
func TestLocalLane_TransitiveChainStillCascades(t *testing.T) {
	for _, chain := range []string{"", jobdefschema.CacheChainTransitive} {
		first := buildTaskHashInput(chainArgs(chain, "hash-run-1", "same-value")).Compute()
		second := buildTaskHashInput(chainArgs(chain, "hash-run-2", "same-value")).Compute()
		assert.NotEqual(t, first, second,
			"chain %q must keep cascading predecessor identity", chain)
	}

	// And an unset Chain must hash identically to an explicit "transitive" —
	// ResolveCacheConfig now always sets the latter, and existing cache entries
	// were written by callers that set neither.
	unset := buildTaskHashInput(chainArgs("", "hash-run-1", "v1")).Compute()
	explicit := buildTaskHashInput(chainArgs(jobdefschema.CacheChainTransitive, "hash-run-1", "v1")).Compute()
	assert.Equal(t, unset, explicit,
		"an explicit transitive chain must not change any existing cache key")
}

// TestLocalLane_ChainReachesPersistedBlob: `caesium why` reads the blob, so the
// mode has to be recorded on the way through the local construction site.
func TestLocalLane_ChainReachesPersistedBlob(t *testing.T) {
	in := buildTaskHashInput(chainArgs(jobdefschema.CacheChainValues, "hash-run-1", "v1"))
	require.Equal(t, cache.ChainValues, in.Chain)

	blob, err := in.CanonicalJSON(in.Compute())
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"chain":"values"`)
}
