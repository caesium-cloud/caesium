package saml

import (
	"context"
	"crypto/subtle"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	crewsaml "github.com/crewjam/saml"
)

var (
	ErrMissingSAMLResponse = errors.New("saml callback missing response")
	ErrNoRedirectBinding   = errors.New("saml idp metadata does not expose HTTP-Redirect SSO")
)

// Provider implements the Caesium browser redirect authenticator for SAML.
type Provider struct {
	serviceProvider *crewsaml.ServiceProvider

	groupsAttribute string
	stateCookieName string
	stateTTL        time.Duration
	cookieSecure    bool
	cookieSecret    []byte
	publicOrigin    *url.URL
	replayCache     AssertionReplayCache
	now             func() time.Time
}

var _ authpkg.RedirectAuthenticator = (*Provider)(nil)

// New constructs a SAML SP-initiated redirect provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	idpMetadata, err := loadIDPMetadata(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cert, key, err := loadKeyPair(cfg.SPCertPath, cfg.SPKeyPath)
	if err != nil {
		return nil, err
	}

	acsURL, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return nil, fmt.Errorf("parse saml ACS URL: %w", err)
	}
	metadataURL, err := url.Parse(cfg.MetadataURL)
	if err != nil {
		return nil, fmt.Errorf("parse saml metadata URL: %w", err)
	}

	return &Provider{
		serviceProvider: &crewsaml.ServiceProvider{
			EntityID:              cfg.SPEntityID,
			Key:                   key,
			Certificate:           cert,
			HTTPClient:            cfg.HTTPClient,
			MetadataURL:           *metadataURL,
			AcsURL:                *acsURL,
			IDPMetadata:           idpMetadata,
			AuthnNameIDFormat:     crewsaml.PersistentNameIDFormat,
			AllowIDPInitiated:     false,
			DefaultRedirectURI:    "/",
			MetadataValidDuration: crewsaml.DefaultValidDuration,
		},
		groupsAttribute: cfg.GroupsAttribute,
		stateCookieName: cfg.StateCookieName,
		stateTTL:        cfg.StateTTL,
		cookieSecure:    cfg.CookieSecure,
		cookieSecret:    append([]byte(nil), cfg.CookieSecret...),
		publicOrigin:    &url.URL{Scheme: acsURL.Scheme, Host: acsURL.Host},
		replayCache:     cfg.ReplayCache,
		now:             time.Now,
	}, nil
}

// Name reports the provider id used by the shared SSO completion path.
func (p *Provider) Name() string {
	return ProviderName
}

// Begin creates a tracked AuthnRequest and returns the IdP redirect URL.
func (p *Provider) Begin(w http.ResponseWriter, _ *http.Request, returnTo string) (string, error) {
	validReturnTo, err := p.validateReturnTo(returnTo)
	if err != nil {
		return "", err
	}

	bindingLocation := p.serviceProvider.GetSSOBindingLocation(crewsaml.HTTPRedirectBinding)
	if bindingLocation == "" {
		return "", ErrNoRedirectBinding
	}
	req, err := p.serviceProvider.MakeAuthenticationRequest(
		bindingLocation,
		crewsaml.HTTPRedirectBinding,
		crewsaml.HTTPPostBinding,
	)
	if err != nil {
		return "", fmt.Errorf("create saml authn request: %w", err)
	}
	state, err := p.newLoginState(req.ID, validReturnTo)
	if err != nil {
		return "", err
	}
	if err := p.setStateCookie(w, state); err != nil {
		return "", err
	}

	redirectURL, err := req.Redirect(state.RelayState, p.serviceProvider)
	if err != nil {
		return "", fmt.Errorf("build saml redirect URL: %w", err)
	}
	return redirectURL.String(), nil
}

// Complete validates the ACS callback and returns the normalized external identity.
func (p *Provider) Complete(r *http.Request) (*authpkg.ExternalIdentity, error) {
	ext, _, err := p.CompleteWithReturnTo(r)
	return ext, err
}

// CompleteWithReturnTo validates the ACS callback and returns the RelayState
// return destination stored during Begin.
func (p *Provider) CompleteWithReturnTo(r *http.Request) (*authpkg.ExternalIdentity, string, error) {
	if err := r.ParseForm(); err != nil {
		return nil, "", fmt.Errorf("parse saml callback: %w", err)
	}
	if r.Form.Get("SAMLResponse") == "" && r.Form.Get("SAMLart") == "" {
		return nil, "", ErrMissingSAMLResponse
	}
	gotRelayState := r.Form.Get("RelayState")
	if gotRelayState == "" {
		return nil, "", fmt.Errorf("%w: missing relay state", ErrInvalidState)
	}

	state, err := p.readStateCookie(r)
	if err != nil {
		return nil, "", err
	}
	if subtle.ConstantTimeCompare([]byte(gotRelayState), []byte(state.RelayState)) != 1 {
		return nil, "", fmt.Errorf("%w: relay state mismatch", ErrInvalidState)
	}

	assertion, err := p.serviceProvider.ParseResponse(r, []string{state.RequestID})
	if err != nil {
		return nil, "", fmt.Errorf("validate saml response: %w", err)
	}
	if assertion == nil {
		return nil, "", fmt.Errorf("validate saml response: missing assertion")
	}
	if err := p.recordAssertion(r.Context(), assertion); err != nil {
		return nil, "", err
	}

	identity, err := identityFromAssertion(assertion, p.groupsAttribute)
	if err != nil {
		return nil, "", err
	}
	return identity, state.ReturnTo, nil
}

// Metadata writes the SP metadata document used to register Caesium with an IdP.
func (p *Provider) Metadata(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	w.WriteHeader(http.StatusOK)
	return xml.NewEncoder(w).Encode(p.serviceProvider.Metadata())
}

func (p *Provider) recordAssertion(ctx context.Context, assertion *crewsaml.Assertion) error {
	if p.replayCache == nil {
		return fmt.Errorf("saml assertion replay cache is unavailable")
	}
	issuer := assertion.Issuer.Value
	if issuer == "" && p.serviceProvider.IDPMetadata != nil {
		issuer = p.serviceProvider.IDPMetadata.EntityID
	}
	if err := p.replayCache.Record(ctx, issuer, assertion.ID, assertionReplayExpiresAt(assertion, p.now())); err != nil {
		return err
	}
	return nil
}

func assertionReplayExpiresAt(assertion *crewsaml.Assertion, now time.Time) time.Time {
	expiresAt := time.Time{}
	extend := func(candidate time.Time) {
		if !candidate.IsZero() && candidate.After(expiresAt) {
			expiresAt = candidate
		}
	}
	if assertion != nil {
		if assertion.Conditions != nil {
			extend(assertion.Conditions.NotOnOrAfter.Add(crewsaml.MaxClockSkew))
		}
		if assertion.Subject != nil {
			for _, confirmation := range assertion.Subject.SubjectConfirmations {
				if confirmation.SubjectConfirmationData != nil {
					extend(confirmation.SubjectConfirmationData.NotOnOrAfter.Add(crewsaml.MaxClockSkew))
				}
			}
		}
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(crewsaml.MaxIssueDelay + crewsaml.MaxClockSkew)
	}
	return expiresAt.UTC()
}
