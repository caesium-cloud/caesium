package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
)

var _ authpkg.GroupRefresher = (*Provider)(nil)

// maxTokenResponseBytes bounds a token endpoint response, as golang.org/x/oauth2 does.
const maxTokenResponseBytes = 1 << 20

// postOnlyClientAuth reports whether the IdP advertises client_secret_post
// but not client_secret_basic. Basic is the OAuth 2.0 default every server
// must support (RFC 6749 §2.3.1), so it is used unless ruled out.
func postOnlyClientAuth(methods []string) bool {
	return len(methods) > 0 &&
		!slices.Contains(methods, "client_secret_basic") &&
		slices.Contains(methods, "client_secret_post")
}

type refreshTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
}

// RefreshGroups redeems the user's refresh token with one explicit request to
// the token endpoint and returns the groups of the verified, refreshed
// identity. It never applies groups from an unverified source, and no token
// reaches a log line or an error.
//
// Classification (see authpkg.RefreshErrorKind):
//   - transient: refused before sending (the grant is not for this provider
//     or issuer; the context is done), or nothing was written (dial, DNS or
//     TLS failure; cancelled before the request went out);
//   - uncertain: the request was, or may have been, written and no
//     definitive answer came back (a timeout or reset after the write, a 5xx
//     or 3xx, an unparsable or incomplete body, any OAuth error other than
//     invalid_grant), or the answer names another identity (a verified ID
//     token, or a userinfo answer, for another subject or issuer);
//   - definitive: invalid_grant, only.
//
// A successful redemption whose groups cannot be verified is never a failure:
// the token endpoint has redeemed the grant, so a rotated token must be kept.
// Groups then come from a verified ID token, else (no ID token, OpenID Connect
// Core §12.2) from the userinfo endpoint; failing those (an ID token that
// cannot be verified, no usable userinfo answer) the result carries
// GroupsKnown=false, so the caller persists the rotated token and keeps the
// stored groups and their timestamp.
func (p *Provider) RefreshGroups(ctx context.Context, req authpkg.RefreshRequest) (authpkg.GroupRefresh, error) {
	grant := req.Grant
	if grant.Provider != ProviderName {
		return authpkg.GroupRefresh{}, authpkg.NewRefreshError(authpkg.RefreshTransient, "provider_mismatch", nil)
	}
	// A grant minted by another issuer (the provider was repointed) is never
	// sent to this one: nothing is sent, so nothing is lost.
	if grant.Issuer != p.issuer || req.Issuer != p.issuer {
		log.Warn("refusing to redeem a refresh grant minted by another issuer",
			"user_id", req.UserID.String(), "configured_issuer", p.issuer)
		return authpkg.GroupRefresh{}, authpkg.NewRefreshError(authpkg.RefreshTransient, "issuer_mismatch", nil)
	}
	if grant.Handle.IsZero() {
		return authpkg.GroupRefresh{}, authpkg.NewRefreshError(authpkg.RefreshTransient, "empty_grant", nil)
	}
	if err := ctx.Err(); err != nil {
		return authpkg.GroupRefresh{}, authpkg.NewRefreshError(authpkg.RefreshTransient, "context_done", err)
	}

	token, err := p.redeemRefreshToken(ctx, grant.Handle)
	if err != nil {
		return authpkg.GroupRefresh{}, err
	}

	result := authpkg.GroupRefresh{Handle: grant.Handle}
	if token.RefreshToken != "" {
		result.Handle = authpkg.NewRefreshHandle(token.RefreshToken)
		result.Rotated = true
	}

	oauthCtx := p.oauthContext(ctx)
	if token.IDToken != "" {
		groups, known, err := p.groupsFromRefreshedIDToken(oauthCtx, token.IDToken, req)
		if err != nil {
			return authpkg.GroupRefresh{}, err
		}
		result.Groups, result.GroupsKnown = groups, known
		return result, nil
	}
	groups, known, err := p.groupsFromUserInfo(oauthCtx, token, req)
	if err != nil {
		return authpkg.GroupRefresh{}, err
	}
	result.Groups, result.GroupsKnown = groups, known
	return result, nil
}

// redeemRefreshToken sends one refresh_token grant. It deliberately bypasses
// oauth2.TokenSource: that caches, and with an undetected auth style it
// re-sends a failed request in a second form, which could redeem a rotating
// token twice. Redirects are not followed, so the token is never re-posted
// to another location.
func (p *Provider) redeemRefreshToken(ctx context.Context, handle authpkg.RefreshHandle) (refreshTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", handle.Reveal())
	if p.tokenAuthInParams {
		form.Set("client_id", p.oauth2Config.ClientID)
		form.Set("client_secret", p.oauth2Config.ClientSecret)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.oauth2Config.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		// Only a malformed token URL fails here; nothing was sent.
		return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshTransient, "bad_token_endpoint", nil)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	if !p.tokenAuthInParams {
		// RFC 6749 §2.3.1: form-encode the credentials before Basic encoding.
		httpReq.SetBasicAuth(url.QueryEscape(p.oauth2Config.ClientID), url.QueryEscape(p.oauth2Config.ClientSecret))
	}

	// WroteHeaders fires before the first byte can reach the network, so a
	// failure before it means the IdP saw nothing. After it, the request may
	// have been received and the token redeemed.
	var maybeSent atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { maybeSent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { maybeSent.Store(true) },
	}
	httpReq = httpReq.WithContext(httptrace.WithClientTrace(ctx, trace))

	resp, err := p.refreshHTTPClient().Do(httpReq)
	if err != nil {
		if !maybeSent.Load() {
			return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshTransient, "not_sent", err)
		}
		return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, "no_response", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
	if err != nil {
		return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, "response_read_failed", err)
	}
	defer clear(body)
	if len(body) > maxTokenResponseBytes {
		return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, "response_too_large", nil)
	}
	var parsed refreshTokenResponse
	// The decode error is dropped, never wrapped: it could quote the body.
	parseErr := json.Unmarshal(body, &parsed)

	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		if parseErr == nil && parsed.Error == "invalid_grant" {
			// RFC 6749 §5.2: the refresh token is invalid, expired, revoked
			// or was issued to another client. This is the only definitive
			// refusal.
			return refreshTokenResponse{}, authpkg.NewRefreshDenial(authpkg.DenialOfGrant, "invalid_grant")
		}
		// Any other refusal (client authentication, scope, a malformed
		// request) says nothing definitive about the grant.
		return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, oauthErrorReason(parsed.Error), nil)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		switch {
		case parseErr != nil:
			return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, "unparsable_response", nil)
		case parsed.Error != "":
			return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, oauthErrorReason(parsed.Error), nil)
		case parsed.AccessToken == "":
			return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, "missing_access_token", nil)
		}
		return parsed, nil
	default:
		// 5xx, an unfollowed 3xx, or anything else: the request arrived but
		// its fate is unknown.
		return refreshTokenResponse{}, authpkg.NewRefreshError(authpkg.RefreshUncertain, fmt.Sprintf("http_%d", resp.StatusCode), nil)
	}
}

// oauthErrorReason maps an OAuth error code to a fixed reason, so a reason
// never echoes arbitrary IdP text.
func oauthErrorReason(code string) string {
	switch code {
	case "invalid_request", "invalid_client", "unauthorized_client", "unsupported_grant_type",
		"invalid_scope", "temporarily_unavailable", "server_error", "access_denied":
		return "oauth_" + code
	case "":
		return "oauth_error_unspecified"
	default:
		return "oauth_error"
	}
}

func (p *Provider) refreshHTTPClient() *http.Client {
	base := p.httpClient
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// groupsFromRefreshedIDToken verifies a refreshed ID token exactly as login
// does (signature, issuer, audience, expiry, authorized party; no nonce on a
// refresh) and requires it to name the user's own issuer and subject before
// anything else is read from it.
//
// Its groups count only from a token that passes all of that. When it cannot
// be verified (a JWKS fetch failing on a cold cache, the deadline spent, a bad
// signature, audience, expiry or authorized party, a malformed groups claim),
// the result is GroupsKnown=false: the token endpoint, reached over the
// configured connection with the client's credentials, has already redeemed
// the grant, so its rotated refresh token is the only live one and is kept,
// while the stored groups and their timestamp stay as they are. Only a
// VERIFIED token that names another identity is uncertain: nothing from that
// response is kept, not even its rotated token.
func (p *Provider) groupsFromRefreshedIDToken(ctx context.Context, rawIDToken string, req authpkg.RefreshRequest) ([]string, bool, error) {
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		log.Warn("refreshed oidc id_token failed verification; keeping stored groups", "user_id", req.UserID.String())
		return nil, false, nil
	}
	if idToken.Issuer != req.Issuer || idToken.Subject != req.Subject {
		log.Warn("refreshed oidc id_token names another identity; ignoring it", "user_id", req.UserID.String())
		return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "identity_mismatch", nil)
	}
	identity, err := p.identityFromIDToken(idToken)
	if err != nil {
		log.Warn("refreshed oidc id_token is not acceptable; keeping stored groups",
			"user_id", req.UserID.String(), "claim", p.groupsClaim, "malformed_groups", errors.Is(err, ErrInvalidGroupsClaim))
		return nil, false, nil
	}
	return identity.Groups, true, nil
}

// groupsFromUserInfo asks the userinfo endpoint with the refreshed access
// token and checks the answer as OpenID Connect Core §5.3.2 requires: its
// subject must be the user's, and a present iss the user's issuer; either
// mismatch names another identity and is uncertain. A signed (application/jwt)
// answer must verify against the issuer's keys and carry both iss and an aud
// that includes this client; an answer, signed or not, whose aud does not
// include this client was issued for another client, and one outside its
// exp/nbf bounds is stale. Groups count only when
// all of that holds and the configured claim is present and well formed.
// Every other shortfall (no endpoint, a failed or unverifiable call, a missing
// or foreign audience, no claim) is GroupsKnown=false, which keeps the rotated
// token without applying any group.
func (p *Provider) groupsFromUserInfo(ctx context.Context, token refreshTokenResponse, req authpkg.RefreshRequest) ([]string, bool, error) {
	if p.discovered == nil || p.discovered.UserInfoEndpoint() == "" {
		return nil, false, nil
	}
	claims, signed, err := p.fetchUserInfo(ctx, token.AccessToken)
	if err != nil {
		log.Warn("oidc userinfo after refresh failed; keeping stored groups",
			"user_id", req.UserID.String(), "reason", err.Error())
		return nil, false, nil
	}
	subject, ok := stringClaim(claims, "sub")
	if !ok {
		log.Warn("oidc userinfo after refresh has no subject; keeping stored groups", "user_id", req.UserID.String())
		return nil, false, nil
	}
	if subject != req.Subject {
		log.Warn("oidc userinfo after refresh names another subject; ignoring it", "user_id", req.UserID.String())
		return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "identity_mismatch", nil)
	}
	if rawIssuer, present := claims["iss"]; present {
		var issuer string
		if err := json.Unmarshal(rawIssuer, &issuer); err != nil || issuer != req.Issuer {
			log.Warn("oidc userinfo after refresh names another issuer; ignoring it", "user_id", req.UserID.String())
			return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "identity_mismatch", nil)
		}
	} else if signed {
		log.Warn("signed oidc userinfo lacks iss; keeping stored groups", "user_id", req.UserID.String())
		return nil, false, nil
	}
	if rawAudience, present := claims["aud"]; present {
		if !audienceIncludes(rawAudience, p.oauth2Config.ClientID) {
			log.Warn("oidc userinfo after refresh is for another client; keeping stored groups", "user_id", req.UserID.String())
			return nil, false, nil
		}
	} else if signed {
		log.Warn("signed oidc userinfo lacks aud; keeping stored groups", "user_id", req.UserID.String())
		return nil, false, nil
	}
	if !p.userInfoCurrent(claims) {
		log.Warn("oidc userinfo after refresh is expired or not yet valid; keeping stored groups", "user_id", req.UserID.String())
		return nil, false, nil
	}
	raw, ok := claims[p.groupsClaim]
	if !ok {
		return nil, false, nil
	}
	groups, err := extractGroups(raw)
	if err != nil {
		return nil, false, nil
	}
	return groups, true, nil
}

// fetchUserInfo GETs the userinfo endpoint with the access token. It does not
// follow redirects (the bearer token stays with the issuer) and never returns
// an error that quotes the response body. A signed (application/jwt) answer is
// verified against the issuer's JWKS and its payload returned with signed set.
func (p *Provider) fetchUserInfo(ctx context.Context, accessToken string) (map[string]json.RawMessage, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.discovered.UserInfoEndpoint(), nil)
	if err != nil {
		return nil, false, errors.New("bad userinfo endpoint")
	}
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("Accept", "application/json, application/jwt")
	resp, err := p.refreshHTTPClient().Do(httpReq)
	if err != nil {
		return nil, false, errors.New("userinfo request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
	if err != nil || len(body) > maxTokenResponseBytes {
		return nil, false, errors.New("userinfo response unreadable")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("userinfo answered http %d", resp.StatusCode)
	}
	signed := false
	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "application/jwt" {
		if p.userInfoKeys == nil {
			return nil, false, errors.New("signed userinfo without a jwks_uri")
		}
		payload, err := p.userInfoKeys.VerifySignature(ctx, strings.TrimSpace(string(body)))
		if err != nil {
			return nil, false, errors.New("signed userinfo failed verification")
		}
		body, signed = payload, true
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, false, errors.New("userinfo response is not a JSON object")
	}
	return claims, signed, nil
}

// userInfoNotBeforeSkew is the clock skew allowed on nbf, as go-oidc allows
// on an ID token's nbf; exp is enforced strictly, as there.
const userInfoNotBeforeSkew = 5 * time.Minute

// userInfoCurrent enforces the RFC 7519 §4.1.4/§4.1.5 bounds of a userinfo
// answer's exp and nbf when present (optional claims, usually on a signed
// answer). A malformed bound counts as not current.
func (p *Provider) userInfoCurrent(claims map[string]json.RawMessage) bool {
	now := p.now()
	if raw, ok := claims["exp"]; ok {
		exp, ok := numericDate(raw)
		if !ok || !now.Before(exp) {
			return false
		}
	}
	if raw, ok := claims["nbf"]; ok {
		nbf, ok := numericDate(raw)
		if !ok || now.Add(userInfoNotBeforeSkew).Before(nbf) {
			return false
		}
	}
	return true
}

// numericDate decodes a JWT NumericDate (seconds since the epoch, possibly
// fractional).
func numericDate(raw json.RawMessage) (time.Time, bool) {
	var seconds float64
	if err := json.Unmarshal(raw, &seconds); err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}, false
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(frac*1e9)), true
}

func stringClaim(claims map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := claims[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

// audienceIncludes reports whether an aud claim (a string or an array of
// strings) includes clientID.
func audienceIncludes(raw json.RawMessage, clientID string) bool {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == clientID
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	return slices.Contains(many, clientID)
}
