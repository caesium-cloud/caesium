package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/labstack/echo/v5"
)

// ContextKeyCSRFToken stores the resolved session's CSRF token for handlers.
const ContextKeyCSRFToken = "auth.csrf_token"

// EnforceSessionCSRF validates the synchronizer CSRF token for cookie-authenticated
// unsafe requests. Bearer/API-key requests are exempt at the auth middleware
// call site because they are not ambient browser credentials.
func EnforceSessionCSRF(c *echo.Context, expected string) error {
	if isSafeMethod(c.Request().Method) {
		return nil
	}

	header := c.Request().Header.Get("X-CSRF-Token")
	if expected == "" || header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(expected)) != 1 {
		return echo.NewHTTPError(http.StatusForbidden, "invalid csrf token")
	}
	return nil
}

// GetCSRFToken returns the session CSRF token stashed by auth middleware, or "".
func GetCSRFToken(c *echo.Context) string {
	if v, ok := c.Get(ContextKeyCSRFToken).(string); ok {
		return v
	}
	return ""
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}
