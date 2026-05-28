package saml

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	crewsaml "github.com/crewjam/saml"
	"github.com/stretchr/testify/require"
)

const minimalIDPMetadata = `
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com/metadata">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`

func TestBeginCreatesTrackedRedirect(t *testing.T) {
	provider, err := New(t.Context(), Config{
		IDPMetadataXML: minimalIDPMetadata,
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/login", nil)
	redirectURL, err := provider.Begin(rec, req, "/runs?status=failed#latest")
	require.NoError(t, err)

	require.Contains(t, redirectURL, "https://idp.example.com/sso?")
	require.Contains(t, redirectURL, "SAMLRequest=")
	require.Contains(t, redirectURL, "RelayState=")

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, DefaultStateCookieName, cookies[0].Name)
	require.True(t, cookies[0].HttpOnly)

	req.AddCookie(cookies[0])
	state, err := provider.readStateCookie(req)
	require.NoError(t, err)
	require.NotEmpty(t, state.RequestID)
	require.NotEmpty(t, state.RelayState)
	require.Equal(t, "/runs?status=failed#latest", state.ReturnTo)
}

func TestBeginRejectsCrossOriginReturnTo(t *testing.T) {
	provider, err := New(t.Context(), Config{
		IDPMetadataXML: minimalIDPMetadata,
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/login", nil)
	_, err = provider.Begin(rec, req, "https://evil.example.com/runs")

	require.ErrorIs(t, err, ErrInvalidReturnTo)
	require.Empty(t, rec.Result().Cookies())
}

func TestCompleteRejectsRelayStateMismatchBeforeParsingResponse(t *testing.T) {
	provider, err := New(t.Context(), Config{
		IDPMetadataXML: minimalIDPMetadata,
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/login", nil)
	_, err = provider.Begin(rec, req, "/")
	require.NoError(t, err)

	body := "RelayState=wrong&SAMLResponse=not-a-real-response"
	acsReq := httptest.NewRequest(http.MethodPost, "/auth/sso/saml/acs", strings.NewReader(body))
	acsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acsReq.AddCookie(rec.Result().Cookies()[0])

	_, _, err = provider.CompleteWithReturnTo(acsReq)
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestCompleteRejectsTamperedStateCookieBeforeParsingResponse(t *testing.T) {
	provider, err := New(t.Context(), Config{
		IDPMetadataXML: minimalIDPMetadata,
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/login", nil)
	_, err = provider.Begin(rec, req, "/")
	require.NoError(t, err)

	cookie := rec.Result().Cookies()[0]
	stateReq := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/acs", nil)
	stateReq.AddCookie(cookie)
	state, err := provider.readStateCookie(stateReq)
	require.NoError(t, err)

	cookie.Value += "tampered"
	body := "RelayState=" + state.RelayState + "&SAMLResponse=not-a-real-response"
	acsReq := httptest.NewRequest(http.MethodPost, "/auth/sso/saml/acs", strings.NewReader(body))
	acsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acsReq.AddCookie(cookie)

	_, _, err = provider.CompleteWithReturnTo(acsReq)
	require.ErrorIs(t, err, ErrInvalidState)
	require.NotContains(t, err.Error(), "validate saml response")
}

func TestCompleteRejectsExpiredStateCookieBeforeParsingResponse(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	provider, err := New(t.Context(), Config{
		IDPMetadataXML: minimalIDPMetadata,
		PublicBaseURL:  "https://app.example.com",
		StateTTL:       time.Minute,
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	})
	require.NoError(t, err)
	provider.now = func() time.Time { return now }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/login", nil)
	_, err = provider.Begin(rec, req, "/")
	require.NoError(t, err)

	cookie := rec.Result().Cookies()[0]
	stateReq := httptest.NewRequest(http.MethodGet, "/auth/sso/saml/acs", nil)
	stateReq.AddCookie(cookie)
	state, err := provider.readStateCookie(stateReq)
	require.NoError(t, err)

	provider.now = func() time.Time { return now.Add(time.Minute + time.Second) }
	body := "RelayState=" + state.RelayState + "&SAMLResponse=not-a-real-response"
	acsReq := httptest.NewRequest(http.MethodPost, "/auth/sso/saml/acs", strings.NewReader(body))
	acsReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acsReq.AddCookie(cookie)

	_, _, err = provider.CompleteWithReturnTo(acsReq)
	require.ErrorIs(t, err, ErrInvalidState)
	require.NotContains(t, err.Error(), "validate saml response")
}

func TestIdentityFromAssertion(t *testing.T) {
	assertion := &crewsaml.Assertion{
		ID:     "assertion-1",
		Issuer: crewsaml.Issuer{Value: "https://idp.example.com/metadata"},
		Subject: &crewsaml.Subject{
			NameID: &crewsaml.NameID{Value: "subject-1"},
		},
		AttributeStatements: []crewsaml.AttributeStatement{{
			Attributes: []crewsaml.Attribute{
				{Name: "email", Values: []crewsaml.AttributeValue{{Value: "user@example.com"}}},
				{FriendlyName: "displayName", Values: []crewsaml.AttributeValue{{Value: "User Example"}}},
				{Name: "groups", Values: []crewsaml.AttributeValue{{Value: "eng"}, {Value: "admins"}}},
			},
		}},
	}

	identity, err := identityFromAssertion(assertion, "groups")
	require.NoError(t, err)
	require.Equal(t, "https://idp.example.com/metadata", identity.Issuer)
	require.Equal(t, "subject-1", identity.Subject)
	require.Equal(t, "user@example.com", identity.Email)
	require.Equal(t, "User Example", identity.DisplayName)
	require.Equal(t, []string{"eng", "admins"}, identity.Groups)
}

func TestRecordAssertionUsesConditionExpiry(t *testing.T) {
	replay := &capturingReplayCache{}
	provider := &Provider{
		replayCache: replay,
		now:         time.Now,
		serviceProvider: &crewsaml.ServiceProvider{
			IDPMetadata: &crewsaml.EntityDescriptor{EntityID: "https://idp.example.com/metadata"},
		},
	}
	expiresAt := time.Now().UTC().Add(2 * time.Minute)
	assertion := &crewsaml.Assertion{
		Issuer:     crewsaml.Issuer{Value: "https://idp.example.com/metadata"},
		ID:         "assertion-1",
		Conditions: &crewsaml.Conditions{NotOnOrAfter: expiresAt},
	}

	require.NoError(t, provider.recordAssertion(t.Context(), assertion))
	require.Equal(t, "https://idp.example.com/metadata", replay.issuer)
	require.Equal(t, "assertion-1", replay.assertionID)
	require.Equal(t, expiresAt.Add(crewsaml.MaxClockSkew), replay.expiresAt)
}

type capturingReplayCache struct {
	issuer      string
	assertionID string
	expiresAt   time.Time
}

func (c *capturingReplayCache) Record(_ context.Context, issuer, assertionID string, expiresAt time.Time) error {
	c.issuer = issuer
	c.assertionID = assertionID
	c.expiresAt = expiresAt
	return nil
}
