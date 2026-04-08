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

// uuidPattern matches UUID path segments for route normalisation.
var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
var namedParamPattern = regexp.MustCompile(`:[^/]+`)

// skipPaths lists exact paths that never require authentication.
var skipPaths = map[string]bool{
	"/health": true,
}

// Auth returns Echo middleware that enforces API key authentication and RBAC.
func Auth(svc *auth.Service, auditor *auth.AuditLogger, limiter *auth.RateLimiter) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			path := c.Request().URL.Path

			// Skip auth for explicitly public paths.
			if skipPaths[path] {
				return next(c)
			}

			ip := c.RealIP()

			// Rate limit check before doing any work.
			if limiter.IsLimited(ip) {
				metrics.AuthFailuresTotal.WithLabelValues("rate_limited").Inc()
				retryAfter := limiter.RetryAfter(ip)
				c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
				return echo.NewHTTPError(http.StatusTooManyRequests, "too many failed authentication attempts")
			}

			// Extract bearer token.
			token := extractBearerToken(c)
			if token == "" {
				metrics.AuthFailuresTotal.WithLabelValues("missing").Inc()
				return echo.NewHTTPError(http.StatusUnauthorized, "missing authorization header")
			}

			// Validate the API key.
			key, err := svc.ValidateKey(token)
			if err != nil {
				reason := classifyAuthError(err)
				metrics.AuthFailuresTotal.WithLabelValues(reason).Inc()

				limited := limiter.RecordFailure(ip)
				logAuditFailure(auditor.Log(auth.AuditEntry{
					Actor:    tokenPrefix(token),
					Action:   auth.ActionAuthDenied,
					SourceIP: ip,
					Outcome:  auth.OutcomeDenied,
					Metadata: map[string]interface{}{
						"reason": reason,
						"method": c.Request().Method,
						"path":   path,
					},
				}))

				if limited {
					retryAfter := limiter.RetryAfter(ip)
					c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
					return echo.NewHTTPError(http.StatusTooManyRequests, "too many failed authentication attempts")
				}

				return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired api key")
			}

			// Determine the required role for this endpoint.
			routePath := normalisePath(c)
			required, ok := auth.RequiredRole(c.Request().Method, routePath)
			if !ok {
				return denyAccess(c, auditor, key.KeyPrefix, key.Role, routePath, "unknown_route", "")
			}

			if !auth.HasRole(key.Role, required) {
				return denyAccess(c, auditor, key.KeyPrefix, key.Role, routePath, "insufficient_role", required)
			}

			// Store authenticated identity in context for downstream handlers.
			scopeContext, err := authorizeScope(c, svc, key, routePath)
			if err != nil {
				if he, ok := err.(*echo.HTTPError); ok && he.Code == http.StatusForbidden {
					return denyAccess(c, auditor, key.KeyPrefix, key.Role, routePath, "insufficient_scope", required)
				}
				return err
			}

			c.Set(ContextKeyAuth, key)
			metrics.AuthKeyAgeSeconds.Observe(time.Since(key.CreatedAt).Seconds())

			if err := next(c); err != nil {
				return err
			}

			metrics.AuthRequestsTotal.WithLabelValues("success", string(key.Role), c.Request().Method, routePath).Inc()
			logSuccessfulAction(auditor, c, key, routePath, scopeContext)
			return nil
		}
	}
}

// extractBearerToken extracts the token from the Authorization header.
func extractBearerToken(c *echo.Context) string {
	header := c.Request().Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// normalisePath returns the Echo route pattern (with :param placeholders)
// so RBAC policy lookup works for parametric routes. Falls back to replacing
// UUID segments in the raw path.
func normalisePath(c *echo.Context) string {
	ri := c.RouteInfo()
	path := ri.Path
	if path == "" {
		path = uuidPattern.ReplaceAllString(c.Request().URL.Path, ":id")
	}
	return namedParamPattern.ReplaceAllString(path, ":id")
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

func denyAccess(
	c *echo.Context,
	auditor *auth.AuditLogger,
	actor string,
	keyRole models.Role,
	routePath string,
	reason string,
	required models.Role,
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
	return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
}

func logSuccessfulAction(
	auditor *auth.AuditLogger,
	c *echo.Context,
	key *models.APIKey,
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
		Actor:    key.KeyPrefix,
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
