package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/api/gql"
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo-contrib/v5/echoprometheus"
	"github.com/labstack/echo/v5"
)

type InternalWakeupHandler func(ctx context.Context, id string, ttl int)

// Start launches Caesium's API.
func Start(ctx context.Context, bus event.Bus, authSvc *auth.Service, auditor *auth.AuditLogger, limiter *auth.RateLimiter, wakeupHandler InternalWakeupHandler) error {
	e := echo.New()
	vars := env.Variables()
	configureIPExtractor(e, vars)

	// health
	e.GET("/health", Health)
	e.GET("/auth/status", authStatus(vars))
	registerInternalWakeup(e, vars, wakeupHandler)

	// metrics
	e.Use(echoprometheus.NewMiddleware("caesium"))
	registerMetrics(e, vars, authSvc, auditor, limiter)

	// REST
	bind.All(e.Group("/v1"), bus, authSvc, auditor, limiter)

	registerGraphQL(e, vars)

	// Embedded web UI
	RegisterUI(e)

	sc := echo.StartConfig{
		Address: fmt.Sprintf(":%v", vars.Port),
	}
	return sc.Start(ctx, e)
}

type internalWakeupRequest struct {
	ID  string `json:"id,omitempty"`
	TTL int    `json:"ttl,omitempty"`
}

func registerInternalWakeup(e *echo.Echo, vars env.Environment, handler InternalWakeupHandler) {
	token := strings.TrimSpace(vars.InternalWakeupToken)
	if token == "" || handler == nil {
		return
	}

	e.POST("/internal/wakeup", func(c *echo.Context) error {
		if !authorizedInternalWakeup(c.Request(), token) {
			return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
		}

		var req internalWakeupRequest
		if c.Request().Body != nil {
			err := json.NewDecoder(c.Request().Body).Decode(&req)
			if err != nil && err != io.EOF {
				return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
			}
		}
		if req.TTL < 0 {
			req.TTL = 0
		}
		if req.TTL > 8 {
			req.TTL = 8
		}

		handler(c.Request().Context(), req.ID, req.TTL)
		return c.NoContent(http.StatusNoContent)
	})
}

func authorizedInternalWakeup(req *http.Request, token string) bool {
	if token == "" {
		return false
	}

	if constantTimeEqual(req.Header.Get("X-Caesium-Wakeup-Token"), token) {
		return true
	}

	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	if len(auth) > len("Bearer ") && strings.EqualFold(auth[:len("Bearer ")], "Bearer ") {
		return constantTimeEqual(strings.TrimSpace(auth[len("Bearer "):]), token)
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func authStatus(vars env.Environment) echo.HandlerFunc {
	return func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]bool{
			"enabled": vars.AuthMode == "api-key",
		})
	}
}

func registerMetrics(e *echo.Echo, vars env.Environment, authSvc *auth.Service, auditor *auth.AuditLogger, limiter *auth.RateLimiter) {
	metrics.Register()

	handler := echoprometheus.NewHandler()
	if vars.AuthMode == "api-key" && authSvc != nil {
		e.GET("/metrics", handler, authmw.Auth(authSvc, auditor, limiter))
		return
	}

	e.GET("/metrics", handler)
}

func registerGraphQL(e *echo.Echo, vars env.Environment) {
	if vars.AuthMode == "none" {
		e.GET("/gql", gql.Handler())
		return
	}

	log.Info("graphql endpoint disabled while authentication is enabled")
}

func configureIPExtractor(e *echo.Echo, vars env.Environment) {
	trusted := parseTrustedProxyRanges(vars.TrustedProxies)
	if len(trusted) == 0 {
		e.IPExtractor = echo.ExtractIPDirect()
		return
	}

	options := []echo.TrustOption{
		echo.TrustLoopback(false),
		echo.TrustLinkLocal(false),
		echo.TrustPrivateNet(false),
	}
	for _, ipNet := range trusted {
		options = append(options, echo.TrustIPRange(ipNet))
	}
	e.IPExtractor = echo.ExtractIPFromXFFHeader(options...)
}

func parseTrustedProxyRanges(raw string) []*net.IPNet {
	parts := strings.Split(raw, ",")
	ranges := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if _, ipNet, err := net.ParseCIDR(part); err == nil {
			ranges = append(ranges, ipNet)
			continue
		}

		if ip := net.ParseIP(part); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			ranges = append(ranges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}

		log.Warn("ignoring invalid trusted proxy entry", "value", part)
	}
	return ranges
}

func Shutdown() error {
	return nil
}
