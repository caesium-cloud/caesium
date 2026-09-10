package env

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryAuthDecode(t *testing.T) {
	var r RegistryAuth
	require.NoError(t, r.Decode(" GHCR.io = secret://env/GHCR_PULL , registry.example.com:5000=secret://vault/kv/data/registry#creds,, https://index.docker.io/v1/=secret://k8s/regcred?key=.dockerconfigjson "))
	assert.Equal(t, RegistryAuth{
		"ghcr.io":                     "secret://env/GHCR_PULL",
		"registry.example.com:5000":   "secret://vault/kv/data/registry#creds",
		"https://index.docker.io/v1/": "secret://k8s/regcred?key=.dockerconfigjson",
	}, r)
}

func TestRegistryAuthDecode_Empty(t *testing.T) {
	var r RegistryAuth
	require.NoError(t, r.Decode("   "))
	assert.Nil(t, r)
	require.NoError(t, r.Decode(","))
	assert.Nil(t, r)
}

func TestRegistryAuthDecode_Invalid(t *testing.T) {
	for _, in := range []string{
		"ghcr.io",               // no "="
		"=secret://env/X",       // no host
		"ghcr.io=",              // no ref
		"ghcr.io=user:password", // literal credential, not a secret ref
		"ghcr.io=env://X",       // wrong scheme
		"ghcr.io=secret://env/A,ghcr.io=secret://env/B", // duplicate host
	} {
		var r RegistryAuth
		err := r.Decode(in)
		assert.Error(t, err, "Decode(%q)", in)
		if in == "ghcr.io=user:password" && err != nil {
			assert.NotContains(t, err.Error(), "password", "a mistakenly literal credential must not be echoed back")
		}
	}
}

func TestProcess_RegistryAuth(t *testing.T) {
	t.Setenv("CAESIUM_REGISTRY_AUTH", "ghcr.io=secret://env/GHCR_PULL")
	require.NoError(t, Process())
	assert.Equal(t, RegistryAuth{"ghcr.io": "secret://env/GHCR_PULL"}, Variables().RegistryAuth)

	t.Setenv("CAESIUM_REGISTRY_AUTH", "ghcr.io=user:password")
	err := Process()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "password")
}
