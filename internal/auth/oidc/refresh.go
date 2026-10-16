package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
	"golang.org/x/oauth2"
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
//     invalid_grant), or the answer cannot be trusted (an ID token that fails
//     verification, or an ID token or userinfo answer for another subject);
//   - definitive: invalid_grant, only.
//
// A successful redemption without an ID token is never a failure (OpenID
// Connect Core §12.2): groups then come from the userinfo endpoint, and
// failing that the result carries GroupsKnown=false so the caller persists
// the rotated token and keeps the stored groups.
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
// anything else is read from it. A token that fails is uncertain: the
// response cannot be trusted, so neither its groups nor its rotated refresh
// token are kept. A verified token for the user whose groups claim is
// malformed yields GroupsKnown=false (the rotation is real).
func (p *Provider) groupsFromRefreshedIDToken(ctx context.Context, rawIDToken string, req authpkg.RefreshRequest) ([]string, bool, error) {
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "invalid_id_token", nil)
	}
	// The identity binding comes first: nothing from a token for another
	// identity is used, not even its rotated refresh token.
	if idToken.Issuer != req.Issuer || idToken.Subject != req.Subject {
		log.Warn("refreshed oidc id_token names another identity; ignoring it", "user_id", req.UserID.String())
		return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "identity_mismatch", nil)
	}
	identity, err := p.identityFromIDToken(idToken)
	switch {
	case errors.Is(err, ErrInvalidGroupsClaim):
		log.Warn("refreshed oidc id_token has a malformed groups claim; keeping stored groups",
			"user_id", req.UserID.String(), "claim", p.groupsClaim)
		return nil, false, nil
	case err != nil:
		return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "invalid_id_token", nil)
	}
	return identity.Groups, true, nil
}

// groupsFromUserInfo asks the userinfo endpoint with the refreshed access
// token. Its subject must be the user's (OpenID Connect Core §5.3.2) and an
// iss, when present, the user's issuer; a mismatch is uncertain. Groups
// count only when the configured claim is present and well formed. Any other
// shortfall (no endpoint, a failed call, no claim) is GroupsKnown=false.
func (p *Provider) groupsFromUserInfo(ctx context.Context, token refreshTokenResponse, req authpkg.RefreshRequest) ([]string, bool, error) {
	if p.discovered == nil || p.discovered.UserInfoEndpoint() == "" {
		return nil, false, nil
	}
	info, err := p.discovered.UserInfo(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token.AccessToken, TokenType: "Bearer"}))
	if err != nil {
		// The error can quote the response body; it is not logged.
		log.Warn("oidc userinfo after refresh failed; keeping stored groups", "user_id", req.UserID.String())
		return nil, false, nil
	}
	if info.Subject != req.Subject {
		log.Warn("oidc userinfo after refresh names another subject; ignoring it", "user_id", req.UserID.String())
		return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "identity_mismatch", nil)
	}
	var claims map[string]json.RawMessage
	if err := info.Claims(&claims); err != nil {
		return nil, false, nil
	}
	// go-oidc checks neither iss nor aud on userinfo (a signed response is
	// only signature-checked), so an iss naming another issuer is refused
	// here, like an ID token for another identity.
	if rawIssuer, ok := claims["iss"]; ok {
		var issuer string
		if err := json.Unmarshal(rawIssuer, &issuer); err != nil || issuer != req.Issuer {
			log.Warn("oidc userinfo after refresh names another issuer; ignoring it", "user_id", req.UserID.String())
			return nil, false, authpkg.NewRefreshError(authpkg.RefreshUncertain, "identity_mismatch", nil)
		}
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
