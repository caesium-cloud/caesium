package saml

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrInvalidReturnTo = errors.New("invalid returnTo")
	ErrInvalidState    = errors.New("invalid saml state")
)

type loginState struct {
	RelayState string `json:"relay_state"`
	RequestID  string `json:"request_id"`
	ReturnTo   string `json:"return_to"`
	ExpiresAt  int64  `json:"expires_at"`
}

func (p *Provider) newLoginState(requestID, returnTo string) (loginState, error) {
	relayState, err := randomURLSafe(32)
	if err != nil {
		return loginState{}, err
	}
	return loginState{
		RelayState: relayState,
		RequestID:  requestID,
		ReturnTo:   returnTo,
		ExpiresAt:  p.now().Add(p.stateTTL).Unix(),
	}, nil
}

func (p *Provider) setStateCookie(w http.ResponseWriter, state loginState) error {
	value, err := p.encodeStateCookie(state)
	if err != nil {
		return err
	}
	expires := time.Unix(state.ExpiresAt, 0).UTC()
	sameSite := http.SameSiteLaxMode
	if p.cookieSecure {
		sameSite = http.SameSiteNoneMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     p.stateCookieName,
		Value:    value,
		Path:     "/auth/sso/saml",
		Expires:  expires,
		MaxAge:   int(p.stateTTL.Seconds()),
		HttpOnly: true,
		Secure:   p.cookieSecure,
		SameSite: sameSite,
	})
	return nil
}

func (p *Provider) readStateCookie(r *http.Request) (loginState, error) {
	cookie, err := r.Cookie(p.stateCookieName)
	if err != nil {
		return loginState{}, fmt.Errorf("%w: missing state cookie", ErrInvalidState)
	}
	return p.decodeStateCookie(cookie.Value)
}

func (p *Provider) encodeStateCookie(state loginState) (string, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("marshal saml state: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	sig := p.signStatePayload([]byte(encodedPayload))
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (p *Provider) decodeStateCookie(raw string) (loginState, error) {
	payloadPart, sigPart, ok := strings.Cut(raw, ".")
	if !ok || payloadPart == "" || sigPart == "" {
		return loginState{}, fmt.Errorf("%w: malformed state cookie", ErrInvalidState)
	}

	gotSig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return loginState{}, fmt.Errorf("%w: malformed state signature", ErrInvalidState)
	}
	wantSig := p.signStatePayload([]byte(payloadPart))
	if subtle.ConstantTimeCompare(gotSig, wantSig) != 1 {
		return loginState{}, fmt.Errorf("%w: signature mismatch", ErrInvalidState)
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return loginState{}, fmt.Errorf("%w: malformed state payload", ErrInvalidState)
	}
	var state loginState
	if err := json.Unmarshal(payload, &state); err != nil {
		return loginState{}, fmt.Errorf("%w: decode state payload", ErrInvalidState)
	}
	if state.RelayState == "" || state.RequestID == "" || state.ReturnTo == "" {
		return loginState{}, fmt.Errorf("%w: incomplete state", ErrInvalidState)
	}
	if p.now().After(time.Unix(state.ExpiresAt, 0)) {
		return loginState{}, fmt.Errorf("%w: expired state", ErrInvalidState)
	}
	return state, nil
}

func (p *Provider) signStatePayload(payload []byte) []byte {
	mac := hmac.New(sha256.New, p.cookieSecret)
	mac.Write(payload)
	return mac.Sum(nil)
}

func (p *Provider) validateReturnTo(returnTo string) (string, error) {
	returnTo = strings.TrimSpace(returnTo)
	if returnTo == "" {
		return "/", nil
	}
	if strings.HasPrefix(returnTo, `\`) || strings.HasPrefix(returnTo, `//`) {
		return "", ErrInvalidReturnTo
	}

	u, err := url.Parse(returnTo)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidReturnTo, err)
	}
	if u.IsAbs() {
		if !sameOrigin(u, p.publicOrigin) {
			return "", ErrInvalidReturnTo
		}
		return requestURI(u), nil
	}
	if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "", ErrInvalidReturnTo
	}
	return requestURI(u), nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func requestURI(u *url.URL) string {
	out := u.EscapedPath()
	if out == "" {
		out = "/"
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
