package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func newMiddlewareContext(method, target string, headers map[string]string) *echo.Context {
	e := echo.New()
	req := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}

func TestEnforceSessionCSRF(t *testing.T) {
	c := newMiddlewareContext(http.MethodGet, "/v1/jobs", nil)
	require.NoError(t, authmw.EnforceSessionCSRF(c, "tok123"))

	c = newMiddlewareContext(http.MethodPost, "/v1/jobs", map[string]string{"X-CSRF-Token": "tok123"})
	require.NoError(t, authmw.EnforceSessionCSRF(c, "tok123"))

	c = newMiddlewareContext(http.MethodPost, "/v1/jobs", nil)
	err := authmw.EnforceSessionCSRF(c, "tok123")
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)

	c = newMiddlewareContext(http.MethodPost, "/v1/jobs", map[string]string{"X-CSRF-Token": "wrong"})
	err = authmw.EnforceSessionCSRF(c, "tok123")
	require.Error(t, err)

	he, ok = err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestEnforceSessionCSRFIgnoresReadableCookie(t *testing.T) {
	c := newMiddlewareContext(http.MethodPost, "/v1/jobs", nil)
	c.Request().AddCookie(&http.Cookie{Name: "X-CSRF-Token", Value: "tok123"})

	err := authmw.EnforceSessionCSRF(c, "tok123")
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func TestGetCSRFToken(t *testing.T) {
	c := newMiddlewareContext(http.MethodGet, "/v1/jobs", nil)
	require.Empty(t, authmw.GetCSRFToken(c))

	c.Set(authmw.ContextKeyCSRFToken, "tok123")
	require.Equal(t, "tok123", authmw.GetCSRFToken(c))
}
