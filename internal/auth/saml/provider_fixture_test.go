package saml

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	crewsaml "github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/stretchr/testify/require"
)

const samlFixturePublicBaseURL = "https://app.example.com"

var samlFixtureNow = time.Date(2026, 5, 28, 21, 0, 0, 0, time.UTC)

func TestCompleteWithReturnToAcceptsSignedSAMLResponseFixture(t *testing.T) {
	fixture := newSignedSAMLFixture(t, nil)

	identity, returnTo, err := fixture.provider.CompleteWithReturnTo(fixture.acsRequest(t, fixture.samlResponse))
	require.NoError(t, err)

	require.Equal(t, "/runs?status=failed#latest", returnTo)
	require.Equal(t, "https://idp.example.com/metadata", identity.Issuer)
	require.Equal(t, "fixture-subject", identity.Subject)
	require.Equal(t, "user@example.com", identity.Email)
	require.Equal(t, "Fixture User", identity.DisplayName)
	require.Equal(t, []string{"eng", "admins"}, identity.Groups)
}

func TestCompleteWithReturnToRejectsTamperedSignedSAMLResponseFixture(t *testing.T) {
	fixture := newSignedSAMLFixture(t, nil)
	tamperedResponse := tamperSAMLResponse(t, fixture.samlResponse, "user@example.com", "attacker@example.com")

	_, _, err := fixture.provider.CompleteWithReturnTo(fixture.acsRequest(t, tamperedResponse))
	require.Error(t, err)
	require.ErrorContains(t, err, "validate saml response")
	require.Zero(t, fixture.replay.recordCount())
}

func TestCompleteWithReturnToRejectsReplayOfSignedSAMLResponseFixture(t *testing.T) {
	fixture := newSignedSAMLFixture(t, nil)

	_, _, err := fixture.provider.CompleteWithReturnTo(fixture.acsRequest(t, fixture.samlResponse))
	require.NoError(t, err)

	_, _, err = fixture.provider.CompleteWithReturnTo(fixture.acsRequest(t, fixture.samlResponse))
	require.ErrorIs(t, err, ErrAssertionReplay)
}

func TestCompleteWithReturnToRejectsExpiredSignedAssertionFixture(t *testing.T) {
	expiredAt := samlFixtureNow.Add(-crewsaml.MaxClockSkew - time.Minute)
	fixture := newSignedSAMLFixture(t, func(assertion *crewsaml.Assertion) {
		assertion.Conditions.NotOnOrAfter = expiredAt
		for i := range assertion.Subject.SubjectConfirmations {
			assertion.Subject.SubjectConfirmations[i].SubjectConfirmationData.NotOnOrAfter = expiredAt
		}
	})

	_, _, err := fixture.provider.CompleteWithReturnTo(fixture.acsRequest(t, fixture.samlResponse))
	require.Error(t, err)
	require.ErrorContains(t, err, "validate saml response")
	require.Zero(t, fixture.replay.recordCount())
}

type signedSAMLFixture struct {
	provider     *Provider
	replay       *memoryReplayCache
	cookie       *http.Cookie
	relayState   string
	samlResponse string
}

func newSignedSAMLFixture(t *testing.T, mutateAssertion func(*crewsaml.Assertion)) signedSAMLFixture {
	t.Helper()
	setSAMLFixtureGlobals(t)

	idp := newFixtureIDP(t)
	replay := &memoryReplayCache{records: map[string]time.Time{}}
	provider, err := New(t.Context(), Config{
		IDPMetadataXML: metadataXML(t, idp.Metadata()),
		PublicBaseURL:  samlFixturePublicBaseURL,
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    replay,
	})
	require.NoError(t, err)
	provider.now = func() time.Time { return samlFixtureNow }

	rec := httptest.NewRecorder()
	beginReq := httptest.NewRequest(http.MethodGet, samlFixturePublicBaseURL+"/auth/sso/saml/login", nil)
	_, err = provider.Begin(rec, beginReq, "/runs?status=failed#latest")
	require.NoError(t, err)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)

	stateReq := httptest.NewRequest(http.MethodGet, samlFixturePublicBaseURL+"/auth/sso/saml/acs", nil)
	stateReq.AddCookie(cookies[0])
	state, err := provider.readStateCookie(stateReq)
	require.NoError(t, err)

	return signedSAMLFixture{
		provider:     provider,
		replay:       replay,
		cookie:       cookies[0],
		relayState:   state.RelayState,
		samlResponse: makeSignedSAMLResponse(t, provider, idp, state, mutateAssertion),
	}
}

func (f signedSAMLFixture) acsRequest(t *testing.T, samlResponse string) *http.Request {
	t.Helper()
	form := url.Values{
		"RelayState":   {f.relayState},
		"SAMLResponse": {samlResponse},
	}
	req := httptest.NewRequest(
		http.MethodPost,
		samlFixturePublicBaseURL+"/auth/sso/saml/acs",
		strings.NewReader(form.Encode()),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.cookie)
	return req
}

func makeSignedSAMLResponse(
	t *testing.T,
	provider *Provider,
	idp *crewsaml.IdentityProvider,
	state loginState,
	mutateAssertion func(*crewsaml.Assertion),
) string {
	t.Helper()

	spMetadata := provider.serviceProvider.Metadata()
	require.NotEmpty(t, spMetadata.SPSSODescriptors)
	spDescriptor := &spMetadata.SPSSODescriptors[0]
	require.NotEmpty(t, spDescriptor.AssertionConsumerServices)
	acsEndpoint := &spDescriptor.AssertionConsumerServices[0]

	assertion := fixtureAssertion(provider, idp, state)
	if mutateAssertion != nil {
		mutateAssertion(assertion)
	}

	req := &crewsaml.IdpAuthnRequest{
		Now:                     samlFixtureNow,
		IDP:                     idp,
		RelayState:              state.RelayState,
		HTTPRequest:             httptest.NewRequest(http.MethodPost, idp.SSOURL.String(), nil),
		Request:                 fixtureAuthnRequest(provider, state.RequestID),
		ServiceProviderMetadata: spMetadata,
		SPSSODescriptor:         spDescriptor,
		ACSEndpoint:             acsEndpoint,
		Assertion:               assertion,
	}
	require.NoError(t, req.MakeResponse())
	form, err := req.PostBinding()
	require.NoError(t, err)
	require.Equal(t, provider.serviceProvider.AcsURL.String(), form.URL)
	require.Equal(t, state.RelayState, form.RelayState)
	return form.SAMLResponse
}

func fixtureAuthnRequest(provider *Provider, requestID string) crewsaml.AuthnRequest {
	return crewsaml.AuthnRequest{
		ID:                          requestID,
		Version:                     "2.0",
		IssueInstant:                samlFixtureNow,
		Destination:                 "https://idp.example.com/sso",
		Issuer:                      &crewsaml.Issuer{Value: provider.serviceProvider.EntityID},
		AssertionConsumerServiceURL: provider.serviceProvider.AcsURL.String(),
		ProtocolBinding:             crewsaml.HTTPPostBinding,
	}
}

func fixtureAssertion(provider *Provider, idp *crewsaml.IdentityProvider, state loginState) *crewsaml.Assertion {
	validUntil := samlFixtureNow.Add(2 * time.Minute)
	return &crewsaml.Assertion{
		ID:           "fixture-assertion",
		IssueInstant: samlFixtureNow,
		Version:      "2.0",
		Issuer: crewsaml.Issuer{
			Format: "urn:oasis:names:tc:SAML:2.0:nameid-format:entity",
			Value:  idp.MetadataURL.String(),
		},
		Subject: &crewsaml.Subject{
			NameID: &crewsaml.NameID{
				Format:          string(crewsaml.PersistentNameIDFormat),
				NameQualifier:   idp.MetadataURL.String(),
				SPNameQualifier: provider.serviceProvider.EntityID,
				Value:           "fixture-subject",
			},
			SubjectConfirmations: []crewsaml.SubjectConfirmation{{
				Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer",
				SubjectConfirmationData: &crewsaml.SubjectConfirmationData{
					InResponseTo: state.RequestID,
					NotOnOrAfter: validUntil,
					Recipient:    provider.serviceProvider.AcsURL.String(),
				},
			}},
		},
		Conditions: &crewsaml.Conditions{
			NotBefore:    samlFixtureNow.Add(-time.Minute),
			NotOnOrAfter: validUntil,
			AudienceRestrictions: []crewsaml.AudienceRestriction{{
				Audience: crewsaml.Audience{Value: provider.serviceProvider.EntityID},
			}},
		},
		AuthnStatements: []crewsaml.AuthnStatement{{
			AuthnInstant: samlFixtureNow.Add(-time.Minute),
			SessionIndex: "fixture-session",
			AuthnContext: crewsaml.AuthnContext{
				AuthnContextClassRef: &crewsaml.AuthnContextClassRef{
					Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport",
				},
			},
		}},
		AttributeStatements: []crewsaml.AttributeStatement{{
			Attributes: []crewsaml.Attribute{
				{Name: "email", Values: []crewsaml.AttributeValue{{Type: "xs:string", Value: "user@example.com"}}},
				{FriendlyName: "displayName", Values: []crewsaml.AttributeValue{{Type: "xs:string", Value: "Fixture User"}}},
				{Name: "groups", Values: []crewsaml.AttributeValue{
					{Type: "xs:string", Value: "eng"},
					{Type: "xs:string", Value: "admins"},
				}},
			},
		}},
	}
}

func newFixtureIDP(t *testing.T) *crewsaml.IdentityProvider {
	t.Helper()
	cert, signer := fixtureKeyPair(t)
	metadataURL := mustParseFixtureURL("https://idp.example.com/metadata")
	return &crewsaml.IdentityProvider{
		Signer:          signer,
		Certificate:     cert,
		MetadataURL:     metadataURL,
		SSOURL:          mustParseFixtureURL("https://idp.example.com/sso"),
		SignatureMethod: dsig.RSASHA256SignatureMethod,
	}
}

func fixtureKeyPair(t *testing.T) (*x509.Certificate, crypto.Signer) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "fixture-idp.example.com",
			Organization: []string{"Caesium Test"},
		},
		NotBefore:             samlFixtureNow.Add(-time.Hour),
		NotAfter:              samlFixtureNow.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func metadataXML(t *testing.T, descriptor *crewsaml.EntityDescriptor) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, xml.NewEncoder(&buf).Encode(descriptor))
	return buf.String()
}

func mustParseFixtureURL(raw string) url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return *u
}

func tamperSAMLResponse(t *testing.T, encoded, oldValue, newValue string) string {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	require.Contains(t, string(decoded), oldValue)
	return base64.StdEncoding.EncodeToString(bytes.Replace(decoded, []byte(oldValue), []byte(newValue), 1))
}

func setSAMLFixtureGlobals(t *testing.T) {
	t.Helper()
	oldTimeNow := crewsaml.TimeNow
	oldClock := crewsaml.Clock
	oldRandReader := crewsaml.RandReader
	crewsaml.TimeNow = func() time.Time { return samlFixtureNow }
	crewsaml.Clock = dsig.NewFakeClockAt(samlFixtureNow)
	crewsaml.RandReader = &samlFixtureRandReader{}
	t.Cleanup(func() {
		crewsaml.TimeNow = oldTimeNow
		crewsaml.Clock = oldClock
		crewsaml.RandReader = oldRandReader
	})
}

type samlFixtureRandReader struct {
	next byte
}

func (r *samlFixtureRandReader) Read(p []byte) (int, error) {
	for i := range p {
		r.next++
		p[i] = r.next
	}
	return len(p), nil
}

type memoryReplayCache struct {
	mu      sync.Mutex
	records map[string]time.Time
}

func (m *memoryReplayCache) Record(_ context.Context, issuer, assertionID string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := fmt.Sprintf("%s\x00%s", issuer, assertionID)
	if _, ok := m.records[key]; ok {
		return ErrAssertionReplay
	}
	m.records[key] = expiresAt
	return nil
}

func (m *memoryReplayCache) recordCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

var _ AssertionReplayCache = (*memoryReplayCache)(nil)
