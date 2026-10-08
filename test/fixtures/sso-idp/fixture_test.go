//go:build integration

package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	crewsaml "github.com/crewjam/saml"
)

func TestOIDCFixtureEnforcesPKCEAndSingleRedemption(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var fixture *oidcFixture
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) { fixture.authorize(w, r) })
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { fixture.token(w, r) })
	server := httptest.NewServer(mux)
	defer server.Close()
	fixture = newOIDC(server.URL, "http://server.invalid", "test-client-secret", key)
	verifier := randomID()
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {clientID}, "redirect_uri": {"http://server.invalid/auth/sso/oidc/callback"}, "response_type": {"code"}, "scope": {"openid profile"}, "state": {"state-value"}, "nonce": {"nonce-value"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	c := client()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/authorize?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 302 {
		t.Fatalf("authorize status %d", res.StatusCode)
	}
	callback, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := callback.Query().Get("code")
	if code == "" || callback.Query().Get("state") != "state-value" {
		t.Fatal("callback state/code missing")
	}
	exchange := func(v, secret string) (int, []byte) {
		t.Helper()
		form := url.Values{"code": {code}, "grant_type": {"authorization_code"}, "redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {v}}
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/token", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(clientID, secret)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, data
	}
	if status, _ := exchange("wrong-verifier", "test-client-secret"); status != 400 {
		t.Fatal("bad verifier accepted")
	}
	if status, _ := exchange(verifier, "wrong-secret"); status != 400 {
		t.Fatal("bad client accepted")
	}
	status, raw := exchange(verifier, "test-client-secret")
	if status != 200 {
		t.Fatalf("valid exchange status %d", status)
	}
	var body map[string]any
	if err = json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(body["id_token"].(string), ".")
	if len(parts) != 3 {
		t.Fatal("ID token shape")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatal("invalid token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err = json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["nonce"] != "nonce-value" || claims["aud"] != clientID || claims["iss"] != server.URL {
		t.Fatal("signed protocol binding mismatch")
	}
	if status, _ := exchange(verifier, "test-client-secret"); status != 400 {
		t.Fatal("code replay accepted")
	}
	stats := fixture.stats()
	if stats["redeemed"] != 1 || stats["rejected"] != 3 {
		t.Fatalf("unexpected redemption counters %v", stats)
	}
}

func TestSAMLFixtureProducesVerifiableBoundSignedResponses(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var sp *crewsaml.ServiceProvider
	metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/sso/saml/metadata" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		if err := xml.NewEncoder(w).Encode(sp.Metadata()); err != nil {
			t.Error(err)
		}
	}))
	defer metadataServer.Close()
	fixture, err := newSAML("http://idp.invalid", metadataServer.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	metadataURL, _ := url.Parse(metadataServer.URL + "/auth/sso/saml/metadata")
	acsURL, _ := url.Parse(metadataServer.URL + "/auth/sso/saml/acs")
	sp = &crewsaml.ServiceProvider{EntityID: metadataURL.String(), MetadataURL: *metadataURL, AcsURL: *acsURL, IDPMetadata: fixture.idp.Metadata(), AllowIDPInitiated: false, MetadataValidDuration: time.Hour}
	for _, mode := range []string{"", "tampered", "bad_audience", "expired"} {
		t.Run(mode, func(t *testing.T) {
			request, err := sp.MakeAuthenticationRequest(fixture.idp.SSOURL.String(), crewsaml.HTTPRedirectBinding, crewsaml.HTTPPostBinding)
			if err != nil {
				t.Fatal(err)
			}
			form, id, err := fixture.response(t.Context(), *request, "relay-value", mode)
			if err != nil {
				t.Fatal(err)
			}
			if form.URL != acsURL.String() || form.RelayState != "relay-value" {
				t.Fatal("POST binding mismatch")
			}
			data := url.Values{"RelayState": {form.RelayState}, "SAMLResponse": {form.SAMLResponse}}
			req := httptest.NewRequestWithContext(context.Background(), "POST", form.URL, strings.NewReader(data.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			assertion, err := sp.ParseResponse(req, []string{request.ID})
			if mode != "" {
				if err == nil {
					t.Fatal("invalid signed response accepted")
				}
				return
			}
			if err != nil {
				var invalid *crewsaml.InvalidResponseError
				if errors.As(err, &invalid) {
					t.Fatal(invalid.PrivateErr)
				}
				t.Fatal(err)
			}
			if assertion.ID != id || assertion.Subject.NameID.Value != "coverage-saml-subject" {
				t.Fatal("verified assertion identity mismatch")
			}
			// A different tracked request must fail even though the signature is genuine.
			wrong := httptest.NewRequestWithContext(t.Context(), "POST", form.URL, strings.NewReader(data.Encode()))
			wrong.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := wrong.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if _, err := sp.ParseResponse(wrong, []string{"different-request"}); err == nil {
				t.Fatal("wrong InResponseTo accepted")
			}
		})
	}
}

func TestTamperCookieChangesSignificantSignatureByte(t *testing.T) {
	value := "payload.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if got := tamperCookie(value); got == value || !strings.HasPrefix(got, "payload.B") {
		t.Fatal("tamper did not change first significant byte")
	}
}

func TestReplayEvidenceRefusesExpiredIssueInstantDespiteLongConditions(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	envelope := `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" IssueInstant="2026-10-04T11:59:01Z"><saml:Assertion xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" ID="assertion-original" IssueInstant="2026-10-04T11:59:01Z"><saml:Conditions NotOnOrAfter="2026-10-04T12:04:01Z"/></saml:Assertion></samlp:Response>`
	response, assertion, err := samlIssueInstants(base64.StdEncoding.EncodeToString([]byte(envelope)), "assertion-original")
	if err != nil {
		t.Fatal(err)
	}
	original := login{replayStartedAt: now.Add(-59 * time.Second), responseIssuedAt: response, assertionIssuedAt: assertion}
	if err := requireFreshReplayEvidence(original, now); err != nil {
		t.Fatal(err)
	}
	// A callback that begins inside the limit but returns at/after the limit
	// cannot count its 401 as replay-store evidence, even with five-minute conditions.
	if err := requireFreshReplayEvidence(original, now.Add(time.Second)); err == nil {
		t.Fatal("aged signed envelope counted as replay evidence")
	}
	for _, field := range []string{"response", "assertion", "monotonic", "future", "missing"} {
		stale := original
		switch field {
		case "response":
			stale.responseIssuedAt = now.Add(-91 * time.Second)
		case "assertion":
			stale.assertionIssuedAt = now.Add(-91 * time.Second)
		case "monotonic":
			stale.replayStartedAt = now.Add(-time.Minute)
		case "future":
			stale.responseIssuedAt = now.Add(time.Second)
		case "missing":
			stale.assertionIssuedAt = time.Time{}
		}
		if err := requireFreshReplayEvidence(stale, now); err == nil {
			t.Fatalf("%s age guard did not refuse", field)
		}
	}
	if _, _, err := samlIssueInstants(base64.StdEncoding.EncodeToString([]byte(envelope)), "assertion-other"); err == nil {
		t.Fatal("foreign assertion envelope accepted")
	}
}

type failingMetadataReader struct{}

func (failingMetadataReader) Read([]byte) (int, error) {
	return 0, errors.New("metadata stream failed")
}

func TestSPMetadataRequiresCompletedBoundedTransport(t *testing.T) {
	entity := "http://owned-sp/auth/sso/saml/metadata"
	document := `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="` + entity + `"/>`
	if _, err := readSPMetadata(strings.NewReader(document), entity); err != nil {
		t.Fatal(err)
	}
	cases := map[string]io.Reader{
		"read error after complete XML": io.MultiReader(strings.NewReader(document), failingMetadataReader{}),
		"oversized trailing body":       strings.NewReader(document + strings.Repeat(" ", 1024*1024)),
		"truncated XML":                 strings.NewReader(document[:len(document)-1]),
		"foreign entity":                strings.NewReader(strings.Replace(document, entity, "http://foreign-sp/metadata", 1)),
	}
	for name, reader := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readSPMetadata(reader, entity); err == nil {
				t.Fatal("incomplete/foreign metadata accepted")
			}
		})
	}
}
