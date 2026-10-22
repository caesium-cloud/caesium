package cache

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/stretchr/testify/require"
)

// TestCompute_KubernetesMappingOnlyKeysMappedTasks pins identity-and-access
// C1: an absent or zero namespace mapping hashes byte-identically to the
// pre-C1 key (no CacheVersion bump), and any mapping change re-keys.
func TestCompute_KubernetesMappingOnlyKeysMappedTasks(t *testing.T) {
	zero := baseInput()
	zero.KubernetesTarget = &atom.KubeTarget{}
	require.Equal(t, goldenStringFormHash, zero.Compute(), "a zero target is the legacy default")
	blob, err := zero.CanonicalJSON(zero.Compute())
	require.NoError(t, err)
	plain, err := baseInput().CanonicalJSON(baseInput().Compute())
	require.NoError(t, err)
	require.JSONEq(t, string(plain), string(blob), "the blob is unchanged too")
	require.NotContains(t, string(blob), "kubernetesTarget")

	mapped := baseInput()
	mapped.KubernetesTarget = &atom.KubeTarget{Namespace: "caesium-marketing"}
	remapped := baseInput()
	remapped.KubernetesTarget = &atom.KubeTarget{Namespace: "caesium-marketing-v2"}
	account := baseInput()
	account.KubernetesTarget = &atom.KubeTarget{Namespace: "caesium-marketing", ServiceAccountName: "marketing-jobs"}
	secretNS := baseInput()
	secretNS.KubernetesSecretNamespace = "caesium-marketing"
	keys := map[string]string{}
	for name, input := range map[string]HashInput{"zero": zero, "mapped": mapped, "remapped": remapped, "account": account, "secretNS": secretNS} {
		key := input.Compute()
		for other, seen := range keys {
			require.NotEqual(t, seen, key, "%s and %s must not share a key", name, other)
		}
		keys[name] = key
	}

	blob, err = account.CanonicalJSON(account.Compute())
	require.NoError(t, err)
	var decoded HashInputBlob
	require.NoError(t, json.Unmarshal(blob, &decoded))
	require.Equal(t, account.KubernetesTarget, decoded.KubernetesTarget)
	blob, err = secretNS.CanonicalJSON(secretNS.Compute())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(blob, &decoded))
	require.Equal(t, "caesium-marketing", decoded.KubernetesSecretNamespace)
}
