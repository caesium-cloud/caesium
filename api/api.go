package api

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/caesium-cloud/caesium/api/gql"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo-contrib/v5/echoprometheus"
	"github.com/labstack/echo/v5"
)

// Start launches Caesium's API.
func Start(ctx context.Context, bus event.Bus, authSvc *auth.Service, auditor *auth.AuditLogger, limiter *auth.RateLimiter) error {
	e := echo.New()
	vars := env.Variables()
	configureIPExtractor(e, vars)

	// health
	e.GET("/health", Health)

	// metrics
	metrics.Register()
	e.Use(echoprometheus.NewMiddleware("caesium"))
	e.GET("/metrics", echoprometheus.NewHandler())

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
