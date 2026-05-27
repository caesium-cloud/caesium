package auth

import (
	"encoding/json"
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
)

func TestWhoamiRequiresPrincipal(t *testing.T) {
	ctrl := NewSSO(nil, "caesium_session")
	c, _ := newAuthContext(t, http.MethodGet, "/v1/auth/whoami", "")

	err := ctrl.Whoami(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, he.Code)
}

func TestWhoamiReturnsPrincipalAndCSRF(t *testing.T) {
	ctrl := NewSSO(nil, "caesium_session")
	c, rec := newAuthContext(t, http.MethodGet, "/v1/auth/whoami", "")
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

	ctrl := NewSSO(sessions, "caesium_session")
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
	require.True(t, cookies[0].Secure)
	require.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
}
