package auth

import (
	"net"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// SSOController serves session-aware endpoints. Provider-specific login and
// callback handlers are added by the OIDC/SAML/LDAP plans.
type SSOController struct {
	sessions       *iauth.SessionStore
	sso            *iauth.SSOService
	cookieName     string
	trustedProxies []*net.IPNet
}

// NewSSO constructs a controller for cookie-session endpoints.
func NewSSO(sessions *iauth.SessionStore, sso *iauth.SSOService, cookieName string, trustedProxies ...*net.IPNet) *SSOController {
	return &SSOController{
		sessions:       sessions,
		sso:            sso,
		cookieName:     cookieName,
		trustedProxies: append([]*net.IPNet(nil), trustedProxies...),
	}
}

// SSOService returns the shared login completion service for provider handlers.
func (s *SSOController) SSOService() *iauth.SSOService {
	return s.sso
}

func (s *SSOController) Whoami(c *echo.Context) error {
	principal := authmw.GetPrincipal(c)
	if principal == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not authenticated")
	}

	body := map[string]any{
		"kind":    principal.Kind,
		"subject": principal.Subject,
		"role":    principal.Role,
	}
	if principal.Kind == iauth.PrincipalUser {
		body["email"] = principal.Subject
	}
	if csrf := authmw.GetCSRFToken(c); csrf != "" {
		body["csrf_token"] = csrf
	}
	return c.JSON(http.StatusOK, body)
}

func (s *SSOController) Logout(c *echo.Context) error {
	if s.sessions != nil {
		if cookie, err := c.Request().Cookie(s.cookieName); err == nil && cookie.Value != "" {
			if sess, _, err := s.sessions.Validate(c.Request().Context(), cookie.Value); err == nil {
				if err := s.sessions.Revoke(c.Request().Context(), sess.ID); err != nil {
					log.Warn("failed to revoke session during logout", "error", err)
					return echo.NewHTTPError(http.StatusInternalServerError, "logout failed").Wrap(err)
				}
			}
		}
	}

	c.SetCookie(&http.Cookie{
		Name:     s.cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsSecure(c.Request(), s.trustedProxies),
		SameSite: http.SameSiteLaxMode,
	})
	return c.NoContent(http.StatusNoContent)
}

func requestIsSecure(r *http.Request, trustedProxies []*net.IPNet) bool {
	return authmw.RequestIsSecure(r, trustedProxies)
}
