package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestWhoamiRequiresPrincipal(t *testing.T) {
	ctrl := NewSSO(nil, nil, "caesium_session")
	c, _ := newAuthContext(t, http.MethodGet, "/auth/whoami", "")

	err := ctrl.Whoami(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestWhoamiReturnsPrincipalAndCSRF(t *testing.T) {
	ctrl := NewSSO(nil, nil, "caesium_session")
	c, rec := newAuthContext(t, http.MethodGet, "/auth/whoami", "")
	c.Set(authmw.ContextKeyPrincipal, &iauth.Principal{
		Kind:    iauth.PrincipalUser,
		Subject: "viewer@example.com",
		Role:    models.RoleViewer,
	})
	c.Set(authmw.ContextKeyCSRFToken, "csrf-token")

	err := ctrl.Whoami(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, string(iauth.PrincipalUser), body["kind"])
	require.Equal(t, "viewer@example.com", body["subject"])
	require.Equal(t, "viewer@example.com", body["email"])
	require.Equal(t, string(models.RoleViewer), body["role"])
	require.Equal(t, "csrf-token", body["csrf_token"])
}

func TestNewSSORetainsCompletionService(t *testing.T) {
	sso := iauth.NewSSOService(nil, nil, nil)
	ctrl := NewSSO(nil, sso, "caesium_session")

	require.Equal(t, sso, ctrl.SSOService())
}

func TestOIDCLoginRedirectsThroughProvider(t *testing.T) {
	provider := &fakeRedirectAuthenticator{
		name:     "oidc",
		beginURL: "https://idp.example/authorize",
	}
	ctrl := NewSSO(nil, nil, "caesium_session")
	ctrl.SetOIDCProvider(provider)
	c, rec := newAuthContext(t, http.MethodGet, "/auth/sso/oidc/login?returnTo=%2Fjobs%3Fstatus%3Drunning%23stage-1", "")

	err := ctrl.OIDCLogin(c)
	require.NoError(t, err)

	require.True(t, provider.beginCalled)
	require.Equal(t, "/jobs?status=running#stage-1", provider.beginReturnTo)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://idp.example/authorize", rec.Header().Get("Location"))
}

func TestOIDCLoginSanitizesExternalReturnTo(t *testing.T) {
	provider := &fakeRedirectAuthenticator{
		name:     "oidc",
		beginURL: "https://idp.example/authorize",
	}
	ctrl := NewSSO(nil, nil, "caesium_session")
	ctrl.SetOIDCProvider(provider)
	c, _ := newAuthContext(t, http.MethodGet, "/auth/sso/oidc/login?returnTo=https%3A%2F%2Fevil.example%2Fsteal", "")

	err := ctrl.OIDCLogin(c)
	require.NoError(t, err)

	require.True(t, provider.beginCalled)
	require.Equal(t, "/", provider.beginReturnTo)
}

func TestRedirectLoginReturnToHonorsTrustedProxyOrigin(t *testing.T) {
	_, trustedProxy, err := net.ParseCIDR("127.0.0.1/32")
	require.NoError(t, err)

	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{
			name:       "trusted proxy allows forwarded https same origin",
			remoteAddr: "127.0.0.1:1234",
			want:       "/runs?status=mine#latest",
		},
		{
			name:       "untrusted peer cannot upgrade origin with forwarded proto",
			remoteAddr: "203.0.113.10:1234",
			want:       "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &fakeRedirectAuthenticator{
				name:     "oidc",
				beginURL: "https://idp.example/authorize",
			}
			ctrl := NewSSO(nil, nil, "caesium_session", trustedProxy)
			ctrl.SetOIDCProvider(provider)
			target := "/auth/sso/oidc/login?returnTo=https%3A%2F%2Fexample.com%2Fruns%3Fstatus%3Dmine%23latest"
			c, _ := newAuthContext(t, http.MethodGet, target, "")
			c.Request().RemoteAddr = tt.remoteAddr
			c.Request().Header.Set("X-Forwarded-Proto", "https")

			err := ctrl.OIDCLogin(c)
			require.NoError(t, err)

			require.True(t, provider.beginCalled)
			require.Equal(t, tt.want, provider.beginReturnTo)
		})
	}
}

func TestSAMLLoginRedirectsThroughProvider(t *testing.T) {
	provider := &fakeRedirectAuthenticator{
		name:     "saml",
		beginURL: "https://idp.example/sso",
	}
	ctrl := NewSSO(nil, nil, "caesium_session")
	ctrl.SetSAMLProvider(provider)
	c, rec := newAuthContext(t, http.MethodGet, "/auth/sso/saml/login?returnTo=%2Fruns%3Fstatus%3Dfailed", "")

	err := ctrl.SAMLLogin(c)
	require.NoError(t, err)

	require.True(t, provider.beginCalled)
	require.Equal(t, "/runs?status=failed", provider.beginReturnTo)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://idp.example/sso", rec.Header().Get("Location"))
}

func TestSAMLMetadataServesProviderMetadata(t *testing.T) {
	provider := &fakeRedirectAuthenticator{
		name:         "saml",
		metadataBody: "<EntityDescriptor/>",
	}
	ctrl := NewSSO(nil, nil, "caesium_session")
	ctrl.SetSAMLProvider(provider)
	c, rec := newAuthContext(t, http.MethodGet, "/auth/sso/saml/metadata", "")

	err := ctrl.SAMLMetadata(c)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "<EntityDescriptor/>", rec.Body.String())
}

func TestRedirectCallbacksSanitizeUnsafeReturnTo(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		method   string
		target   string
		returnTo string
		call     func(*SSOController, *echo.Context) error
	}{
		{
			name:     "oidc encoded scheme relative",
			provider: "oidc",
			method:   http.MethodGet,
			target:   "/auth/sso/oidc/callback?code=abc&state=xyz",
			returnTo: "/%2f%2fevil.example/steal",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.OIDCCallback(c) },
		},
		{
			name:     "saml encoded scheme relative",
			provider: "saml",
			method:   http.MethodPost,
			target:   "/auth/sso/saml/acs",
			returnTo: "/%5c%5cevil.example/steal",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.SAMLACS(c) },
		},
		{
			name:     "oidc encoded tab scheme relative",
			provider: "oidc",
			method:   http.MethodGet,
			target:   "/auth/sso/oidc/callback?code=abc&state=xyz",
			returnTo: "/%09/evil.example/steal",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.OIDCCallback(c) },
		},
		{
			name:     "saml encoded newline scheme relative",
			provider: "saml",
			method:   http.MethodPost,
			target:   "/auth/sso/saml/acs",
			returnTo: "/%0a/evil.example/steal",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.SAMLACS(c) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, sso := newTestSSOService(t)
			provider := &fakeRedirectAuthenticator{
				name:     tt.provider,
				returnTo: tt.returnTo,
				identity: testExternalIdentity(tt.provider),
			}
			ctrl := NewSSO(sessions, sso, "caesium_session")
			if tt.provider == "oidc" {
				ctrl.SetOIDCProvider(provider)
			} else {
				ctrl.SetSAMLProvider(provider)
			}
			c, rec := newAuthContext(t, tt.method, tt.target, "")

			err := tt.call(ctrl, c)
			require.NoError(t, err)

			require.True(t, provider.completeCalled)
			require.Equal(t, http.StatusFound, rec.Code)
			require.Equal(t, "/", rec.Header().Get("Location"))
			require.NotNil(t, responseCookie(rec.Result().Cookies(), "caesium_session"))
		})
	}
}

func TestRedirectCallbacksAllowSameOriginAbsoluteReturnTo(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		method   string
		target   string
		call     func(*SSOController, *echo.Context) error
	}{
		{
			name:     "oidc",
			provider: "oidc",
			method:   http.MethodGet,
			target:   "/auth/sso/oidc/callback?code=abc&state=xyz",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.OIDCCallback(c) },
		},
		{
			name:     "saml",
			provider: "saml",
			method:   http.MethodPost,
			target:   "/auth/sso/saml/acs",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.SAMLACS(c) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, sso := newTestSSOService(t)
			provider := &fakeRedirectAuthenticator{
				name:     tt.provider,
				returnTo: "http://example.com/runs?status=mine#latest",
				identity: testExternalIdentity(tt.provider),
			}
			ctrl := NewSSO(sessions, sso, "caesium_session")
			if tt.provider == "oidc" {
				ctrl.SetOIDCProvider(provider)
			} else {
				ctrl.SetSAMLProvider(provider)
			}
			c, rec := newAuthContext(t, tt.method, tt.target, "")

			err := tt.call(ctrl, c)
			require.NoError(t, err)

			require.Equal(t, http.StatusFound, rec.Code)
			require.Equal(t, "/runs?status=mine#latest", rec.Header().Get("Location"))
		})
	}
}

func TestRedirectCallbacksSetSecureSessionCookieBehindTrustedHTTPSProxy(t *testing.T) {
	_, trustedProxy, err := net.ParseCIDR("127.0.0.1/32")
	require.NoError(t, err)

	tests := []struct {
		name     string
		provider string
		method   string
		target   string
		call     func(*SSOController, *echo.Context) error
	}{
		{
			name:     "oidc",
			provider: "oidc",
			method:   http.MethodGet,
			target:   "/auth/sso/oidc/callback?code=abc&state=xyz",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.OIDCCallback(c) },
		},
		{
			name:     "saml",
			provider: "saml",
			method:   http.MethodPost,
			target:   "/auth/sso/saml/acs",
			call:     func(ctrl *SSOController, c *echo.Context) error { return ctrl.SAMLACS(c) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, sso := newTestSSOService(t)
			provider := &fakeRedirectAuthenticator{
				name:     tt.provider,
				returnTo: "/runs",
				identity: testExternalIdentity(tt.provider),
			}
			ctrl := NewSSO(sessions, sso, "caesium_session", trustedProxy)
			if tt.provider == "oidc" {
				ctrl.SetOIDCProvider(provider)
			} else {
				ctrl.SetSAMLProvider(provider)
			}
			c, rec := newAuthContext(t, tt.method, tt.target, "")
			c.Request().RemoteAddr = "127.0.0.1:1234"
			c.Request().Header.Set("X-Forwarded-Proto", "https")

			err := tt.call(ctrl, c)
			require.NoError(t, err)

			sessionCookie := requireResponseCookie(t, rec.Result().Cookies(), "caesium_session")
			require.True(t, sessionCookie.HttpOnly)
			require.True(t, sessionCookie.Secure)
			require.Equal(t, http.SameSiteLaxMode, sessionCookie.SameSite)
		})
	}
}

func TestOIDCCallbackCompletesSSOAndSetsSessionCookie(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sessions := iauth.NewSessionStore(db)
	mapper, err := iauth.NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := iauth.NewSSOService(iauth.NewUserStore(db), sessions, mapper)
	provider := &fakeRedirectAuthenticator{
		name:     "oidc",
		returnTo: "/runs?status=mine#latest",
		identity: &iauth.ExternalIdentity{
			Issuer:      "oidc",
			Subject:     "sub-1",
			Email:       "viewer@example.com",
			DisplayName: "Viewer One",
			Groups:      []string{"eng"},
		},
	}
	ctrl := NewSSO(sessions, sso, "caesium_session")
	ctrl.SetOIDCProvider(provider)
	c, rec := newAuthContext(t, http.MethodGet, "/auth/sso/oidc/callback?code=abc&state=xyz", "")
	c.Request().Header.Set("User-Agent", "sso-test-agent")

	err = ctrl.OIDCCallback(c)
	require.NoError(t, err)

	require.True(t, provider.completeCalled)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/runs?status=mine#latest", rec.Header().Get("Location"))

	sessionCookie := requireResponseCookie(t, rec.Result().Cookies(), "caesium_session")
	require.NotEmpty(t, sessionCookie.Value)
	require.True(t, sessionCookie.HttpOnly)
	require.False(t, sessionCookie.Secure)
	require.Equal(t, http.SameSiteLaxMode, sessionCookie.SameSite)
	require.False(t, sessionCookie.Expires.IsZero())

	sess, user, err := sessions.Validate(t.Context(), sessionCookie.Value)
	require.NoError(t, err)
	require.Equal(t, "oidc", sess.AuthMethod)
	require.Equal(t, "198.51.100.8", sess.SourceIP)
	require.Equal(t, "sso-test-agent", sess.UserAgent)
	require.Equal(t, "viewer@example.com", user.Email)
	require.Equal(t, models.RoleOperator, user.Role)
}

func TestSAMLACSCompletesSSOAndSetsSessionCookie(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sessions := iauth.NewSessionStore(db)
	mapper, err := iauth.NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := iauth.NewSSOService(iauth.NewUserStore(db), sessions, mapper)
	provider := &fakeRedirectAuthenticator{
		name:     "saml",
		returnTo: "/runs?status=mine",
		identity: &iauth.ExternalIdentity{
			Issuer:      "saml",
			Subject:     "nameid-1",
			Email:       "viewer@example.com",
			DisplayName: "Viewer One",
			Groups:      []string{"eng"},
		},
	}
	ctrl := NewSSO(sessions, sso, "caesium_session")
	ctrl.SetSAMLProvider(provider)
	c, rec := newAuthContext(t, http.MethodPost, "/auth/sso/saml/acs", "")
	c.Request().Header.Set("User-Agent", "saml-test-agent")

	err = ctrl.SAMLACS(c)
	require.NoError(t, err)

	require.True(t, provider.completeCalled)
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "/runs?status=mine", rec.Header().Get("Location"))

	sessionCookie := requireResponseCookie(t, rec.Result().Cookies(), "caesium_session")
	require.NotEmpty(t, sessionCookie.Value)
	require.True(t, sessionCookie.HttpOnly)
	require.False(t, sessionCookie.Secure)
	require.Equal(t, http.SameSiteLaxMode, sessionCookie.SameSite)
	require.False(t, sessionCookie.Expires.IsZero())

	sess, user, err := sessions.Validate(t.Context(), sessionCookie.Value)
	require.NoError(t, err)
	require.Equal(t, "saml", sess.AuthMethod)
	require.Equal(t, "198.51.100.8", sess.SourceIP)
	require.Equal(t, "saml-test-agent", sess.UserAgent)
	require.Equal(t, "viewer@example.com", user.Email)
	require.Equal(t, models.RoleOperator, user.Role)
}

func TestLDAPLoginCompletesSSOAndSetsSessionCookie(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	_, trustedProxy, err := net.ParseCIDR("127.0.0.1/32")
	require.NoError(t, err)

	sessions := iauth.NewSessionStore(db)
	mapper, err := iauth.NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := iauth.NewSSOService(iauth.NewUserStore(db), sessions, mapper)
	provider := &fakeCredentialAuthenticator{
		name: "ldap",
		identity: &iauth.ExternalIdentity{
			Issuer:      "ldap",
			Subject:     "uid=viewer,ou=people,dc=example,dc=com",
			Email:       "viewer@example.com",
			DisplayName: "Viewer One",
			Groups:      []string{"eng"},
		},
	}
	ctrl := NewSSO(sessions, sso, "caesium_session", trustedProxy)
	ctrl.SetLDAPProvider(provider)

	e := echo.New()
	e.IPExtractor = echo.ExtractIPFromXFFHeader(echo.TrustIPRange(trustedProxy))
	req := httptest.NewRequest(http.MethodPost, "/auth/sso/ldap/login", strings.NewReader(`{"username":" viewer ","password":"secret"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("User-Agent", "ldap-test-agent")
	req.Header.Set("X-Forwarded-For", "203.0.113.55")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = ctrl.LDAPLogin(c)
	require.NoError(t, err)

	require.True(t, provider.authenticateCalled)
	require.Equal(t, "viewer", provider.username)
	require.Equal(t, "secret", provider.password)
	require.Equal(t, http.StatusNoContent, rec.Code)

	sessionCookie := requireResponseCookie(t, rec.Result().Cookies(), "caesium_session")
	require.NotEmpty(t, sessionCookie.Value)
	require.True(t, sessionCookie.HttpOnly)
	require.True(t, sessionCookie.Secure)
	require.Equal(t, http.SameSiteLaxMode, sessionCookie.SameSite)
	require.False(t, sessionCookie.Expires.IsZero())

	sess, user, err := sessions.Validate(t.Context(), sessionCookie.Value)
	require.NoError(t, err)
	require.Equal(t, "ldap", sess.AuthMethod)
	require.Equal(t, "203.0.113.55", sess.SourceIP)
	require.Equal(t, "ldap-test-agent", sess.UserAgent)
	require.Equal(t, "viewer@example.com", user.Email)
	require.Equal(t, models.RoleOperator, user.Role)
}

func TestLDAPLoginIgnoresReturnToAndDoesNotRedirect(t *testing.T) {
	sessions, sso := newTestSSOService(t)
	provider := &fakeCredentialAuthenticator{
		name:     "ldap",
		identity: testExternalIdentity("ldap"),
	}
	ctrl := NewSSO(sessions, sso, "caesium_session")
	ctrl.SetLDAPProvider(provider)
	c, rec := newAuthContext(t, http.MethodPost, "/auth/sso/ldap/login?returnTo=https%3A%2F%2Fevil.example%2Fsteal", `{"username":"viewer","password":"secret"}`)

	err := ctrl.LDAPLogin(c)
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))
	require.NotNil(t, responseCookie(rec.Result().Cookies(), "caesium_session"))
}

func TestLDAPLoginRejectsDeniedLogin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sessions := iauth.NewSessionStore(db)
	mapper, err := iauth.NewRoleMapper("", "")
	require.NoError(t, err)
	sso := iauth.NewSSOService(iauth.NewUserStore(db), sessions, mapper)
	provider := &fakeCredentialAuthenticator{
		name: "ldap",
		identity: &iauth.ExternalIdentity{
			Issuer:  "ldap",
			Subject: "uid=denied,ou=people,dc=example,dc=com",
			Groups:  []string{"unknown"},
		},
	}
	ctrl := NewSSO(sessions, sso, "caesium_session")
	ctrl.SetLDAPProvider(provider)
	c, rec := newAuthContext(t, http.MethodPost, "/auth/sso/ldap/login", `{"username":"denied","password":"secret"}`)

	err = ctrl.LDAPLogin(c)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.True(t, provider.authenticateCalled)
	require.Nil(t, responseCookie(rec.Result().Cookies(), "caesium_session"))
}

func TestLDAPLoginRejectsInvalidCredentials(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	auditor := iauth.NewAuditLogger(db)
	deniedBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "ldap", iauth.OutcomeDenied)
	provider := &fakeCredentialAuthenticator{
		name: "ldap",
		err:  iauth.ErrLoginDenied,
	}
	ctrl := NewSSO(nil, iauth.NewSSOService(nil, nil, nil), "caesium_session")
	ctrl.SetAuditLogger(auditor)
	ctrl.SetLDAPProvider(provider)
	c, _ := newAuthContext(t, http.MethodPost, "/auth/sso/ldap/login", `{"username":"viewer","password":"bad"}`)

	err := ctrl.LDAPLogin(c)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
	require.True(t, provider.authenticateCalled)
	require.Equal(t, deniedBefore+1, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "ldap", iauth.OutcomeDenied))

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLoginDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "viewer", entries[0].Actor)
	require.Equal(t, iauth.OutcomeDenied, entries[0].Outcome)
	metadata := auditMetadata(t, entries[0])
	require.Equal(t, "ldap", metadata["provider"])
	require.Equal(t, "invalid_credentials", metadata["reason"])
}

func TestLDAPLoginReturnsUnauthorizedWhenProviderErrors(t *testing.T) {
	errorBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "ldap", iauth.OutcomeError)
	provider := &fakeCredentialAuthenticator{
		name: "ldap",
		err:  errors.New("directory unavailable"),
	}
	ctrl := NewSSO(nil, iauth.NewSSOService(nil, nil, nil), "caesium_session")
	ctrl.SetLDAPProvider(provider)
	c, _ := newAuthContext(t, http.MethodPost, "/auth/sso/ldap/login", `{"username":"viewer","password":"secret"}`)

	err := ctrl.LDAPLogin(c)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
	require.True(t, provider.authenticateCalled)
	require.Equal(t, errorBefore+1, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "ldap", iauth.OutcomeError))
}

func TestOIDCCallbackRecordsProviderErrorsAsErrors(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	auditor := iauth.NewAuditLogger(db)
	errorBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", iauth.OutcomeError)
	deniedBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", iauth.OutcomeDenied)
	provider := &fakeRedirectAuthenticator{
		name:        "oidc",
		completeErr: errors.New("idp unavailable"),
	}
	ctrl := NewSSO(nil, iauth.NewSSOService(nil, nil, nil), "caesium_session")
	ctrl.SetAuditLogger(auditor)
	ctrl.SetOIDCProvider(provider)
	c, _ := newAuthContext(t, http.MethodGet, "/auth/sso/oidc/callback?code=abc&state=xyz", "")

	err := ctrl.OIDCCallback(c)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
	require.True(t, provider.completeCalled)
	require.Equal(t, errorBefore+1, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", iauth.OutcomeError))
	require.Equal(t, deniedBefore, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", iauth.OutcomeDenied))

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLogin, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, iauth.OutcomeError, entries[0].Outcome)
	metadata := auditMetadata(t, entries[0])
	require.Equal(t, "oidc", metadata["provider"])
	require.Equal(t, "callback_failed", metadata["reason"])

	deniedEntries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLoginDenied, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, deniedEntries)
}

func TestOIDCCallbackRejectsDeniedLogin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sessions := iauth.NewSessionStore(db)
	mapper, err := iauth.NewRoleMapper("", "")
	require.NoError(t, err)
	sso := iauth.NewSSOService(iauth.NewUserStore(db), sessions, mapper)
	provider := &fakeRedirectAuthenticator{
		name:     "oidc",
		returnTo: "/jobs",
		identity: &iauth.ExternalIdentity{
			Issuer:  "oidc",
			Subject: "sub-denied",
			Groups:  []string{"unknown"},
		},
	}
	ctrl := NewSSO(sessions, sso, "caesium_session")
	ctrl.SetOIDCProvider(provider)
	c, rec := newAuthContext(t, http.MethodGet, "/auth/sso/oidc/callback?code=abc&state=xyz", "")

	err = ctrl.OIDCCallback(c)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Nil(t, responseCookie(rec.Result().Cookies(), "caesium_session"))
}

func TestLogoutRevokesSessionAndClearsCookie(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sessions := iauth.NewSessionStore(db)
	auditor := iauth.NewAuditLogger(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)

	token, _, err := sessions.Create(t.Context(), iauth.CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: "oidc",
	})
	require.NoError(t, err)

	logoutBefore := metrictestutil.CounterValue(t, metrics.SSOLogoutsTotal, iauth.OutcomeSuccess)
	ctrl := NewSSO(sessions, nil, "caesium_session")
	ctrl.SetAuditLogger(auditor)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = ctrl.Logout(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, rec.Code)

	_, _, err = sessions.Validate(t.Context(), token)
	require.Error(t, err)

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "caesium_session", cookies[0].Name)
	require.LessOrEqual(t, cookies[0].MaxAge, 0)
	require.True(t, cookies[0].HttpOnly)
	require.False(t, cookies[0].Secure)
	require.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
	require.Equal(t, logoutBefore+1, metrictestutil.CounterValue(t, metrics.SSOLogoutsTotal, iauth.OutcomeSuccess))

	logoutEntries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLogout, Limit: 10})
	require.NoError(t, err)
	require.Len(t, logoutEntries, 1)
	require.Equal(t, "viewer@example.com", logoutEntries[0].Actor)
	require.Equal(t, iauth.OutcomeSuccess, logoutEntries[0].Outcome)
	require.Equal(t, "session", logoutEntries[0].ResourceType)

	revokedEntries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthSessionRevoked, Limit: 10})
	require.NoError(t, err)
	require.Len(t, revokedEntries, 1)
	require.Equal(t, "viewer@example.com", revokedEntries[0].Actor)
	require.Equal(t, iauth.OutcomeSuccess, revokedEntries[0].Outcome)
}

func TestLogoutWithoutValidSessionOnlyClearsCookie(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{name: "missing cookie"},
		{name: "unknown cookie", token: "unknown-session-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			t.Cleanup(func() { testutil.CloseDB(db) })
			sessions := iauth.NewSessionStore(db)
			auditor := iauth.NewAuditLogger(db)
			successBefore := metrictestutil.CounterValue(t, metrics.SSOLogoutsTotal, iauth.OutcomeSuccess)
			errorBefore := metrictestutil.CounterValue(t, metrics.SSOLogoutsTotal, iauth.OutcomeError)

			ctrl := NewSSO(sessions, nil, "caesium_session")
			ctrl.SetAuditLogger(auditor)
			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
			if tt.token != "" {
				req.AddCookie(&http.Cookie{Name: "caesium_session", Value: tt.token})
			}
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := ctrl.Logout(c)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Equal(t, successBefore, metrictestutil.CounterValue(t, metrics.SSOLogoutsTotal, iauth.OutcomeSuccess))
			require.Equal(t, errorBefore, metrictestutil.CounterValue(t, metrics.SSOLogoutsTotal, iauth.OutcomeError))

			cookie := requireResponseCookie(t, rec.Result().Cookies(), "caesium_session")
			require.LessOrEqual(t, cookie.MaxAge, 0)

			entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionAuthLogout, Limit: 10})
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestLogoutReturnsErrorWhenSessionRevokeFails(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sessions := iauth.NewSessionStore(db)
	user := &models.User{
		ID:        uuid.New(),
		Issuer:    "oidc",
		Subject:   "sub-1",
		Email:     "viewer@example.com",
		Role:      models.RoleViewer,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(user).Error)
	token, _, err := sessions.Create(t.Context(), iauth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)

	const callbackName = "test:fail_session_revoke"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Session" {
			tx.AddError(errors.New("forced revoke failure"))
		}
	}))

	ctrl := NewSSO(sessions, nil, "caesium_session")
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "caesium_session", Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = ctrl.Logout(c)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusInternalServerError, he.Code)
	require.Empty(t, rec.Result().Cookies())
	_, _, err = sessions.Validate(t.Context(), token)
	require.NoError(t, err)
}

func TestLogoutClearsSecureCookieBehindHTTPSProxy(t *testing.T) {
	_, trustedProxy, err := net.ParseCIDR("127.0.0.1/32")
	require.NoError(t, err)
	ctrl := NewSSO(nil, nil, "caesium_session", trustedProxy)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, ctrl.Logout(c))

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].Secure)
}

func TestLogoutIgnoresForwardedProtoFromUntrustedPeer(t *testing.T) {
	_, trustedProxy, err := net.ParseCIDR("127.0.0.1/32")
	require.NoError(t, err)
	ctrl := NewSSO(nil, nil, "caesium_session", trustedProxy)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.RemoteAddr = "203.0.113.12:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, ctrl.Logout(c))

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.False(t, cookies[0].Secure)
}

type fakeRedirectAuthenticator struct {
	name           string
	beginURL       string
	beginErr       error
	returnTo       string
	identity       *iauth.ExternalIdentity
	completeErr    error
	beginCalled    bool
	beginReturnTo  string
	completeCalled bool
	metadataBody   string
}

func (f *fakeRedirectAuthenticator) Name() string {
	return f.name
}

func (f *fakeRedirectAuthenticator) Begin(_ http.ResponseWriter, _ *http.Request, returnTo string) (string, error) {
	f.beginCalled = true
	f.beginReturnTo = returnTo
	return f.beginURL, f.beginErr
}

func (f *fakeRedirectAuthenticator) Complete(_ *http.Request) (*iauth.ExternalIdentity, error) {
	f.completeCalled = true
	return f.identity, f.completeErr
}

func (f *fakeRedirectAuthenticator) CompleteWithReturnTo(_ *http.Request) (*iauth.ExternalIdentity, string, error) {
	f.completeCalled = true
	return f.identity, f.returnTo, f.completeErr
}

func (f *fakeRedirectAuthenticator) Metadata(w http.ResponseWriter, _ *http.Request) error {
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(f.metadataBody))
	return err
}

type fakeCredentialAuthenticator struct {
	name               string
	identity           *iauth.ExternalIdentity
	err                error
	authenticateCalled bool
	username           string
	password           string
}

func (f *fakeCredentialAuthenticator) Name() string {
	return f.name
}

func (f *fakeCredentialAuthenticator) Authenticate(_ context.Context, username, password string) (*iauth.ExternalIdentity, error) {
	f.authenticateCalled = true
	f.username = username
	f.password = password
	return f.identity, f.err
}

func newTestSSOService(t *testing.T) (*iauth.SessionStore, *iauth.SSOService) {
	t.Helper()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	sessions := iauth.NewSessionStore(db)
	mapper, err := iauth.NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	return sessions, iauth.NewSSOService(iauth.NewUserStore(db), sessions, mapper)
}

func testExternalIdentity(issuer string) *iauth.ExternalIdentity {
	return &iauth.ExternalIdentity{
		Issuer:      issuer,
		Subject:     issuer + "-subject",
		Email:       "viewer@example.com",
		DisplayName: "Viewer One",
		Groups:      []string{"eng"},
	}
}

func requireResponseCookie(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()

	if cookie := responseCookie(cookies, name); cookie != nil {
		return cookie
	}
	t.Fatalf("missing response cookie %q", name)
	return nil
}

func responseCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func auditMetadata(t *testing.T, entry models.AuditLog) map[string]any {
	t.Helper()

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(entry.Metadata, &metadata))
	return metadata
}
