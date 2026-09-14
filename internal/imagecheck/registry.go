package imagecheck

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrRegistryUnauthorized is returned when a registry rejects the request for
// a manifest (or the token exchange that precedes it) as unauthenticated or
// forbidden. The wrapped message names the registry host and the operator
// knob, never the credential.
var ErrRegistryUnauthorized = errors.New("imagecheck: registry authentication failed")

// ErrRegistryNotFound is returned when the registry reports that the
// repository or tag does not exist.
var ErrRegistryNotFound = errors.New("imagecheck: manifest not found")

// ErrInsecureRegistryTransport is returned when a request the client is about
// to make — the manifest request, the bearer token exchange, or any redirect
// either of them is sent on — would travel over plaintext HTTP to a
// non-loopback host. The registry controls the challenge realm and redirect
// targets, so without this check an HTTPS registry could point the credential
// exchange at `http://` and receive the configured password in the clear.
var ErrInsecureRegistryTransport = errors.New("imagecheck: refusing to send registry request over plaintext HTTP")

// checkTransportURL enforces the transport policy every registry request
// follows: HTTPS anywhere, plain HTTP only to loopback hosts (localhost,
// 127.0.0.0/8, ::1 — the Docker daemon's default insecure-registry rule, and
// the path a local dev registry or the integration stub uses).
func checkTransportURL(u *url.URL) error {
	if u == nil || u.Host == "" {
		return fmt.Errorf("%w: request has no host", ErrInsecureRegistryTransport)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Host) {
			return nil
		}
		return fmt.Errorf("%w: %s is not a loopback host (only https:// is allowed there)", ErrInsecureRegistryTransport, u.Host)
	default:
		return fmt.Errorf("%w: unsupported scheme %q for %s", ErrInsecureRegistryTransport, u.Scheme, u.Host)
	}
}

// maxRedirects bounds a redirect chain, matching net/http's default.
const maxRedirects = 10

// registryRedirectPolicy is the http.Client CheckRedirect every RegistryClient
// uses: a redirect target is held to the same transport policy as the original
// request, and Authorization is dropped whenever the hop leaves the host the
// credentials were issued for.
//
// Comparison is against via[0] (the authorized origin), not the previous hop.
// net/http copies Authorization from the initial request onto every hop whose
// host is that origin or a subdomain of it, so a chain like
//
//	auth.example -> child.auth.example -> child.auth.example
//
// would reattach the original Basic on the second child hop if we only
// compared against via[len(via)-1]. Host-exact: a credential for auth.example
// never reaches auth2.example even as a subdomain.
func registryRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("imagecheck: stopped after %d redirects", maxRedirects)
	}
	if err := checkTransportURL(req.URL); err != nil {
		from := req.URL.Host
		if len(via) > 0 && via[len(via)-1].URL != nil {
			from = via[len(via)-1].URL.Host
		}
		return fmt.Errorf("redirect from %s: %w", from, err)
	}
	if !sameRedirectOrigin(req, via) {
		req.Header.Del("Authorization")
	}
	return nil
}

// sameRedirectOrigin reports whether req is still on the host that originally
// received Authorization. via[0] is that origin; an empty via (should not
// happen — CheckRedirect is only called after a hop) is treated as a leave.
func sameRedirectOrigin(req *http.Request, via []*http.Request) bool {
	if req == nil || req.URL == nil || len(via) == 0 || via[0] == nil || via[0].URL == nil {
		return false
	}
	return req.URL.Host == via[0].URL.Host
}

// withRedirectPolicy returns a shallow copy of c with the registry redirect
// policy installed, so a caller-supplied client is never mutated and can never
// bypass the policy.
func withRedirectPolicy(c *http.Client) *http.Client {
	copied := *c
	copied.CheckRedirect = registryRedirectPolicy
	return &copied
}

const (
	// manifestAccept advertises both multi-platform indexes and
	// single-platform manifests so the registry returns the digest of
	// whatever the tag actually points at (an index for most public images).
	manifestAccept = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"
	registryUserAgent = "caesium-imagecheck"
	// defaultRegistryTimeout bounds one whole resolution (challenge, token
	// exchange and manifest request together).
	defaultRegistryTimeout = 15 * time.Second
	// maxManifestBytes caps the body read when a registry omits
	// Docker-Content-Digest and the manifest has to be hashed locally.
	maxManifestBytes = 8 << 20
)

// Credentials is a username/password pair for a registry. Registries that
// authenticate with tokens (GHCR PATs, `oauth2accesstoken` for GCR, `AWS` for
// ECR) still take that shape.
type Credentials struct {
	Username string
	Password string
}

// CredentialFunc looks up the credentials for a registry host[:port]. It
// reports false when none are configured for the host — which is not an
// error: the registry is then probed anonymously. An error means the lookup
// itself failed (e.g. the backing secret could not be resolved) and the
// resolution is aborted rather than silently degraded to an anonymous probe.
type CredentialFunc func(ctx context.Context, registry string) (Credentials, bool, error)

// RegistryClient resolves image tags to content digests directly against the
// Docker Registry HTTP API v2, independently of any container engine: a HEAD
// on the manifest endpoint returns the digest the tag currently points at
// without pulling a single layer. It answers a single WWW-Authenticate
// challenge (bearer token or basic) using the configured CredentialFunc and
// never retries beyond that, so an unreachable or rejecting registry costs
// one bounded round-trip.
type RegistryClient struct {
	httpClient  *http.Client
	credentials CredentialFunc
	timeout     time.Duration
}

// RegistryOption configures a RegistryClient.
type RegistryOption func(*RegistryClient)

// WithHTTPClient overrides the HTTP client (test seam / custom TLS). The
// client's CheckRedirect is replaced by the registry transport policy on a
// copy; the supplied client is not mutated.
func WithHTTPClient(c *http.Client) RegistryOption {
	return func(r *RegistryClient) {
		if c != nil {
			r.httpClient = c
		}
	}
}

// WithCredentials sets the credential source consulted per registry host.
func WithCredentials(fn CredentialFunc) RegistryOption {
	return func(r *RegistryClient) { r.credentials = fn }
}

// WithTimeout bounds one whole resolution. Non-positive values keep the
// default.
func WithTimeout(d time.Duration) RegistryOption {
	return func(r *RegistryClient) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// NewRegistryClient builds a RegistryClient with a bounded default timeout and
// no credentials (every registry is probed anonymously) unless options say
// otherwise.
func NewRegistryClient(opts ...RegistryOption) *RegistryClient {
	c := &RegistryClient{
		httpClient: &http.Client{},
		timeout:    defaultRegistryTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	// Installed last so neither the default client nor a WithHTTPClient one
	// can follow a redirect the transport policy forbids.
	c.httpClient = withRedirectPolicy(c.httpClient)
	return c
}

// ResolveDigest returns the sha256 content digest the image reference's tag
// currently points at. A reference that already carries a digest is returned
// verbatim without any network I/O. It satisfies DigestFunc.
func (c *RegistryClient) ResolveDigest(ctx context.Context, imageRef string) (string, error) {
	ref, err := ParseReference(imageRef)
	if err != nil {
		return "", err
	}
	if ref.Digest != "" {
		return ref.Digest, nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	creds, hasCreds, err := c.lookupCredentials(ctx, ref.Registry)
	if err != nil {
		return "", fmt.Errorf("registry %s: credentials: %w", ref.Registry, err)
	}

	manifestURL := ref.manifestURL()
	resp, err := c.request(ctx, http.MethodHead, manifestURL, "")
	if err != nil {
		return "", fmt.Errorf("registry %s: %w", ref.Registry, err)
	}
	defer discard(resp)

	if resp.StatusCode != http.StatusUnauthorized {
		return c.digestFromResponse(ctx, ref, manifestURL, "", resp)
	}

	// One challenge, one answer, one authenticated retry — never more.
	authorization, err := c.answerChallenge(ctx, ref, resp.Header.Get("WWW-Authenticate"), creds, hasCreds)
	if err != nil {
		return "", err
	}
	authed, err := c.request(ctx, http.MethodHead, manifestURL, authorization)
	if err != nil {
		return "", fmt.Errorf("registry %s: %w", ref.Registry, err)
	}
	defer discard(authed)
	return c.digestFromResponse(ctx, ref, manifestURL, authorization, authed)
}

// digestFromResponse reads the digest off a manifest HEAD response, fetching
// and hashing the manifest body when the registry omitted the header.
func (c *RegistryClient) digestFromResponse(ctx context.Context, ref Reference, manifestURL, authorization string, resp *http.Response) (string, error) {
	if err := checkManifestStatus(ref, resp.StatusCode); err != nil {
		return "", err
	}

	digest := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		// Some registries (and proxies) omit the header on HEAD; the digest is
		// by definition the sha256 of the manifest bytes, so fetch and hash.
		var err error
		digest, err = c.hashManifest(ctx, ref, manifestURL, authorization)
		if err != nil {
			return "", err
		}
	}
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("registry %s: unsupported digest %q for %s", ref.Registry, digest, ref.Repository)
	}
	return digest, nil
}

func (c *RegistryClient) lookupCredentials(ctx context.Context, registry string) (Credentials, bool, error) {
	if c.credentials == nil {
		return Credentials{}, false, nil
	}
	return c.credentials(ctx, registry)
}

// answerChallenge turns a WWW-Authenticate challenge into an Authorization
// header value, exchanging credentials for a bearer token when the registry
// asks for one.
func (c *RegistryClient) answerChallenge(ctx context.Context, ref Reference, header string, creds Credentials, hasCreds bool) (string, error) {
	ch, ok := parseAuthChallenge(header)
	if !ok {
		return "", fmt.Errorf("%w: registry %s returned 401 without a usable WWW-Authenticate challenge", ErrRegistryUnauthorized, ref.Registry)
	}
	switch ch.scheme {
	case "basic":
		if !hasCreds {
			return "", fmt.Errorf("%w: registry %s requires credentials and none are configured for it (set CAESIUM_REGISTRY_AUTH)", ErrRegistryUnauthorized, ref.Registry)
		}
		return "Basic " + basicAuth(creds), nil
	case "bearer":
		token, err := c.fetchToken(ctx, ref, ch, creds, hasCreds)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	default:
		return "", fmt.Errorf("%w: registry %s uses unsupported auth scheme %q", ErrRegistryUnauthorized, ref.Registry, ch.scheme)
	}
}

// fetchToken performs the bearer token exchange described by the challenge:
// GET realm?service=&scope=, with the credentials (if any) as basic auth.
// Public registries such as Docker Hub hand out anonymous pull tokens when no
// credentials are sent.
func (c *RegistryClient) fetchToken(ctx context.Context, ref Reference, ch authChallenge, creds Credentials, hasCreds bool) (string, error) {
	realm := ch.params["realm"]
	if realm == "" {
		return "", fmt.Errorf("%w: registry %s bearer challenge has no realm", ErrRegistryUnauthorized, ref.Registry)
	}
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("%w: registry %s bearer realm is not a URL", ErrRegistryUnauthorized, ref.Registry)
	}
	// The realm is chosen by the registry, and this request carries the
	// operator's credentials as basic auth: hold it to the same transport
	// policy as the manifest request, or an HTTPS registry could advertise
	// an http:// realm and receive the password in the clear.
	if err := checkTransportURL(tokenURL); err != nil {
		return "", fmt.Errorf("registry %s bearer realm: %w", ref.Registry, err)
	}
	q := tokenURL.Query()
	if service := ch.params["service"]; service != "" {
		q.Set("service", service)
	}
	scope := ch.params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	q.Set("scope", scope)
	tokenURL.RawQuery = q.Encode()

	authorization := ""
	if hasCreds {
		authorization = "Basic " + basicAuth(creds)
	}
	resp, err := c.request(ctx, http.MethodGet, tokenURL.String(), authorization)
	if err != nil {
		return "", fmt.Errorf("registry %s: token exchange: %w", ref.Registry, err)
	}
	defer discard(resp)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		if hasCreds {
			return "", fmt.Errorf("%w: token endpoint for registry %s rejected the configured credentials", ErrRegistryUnauthorized, ref.Registry)
		}
		return "", fmt.Errorf("%w: registry %s requires credentials and none are configured for it (set CAESIUM_REGISTRY_AUTH)", ErrRegistryUnauthorized, ref.Registry)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("registry %s: token endpoint returned HTTP %d", ref.Registry, resp.StatusCode)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("registry %s: token endpoint returned an unreadable body: %w", ref.Registry, err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("%w: token endpoint for registry %s returned no token", ErrRegistryUnauthorized, ref.Registry)
	}
	return token, nil
}

// hashManifest GETs the manifest and returns the sha256 of its bytes.
func (c *RegistryClient) hashManifest(ctx context.Context, ref Reference, manifestURL, authorization string) (string, error) {
	resp, err := c.request(ctx, http.MethodGet, manifestURL, authorization)
	if err != nil {
		return "", fmt.Errorf("registry %s: %w", ref.Registry, err)
	}
	defer discard(resp)
	if err := checkManifestStatus(ref, resp.StatusCode); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return "", fmt.Errorf("registry %s: read manifest: %w", ref.Registry, err)
	}
	if len(body) > maxManifestBytes {
		return "", fmt.Errorf("registry %s: manifest for %s exceeds %d bytes", ref.Registry, ref.Repository, maxManifestBytes)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (c *RegistryClient) request(ctx context.Context, method, target, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	req.Header.Set("User-Agent", registryUserAgent)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return c.httpClient.Do(req)
}

func checkManifestStatus(ref Reference, status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: registry %s rejected access to %s (HTTP %d)", ErrRegistryUnauthorized, ref.Registry, ref.Repository, status)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s:%s on registry %s", ErrRegistryNotFound, ref.Repository, ref.Tag, ref.Registry)
	default:
		return fmt.Errorf("registry %s: manifest request for %s returned HTTP %d", ref.Registry, ref.Repository, status)
	}
}

func basicAuth(creds Credentials) string {
	return base64.StdEncoding.EncodeToString([]byte(creds.Username + ":" + creds.Password))
}

// discard drains and closes a response body so the connection can be reused.
func discard(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxManifestBytes))
	_ = resp.Body.Close()
}

// authChallenge is a parsed WWW-Authenticate header.
type authChallenge struct {
	scheme string            // lower-cased: "bearer" or "basic"
	params map[string]string // realm, service, scope, ...
}

// parseAuthChallenge parses `Scheme k="v",k2="v2"` (RFC 7235). Quoted values
// may contain commas (Docker scopes do), so it tokenises by hand.
func parseAuthChallenge(header string) (authChallenge, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return authChallenge{}, false
	}
	scheme, rest, _ := strings.Cut(header, " ")
	ch := authChallenge{scheme: strings.ToLower(scheme), params: map[string]string{}}

	rest = strings.TrimSpace(rest)
	for rest != "" {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		after = strings.TrimSpace(after)
		var value string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				return authChallenge{}, false
			}
			value = after[1 : 1+end]
			rest = strings.TrimSpace(after[1+end+1:])
		} else {
			value, rest, _ = strings.Cut(after, ",")
			value = strings.TrimSpace(value)
		}
		ch.params[key] = value
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	if ch.scheme == "" {
		return authChallenge{}, false
	}
	return ch, true
}
