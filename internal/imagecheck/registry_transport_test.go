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
