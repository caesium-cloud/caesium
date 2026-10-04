package oidc

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/ssostate"
)

var (
	ErrInvalidReturnTo = errors.New("invalid returnTo")
	ErrInvalidState    = errors.New("invalid oidc state")
)

const stateCookiePath = "/auth/sso/oidc"

type loginState struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
	ReturnTo     string `json:"return_to"`
	ExpiresAt    int64  `json:"expires_at"`
}

func (p *Provider) newLoginState(returnTo string) (loginState, error) {
	state, err := ssostate.RandomURLSafe(32)
	if err != nil {
		return loginState{}, err
	}
	nonce, err := ssostate.RandomURLSafe(32)
	if err != nil {
		return loginState{}, err
	}
	return loginState{
		State:     state,
		Nonce:     nonce,
		ReturnTo:  returnTo,
		ExpiresAt: p.now().Add(p.stateTTL).Unix(),
	}, nil
}

func (p *Provider) setStateCookie(w http.ResponseWriter, state loginState) error {
	value, err := p.encodeStateCookie(state)
	if err != nil {
		return err
	}
	expires := time.Unix(state.ExpiresAt, 0).UTC()
	http.SetCookie(w, p.stateCookie(value, expires, int(time.Until(expires).Seconds())))
	return nil
}

// ClearStateCookie expires the one-time pre-login state cookie.
func (p *Provider) ClearStateCookie(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, p.stateCookie("", time.Unix(0, 0).UTC(), -1))
}

func (p *Provider) stateCookie(value string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     p.stateCookieName,
		Value:    value,
		Path:     stateCookiePath,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   p.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
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
		return "", fmt.Errorf("marshal oidc state: %w", err)
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
	if state.State == "" || state.Nonce == "" || state.CodeVerifier == "" || state.ReturnTo == "" {
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
	return ssostate.ValidateReturnTo(returnTo, p.publicOrigin, ErrInvalidReturnTo)
}
