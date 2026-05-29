package auth

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// SSOController serves session-aware endpoints and browser-redirect provider
// callbacks.
type SSOController struct {
	sessions       *iauth.SessionStore
	sso            *iauth.SSOService
	auditor        *iauth.AuditLogger
	oidc           iauth.RedirectAuthenticator
	saml           iauth.RedirectAuthenticator
	ldap           iauth.CredentialAuthenticator
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

// SetSAMLProvider wires the SAML redirect provider into the controller.
func (s *SSOController) SetSAMLProvider(provider iauth.RedirectAuthenticator) {
	s.saml = provider
}

// SetLDAPProvider wires the LDAP credential provider into the controller.
func (s *SSOController) SetLDAPProvider(provider iauth.CredentialAuthenticator) {
	s.ldap = provider
}

// SetAuditLogger wires audit logging for provider-level login/logout events.
func (s *SSOController) SetAuditLogger(auditor *iauth.AuditLogger) {
	s.auditor = auditor
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

func (s *SSOController) SAMLLogin(c *echo.Context) error {
	return s.beginRedirectLogin(c, s.saml, "saml")
}

func (s *SSOController) SAMLACS(c *echo.Context) error {
	return s.completeRedirectLogin(c, s.saml, "saml")
}

func (s *SSOController) SAMLMetadata(c *echo.Context) error {
	provider, ok := s.saml.(interface {
		Metadata(http.ResponseWriter, *http.Request) error
	})
	if !ok {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "saml provider unavailable")
	}
	if err := provider.Metadata(c.Response(), c.Request()); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "saml metadata failed").Wrap(err)
	}
	return nil
}

func (s *SSOController) LDAPLogin(c *echo.Context) error {
	return s.completeCredentialLogin(c, s.ldap, "ldap")
}

func (s *SSOController) Logout(c *echo.Context) error {
	if principal := authmw.GetPrincipal(c); principal != nil && principal.Kind == iauth.PrincipalAPIKey {
		s.recordAPIKeyLogoutNoop(c, principal)
		return c.NoContent(http.StatusNoContent)
	}

	var sess *models.Session
	var user *models.User
	recordLogout := false
	if s.sessions != nil {
		if cookie, err := c.Request().Cookie(s.cookieName); err == nil && cookie.Value != "" {
			if validSession, validUser, err := s.sessions.Validate(c.Request().Context(), cookie.Value); err == nil {
				sess = validSession
				user = validUser
				recordLogout = true
				if err := s.sessions.Revoke(c.Request().Context(), sess.ID); err != nil {
					s.recordLogout(c, iauth.OutcomeError, sess, user)
					log.Warn("failed to revoke session during logout", "error", err)
					return echo.NewHTTPError(http.StatusInternalServerError, "logout failed").Wrap(err)
				}
				s.recordSessionRevoked(c, sess, user)
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
	if recordLogout {
		s.recordLogout(c, iauth.OutcomeSuccess, sess, user)
	}
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
	clearRedirectStateCookie(provider, c.Response(), c.Request())

	providerName := providerMethod(provider, fallbackName)
	if err != nil {
		s.recordProviderLoginFailure(c, providerName, "unknown", "callback_failed", iauth.OutcomeError)
		return echo.NewHTTPError(http.StatusUnauthorized, fallbackName+" callback failed").Wrap(err)
	}
	if ext == nil {
		s.recordProviderLoginFailure(c, providerName, "unknown", "missing_identity", iauth.OutcomeDenied)
		return echo.NewHTTPError(http.StatusUnauthorized, fallbackName+" callback failed")
	}

	cookieValue, sess, err := s.sso.Complete(
		c.Request().Context(),
		ext,
		providerName,
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

type credentialLoginRequest struct {
	Username string `json:"username" form:"username"`
	Password string `json:"password" form:"password"`
}

func (s *SSOController) completeCredentialLogin(c *echo.Context, provider iauth.CredentialAuthenticator, fallbackName string) error {
	if provider == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, fallbackName+" provider unavailable")
	}
	if s.sso == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "sso service unavailable")
	}

	var req credentialLoginRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "username and password are required")
	}

	ext, err := provider.Authenticate(c.Request().Context(), username, req.Password)
	providerName := credentialProviderMethod(provider, fallbackName)
	if err != nil {
		outcome, reason := credentialProviderFailure(err)
		s.recordProviderLoginFailure(c, providerName, username, reason, outcome)
		return echo.NewHTTPError(http.StatusUnauthorized, fallbackName+" login failed").Wrap(err)
	}
	if ext == nil {
		s.recordProviderLoginFailure(c, providerName, username, "missing_identity", iauth.OutcomeDenied)
		return echo.NewHTTPError(http.StatusUnauthorized, fallbackName+" login failed")
	}

	cookieValue, sess, err := s.sso.Complete(
		c.Request().Context(),
		ext,
		providerName,
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
	return c.NoContent(http.StatusNoContent)
}

func (s *SSOController) recordProviderLoginFailure(c *echo.Context, provider, actor, reason, outcome string) {
	if outcome == "" {
		outcome = iauth.OutcomeError
	}
	metrics.SSOLoginsTotal.WithLabelValues(metricProvider(provider), outcome).Inc()
	if s.auditor == nil {
		return
	}
	action := iauth.ActionAuthLogin
	if outcome == iauth.OutcomeDenied {
		action = iauth.ActionAuthLoginDenied
	}
	logAuditFailure(s.auditor.Log(iauth.AuditEntry{
		Actor:    auditActor(actor),
		Action:   action,
		SourceIP: c.RealIP(),
		Outcome:  outcome,
		Metadata: map[string]interface{}{
			"provider": provider,
			"reason":   reason,
			"method":   c.Request().Method,
			"path":     c.Request().URL.Path,
		},
	}))
}

func (s *SSOController) recordLogout(c *echo.Context, outcome string, sess *models.Session, user *models.User) {
	if outcome == "" {
		outcome = iauth.OutcomeSuccess
	}
	metrics.SSOLogoutsTotal.WithLabelValues(outcome).Inc()
	if s.auditor == nil {
		return
	}

	entry := iauth.AuditEntry{
		Actor:    logoutActor(c, user),
		Action:   iauth.ActionAuthLogout,
		SourceIP: c.RealIP(),
		Outcome:  outcome,
		Metadata: map[string]interface{}{
			"method": c.Request().Method,
			"path":   c.Request().URL.Path,
		},
	}
	if sess != nil {
		entry.ResourceType = "session"
		entry.ResourceID = sess.ID.String()
		entry.Metadata["provider"] = sess.AuthMethod
	}
	logAuditFailure(s.auditor.Log(entry))
}

func (s *SSOController) recordAPIKeyLogoutNoop(c *echo.Context, principal *iauth.Principal) {
	if s.auditor == nil || principal == nil {
		return
	}
	entry := iauth.AuditEntry{
		Actor:        principal.Subject,
		Action:       iauth.ActionAuthLogout,
		ResourceType: "api_key",
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
		Metadata: map[string]interface{}{
			"method": c.Request().Method,
			"path":   c.Request().URL.Path,
			"noop":   true,
		},
	}
	if principal.KeyID != nil {
		entry.ResourceID = principal.KeyID.String()
	}
	logAuditFailure(s.auditor.Log(entry))
}

func (s *SSOController) recordSessionRevoked(c *echo.Context, sess *models.Session, user *models.User) {
	if s.auditor == nil || sess == nil {
		return
	}
	logAuditFailure(s.auditor.Log(iauth.AuditEntry{
		Actor:        logoutActor(c, user),
		Action:       iauth.ActionAuthSessionRevoked,
		ResourceType: "session",
		ResourceID:   sess.ID.String(),
		SourceIP:     c.RealIP(),
		Outcome:      iauth.OutcomeSuccess,
		Metadata: map[string]interface{}{
			"provider": sess.AuthMethod,
			"method":   c.Request().Method,
			"path":     c.Request().URL.Path,
		},
	}))
}

func credentialProviderFailure(err error) (string, string) {
	if errors.Is(err, iauth.ErrLoginDenied) {
		return iauth.OutcomeDenied, "invalid_credentials"
	}
	return iauth.OutcomeError, "provider_error"
}

func logoutActor(c *echo.Context, user *models.User) string {
	if principal := authmw.GetPrincipal(c); principal != nil {
		return auditActor(principal.Subject)
	}
	if user != nil {
		if user.Email != "" {
			return auditActor(user.Email)
		}
		return auditActor(user.Subject)
	}
	return "unknown"
}

func auditActor(actor string) string {
	if actor = strings.TrimSpace(actor); actor != "" {
		return actor
	}
	return "unknown"
}

func metricProvider(provider string) string {
	if provider = strings.TrimSpace(provider); provider != "" {
		return provider
	}
	return "unknown"
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

func credentialProviderMethod(provider iauth.CredentialAuthenticator, fallbackName string) string {
	if name := strings.TrimSpace(provider.Name()); name != "" {
		return name
	}
	return fallbackName
}

type redirectAuthenticatorWithReturnTo interface {
	CompleteWithReturnTo(r *http.Request) (*iauth.ExternalIdentity, string, error)
}

type redirectAuthenticatorStateClearer interface {
	ClearStateCookie(w http.ResponseWriter, r *http.Request)
}

func completeRedirectProvider(provider iauth.RedirectAuthenticator, r *http.Request) (*iauth.ExternalIdentity, string, error) {
	if withReturnTo, ok := provider.(redirectAuthenticatorWithReturnTo); ok {
		return withReturnTo.CompleteWithReturnTo(r)
	}
	ext, err := provider.Complete(r)
	return ext, "/", err
}

func clearRedirectStateCookie(provider iauth.RedirectAuthenticator, w http.ResponseWriter, r *http.Request) {
	if clearer, ok := provider.(redirectAuthenticatorStateClearer); ok {
		clearer.ClearStateCookie(w, r)
	}
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
		if unsafeLocalReturnPath(target.Path) {
			return "/"
		}
		return pathAndQuery(target)
	}
	if unsafeLocalReturnPath(target.Path) {
		return "/"
	}
	return pathAndQuery(target)
}

func unsafeLocalReturnPath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return true
	}
	for i := 0; i < len(path); i++ {
		if path[i] < 32 || path[i] == 127 {
			return true
		}
	}
	normalized := strings.ReplaceAll(path, `\`, "/")
	return strings.HasPrefix(normalized, "//")
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
