package imagecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
)

// fakeRegistry emulates the slice of the Docker Registry HTTP API v2 the
// resolver drives: a manifest endpoint guarded by an optional auth scheme, a
// token endpoint for the bearer flow, and knobs for the response shape.
type fakeRegistry struct {
	srv *httptest.Server

	// auth is "" (anonymous), "bearer" or "basic".
	auth     string
	username string
	password string
	token    string

	// omitDigestHeader drops Docker-Content-Digest from the manifest
	// response so the client must fall back to hashing a GET body.
	omitDigestHeader bool
	// delay stalls every manifest response (timeout tests).
	delay time.Duration
	// contentType is the manifest media type the server reports.
	contentType string

	mu sync.Mutex
	// observed is guarded by mu; tests read it through obs(), which returns a
	// copy, so a handler still running (the timeout test) never races a read.
	observed registryObservations
}

// registryObservations is what the fake saw: one entry per manifest request
// plus the details of the token exchange.
type registryObservations struct {
	manifestHits   []string // "METHOD path" per manifest request
	acceptHeaders  []string
	authHeaders    []string // Authorization values seen on manifest requests
	tokenRequests  int
	tokenBasicUser string
	tokenBasicPass string
	tokenHadBasic  bool
	tokenScope     string
}

func (f *fakeRegistry) obs() registryObservations {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.observed
	o.manifestHits = append([]string(nil), o.manifestHits...)
	o.acceptHeaders = append([]string(nil), o.acceptHeaders...)
	o.authHeaders = append([]string(nil), o.authHeaders...)
	return o
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{
		username:    "ci-user",
		password:    "s3cret:with:colons",
		token:       "tok-" + hex.EncodeToString([]byte("abc")),
		contentType: "application/vnd.oci.image.index.v1+json",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/v2/", f.handleV2)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// host is the registry host[:port] as it appears in an image reference.
func (f *fakeRegistry) host() string {
	return strings.TrimPrefix(f.srv.URL, "http://")
}

func (f *fakeRegistry) handleToken(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	f.mu.Lock()
	f.observed.tokenRequests++
	f.observed.tokenScope = r.URL.Query().Get("scope")
	f.observed.tokenHadBasic = ok
	f.observed.tokenBasicUser, f.observed.tokenBasicPass = user, pass
	f.mu.Unlock()
	if ok && (user != f.username || pass != f.password) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": f.token})
}

func (f *fakeRegistry) handleV2(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.observed.manifestHits = append(f.observed.manifestHits, r.Method+" "+r.URL.Path)
	f.observed.acceptHeaders = append(f.observed.acceptHeaders, r.Header.Get("Accept"))
	f.observed.authHeaders = append(f.observed.authHeaders, r.Header.Get("Authorization"))
	f.mu.Unlock()

	switch f.auth {
	case "bearer":
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake-registry",scope="repository:private/app:pull"`, f.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	case "basic":
		user, pass, ok := r.BasicAuth()
		if !ok || user != f.username || pass != f.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake-registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	if !strings.HasPrefix(r.URL.Path, "/v2/private/app/manifests/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	w.Header().Set("Content-Type", f.contentType)
	if !f.omitDigestHeader {
		w.Header().Set("Docker-Content-Digest", testDigest)
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(testManifest)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(testManifest))
	}
}

func staticCredentials(user, pass string) CredentialFunc {
	return func(_ context.Context, _ string) (Credentials, bool, error) {
		return Credentials{Username: user, Password: pass}, true, nil
	}
}

func noCredentials(_ context.Context, _ string) (Credentials, bool, error) {
	return Credentials{}, false, nil
}

func TestRegistryClient_AnonymousHEAD(t *testing.T) {
	reg := newFakeRegistry(t)
	c := NewRegistryClient(WithCredentials(noCredentials))

	got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	require.Equal(t, []string{"HEAD /v2/private/app/manifests/1.0"}, reg.obs().manifestHits, "a single HEAD, no GET when the digest header is present")
	accept := reg.obs().acceptHeaders[0]
	for _, mt := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	} {
		assert.Contains(t, accept, mt, "Accept must advertise index and manifest media types")
	}
	assert.Equal(t, 0, reg.obs().tokenRequests, "anonymous registry must not trigger a token exchange")
}

func TestRegistryClient_BearerChallengeWithCredentials(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = "bearer"
	c := NewRegistryClient(WithCredentials(staticCredentials(reg.username, reg.password)))

	got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	// Challenge -> token -> retry: exactly two manifest requests, one token
	// request carrying the credentials as basic auth and the challenge scope.
	assert.Equal(t, []string{"HEAD /v2/private/app/manifests/1.0", "HEAD /v2/private/app/manifests/1.0"}, reg.obs().manifestHits)
	assert.Equal(t, 1, reg.obs().tokenRequests)
	assert.True(t, reg.obs().tokenHadBasic)
	assert.Equal(t, reg.username, reg.obs().tokenBasicUser)
	assert.Equal(t, reg.password, reg.obs().tokenBasicPass, "a password containing colons must survive intact")
	assert.Equal(t, "repository:private/app:pull", reg.obs().tokenScope)
	assert.Equal(t, "Bearer "+reg.token, reg.obs().authHeaders[1])
}

func TestRegistryClient_BearerChallengeAnonymousToken(t *testing.T) {
	// Docker Hub style: public images still go through the token endpoint,
	// just without basic credentials.
	reg := newFakeRegistry(t)
	reg.auth = "bearer"
	c := NewRegistryClient(WithCredentials(noCredentials))

	got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)
	assert.Equal(t, 1, reg.obs().tokenRequests)
	assert.False(t, reg.obs().tokenHadBasic, "no credentials configured -> anonymous token request")
}

func TestRegistryClient_BearerBadCredentials(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = "bearer"
	c := NewRegistryClient(WithCredentials(staticCredentials(reg.username, "wrong-password")))

	_, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRegistryUnauthorized)
	assert.NotContains(t, err.Error(), "wrong-password", "errors must never carry the credential")
	assert.NotContains(t, err.Error(), reg.username, "errors must never carry the credential")
	// One token attempt, no retry storm.
	assert.Equal(t, 1, reg.obs().tokenRequests)
}

func TestRegistryClient_BasicAuth(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = "basic"
	c := NewRegistryClient(WithCredentials(staticCredentials(reg.username, reg.password)))

	got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)
	assert.Equal(t, []string{"HEAD /v2/private/app/manifests/1.0", "HEAD /v2/private/app/manifests/1.0"}, reg.obs().manifestHits)
	assert.True(t, strings.HasPrefix(reg.obs().authHeaders[1], "Basic "))
	assert.Equal(t, 0, reg.obs().tokenRequests)
}

func TestRegistryClient_BasicAuthBadCredentials(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = "basic"
	c := NewRegistryClient(WithCredentials(staticCredentials(reg.username, "nope")))

	_, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	assert.ErrorIs(t, err, ErrRegistryUnauthorized)
	assert.NotContains(t, err.Error(), "nope")
	// The challenge is answered once; a second 401 is final.
	assert.Len(t, reg.obs().manifestHits, 2)
}

func TestRegistryClient_AuthRequiredButNoCredentials(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.auth = "basic"
	c := NewRegistryClient(WithCredentials(noCredentials))

	_, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRegistryUnauthorized)
	assert.Contains(t, err.Error(), "CAESIUM_REGISTRY_AUTH", "the error should point the operator at the credential mapping")
	assert.Len(t, reg.obs().manifestHits, 1, "without credentials a basic challenge cannot be answered; no blind retry")
}

func TestRegistryClient_CredentialLookupErrorSurfaces(t *testing.T) {
	reg := newFakeRegistry(t)
	c := NewRegistryClient(WithCredentials(func(_ context.Context, _ string) (Credentials, bool, error) {
		return Credentials{}, false, errors.New("vault sealed")
	}))
	_, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vault sealed")
	assert.Empty(t, reg.obs().manifestHits, "a credential-source failure must not fall through to an anonymous probe that could mask it")
}

func TestRegistryClient_DigestHeaderAbsentFallsBackToHashingManifest(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.omitDigestHeader = true
	c := NewRegistryClient(WithCredentials(noCredentials))

	got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.NoError(t, err)

	sum := sha256.Sum256([]byte(testManifest))
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), got, "without Docker-Content-Digest the digest is the sha256 of the manifest body")
	assert.Equal(t, []string{"HEAD /v2/private/app/manifests/1.0", "GET /v2/private/app/manifests/1.0"}, reg.obs().manifestHits)
}

func TestRegistryClient_ManifestMediaTypeDoesNotMatter(t *testing.T) {
	// Single-platform manifests and multi-platform indexes both resolve: the
	// digest is whatever the registry serves for the tag.
	for _, ct := range []string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	} {
		reg := newFakeRegistry(t)
		reg.contentType = ct
		c := NewRegistryClient(WithCredentials(noCredentials))
		got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
		require.NoError(t, err, ct)
		assert.Equal(t, testDigest, got, ct)
	}
}

func TestRegistryClient_NotFound(t *testing.T) {
	reg := newFakeRegistry(t)
	c := NewRegistryClient(WithCredentials(noCredentials))
	_, err := c.ResolveDigest(context.Background(), reg.host()+"/private/missing:1.0")
	assert.ErrorIs(t, err, ErrRegistryNotFound)
}

func TestRegistryClient_Timeout(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.delay = 500 * time.Millisecond
	c := NewRegistryClient(WithCredentials(noCredentials), WithTimeout(100*time.Millisecond))

	start := time.Now()
	_, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app:1.0")
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "the bounded timeout must cut the request short")
	assert.Len(t, reg.obs().manifestHits, 1, "no retry after a timeout")
}

func TestRegistryClient_DigestReferenceShortCircuits(t *testing.T) {
	reg := newFakeRegistry(t)
	c := NewRegistryClient(WithCredentials(noCredentials))
	got, err := c.ResolveDigest(context.Background(), reg.host()+"/private/app@"+testDigest)
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)
	assert.Empty(t, reg.obs().manifestHits, "a digest reference needs no round-trip")
}

func TestRegistryClient_InvalidReference(t *testing.T) {
	c := NewRegistryClient(WithCredentials(noCredentials))
	_, err := c.ResolveDigest(context.Background(), "not a ref")
	assert.Error(t, err)
}

func TestParseAuthChallenge(t *testing.T) {
	ch, ok := parseAuthChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/redis:pull,push"`)
	require.True(t, ok)
	assert.Equal(t, "bearer", ch.scheme)
	assert.Equal(t, "https://auth.docker.io/token", ch.params["realm"])
	assert.Equal(t, "registry.docker.io", ch.params["service"])
	assert.Equal(t, "repository:library/redis:pull,push", ch.params["scope"], "commas inside quoted values must not split the parameter")

	ch, ok = parseAuthChallenge(`Basic realm="Registry Realm"`)
	require.True(t, ok)
	assert.Equal(t, "basic", ch.scheme)
	assert.Equal(t, "Registry Realm", ch.params["realm"])

	_, ok = parseAuthChallenge("")
	assert.False(t, ok)
}
