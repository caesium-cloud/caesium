package auth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
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

func TestLogoutRevokesSessionAndClearsCookie(t *testing.T) {
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

	token, _, err := sessions.Create(t.Context(), iauth.CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: "oidc",
	})
	require.NoError(t, err)

	ctrl := NewSSO(sessions, nil, "caesium_session")
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
