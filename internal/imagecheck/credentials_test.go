package imagecheck

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapResolver is a secret.Resolver backed by a map of ref -> value.
type mapResolver map[string]string

func (m mapResolver) Resolve(ctx context.Context, ref string) (string, error) {
	v, _, err := m.ResolveWithIdentity(ctx, ref)
	return v, err
}

func (m mapResolver) ResolveWithIdentity(_ context.Context, ref string) (string, secret.Identity, error) {
	v, ok := m[ref]
	if !ok {
		return "", secret.Identity{}, errors.New("secret " + ref + " not found")
	}
	return v, secret.Identity{}, nil
}

func TestCredentialsFromSecrets_LookupAndNormalisation(t *testing.T) {
	resolver := mapResolver{
		"secret://env/HUB":     "hubuser:hubpass",
		"secret://env/PRIVATE": "priv:p@ss:word",
		"secret://env/PORTED":  "ported:pw",
	}
	fn := CredentialsFromSecrets(map[string]string{
		"https://index.docker.io/v1/": "secret://env/HUB",
		"Registry.Example.com":        "secret://env/PRIVATE",
		"registry.example.com:5000":   "secret://env/PORTED",
	}, resolver)

	ctx := context.Background()

	// Every Docker Hub alias resolves to the Hub entry.
	for _, host := range []string{"docker.io", "index.docker.io", "registry-1.docker.io"} {
		creds, ok, err := fn(ctx, host)
		require.NoError(t, err, host)
		require.True(t, ok, host)
		assert.Equal(t, Credentials{Username: "hubuser", Password: "hubpass"}, creds, host)
	}

	// Exact host; the password keeps its colons.
	creds, ok, err := fn(ctx, "registry.example.com")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, Credentials{Username: "priv", Password: "p@ss:word"}, creds)

	// A key with a port wins over the bare-host key for that port...
	creds, ok, err = fn(ctx, "registry.example.com:5000")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ported", creds.Username)

	// ...and a bare-host key matches any other port.
	creds, ok, err = fn(ctx, "registry.example.com:6000")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "priv", creds.Username)

	// Unmapped hosts are anonymous, not an error.
	_, ok, err = fn(ctx, "ghcr.io")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestCredentialsFromSecrets_ResolutionErrorIsSurfacedAndRedacted(t *testing.T) {
	fn := CredentialsFromSecrets(map[string]string{"ghcr.io": "secret://env/MISSING"}, mapResolver{})
	_, _, err := fn(context.Background(), "ghcr.io")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret://env/MISSING", "the reference (not a value) is named so the operator can fix the mapping")

	fn = CredentialsFromSecrets(map[string]string{"ghcr.io": "secret://env/BAD"}, mapResolver{"secret://env/BAD": "no-colon-here"})
	_, _, err = fn(context.Background(), "ghcr.io")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "no-colon-here", "a malformed value must not be echoed")
}

func TestCredentialsFromSecrets_NilResolverWithMappingFails(t *testing.T) {
	fn := CredentialsFromSecrets(map[string]string{"ghcr.io": "secret://env/X"}, nil)
	_, _, err := fn(context.Background(), "ghcr.io")
	assert.Error(t, err)
}

func TestCredentialsFromSecrets_EmptyMappingIsAnonymous(t *testing.T) {
	fn := CredentialsFromSecrets(nil, nil)
	_, ok, err := fn(context.Background(), "ghcr.io")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestParseCredentialValue(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("k8suser:k8spass"))

	creds, err := ParseCredentialValue("user:pass", "ghcr.io")
	require.NoError(t, err)
	assert.Equal(t, Credentials{Username: "user", Password: "pass"}, creds)

	creds, err = ParseCredentialValue(`{"username":"ju","password":"jp"}`, "ghcr.io")
	require.NoError(t, err)
	assert.Equal(t, Credentials{Username: "ju", Password: "jp"}, creds)

	// Kubernetes .dockerconfigjson, keyed by the config.json Hub form.
	creds, err = ParseCredentialValue(`{"auths":{"https://index.docker.io/v1/":{"auth":"`+b64+`"}}}`, "docker.io")
	require.NoError(t, err)
	assert.Equal(t, Credentials{Username: "k8suser", Password: "k8spass"}, creds)

	// dockerconfigjson with explicit fields, matched on the bare host for a
	// ported reference.
	creds, err = ParseCredentialValue(`{"auths":{"registry.example.com":{"username":"eu","password":"ep"}}}`, "registry.example.com:5000")
	require.NoError(t, err)
	assert.Equal(t, Credentials{Username: "eu", Password: "ep"}, creds)

	for _, bad := range []string{
		"",
		"   ",
		"nocolon",
		":nouser",
		"{not json",
		`{"password":"only"}`,
		`{"auths":{"other.example.com":{"auth":"` + b64 + `"}}}`,
		`{"auths":{"ghcr.io":{"auth":"%%%not-base64"}}}`,
	} {
		_, err := ParseCredentialValue(bad, "ghcr.io")
		assert.Error(t, err, "ParseCredentialValue(%q)", bad)
		if err != nil && bad != "" {
			assert.NotContains(t, err.Error(), "nouser")
			assert.NotContains(t, err.Error(), "only")
		}
	}
}
