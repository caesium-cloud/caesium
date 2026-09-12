package imagecheck

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTransport is an http.RoundTripper that answers from a table keyed
// by "METHOD scheme://host/path" and records every request it is asked to
// send — including the Authorization header — so a test can prove which wire
// a credential would have travelled on. Anything not in the table is answered
// 404 and still recorded.
type recordingTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(req *http.Request) *http.Response
}

type recordedRequest struct {
	method        string
	url           string
	authorization string
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.requests = append(t.requests, recordedRequest{
		method:        req.Method,
		url:           req.URL.String(),
		authorization: req.Header.Get("Authorization"),
	})
	t.mu.Unlock()
	resp := t.respond(req)
	if resp == nil {
		resp = canned(http.StatusNotFound, nil, "")
	}
	resp.Request = req
	return resp, nil
}

func (t *recordingTransport) seen() []recordedRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]recordedRequest(nil), t.requests...)
}

func (t *recordingTransport) sawHost(host string) bool {
	for _, r := range t.seen() {
		if strings.Contains(r.url, "://"+host+"/") {
			return true
		}
	}
	return false
}

func canned(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func bearerChallenge(realm string) *http.Response {
	h := http.Header{}
	h.Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="reg",scope="repository:team/app:pull"`)
	return canned(http.StatusUnauthorized, h, "")
}

func tokenOK() *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return canned(http.StatusOK, h, `{"token":"tok"}`)
}

func manifestOK() *http.Response {
	h := http.Header{}
	h.Set("Docker-Content-Digest", testDigest)
	return canned(http.StatusOK, h, "")
}

func redirectTo(location string) *http.Response {
	h := http.Header{}
	h.Set("Location", location)
	return canned(http.StatusFound, h, "")
}

func newRecordingClient(t *testing.T, respond func(req *http.Request) *http.Response) (*RegistryClient, *recordingTransport) {
	t.Helper()
	rt := &recordingTransport{respond: respond}
	c := NewRegistryClient(
		WithHTTPClient(&http.Client{Transport: rt}),
		WithCredentials(staticCredentials("ci-user", "hunter2")),
	)
	return c, rt
}

func assertCredentialNeverLeft(t *testing.T, rt *recordingTransport, forbiddenHost string) {
	t.Helper()
	for _, r := range rt.seen() {
		if strings.HasPrefix(r.url, "http://") && !strings.Contains(r.url, "://127.0.0.1") && !strings.Contains(r.url, "://localhost") {
			t.Errorf("a plaintext request was sent: %s %s", r.method, r.url)
		}
		if forbiddenHost != "" && strings.Contains(r.url, "://"+forbiddenHost+"/") {
			t.Errorf("a request reached %s: %s %s (authorization=%q)", forbiddenHost, r.method, r.url, r.authorization)
		}
	}
}

// The reviewer's reproduction: an HTTPS registry whose challenge points the
// token exchange at a plaintext realm. The configured password must never be
// put on that wire.
func TestRegistryClient_RefusesPlaintextTokenRealm(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		switch {
		case req.Method == http.MethodHead && req.URL.Host == "registry.example.com":
			return bearerChallenge("http://auth.example/token")
		case req.URL.Host == "auth.example":
			return tokenOK()
		}
		return nil
	})

	_, err := c.ResolveDigest(context.Background(), "registry.example.com/team/app:1.0")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInsecureRegistryTransport)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.False(t, rt.sawHost("auth.example"), "no request may reach the plaintext realm at all")
	assertCredentialNeverLeft(t, rt, "auth.example")
}

// A redirect from an HTTPS token endpoint to a plaintext host is the same
// attack one hop later; the redirect must be refused before it is followed.
func TestRegistryClient_RefusesPlaintextRedirectDuringTokenExchange(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		switch {
		case req.Method == http.MethodHead && req.URL.Host == "registry.example.com":
			return bearerChallenge("https://auth.example/token")
		case req.URL.Host == "auth.example":
			return redirectTo("http://evil.example/token")
		case req.URL.Host == "evil.example":
			return tokenOK()
		}
		return nil
	})

	_, err := c.ResolveDigest(context.Background(), "registry.example.com/team/app:1.0")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInsecureRegistryTransport)
	assert.False(t, rt.sawHost("evil.example"), "the plaintext redirect target must never be contacted")
	assertCredentialNeverLeft(t, rt, "evil.example")
}

// Manifest requests carry the bearer token; a plaintext redirect there is
// refused too.
func TestRegistryClient_RefusesPlaintextRedirectOnManifest(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		if req.URL.Host == "registry.example.com" {
			return redirectTo("http://mirror.example/v2/team/app/manifests/1.0")
		}
		if req.URL.Host == "mirror.example" {
			return manifestOK()
		}
		return nil
	})

	_, err := c.ResolveDigest(context.Background(), "registry.example.com/team/app:1.0")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInsecureRegistryTransport)
	assert.False(t, rt.sawHost("mirror.example"))
}

// An HTTPS -> HTTPS redirect to a different host is allowed, but the
// credential is for the original host only: Authorization must be dropped.
func TestRegistryClient_CrossHostRedirectDropsAuthorization(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		switch req.URL.Host {
		case "registry.example.com":
			if req.Header.Get("Authorization") == "Bearer tok" {
				return manifestOK()
			}
			return bearerChallenge("https://auth.example/token")
		case "auth.example":
			return redirectTo("https://auth2.example/token")
		case "auth2.example":
			return tokenOK()
		}
		return nil
	})

	got, err := c.ResolveDigest(context.Background(), "registry.example.com/team/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	var sawAuth, sawAuth2 bool
	for _, r := range rt.seen() {
		switch {
		case strings.Contains(r.url, "://auth.example/"):
			sawAuth = true
			assert.True(t, strings.HasPrefix(r.authorization, "Basic "), "the original token endpoint receives the credentials")
		case strings.Contains(r.url, "://auth2.example/"):
			sawAuth2 = true
			assert.Empty(t, r.authorization, "a cross-host redirect must not carry the credentials to the new host")
		}
	}
	assert.True(t, sawAuth && sawAuth2)
	assertCredentialNeverLeft(t, rt, "")
}

// Go copies Authorization from the INITIAL request onto every hop whose host
// is that origin or a subdomain of it. Comparing only against the previous
// hop lets a chain like
//
//	auth.example -> child.auth.example -> child.auth.example
//
// reattach the original Basic on the second child hop (host equals previous).
// Strip against the authorized origin on every hop.
func TestRegistryClient_MultiHopSubdomainRedirectDoesNotReattachAuthorization(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		switch req.URL.Host {
		case "registry.example.com":
			if req.Header.Get("Authorization") == "Bearer tok" {
				return manifestOK()
			}
			return bearerChallenge("https://auth.example/token")
		case "auth.example":
			return redirectTo("https://child.auth.example/hop")
		case "child.auth.example":
			if req.URL.Path == "/hop" {
				return redirectTo("https://child.auth.example/final")
			}
			return tokenOK()
		}
		return nil
	})

	got, err := c.ResolveDigest(context.Background(), "registry.example.com/team/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	var sawOrigin, sawHop, sawFinal bool
	for _, r := range rt.seen() {
		switch {
		case strings.Contains(r.url, "://auth.example/"):
			sawOrigin = true
			assert.True(t, strings.HasPrefix(r.authorization, "Basic "), "the original token endpoint receives the credentials")
		case strings.Contains(r.url, "://child.auth.example/hop"):
			sawHop = true
			assert.Empty(t, r.authorization, "the first child hop must not carry Authorization")
		case strings.Contains(r.url, "://child.auth.example/final"):
			sawFinal = true
			assert.Empty(t, r.authorization, "a later same-host hop must not reattach the original Authorization")
		}
	}
	assert.True(t, sawOrigin && sawHop && sawFinal, "the multi-hop token redirect must be followed")
	assertCredentialNeverLeft(t, rt, "")
}

// Same-host hops keep Authorization: the credential is still for the origin
// it was issued to.
func TestRegistryClient_SameHostRedirectKeepsAuthorization(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		switch req.URL.Host {
		case "registry.example.com":
			if req.Header.Get("Authorization") == "Bearer tok" {
				return manifestOK()
			}
			return bearerChallenge("https://auth.example/token")
		case "auth.example":
			if req.URL.Path == "/token" {
				return redirectTo("https://auth.example/token2")
			}
			return tokenOK()
		}
		return nil
	})

	got, err := c.ResolveDigest(context.Background(), "registry.example.com/team/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	var sawToken, sawToken2 bool
	for _, r := range rt.seen() {
		switch {
		case strings.Contains(r.url, "://auth.example/token?"):
			sawToken = true
			assert.True(t, strings.HasPrefix(r.authorization, "Basic "))
		case strings.Contains(r.url, "://auth.example/token2"):
			sawToken2 = true
			assert.True(t, strings.HasPrefix(r.authorization, "Basic "), "a same-host hop must keep the credentials")
		}
	}
	assert.True(t, sawToken && sawToken2)
	assertCredentialNeverLeft(t, rt, "")
}

// The explicitly permitted path: loopback registries (the integration stub,
// a local dev registry) speak plain HTTP for both the manifest and the token
// endpoint, matching the Docker daemon's default insecure-registry rule.
func TestRegistryClient_AllowsPlaintextLoopbackRealm(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		switch {
		case req.URL.Path == "/token":
			return tokenOK()
		case req.Header.Get("Authorization") == "Bearer tok":
			return manifestOK()
		default:
			return bearerChallenge("http://127.0.0.1:5000/token")
		}
	})

	got, err := c.ResolveDigest(context.Background(), "127.0.0.1:5000/private/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, testDigest, got)

	seen := rt.seen()
	require.Len(t, seen, 3)
	assert.Equal(t, "http://127.0.0.1:5000/v2/private/app/manifests/1.0", seen[0].url)
	assert.True(t, strings.HasPrefix(seen[1].url, "http://127.0.0.1:5000/token?"))
	assert.True(t, strings.HasPrefix(seen[1].authorization, "Basic "))
	assert.Equal(t, "Bearer tok", seen[2].authorization)
}

// A loopback registry must not be able to bounce the credential off-box: a
// realm on a non-loopback plaintext host is refused even when the registry
// itself is loopback.
func TestRegistryClient_LoopbackRegistryCannotRedirectCredentialsOffBox(t *testing.T) {
	c, rt := newRecordingClient(t, func(req *http.Request) *http.Response {
		if req.URL.Host == "127.0.0.1:5000" {
			return bearerChallenge("http://auth.example/token")
		}
		return tokenOK()
	})

	_, err := c.ResolveDigest(context.Background(), "127.0.0.1:5000/private/app:1.0")
	assert.ErrorIs(t, err, ErrInsecureRegistryTransport)
	assert.False(t, rt.sawHost("auth.example"))
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestRegistryRedirectPolicy_StripsAgainstOriginalOrigin(t *testing.T) {
	origin, err := http.NewRequest(http.MethodGet, "https://auth.example/token", nil)
	require.NoError(t, err)
	origin.Header.Set("Authorization", "Basic dXNlcjpzZWNyZXQ=")

	hop1, err := http.NewRequest(http.MethodGet, "https://child.auth.example/hop", nil)
	require.NoError(t, err)
	hop1.Header.Set("Authorization", "Basic dXNlcjpzZWNyZXQ=")
	require.NoError(t, registryRedirectPolicy(hop1, []*http.Request{origin}))
	assert.Empty(t, hop1.Header.Get("Authorization"), "leaving the authorized origin must strip Authorization")

	hop2, err := http.NewRequest(http.MethodGet, "https://child.auth.example/final", nil)
	require.NoError(t, err)
	// net/http re-copies Authorization from the initial request because dest is
	// a subdomain of origin. The previous hop already had it stripped.
	hop2.Header.Set("Authorization", "Basic dXNlcjpzZWNyZXQ=")
	require.NoError(t, registryRedirectPolicy(hop2, []*http.Request{origin, hop1}))
	assert.Empty(t, hop2.Header.Get("Authorization"), "must strip against via[0], not the previous hop")

	sameHost, err := http.NewRequest(http.MethodGet, "https://auth.example/token2", nil)
	require.NoError(t, err)
	sameHost.Header.Set("Authorization", "Basic dXNlcjpzZWNyZXQ=")
	require.NoError(t, registryRedirectPolicy(sameHost, []*http.Request{origin}))
	assert.Equal(t, "Basic dXNlcjpzZWNyZXQ=", sameHost.Header.Get("Authorization"),
		"a same-host hop must keep the credentials")
}

func TestCheckTransportURL(t *testing.T) {
	for _, ok := range []string{
		"https://registry.example.com/v2/",
		"https://auth.example/token?service=x",
		"http://localhost/token",
		"http://localhost:5000/token",
		"http://127.0.0.1:5000/token",
		"http://[::1]:5000/token",
	} {
		assert.NoError(t, checkTransportURL(mustParse(t, ok)), ok)
	}
	for _, bad := range []string{
		"http://auth.example/token",
		"http://10.0.0.5/token",
		"http://registry.example.com:5000/v2/",
		"ftp://auth.example/token",
		"https:///token",
	} {
		assert.ErrorIs(t, checkTransportURL(mustParse(t, bad)), ErrInsecureRegistryTransport, bad)
	}
}
