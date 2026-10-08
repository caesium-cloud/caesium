//go:build integration

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type authorization struct {
	nonce, challenge, redirect, mode string
	expires                          time.Time
}
type oidcFixture struct {
	issuer, spBase, secret     string
	key                        *rsa.PrivateKey
	mu                         sync.Mutex
	codes                      map[string]authorization
	issued, redeemed, rejected int
}

func newOIDC(issuer, sp, secret string, key *rsa.PrivateKey) *oidcFixture {
	return &oidcFixture{issuer: issuer, spBase: sp, secret: secret, key: key, codes: make(map[string]authorization)}
}
func (o *oidcFixture) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"issuer": o.issuer, "authorization_endpoint": o.issuer + "/authorize", "token_endpoint": o.issuer + "/token", "jwks_uri": o.issuer + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}, "scopes_supported": []string{"openid", "profile", "email", "groups"}})
}
func (o *oidcFixture) keys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"keys": []map[string]any{{"kty": "RSA", "use": "sig", "kid": "coverage-key", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(o.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(o.key.E)).Bytes())}}})
}
func (o *oidcFixture) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("fixture_mode")
	if r.Method != "GET" || q.Get("client_id") != clientID || q.Get("redirect_uri") != o.spBase+"/auth/sso/oidc/callback" || q.Get("response_type") != "code" || !strings.Contains(" "+q.Get("scope")+" ", " openid ") || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || !modeAllowed(mode, "bad_nonce", "bad_audience") {
		http.Error(w, "invalid authorization", 400)
		return
	}
	code := randomID()
	o.mu.Lock()
	o.codes[code] = authorization{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), mode: mode, expires: time.Now().Add(2 * time.Minute)}
	o.issued++
	o.mu.Unlock()
	target, _ := url.Parse(q.Get("redirect_uri"))
	target.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}
func (o *oidcFixture) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.ParseForm() != nil {
		http.Error(w, "invalid token request", 400)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	code := r.Form.Get("code")
	a, found := o.codes[code]
	challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if id != clientID || subtle.ConstantTimeCompare([]byte(secret), []byte(o.secret)) != 1 || !found || !time.Now().Before(a.expires) || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != a.redirect || base64.RawURLEncoding.EncodeToString(challenge[:]) != a.challenge {
		o.rejected++
		http.Error(w, "invalid grant", 400)
		return
	}
	delete(o.codes, code)
	o.redeemed++
	nonce, audience := a.nonce, clientID
	if a.mode == "bad_nonce" {
		nonce = "wrong-nonce"
	}
	if a.mode == "bad_audience" {
		audience = "wrong-client"
	}
	claims := map[string]any{"iss": o.issuer, "sub": "coverage-oidc-subject", "aud": audience, "exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Add(-time.Second).Unix(), "nonce": nonce, "email": "oidc-coverage@example.invalid", "name": "Coverage OIDC", "groups": []string{"coverage-admins", "coverage-readers"}}
	token, err := o.sign(claims)
	if err != nil {
		http.Error(w, "signing failed", 500)
		return
	}
	writeJSON(w, map[string]any{"access_token": randomID(), "token_type": "Bearer", "expires_in": 300, "id_token": token})
}
func (o *oidcFixture) sign(claims map[string]any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "coverage-key", "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, o.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
func (o *oidcFixture) stats() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return map[string]int{"issued": o.issued, "redeemed": o.redeemed, "rejected": o.rejected}
}
