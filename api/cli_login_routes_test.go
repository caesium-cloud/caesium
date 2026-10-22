package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

const (
	cliTestVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	cliTestChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	cliTestCookie    = "caesium_session"
)

type cliRoutesFixture struct {
	e        *echo.Echo
	svc      *iauth.Service
	sessions *iauth.SessionStore
	limiter  *iauth.RateLimiter
	auditor  *iauth.AuditLogger
	user     *models.User
	cookie   *http.Cookie
	csrf     string
}

func newCLIRoutesFixture(t *testing.T) *cliRoutesFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	f := &cliRoutesFixture{
		svc:      iauth.NewService(db, iauth.WithPolicySource(mustViewerRoleMapper(t))),
		sessions: iauth.NewSessionStore(db),
		limiter:  iauth.NewRateLimiter(5, time.Minute),
		auditor:  iauth.NewAuditLogger(db),
	}
	f.user = &models.User{
		ID: uuid.New(), Issuer: "oidc", Subject: "cli-1", Email: "viewer@example.com",
		Groups: datatypes.JSON(`["viewers"]`), Role: models.RoleViewer, CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(f.user).Error)
	token, sess, err := f.sessions.Create(t.Context(), iauth.CreateSessionRequest{UserID: f.user.ID})
	require.NoError(t, err)
	f.cookie = &http.Cookie{Name: cliTestCookie, Value: token}
	f.csrf = sess.CSRFToken
	f.e = echo.New()
	registerSSORoutes(f.e, env.Environment{AuthSessionCookieName: cliTestCookie}, f.svc, f.auditor, f.limiter, f.sessions, nil, SSOProviders{})
	return f
}

type cliReq struct {
	method, path, body, bearer, csrf string
	cookie                           *http.Cookie
}

func (f *cliRoutesFixture) do(t *testing.T, r cliReq) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req := httptest.NewRequestWithContext(context.Background(), r.method, r.path, body)
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.cookie != nil {
		req.AddCookie(r.cookie)
	}
	if r.csrf != "" {
		req.Header.Set("X-CSRF-Token", r.csrf)
	}
	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)
	return rec
}

func cliStartPath(state, extra string) string {
	return "/auth/cli/start?state=" + state + "&challenge=" + cliTestChallenge + extra
}

func cliState(c byte) string { return strings.Repeat(string(c), 43) }

func TestCLILoginRoutesFullFlow(t *testing.T) {
	f := newCLIRoutesFixture(t)
	state := cliState('a')

	rec := f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(state, "&port=40123&hostname="+url.QueryEscape("<b>box</b>"))})
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	require.Equal(t, "/auth/cli/finish?state="+state, rec.Header().Get("Location"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	// No session: the sign-in page, not JSON, still 401.
	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/cli/finish?state=" + state})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	require.Contains(t, rec.Body.String(), "not signed in")

	// A bearer key cannot confirm a login.
	apiKey, err := f.svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "seed"})
	require.NoError(t, err)
	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/cli/finish?state=" + state, bearer: apiKey.Plaintext})
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/cli/finish?state=" + state, cookie: f.cookie})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page := rec.Body.String()
	require.Contains(t, page, "viewer@example.com")
	require.Contains(t, page, "&lt;b&gt;box&lt;/b&gt;", "the hostname hint is escaped")
	require.NotContains(t, page, "<b>box</b>")
	require.Contains(t, page, "loopback port 40123")
	require.Contains(t, page, `data-csrf="`+f.csrf+`"`)
	require.NotContains(t, page, iauth.KeyPrefixUser, "the confirmation page carries no key")
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	csp := rec.Header().Get("Content-Security-Policy")
	require.Contains(t, csp, "frame-ancestors 'none'")
	require.Contains(t, csp, "default-src 'none'")

	// Unknown state: an error page.
	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/cli/finish?state=" + cliState('z'), cookie: f.cookie})
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NotContains(t, rec.Body.String(), iauth.KeyPrefixUser)

	body := `{"state":"` + state + `"}`
	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/authorize", body: body, cookie: f.cookie})
	require.Equal(t, http.StatusForbidden, rec.Code, "authorize requires the session's CSRF token")
	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/authorize", body: body, bearer: apiKey.Plaintext})
	require.Equal(t, http.StatusForbidden, rec.Code, "a bearer key cannot authorise")

	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/authorize", body: body, cookie: f.cookie, csrf: f.csrf})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var authorized struct {
		RedirectURL string `json:"redirect_url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &authorized))
	callback, err := url.Parse(authorized.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "http", callback.Scheme)
	require.Equal(t, "127.0.0.1:40123", callback.Host)
	require.Equal(t, "/callback", callback.Path)
	require.Equal(t, state, callback.Query().Get("state"))
	code := callback.Query().Get("code")
	require.Len(t, code, 43)

	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/authorize", body: body, cookie: f.cookie, csrf: f.csrf})
	require.Equal(t, http.StatusConflict, rec.Code, "a second confirmation is a conflict")
	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/cli/finish?state=" + state, cookie: f.cookie})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.NotContains(t, rec.Body.String(), iauth.KeyPrefixUser)

	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/exchange", body: `{"code":"` + code + `","code_verifier":"` + cliTestVerifier + `"}`})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var exchanged struct {
		Key, KeyID, KeyPrefix, Subject string
	}
	var raw map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	exchanged.Key, exchanged.KeyID, exchanged.KeyPrefix, exchanged.Subject = raw["key"], raw["key_id"], raw["key_prefix"], raw["subject"]
	require.True(t, strings.HasPrefix(exchanged.Key, iauth.KeyPrefixUser))
	require.Equal(t, "viewer@example.com", exchanged.Subject)
	require.NotEmpty(t, raw["expires_at"])

	// The key is its user.
	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/whoami", bearer: exchanged.Key})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"kind":"user"`)

	// Logout with the key and the cookie: only the key dies.
	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/logout", bearer: exchanged.Key, cookie: f.cookie})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Result().Cookies(), "the cookie was not the request's credential")
	_, _, err = f.svc.AuthenticateKey(t.Context(), exchanged.Key)
	require.Error(t, err, "the user-bound key is revoked")
	_, _, err = f.sessions.Validate(t.Context(), f.cookie.Value)
	require.NoError(t, err, "the cookie session survives")
	entries, err := f.auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLogout, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, exchanged.KeyID, entries[0].ResourceID)
	require.Equal(t, "viewer@example.com", entries[0].Actor)
}

func TestCLILoginRoutesManualModeShowsCode(t *testing.T) {
	f := newCLIRoutesFixture(t)
	state := cliState('m')
	rec := f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(state, "&mode=manual")})
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	rec = f.do(t, cliReq{method: http.MethodGet, path: "/auth/cli/finish?state=" + state, cookie: f.cookie})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "manual code")
	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/authorize", body: `{"state":"` + state + `"}`, cookie: f.cookie, csrf: f.csrf})
	require.Equal(t, http.StatusOK, rec.Code)
	var shown map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shown))
	require.Len(t, shown["code"], 43)
	require.NotContains(t, rec.Body.String(), "redirect_url")
}

func TestCLILoginRoutesStartValidation(t *testing.T) {
	f := newCLIRoutesFixture(t)
	for name, path := range map[string]string{
		"port below range":  cliStartPath(cliState('b'), "&port=1023"),
		"port above range":  cliStartPath(cliState('b'), "&port=65536"),
		"port not decimal":  cliStartPath(cliState('b'), "&port=4e3"),
		"signed port":       cliStartPath(cliState('b'), "&port=+4000"),
		"port and manual":   cliStartPath(cliState('b'), "&port=4000&mode=manual"),
		"neither":           cliStartPath(cliState('b'), ""),
		"bad challenge":     "/auth/cli/start?state=" + cliState('b') + "&challenge=x&port=4000",
		"missing state":     "/auth/cli/start?challenge=" + cliTestChallenge + "&port=4000",
		"unknown mode name": cliStartPath(cliState('b'), "&mode=auto"),
	} {
		rec := f.do(t, cliReq{method: http.MethodGet, path: path})
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
	rec := f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(cliState('c'), "&port=4000")})
	require.Equal(t, http.StatusFound, rec.Code)
	rec = f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(cliState('c'), "&port=4000")})
	require.Equal(t, http.StatusConflict, rec.Code, "a state names one transaction")
}

func TestCLILoginRoutesExchangeRefusalsAreUniformAndRateLimited(t *testing.T) {
	f := newCLIRoutesFixture(t)
	bodies := []string{
		`{"code":"` + cliState('q') + `","code_verifier":"` + cliTestVerifier + `"}`,
		`{"code":"short","code_verifier":"` + cliTestVerifier + `"}`,
		`not json`,
		`{"code":"` + cliState('q') + `","code_verifier":"` + cliTestVerifier + `","extra":1}`,
		`{}`,
	}
	for _, body := range bodies {
		rec := f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/exchange", body: body})
		if rec.Code == http.StatusTooManyRequests {
			break
		}
		require.Equal(t, http.StatusUnauthorized, rec.Code, body)
		require.Contains(t, rec.Body.String(), "invalid or expired login code")
	}
	rec := f.do(t, cliReq{method: http.MethodPost, path: "/auth/cli/exchange", body: bodies[0]})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "exchange refusals count on the IP limiter")
	rec = f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(cliState('d'), "&port=4000")})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "a limited address cannot start a login either")
}

func TestCLILoginRoutesPendingCapCountsOnTheLimiter(t *testing.T) {
	f := newCLIRoutesFixture(t)
	for i := 0; i < iauth.CLILoginMaxPendingPerIP; i++ {
		state := strings.Repeat("p", 41) + string(rune('A'+i)) + "x"
		rec := f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(state, "&port=4000")})
		require.Equal(t, http.StatusFound, rec.Code)
	}
	rec := f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(cliState('o'), "&port=4000")})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestCLILoginRoutesLogoutKeepsSessionAndAPIKeyContracts(t *testing.T) {
	f := newCLIRoutesFixture(t)
	// Cookie logout still revokes the session.
	rec := f.do(t, cliReq{method: http.MethodPost, path: "/auth/logout", cookie: f.cookie, csrf: f.csrf})
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, _, err := f.sessions.Validate(t.Context(), f.cookie.Value)
	require.ErrorIs(t, err, iauth.ErrSessionRevoked)

	// A user-bound key without a cookie revokes only itself.
	resp, err := f.svc.MintUserBoundKey(t.Context(), iauth.MintUserKeyRequest{UserID: f.user.ID, CreatedBy: f.user.Email})
	require.NoError(t, err)
	other, err := f.svc.MintUserBoundKey(t.Context(), iauth.MintUserKeyRequest{UserID: f.user.ID, CreatedBy: f.user.Email})
	require.NoError(t, err)
	rec = f.do(t, cliReq{method: http.MethodPost, path: "/auth/logout", bearer: resp.Plaintext})
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, _, err = f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.Error(t, err)
	_, _, err = f.svc.AuthenticateKey(t.Context(), other.Plaintext)
	require.NoError(t, err, "the user's other key is untouched")
}

// revokeOnFirstRead is a request body that runs revoke the first time the
// handler reads it, i.e. after the auth middleware validated the session.
type revokeOnFirstRead struct {
	body   *strings.Reader
	revoke func()
	done   bool
}

func (r *revokeOnFirstRead) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		r.revoke()
	}
	return r.body.Read(p)
}

// TestCLILoginRoutesAuthorizeRefusesASessionRevokedMidRequest lands "log out
// everywhere" between the middleware's session validation and the
// confirmation's transaction (a client holding its body back): the
// confirmation is refused and the login stays unconfirmed, so no exchange can
// mint a key afterwards.
func TestCLILoginRoutesAuthorizeRefusesASessionRevokedMidRequest(t *testing.T) {
	f := newCLIRoutesFixture(t)
	state := cliState('r')
	rec := f.do(t, cliReq{method: http.MethodGet, path: cliStartPath(state, "&mode=manual")})
	require.Equal(t, http.StatusFound, rec.Code)

	body := &revokeOnFirstRead{body: strings.NewReader(`{"state":"` + state + `"}`), revoke: func() {
		_, err := iauth.NewUserAdmin(f.sessions).RevokeUserSessions(context.Background(), f.user.ID)
		require.NoError(t, err)
	}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/auth/cli/authorize", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.AddCookie(f.cookie)
	out := httptest.NewRecorder()
	f.e.ServeHTTP(out, req)
	require.True(t, body.done, "the handler read the body after the middleware let the request in")
	require.Equal(t, http.StatusUnauthorized, out.Code, out.Body.String())
	require.NotContains(t, out.Body.String(), `"code"`)
}
