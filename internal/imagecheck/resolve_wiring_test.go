package imagecheck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pullingFakeClient is a fake Docker ImageAPIClient whose inspect result can
// change after a pull, so the three-tier Docker resolution (local daemon ->
// registry -> authenticated pull) can be driven deterministically.
type pullingFakeClient struct {
	client.ImageAPIClient
	inspectErr    error
	afterPull     image.InspectResponse
	pulled        bool
	pullCalls     int
	pullOpts      image.PullOptions
	inspectBefore image.InspectResponse
}

func (f *pullingFakeClient) ImageInspect(_ context.Context, _ string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	if f.pulled {
		return f.afterPull, nil
	}
	if f.inspectErr != nil {
		return image.InspectResponse{}, f.inspectErr
	}
	return f.inspectBefore, nil
}

func (f *pullingFakeClient) ImagePull(_ context.Context, _ string, opts image.PullOptions) (io.ReadCloser, error) {
	f.pullCalls++
	f.pullOpts = opts
	f.pulled = true
	return io.NopCloser(strings.NewReader("")), nil
}

func TestNewResolver_WiresEveryEngine(t *testing.T) {
	r := NewResolver()
	for _, engine := range []models.AtomEngine{models.AtomEngineDocker, models.AtomEnginePodman, models.AtomEngineKubernetes} {
		assert.NotNil(t, r.byEngine[engine], "engine %s must have a DigestFunc wired by default", engine)
	}
}

func TestNewResolver_PodmanAndKubernetesResolveViaRegistry(t *testing.T) {
	// The engine-independent registry client is what Podman and Kubernetes
	// resolve through — no engine daemon involved. Drive it against the v2
	// emulator (bearer-protected) with credentials from the resolver's source.
	reg := newFakeRegistry(t)
	reg.auth = "bearer"
	r := NewResolver(WithCredentialSource(staticCredentials(reg.username, reg.password)))

	ref := reg.host() + "/private/app:1.0"
	for _, engine := range []models.AtomEngine{models.AtomEnginePodman, models.AtomEngineKubernetes} {
		got, err := r.Resolve(context.Background(), engine, ref, 0)
		require.NoError(t, err, engine)
		assert.Equal(t, testDigest, got, engine)
	}
	assert.True(t, reg.obs().tokenHadBasic, "credentials from the resolver's source must reach the token exchange")
}

func TestNewResolver_DefaultRegistryClientUsesResolverCredentials(t *testing.T) {
	// The default wiring (no WithRegistryClient) must consult SetCredentials
	// — including one installed AFTER construction, which is how the server
	// wires it (Default() may be built before the secret providers are).
	reg := newFakeRegistry(t)
	reg.auth = "basic"
	r := NewResolver()

	ref := reg.host() + "/private/app:1.0"
	_, err := r.Resolve(context.Background(), models.AtomEnginePodman, ref, 0)
	assert.ErrorIs(t, err, ErrDigestUnavailable, "before credentials are installed a basic-auth registry is unresolvable")

	r.SetCredentials(staticCredentials(reg.username, reg.password))
	got, err := r.Resolve(context.Background(), models.AtomEnginePodman, ref, 0)
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	// The same source feeds the Kubernetes engine.
	got, err = r.Resolve(context.Background(), models.AtomEngineKubernetes, ref, 0)
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)
}

func TestDockerResolve_PrefersLocalDaemon(t *testing.T) {
	cli := &pullingFakeClient{inspectBefore: image.InspectResponse{
		ID:          "sha256:config",
		RepoDigests: []string{"registry.example.com/app@sha256:local"},
	}}
	registryCalled := false
	registryResolve := func(_ context.Context, _ string) (string, error) {
		registryCalled = true
		return "sha256:fromregistry", nil
	}

	got, err := dockerResolve(context.Background(), cli, registryResolve, noCredentials, "registry.example.com/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, "sha256:local", got, "an image present in the daemon resolves locally")
	assert.False(t, registryCalled, "no registry round-trip when the daemon has the image")
	assert.Equal(t, 0, cli.pullCalls, "no pull when the daemon has the image")
}

func TestDockerResolve_FallsBackToRegistryWithoutPulling(t *testing.T) {
	cli := &pullingFakeClient{inspectErr: errors.New("No such image: registry.example.com/app:1.0")}
	registryResolve := func(_ context.Context, ref string) (string, error) {
		assert.Equal(t, "registry.example.com/app:1.0", ref)
		return "sha256:fromregistry", nil
	}

	got, err := dockerResolve(context.Background(), cli, registryResolve, noCredentials, "registry.example.com/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, "sha256:fromregistry", got, "an absent image resolves via the registry manifest")
	assert.Equal(t, 0, cli.pullCalls, "a registry HEAD must not pull the image")
}

func TestDockerResolve_FallsBackToAuthenticatedPull(t *testing.T) {
	cli := &pullingFakeClient{
		inspectErr: errors.New("No such image"),
		afterPull: image.InspectResponse{
			ID:          "sha256:config",
			RepoDigests: []string{"registry.example.com/app@sha256:pulled"},
		},
	}
	registryResolve := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("registry does not serve manifest HEAD")
	}
	creds := staticCredentials("puller", "pull:pass")

	got, err := dockerResolve(context.Background(), cli, registryResolve, creds, "registry.example.com/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, "sha256:pulled", got)
	assert.Equal(t, 1, cli.pullCalls, "the pull is the last resort, tried exactly once")

	// The pull carried the operator's credentials as Docker RegistryAuth.
	require.NotEmpty(t, cli.pullOpts.RegistryAuth, "RegistryAuth must be set when credentials exist for the registry")
	raw, err := base64.URLEncoding.DecodeString(cli.pullOpts.RegistryAuth)
	require.NoError(t, err)
	var auth struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}
	require.NoError(t, json.Unmarshal(raw, &auth))
	assert.Equal(t, "puller", auth.Username)
	assert.Equal(t, "pull:pass", auth.Password)
	assert.Equal(t, "registry.example.com", auth.ServerAddress)
}

func TestDockerResolve_AnonymousPullWhenNoCredentials(t *testing.T) {
	cli := &pullingFakeClient{
		inspectErr: errors.New("No such image"),
		afterPull:  image.InspectResponse{ID: "sha256:pulledconfig"},
	}
	registryResolve := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("unreachable")
	}

	got, err := dockerResolve(context.Background(), cli, registryResolve, noCredentials, "app:1.0")
	require.NoError(t, err)
	assert.Equal(t, "sha256:pulledconfig", got)
	assert.Empty(t, cli.pullOpts.RegistryAuth, "no credentials -> anonymous pull, exactly the pre-existing behaviour")
}

func TestDockerResolve_CredentialLookupErrorAbortsPull(t *testing.T) {
	cli := &pullingFakeClient{inspectErr: errors.New("No such image")}
	registryResolve := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("unreachable")
	}
	creds := func(_ context.Context, _ string) (Credentials, bool, error) {
		return Credentials{}, false, errors.New("vault sealed")
	}

	_, err := dockerResolve(context.Background(), cli, registryResolve, creds, "registry.example.com/app:1.0")
	require.Error(t, err)
	assert.Equal(t, 0, cli.pullCalls, "a failing credential source must not degrade to an anonymous pull that would mask the misconfiguration")
}
