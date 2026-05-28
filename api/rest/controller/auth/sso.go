package auth

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// SSOController serves session-aware endpoints and browser-redirect provider
// callbacks.
type SSOController struct {
	sessions       *iauth.SessionStore
	sso            *iauth.SSOService
	oidc           iauth.RedirectAuthenticator
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

// SetOIDCProvider wires the OIDC redirect provider into the controller.
func (s *SSOController) SetOIDCProvider(provider iauth.RedirectAuthenticator) {
	s.oidc = provider
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

func (s *SSOController) OIDCLogin(c *echo.Context) error {
	return s.beginRedirectLogin(c, s.oidc, "oidc")
}

func (s *SSOController) OIDCCallback(c *echo.Context) error {
	return s.completeRedirectLogin(c, s.oidc, "oidc")
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

func (s *SSOController) beginRedirectLogin(c *echo.Context, provider iauth.RedirectAuthenticator, fallbackName string) error {
	if provider == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, fallbackName+" provider unavailable")
	}

	returnTo := safeReturnTo(c.Request(), c.QueryParam("returnTo"), s.trustedProxies)
	redirectURL, err := provider.Begin(c.Response(), c.Request(), returnTo)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fallbackName+" login failed").Wrap(err)
	}
	if strings.TrimSpace(redirectURL) == "" {
		return echo.NewHTTPError(http.StatusBadGateway, fallbackName+" login failed")
	}

	return c.Redirect(http.StatusFound, redirectURL)
}

func (s *SSOController) completeRedirectLogin(c *echo.Context, provider iauth.RedirectAuthenticator, fallbackName string) error {
	if provider == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, fallbackName+" provider unavailable")
	}
	if s.sso == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "sso service unavailable")
	}

	ext, returnTo, err := completeRedirectProvider(provider, c.Request())
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, fallbackName+" callback failed").Wrap(err)
	}
	if ext == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, fallbackName+" callback failed")
	}

	cookieValue, sess, err := s.sso.Complete(
		c.Request().Context(),
		ext,
		providerMethod(provider, fallbackName),
		c.RealIP(),
		c.Request().UserAgent(),
	)
	if err != nil {
		if errors.Is(err, iauth.ErrLoginDenied) {
			return echo.NewHTTPError(http.StatusForbidden, "login denied").Wrap(err)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "login failed").Wrap(err)
	}

	s.setSessionCookie(c, cookieValue, sess)
	return c.Redirect(http.StatusFound, safeReturnTo(c.Request(), returnTo, s.trustedProxies))
}

func (s *SSOController) setSessionCookie(c *echo.Context, value string, sess *models.Session) {
	cookie := &http.Cookie{
		Name:     s.cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(c.Request(), s.trustedProxies),
		SameSite: http.SameSiteLaxMode,
	}
	if sess != nil && !sess.AbsoluteExpiresAt.IsZero() {
		cookie.Expires = sess.AbsoluteExpiresAt
	}
	c.SetCookie(cookie)
}

func requestIsSecure(r *http.Request, trustedProxies []*net.IPNet) bool {
	return authmw.RequestIsSecure(r, trustedProxies)
}

func providerMethod(provider iauth.RedirectAuthenticator, fallbackName string) string {
	if name := strings.TrimSpace(provider.Name()); name != "" {
		return name
	}
	return fallbackName
}

type redirectAuthenticatorWithReturnTo interface {
	CompleteWithReturnTo(r *http.Request) (*iauth.ExternalIdentity, string, error)
}

func completeRedirectProvider(provider iauth.RedirectAuthenticator, r *http.Request) (*iauth.ExternalIdentity, string, error) {
	if withReturnTo, ok := provider.(redirectAuthenticatorWithReturnTo); ok {
		return withReturnTo.CompleteWithReturnTo(r)
	}
	ext, err := provider.Complete(r)
	return ext, "/", err
}

func safeReturnTo(r *http.Request, raw string, trustedProxies []*net.IPNet) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/"
	}

	target, err := url.Parse(raw)
	if err != nil {
		return "/"
	}
	if target.Scheme != "" || target.Host != "" {
		if !sameOrigin(r, target, trustedProxies) {
			return "/"
		}
		return pathAndQuery(target)
	}
	if !strings.HasPrefix(target.Path, "/") {
		return "/"
	}
	return pathAndQuery(target)
}

func sameOrigin(r *http.Request, target *url.URL, trustedProxies []*net.IPNet) bool {
	if target.Scheme == "" || target.Host == "" {
		return false
	}
	scheme := "http"
	if requestIsSecure(r, trustedProxies) {
		scheme = "https"
	}
	return strings.EqualFold(target.Scheme, scheme) && strings.EqualFold(target.Host, r.Host)
}

func pathAndQuery(target *url.URL) string {
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	if target.RawQuery != "" {
		path += "?" + target.RawQuery
	}
	if target.Fragment != "" {
		path += "#" + target.EscapedFragment()
	}
	return path
}
