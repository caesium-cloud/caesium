package middleware

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

// ContextKeyAuth is the key used to store the authenticated API key in the Echo context.
const ContextKeyAuth = "auth"

// ContextKeyPrincipal stores the unified authenticated identity for the request.
const ContextKeyPrincipal = "auth.principal"

// uuidPattern matches UUID path segments for route normalisation.
var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var namedParamPattern = regexp.MustCompile(`:[^/]+`)

// skipPaths lists exact paths that never require authentication.
var skipPaths = map[string]bool{
	"/health": true,
}

var publicAuthPaths = map[string]bool{
	"/auth/status":            true,
	"/auth/sso/oidc/login":    true,
	"/auth/sso/oidc/callback": true,
	"/auth/sso/saml/login":    true,
	"/auth/sso/saml/acs":      true,
	"/auth/sso/saml/metadata": true,
	"/auth/sso/ldap/login":    true,
}

var publicAuthPathPrefixes = []string{
	"/v1/hooks/",
}

// AuthDeps bundles the dependencies the auth middleware needs.
type AuthDeps struct {
	Service    *auth.Service
	Auditor    *auth.AuditLogger
	Limiter    *auth.RateLimiter
	Sessions   *auth.SessionStore
	CookieName string
}

// Auth returns Echo middleware that enforces API-key or session-cookie
// authentication and RBAC.
func Auth(d AuthDeps) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			path := c.Request().URL.Path

			// Skip auth for explicitly public paths.
			if IsPublicAuthPath(path) {
				return next(c)
			}

			ip := c.RealIP()

			// Rate limit check before doing any work.
			if d.Limiter.IsLimited(ip) {
				metrics.AuthFailuresTotal.WithLabelValues("rate_limited").Inc()
				retryAfter := d.Limiter.RetryAfter(ip)
				c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
				return echo.NewHTTPError(http.StatusTooManyRequests, "too many failed authentication attempts")
			}

			var key *models.APIKey
			var principal *auth.Principal

			token, malformedAuthorization := extractBearerToken(c)
			if malformedAuthorization {
				if recordCredentialFailure(c, d, ip, "authorization", "malformed_authorization") {
					return rateLimitError(c, d.Limiter, ip)
				}
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid authorization header")
			}

			if token != "" {
				validKey, err := d.Service.ValidateKey(token)
				if err != nil {
					reason := classifyAuthError(err)
					if recordCredentialFailure(c, d, ip, tokenPrefix(token), reason) {
						return rateLimitError(c, d.Limiter, ip)
					}
					return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired api key")
				}

				key = validKey
				principal = auth.PrincipalFromKey(validKey)
			} else if d.Sessions != nil {
				if cookie, err := c.Request().Cookie(d.CookieName); err == nil && cookie.Value != "" {
					sess, user, verr := d.Sessions.Validate(c.Request().Context(), cookie.Value)
					if verr != nil {
						if recordCredentialFailure(c, d, ip, tokenPrefix(cookie.Value), classifySessionAuthError(verr)) {
							return rateLimitError(c, d.Limiter, ip)
						}
						return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired session")
					}
					if err := EnforceSessionCSRF(c, sess.CSRFToken); err != nil {
						if recordCredentialFailure(c, d, ip, tokenPrefix(cookie.Value), "csrf") {
							return rateLimitError(c, d.Limiter, ip)
						}
						return err
					}
					principal = auth.PrincipalFromUser(user)
					c.Set(ContextKeyCSRFToken, sess.CSRFToken)
				}
			}

			if principal == nil {
				metrics.AuthFailuresTotal.WithLabelValues("missing").Inc()
				return echo.NewHTTPError(http.StatusUnauthorized, "missing credentials")
			}

			// Determine the required role for this endpoint.
			routePath := normalisePath(c)
			required, ok := auth.RequiredRole(c.Request().Method, routePath)
			if !ok {
				return denyAccess(c, d.Auditor, principal.Subject, principal.Role, routePath, "unknown_route", "")
			}

			if !auth.HasRole(principal.Role, required) {
				return denyAccess(c, d.Auditor, principal.Subject, principal.Role, routePath, "insufficient_role", required)
			}

			// Store authenticated identity in context for downstream handlers.
			scopeContext, err := authorizeScope(c, d.Service, principal.Scope, routePath)
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok && he.Code == http.StatusForbidden {
					return denyAccessWithMessage(c, d.Auditor, principal.Subject, principal.Role, routePath, "insufficient_scope", required, forbiddenMessage(he))
				}
				return err
			}

			if key != nil {
				c.Set(ContextKeyAuth, key)
				metrics.AuthKeyAgeSeconds.Observe(time.Since(key.CreatedAt).Seconds())
			}
			c.Set(ContextKeyPrincipal, principal)

			if err := next(c); err != nil {
				return err
			}

			metrics.AuthRequestsTotal.WithLabelValues("success", string(principal.Role), c.Request().Method, routePath).Inc()
			logSuccessfulAction(d.Auditor, c, principal, routePath, scopeContext)
			return nil
		}
	}
}

// extractBearerToken extracts a Bearer token and reports malformed
// Authorization headers so cookie auth cannot silently mask them.
func extractBearerToken(c *echo.Context) (string, bool) {
	header := c.Request().Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", true
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", true
	}
	return token, false
}

// IsPublicAuthPath reports whether path is intentionally reachable without the
// Auth middleware enforcing credentials. Keep RBAC completeness tests on this
// helper so their public-route exclusions match the middleware.
func IsPublicAuthPath(path string) bool {
	if skipPaths[path] || publicAuthPaths[path] {
		return true
	}
	for _, prefix := range publicAuthPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// NormalizeRoutePath returns the policy lookup path used by RBAC for Echo route
// patterns or raw request paths.
func NormalizeRoutePath(path string) string {
	path = uuidPattern.ReplaceAllString(path, ":id")
	return namedParamPattern.ReplaceAllString(path, ":id")
}

// normalisePath returns the Echo route pattern (with :param placeholders)
// so RBAC policy lookup works for parametric routes. Falls back to normalising
// UUID segments in the raw path.
func normalisePath(c *echo.Context) string {
	path := c.RouteInfo().Path
	if path == "" {
		path = c.Request().URL.Path
	}
	return NormalizeRoutePath(path)
}

// tokenPrefix returns a safe display prefix for logging, never the full key.
func tokenPrefix(token string) string {
	if len(token) > 13 {
		return token[:13]
	}
	return token
}

// classifyAuthError maps validation errors to metric label values.
func classifyAuthError(err error) string {
	switch err {
	case auth.ErrKeyNotFound:
		return "invalid"
	case auth.ErrKeyExpired:
		return "expired"
	case auth.ErrKeyRevoked:
		return "revoked"
	default:
		log.Error("unexpected auth validation error", "error", err)
		return "error"
	}
}

func classifySessionAuthError(err error) string {
	switch err {
	case auth.ErrSessionInvalid:
		return "session_invalid"
	case auth.ErrSessionExpired:
		return "session_expired"
	case auth.ErrSessionRevoked:
		return "session_revoked"
	case auth.ErrUserDisabled:
		return "user_disabled"
	default:
		log.Error("unexpected session validation error", "error", err)
		return "session_error"
	}
}

func recordCredentialFailure(c *echo.Context, d AuthDeps, ip, actor, reason string) bool {
	metrics.AuthFailuresTotal.WithLabelValues(reason).Inc()
	limited := d.Limiter.RecordFailure(ip)
	logAuditFailure(d.Auditor.Log(auth.AuditEntry{
		Actor:    actor,
		Action:   auth.ActionAuthDenied,
		SourceIP: ip,
		Outcome:  auth.OutcomeDenied,
		Metadata: map[string]interface{}{
			"reason": reason,
			"method": c.Request().Method,
			"path":   c.Request().URL.Path,
		},
	}))
	return limited
}

func rateLimitError(c *echo.Context, limiter *auth.RateLimiter, ip string) error {
	retryAfter := limiter.RetryAfter(ip)
	c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	return echo.NewHTTPError(http.StatusTooManyRequests, "too many failed authentication attempts")
}

func denyAccess(
	c *echo.Context,
	auditor *auth.AuditLogger,
	actor string,
	keyRole models.Role,
	routePath string,
	reason string,
	required models.Role,
) error {
	return denyAccessWithMessage(c, auditor, actor, keyRole, routePath, reason, required, "insufficient permissions")
}

func denyAccessWithMessage(
	c *echo.Context,
	auditor *auth.AuditLogger,
	actor string,
	keyRole models.Role,
	routePath string,
	reason string,
	required models.Role,
	message string,
) error {
	metrics.AuthRequestsTotal.WithLabelValues("denied", string(keyRole), c.Request().Method, routePath).Inc()

	metadata := map[string]interface{}{
		"reason":     reason,
		"method":     c.Request().Method,
		"path":       routePath,
		"key_role":   string(keyRole),
		"policy_key": c.Request().Method + " " + routePath,
	}
	if required != "" {
		metadata["required_role"] = string(required)
	}

	logAuditFailure(auditor.Log(auth.AuditEntry{
		Actor:    actor,
		Action:   auth.ActionAuthDenied,
		SourceIP: c.RealIP(),
		Outcome:  auth.OutcomeDenied,
		Metadata: metadata,
	}))
	return echo.NewHTTPError(http.StatusForbidden, message)
}

func forbiddenMessage(err *echo.HTTPError) string {
	// echo/v5 HTTPError.Message is a concrete string (not interface{} as in v4),
	// so read it directly rather than type-asserting.
	if err != nil && err.Message != "" {
		return err.Message
	}
	return "insufficient permissions"
}

func logSuccessfulAction(
	auditor *auth.AuditLogger,
	c *echo.Context,
	principal *auth.Principal,
	routePath string,
	scopeContext *scopeAuditContext,
) {
	action := auditActionForRoute(c.Request().Method, routePath)
	if action == "" {
		return
	}

	if resp, err := echo.UnwrapResponse(c.Response()); err == nil && resp.Status >= http.StatusBadRequest {
		return
	}

	entry := auth.AuditEntry{
		Actor:    principal.Subject,
		Action:   action,
		SourceIP: c.RealIP(),
		Outcome:  auth.OutcomeSuccess,
		Metadata: map[string]interface{}{
			"method": c.Request().Method,
			"path":   routePath,
		},
	}

	if scopeContext != nil && len(scopeContext.jobAliases) > 0 {
		entry.ResourceType = "job"
		entry.ResourceID = scopeContext.jobAliases[0]
		entry.Metadata["job_aliases"] = append([]string(nil), scopeContext.jobAliases...)
	}

	switch action {
	case auth.ActionJobdefApply:
		entry.ResourceType = "job_definition"
	case auth.ActionCachePrune:
		entry.ResourceType = "cache"
	case auth.ActionCacheDelete:
		entry.ResourceType = "cache"
	}

	logAuditFailure(auditor.Log(entry))
}

func auditActionForRoute(method, routePath string) string {
	switch method + " " + routePath {
	case "POST /v1/jobs":
		return auth.ActionJobCreate
	case "DELETE /v1/jobs/:id":
		return auth.ActionJobDelete
	case "PUT /v1/jobs/:id/pause":
		return auth.ActionJobPause
	case "PUT /v1/jobs/:id/unpause":
		return auth.ActionJobUnpause
	case "POST /v1/jobs/:id/run":
		return auth.ActionRunTrigger
	case "GET /v1/jobs/:id/queue":
		return auth.ActionRunQueueRead
	case "DELETE /v1/jobs/:id/queue/:id":
		return auth.ActionRunQueueCancel
	case "POST /v1/jobs/:id/runs/:id/retry":
		return auth.ActionRunRetry
	case "POST /v1/jobs/:id/backfill":
		return auth.ActionBackfill
	case "PUT /v1/jobs/:id/backfills/:id/cancel":
		return auth.ActionBackfill
	case "POST /v1/jobdefs/apply":
		return auth.ActionJobdefApply
	case "POST /v1/cache/prune":
		return auth.ActionCachePrune
	case "DELETE /v1/jobs/:id/cache":
		return auth.ActionCacheDelete
	case "DELETE /v1/jobs/:id/cache/:id":
		return auth.ActionCacheDelete
	default:
		return ""
	}
}

func logAuditFailure(err error) {
	if err != nil {
		log.Warn("failed to write audit log", "error", err)
	}
}

// GetAuthKey extracts the authenticated API key from the Echo context.
// Returns nil if no key is present (e.g. unauthenticated endpoints).
func GetAuthKey(c *echo.Context) *models.APIKey {
	v := c.Get(ContextKeyAuth)
	if v == nil {
		return nil
	}
	key, ok := v.(*models.APIKey)
	if !ok {
		return nil
	}
	return key
}

// GetPrincipal returns the unified authenticated identity, or nil if unauthenticated.
func GetPrincipal(c *echo.Context) *auth.Principal {
	v := c.Get(ContextKeyPrincipal)
	if v == nil {
		return nil
	}
	principal, ok := v.(*auth.Principal)
	if !ok {
		return nil
	}
	return principal
}
