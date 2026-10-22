package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// CLI login routes (identity-and-access E5, design §13.1):
//
//	GET  /auth/cli/start      public: store a login transaction, redirect to finish
//	GET  /auth/cli/finish     identity: the signed-in user's confirmation page (mints nothing)
//	POST /auth/cli/authorize  identity, CSRF: the confirmation; issues a one-time code
//	POST /auth/cli/exchange   public, IP-limited: code + verifier → user-bound key, once

// cliRequestBodyLimit bounds the JSON bodies of authorize and exchange.
const cliRequestBodyLimit = 4 << 10

// CLISignInMethod is one way to sign in that the finish page offers a browser
// that has no session yet.
type CLISignInMethod struct {
	Label string
	// LoginURL is a redirect provider's login route; the page appends
	// returnTo. Empty means "sign in to the Console, then reload".
	LoginURL string
}

// SetKeyService wires the API-key service (user-bound key mint and
// self-revocation) into the controller.
func (s *SSOController) SetKeyService(keys *iauth.Service) {
	s.keys = keys
}

// SetCLISignInMethods sets the sign-in choices the CLI finish page shows a
// browser without a session.
func (s *SSOController) SetCLISignInMethods(methods []CLISignInMethod) {
	s.cliSignIn = append([]CLISignInMethod(nil), methods...)
}

func noStore(c *echo.Context) {
	h := c.Response().Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
}

// CLIStart is GET /auth/cli/start?state=&challenge=&port=|mode=manual&hostname=.
func (s *SSOController) CLIStart(c *echo.Context) error {
	noStore(c)
	if s.keys == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "cli login unavailable")
	}
	q := c.Request().URL.Query()
	req := iauth.StartCLILoginRequest{
		State:        q.Get("state"),
		Challenge:    q.Get("challenge"),
		HostnameHint: q.Get("hostname"),
		SourceIP:     c.RealIP(),
	}
	rawPort, mode := q.Get("port"), q.Get("mode")
	switch {
	case mode == models.CLILoginModeManual && rawPort == "":
		req.Manual = true
	case mode == "" && rawPort != "":
		port, ok := parseCLIPort(rawPort)
		if !ok {
			return echo.NewHTTPError(http.StatusBadRequest, "port must be an integer from 1024 to 65535")
		}
		req.Port = port
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "exactly one of port or mode=manual is required")
	}
	if _, err := s.keys.StartCLILogin(c.Request().Context(), req); err != nil {
		switch {
		case errors.Is(err, iauth.ErrCLILoginInvalid):
			return echo.NewHTTPError(http.StatusBadRequest, "state and challenge must be base64url S256 values")
		case errors.Is(err, iauth.ErrCLILoginStateInUse):
			return echo.NewHTTPError(http.StatusConflict, "login state already in use")
		case errors.Is(err, iauth.ErrCLILoginTooMany):
			c.Response().Header().Set("Retry-After", strconv.Itoa(int(iauth.CLILoginTransactionTTL.Seconds())))
			return echo.NewHTTPError(http.StatusTooManyRequests, "too many pending cli logins from this address")
		default:
			log.Error("cli login start failed", "error", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "cli login start failed")
		}
	}
	return c.Redirect(http.StatusFound, cliFinishPath(req.State))
}

func cliFinishPath(state string) string {
	return "/auth/cli/finish?state=" + url.QueryEscape(state)
}

// parseCLIPort accepts only a plain decimal port in the loopback range.
func parseCLIPort(raw string) (int, bool) {
	if raw == "" || len(raw) > 5 {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < iauth.CLILoginMinPort || port > iauth.CLILoginMaxPort {
		return 0, false
	}
	return port, true
}

// cliBrowserSession returns the cookie session that authenticated a finish or
// authorize request, re-validated for its user's current disable generation.
// A bearer-authenticated request is refused: the confirmation is an act of
// the signed-in browser, never of a key (a user-bound key minting its own
// successor would outlive its expiry).
func (s *SSOController) cliBrowserSession(c *echo.Context) (*models.Session, *models.User, error) {
	principal := authmw.GetPrincipal(c)
	if principal == nil || principal.Kind != iauth.PrincipalUser || principal.UserKeyID != nil ||
		principal.UserID == nil || c.Request().Header.Get("Authorization") != "" {
		return nil, nil, echo.NewHTTPError(http.StatusForbidden, "confirm the CLI login from a signed-in browser session")
	}
	cookie, err := c.Request().Cookie(s.cookieName)
	if err != nil || cookie.Value == "" || s.sessions == nil {
		return nil, nil, echo.NewHTTPError(http.StatusForbidden, "confirm the CLI login from a signed-in browser session")
	}
	sess, user, err := s.sessions.Validate(c.Request().Context(), cookie.Value)
	if err != nil {
		return nil, nil, echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired session")
	}
	if user.ID != *principal.UserID {
		return nil, nil, echo.NewHTTPError(http.StatusForbidden, "session changed during the request")
	}
	return sess, user, nil
}

// CLIFinish is GET /auth/cli/finish?state=: the confirmation page. It never
// mints and never shows a key; an unknown, expired or used state is an error
// page.
func (s *SSOController) CLIFinish(c *echo.Context) error {
	noStore(c)
	if s.keys == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "cli login unavailable")
	}
	_, user, err := s.cliBrowserSession(c)
	if err != nil {
		return err
	}
	state := c.QueryParam("state")
	txn, err := s.keys.LookupCLILogin(c.Request().Context(), state)
	if err != nil {
		status, message := cliLookupError(err)
		if status == http.StatusInternalServerError {
			log.Error("cli login lookup failed", "error", err)
		}
		return renderCLIPage(c, status, cliPage{Title: "Caesium CLI sign-in", Error: message})
	}
	target := "manual code (no browser redirect)"
	if txn.Mode == models.CLILoginModeLoopback {
		target = fmt.Sprintf("loopback port %d on this computer", txn.Port)
	}
	return renderCLIPage(c, http.StatusOK, cliPage{
		Title:    "Authorise the Caesium CLI",
		Confirm:  true,
		Email:    user.Email,
		Hostname: txn.HostnameHint,
		Target:   target,
		State:    state,
		CSRF:     authmw.GetCSRFToken(c),
		Expires:  txn.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func cliLookupError(err error) (int, string) {
	switch {
	case errors.Is(err, iauth.ErrCLILoginNotFound):
		return http.StatusNotFound, "This sign-in request is unknown. Run caesium login again."
	case errors.Is(err, iauth.ErrCLILoginExpired):
		return http.StatusGone, "This sign-in request has expired. Run caesium login again."
	case errors.Is(err, iauth.ErrCLILoginConsumed):
		return http.StatusConflict, "This sign-in request was already used. Run caesium login again."
	default:
		return http.StatusInternalServerError, "The sign-in request could not be loaded."
	}
}

type cliAuthorizeRequest struct {
	State string `json:"state"`
}

// CLIAuthorize is POST /auth/cli/authorize {state}: the confirmation click.
// The auth middleware has already required the session's CSRF token.
func (s *SSOController) CLIAuthorize(c *echo.Context) error {
	noStore(c)
	if s.keys == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "cli login unavailable")
	}
	sess, user, err := s.cliBrowserSession(c)
	if err != nil {
		return err
	}
	var body cliAuthorizeRequest
	if err := decodeCLIBody(c, &body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	result, err := s.keys.AuthorizeCLILogin(c.Request().Context(), iauth.AuthorizeCLILoginRequest{
		State:             body.State,
		UserID:            user.ID,
		DisableGeneration: user.DisableGeneration,
		SessionID:         sess.ID,
		Email:             user.Email,
		SourceIP:          c.RealIP(),
	})
	if err != nil {
		switch {
		case errors.Is(err, iauth.ErrCLILoginNotFound), errors.Is(err, iauth.ErrCLILoginExpired), errors.Is(err, iauth.ErrCLILoginConsumed):
			status, message := cliLookupError(err)
			return echo.NewHTTPError(status, message)
		case errors.Is(err, iauth.ErrSessionRevoked), errors.Is(err, iauth.ErrSessionExpired), errors.Is(err, iauth.ErrSessionInvalid):
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired session")
		case errors.Is(err, iauth.ErrUserDisabled), errors.Is(err, iauth.ErrUserChanged), errors.Is(err, iauth.ErrUserNotFound):
			return echo.NewHTTPError(http.StatusForbidden, "this user cannot sign in")
		default:
			log.Error("cli login authorize failed", "error", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "cli login authorize failed")
		}
	}
	if result.Mode == models.CLILoginModeLoopback {
		callback := url.URL{
			Scheme: "http",
			Host:   "127.0.0.1:" + strconv.Itoa(result.Port),
			Path:   "/callback",
			RawQuery: url.Values{
				"code":  []string{result.Code},
				"state": []string{body.State},
			}.Encode(),
		}
		return c.JSON(http.StatusOK, map[string]any{"redirect_url": callback.String()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"code":       result.Code,
		"expires_at": result.RedeemBy.UTC().Format(time.RFC3339),
	})
}

type cliExchangeRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
}

// CLIExchange is POST /auth/cli/exchange {code, code_verifier}. Every refusal
// is the same 401, which the route's IP limiter counts.
func (s *SSOController) CLIExchange(c *echo.Context) error {
	noStore(c)
	if s.keys == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "cli login unavailable")
	}
	var body cliExchangeRequest
	if err := decodeCLIBody(c, &body); err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, iauth.ErrCLILoginCodeInvalid.Error())
	}
	result, err := s.keys.ExchangeCLILogin(c.Request().Context(), iauth.ExchangeCLILoginRequest{
		Code:     body.Code,
		Verifier: body.CodeVerifier,
		SourceIP: c.RealIP(),
	})
	if err != nil {
		if errors.Is(err, iauth.ErrCLILoginCodeInvalid) {
			s.recordCLIExchangeRefused(c, err)
			return echo.NewHTTPError(http.StatusUnauthorized, iauth.ErrCLILoginCodeInvalid.Error())
		}
		log.Error("cli login exchange failed", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "cli login exchange failed")
	}
	key := result.Key.Key
	resp := map[string]any{
		"key":        result.Key.Plaintext,
		"key_id":     key.ID.String(),
		"key_prefix": key.KeyPrefix,
		"subject":    result.Subject,
	}
	if key.ExpiresAt != nil {
		resp["expires_at"] = key.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return c.JSON(http.StatusOK, resp)
}

// recordCLIExchangeRefused audits a refusal that left no row to burn (an
// unknown, used or expired code): a wrong verifier's refusal is audited inside
// the burning transaction.
func (s *SSOController) recordCLIExchangeRefused(c *echo.Context, err error) {
	if s.auditor == nil {
		return
	}
	reason := "invalid"
	if refusal, ok := errors.AsType[*iauth.CLILoginRefusal](err); ok {
		if refusal.Audited {
			return
		}
		reason = refusal.Reason
	}
	auditlog.LogFailure(s.auditor.Log(iauth.AuditEntry{
		Actor:        "anonymous",
		Action:       iauth.ActionAuthCLILogin,
		ResourceType: "cli_login",
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeDenied,
		Metadata:     map[string]any{"reason": reason},
	}))
}

// logoutUserKey revokes the user-bound key that authenticated a logout
// (`caesium logout`). It sets no cookie: the bearer, not any cookie sent
// alongside it, was the request's credential.
func (s *SSOController) logoutUserKey(c *echo.Context, principal *iauth.Principal) error {
	if s.keys == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "logout unavailable")
	}
	keyID := *principal.UserKeyID
	var userID = uuidOrNil(principal.UserID)
	outcome := iauth.OutcomeSuccess
	err := s.keys.RevokeOwnUserKey(c.Request().Context(), keyID, userID)
	if err != nil {
		outcome = iauth.OutcomeError
	}
	if s.auditor != nil {
		auditlog.LogFailure(s.auditor.Log(iauth.AuditEntry{
			Actor:        principal.Subject,
			Action:       iauth.ActionAuthLogout,
			ResourceType: "api_key",
			ResourceID:   keyID.String(),
			SourceIP:     c.RealIP(),
			Outcome:      outcome,
			Metadata: map[string]any{
				"method":     c.Request().Method,
				"path":       c.Request().URL.Path,
				"credential": "user_key",
			},
		}))
	}
	if err != nil {
		log.Warn("failed to revoke user-bound key during logout", "key_id", keyID, "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "logout failed").Wrap(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func uuidOrNil(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func decodeCLIBody(c *echo.Context, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, cliRequestBodyLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > cliRequestBodyLimit {
		return errors.New("body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// CLISignInFallback wraps the finish route's auth middleware: a browser that
// arrives without a usable session gets an HTML page offering the configured
// sign-in methods (returning to this page) instead of a JSON 401. The status
// stays 401 and the middleware's failure accounting is unchanged.
func (s *SSOController) CLISignInFallback(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		err := next(c)
		he, ok := errors.AsType[*echo.HTTPError](err)
		if !ok || he.Code != http.StatusUnauthorized {
			return err
		}
		noStore(c)
		returnTo := cliFinishPath(c.QueryParam("state"))
		page := cliPage{Title: "Sign in to authorise the Caesium CLI", SignIn: true}
		for _, m := range s.cliSignIn {
			link := cliSignInLink{Label: m.Label}
			if m.LoginURL != "" {
				link.URL = m.LoginURL + "?returnTo=" + url.QueryEscape(returnTo)
			}
			page.Methods = append(page.Methods, link)
		}
		return renderCLIPage(c, http.StatusUnauthorized, page)
	}
}

type cliSignInLink struct {
	Label string
	URL   string
}

type cliPage struct {
	Title    string
	Error    string
	Confirm  bool
	SignIn   bool
	Methods  []cliSignInLink
	Email    string
	Hostname string
	Target   string
	State    string
	CSRF     string
	Expires  string
	Nonce    string
}

func renderCLIPage(c *echo.Context, status int, page cliPage) error {
	nonce, err := cliNonce()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "render failed")
	}
	page.Nonce = nonce
	var buf bytes.Buffer
	if err := cliPageTemplate.Execute(&buf, page); err != nil {
		log.Error("render cli login page failed", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "render failed")
	}
	h := c.Response().Header()
	h.Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'none'; script-src 'nonce-%[1]s'; style-src 'nonce-%[1]s'; connect-src 'self'; img-src 'none'; frame-ancestors 'none'; form-action 'none'; base-uri 'none'", nonce))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	return c.HTMLBlob(status, buf.Bytes())
}

func cliNonce() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}

var cliPageTemplate = template.Must(template.New("cli").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style nonce="{{.Nonce}}">
body{font-family:system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem;line-height:1.5;color:#1d2330;background:#f7f8fa}
@media (prefers-color-scheme:dark){body{color:#e6e8ee;background:#151821}}
.warn{border-left:4px solid #c77700;padding:.5rem .75rem}
.err{border-left:4px solid #c62828;padding:.5rem .75rem}
button{font-size:1rem;padding:.5rem 1.25rem;cursor:pointer}
code{font-size:1rem;word-break:break-all}
</style></head><body>
<h1>{{.Title}}</h1>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
{{if .SignIn}}<p>You are not signed in to Caesium in this browser.</p><ul>
{{range .Methods}}{{if .URL}}<li><a href="{{.URL}}">{{.Label}}</a></li>{{else}}<li>{{.Label}}: sign in to the <a href="/">Caesium Console</a> in this browser, then reload this page.</li>{{end}}{{end}}
{{if not .Methods}}<li>Sign in to the <a href="/">Caesium Console</a> in this browser, then reload this page.</li>{{end}}
</ul>{{end}}
{{if .Confirm}}
<main id="cli" data-state="{{.State}}" data-csrf="{{.CSRF}}">
<p>Sign the Caesium CLI in as <strong>{{.Email}}</strong>?</p>
<ul><li>Destination: {{.Target}}</li>{{if .Hostname}}<li>Computer (as reported by the CLI, unverified): <strong>{{.Hostname}}</strong></li>{{end}}<li>Request expires: {{.Expires}}</li></ul>
<p class="warn">Only continue if you just ran <code>caesium login</code> yourself. Never share a sign-in code with anyone: whoever redeems it acts as you.</p>
<p><button id="authorize" type="button">Authorise</button></p>
<p id="result" role="status"></p>
</main>
<script nonce="{{.Nonce}}">
(function(){
var main=document.getElementById("cli"),btn=document.getElementById("authorize"),out=document.getElementById("result");
btn.addEventListener("click",function(){
btn.disabled=true;out.textContent="Authorising…";
fetch("/auth/cli/authorize",{method:"POST",credentials:"same-origin",headers:{"Content-Type":"application/json","X-CSRF-Token":main.dataset.csrf},body:JSON.stringify({state:main.dataset.state})})
.then(function(r){return r.json().catch(function(){return {};}).then(function(b){return {ok:r.ok,status:r.status,body:b};});})
.then(function(res){
if(!res.ok){out.textContent=(res.body&&res.body.message)||("Authorisation failed ("+res.status+").");return;}
if(res.body.redirect_url){out.textContent="Authorised. Returning to the CLI…";window.location.assign(res.body.redirect_url);return;}
out.textContent="";var p=document.createElement("p");p.textContent="Authorised. Within a minute, run:";var c=document.createElement("code");c.textContent="caesium login --code "+res.body.code;out.appendChild(p);out.appendChild(c);
}).catch(function(){out.textContent="Authorisation failed.";btn.disabled=false;});
});
})();
</script>
{{end}}
</body></html>
`))
