package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var (
	ErrAuthorizationFailed = errors.New("oidc authorization failed")
	ErrMissingCode         = errors.New("oidc callback missing code")
	ErrMissingIDToken      = errors.New("oidc token response missing id_token")
	ErrInvalidIDToken      = errors.New("invalid oidc id_token")
	ErrInvalidNonce        = errors.New("invalid oidc nonce")
	ErrInvalidGroupsClaim  = errors.New("invalid oidc groups claim")
)

// Provider implements the Caesium browser redirect authenticator for OIDC.
type Provider struct {
	oauth2Config oauth2.Config
	verifier     *gooidc.IDTokenVerifier

	groupsClaim     string
	stateCookieName string
	stateTTL        time.Duration
	cookieSecure    bool
	cookieSecret    []byte
	publicOrigin    *url.URL
	httpClient      *http.Client
	now             func() time.Time
}

var _ authpkg.RedirectAuthenticator = (*Provider)(nil)

// New discovers the OIDC issuer and constructs an Authorization Code + PKCE
// redirect provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	if cfg.HTTPClient != nil {
		ctx = gooidc.ClientContext(ctx, cfg.HTTPClient)
	}

	discovered, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover oidc provider: %w", err)
	}

	redirectURL, err := url.Parse(cfg.RedirectURL)
	if err != nil {
		return nil, fmt.Errorf("parse oidc redirect URL: %w", err)
	}
	return &Provider{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     discovered.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       append([]string(nil), cfg.Scopes...),
		},
		verifier:        discovered.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		groupsClaim:     cfg.GroupsClaim,
		stateCookieName: cfg.StateCookieName,
		stateTTL:        cfg.StateTTL,
		cookieSecure:    cfg.CookieSecure,
		cookieSecret:    append([]byte(nil), cfg.CookieSecret...),
		publicOrigin:    &url.URL{Scheme: redirectURL.Scheme, Host: redirectURL.Host},
		httpClient:      cfg.HTTPClient,
		now:             time.Now,
	}, nil
}

// Name reports the provider id used by the shared SSO completion path.
func (p *Provider) Name() string {
	return ProviderName
}

// Begin creates the signed pre-login state cookie and returns the IdP
// authorization URL.
func (p *Provider) Begin(w http.ResponseWriter, _ *http.Request, returnTo string) (string, error) {
	validReturnTo, err := p.validateReturnTo(returnTo)
	if err != nil {
		return "", err
	}

	state, err := p.newLoginState(validReturnTo)
	if err != nil {
		return "", err
	}
	state.CodeVerifier = oauth2.GenerateVerifier()
	if err := p.setStateCookie(w, state); err != nil {
		return "", err
	}

	return p.oauth2Config.AuthCodeURL(
		state.State,
		oauth2.S256ChallengeOption(state.CodeVerifier),
		oauth2.SetAuthURLParam("nonce", state.Nonce),
	), nil
}

// Complete validates the callback and returns the normalized external identity.
func (p *Provider) Complete(r *http.Request) (*authpkg.ExternalIdentity, error) {
	ext, _, err := p.CompleteWithReturnTo(r)
	return ext, err
}

// CompleteWithReturnTo is the callback variant route handlers can use when they
// need the same-origin return destination stored during Begin.
func (p *Provider) CompleteWithReturnTo(r *http.Request) (*authpkg.ExternalIdentity, string, error) {
	if msg := r.URL.Query().Get("error"); msg != "" {
		if desc := r.URL.Query().Get("error_description"); desc != "" {
			msg += ": " + desc
		}
		return nil, "", fmt.Errorf("%w: %s", ErrAuthorizationFailed, msg)
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return nil, "", ErrMissingCode
	}
	gotState := r.URL.Query().Get("state")
	if gotState == "" {
		return nil, "", fmt.Errorf("%w: missing state parameter", ErrInvalidState)
	}

	state, err := p.readStateCookie(r)
	if err != nil {
		return nil, "", err
	}
	if subtle.ConstantTimeCompare([]byte(gotState), []byte(state.State)) != 1 {
		return nil, "", fmt.Errorf("%w: state mismatch", ErrInvalidState)
	}

	ctx := p.oauthContext(r.Context())
	token, err := p.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(state.CodeVerifier))
	if err != nil {
		return nil, "", fmt.Errorf("exchange oidc code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, "", ErrMissingIDToken
	}

	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalidIDToken, err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(state.Nonce)) != 1 {
		return nil, "", ErrInvalidNonce
	}

	identity, err := p.identityFromIDToken(idToken)
	if err != nil {
		return nil, "", err
	}
	return identity, state.ReturnTo, nil
}

func (p *Provider) oauthContext(ctx context.Context) context.Context {
	if p.httpClient == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.httpClient)
	return gooidc.ClientContext(ctx, p.httpClient)
}

type standardClaims struct {
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

func (p *Provider) identityFromIDToken(idToken *gooidc.IDToken) (*authpkg.ExternalIdentity, error) {
	var claims standardClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: decode standard claims: %v", ErrInvalidIDToken, err)
	}
	var allClaims map[string]json.RawMessage
	if err := idToken.Claims(&allClaims); err != nil {
		return nil, fmt.Errorf("%w: decode claims: %v", ErrInvalidIDToken, err)
	}
	if err := validateIdentityShape(idToken, allClaims, p.oauth2Config.ClientID); err != nil {
		return nil, err
	}
	groups, err := extractGroups(allClaims[p.groupsClaim])
	if err != nil {
		return nil, err
	}

	displayName := claims.Name
	if displayName == "" {
		displayName = claims.PreferredUsername
	}
	if displayName == "" {
		displayName = claims.Email
	}

	return &authpkg.ExternalIdentity{
		Issuer:      idToken.Issuer,
		Subject:     idToken.Subject,
		Email:       claims.Email,
		DisplayName: displayName,
		Groups:      groups,
	}, nil
}

func validateIdentityShape(idToken *gooidc.IDToken, claims map[string]json.RawMessage, clientID string) error {
	if strings.TrimSpace(idToken.Subject) == "" {
		return fmt.Errorf("%w: missing subject", ErrInvalidIDToken)
	}

	rawAuthorizedParty, ok := claims["azp"]
	if !ok {
		return nil
	}
	var authorizedParty string
	if err := json.Unmarshal(rawAuthorizedParty, &authorizedParty); err != nil {
		return fmt.Errorf("%w: decode authorized party: %v", ErrInvalidIDToken, err)
	}
	if authorizedParty != clientID {
		return fmt.Errorf("%w: authorized party mismatch", ErrInvalidIDToken)
	}
	return nil
}

func extractGroups(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var groups []string
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, nil
	}

	var group string
	if err := json.Unmarshal(raw, &group); err == nil {
		if group == "" {
			return nil, nil
		}
		return []string{group}, nil
	}

	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%w: expected string or string array", ErrInvalidGroupsClaim)
	}
	groups = make([]string, 0, len(values))
	for _, value := range values {
		group, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%w: expected string array", ErrInvalidGroupsClaim)
		}
		groups = append(groups, group)
	}
	return groups, nil
}
