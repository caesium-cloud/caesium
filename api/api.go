package api

import (
	"context"
	"fmt"

	"github.com/caesium-cloud/caesium/api/gql"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo-contrib/v5/echoprometheus"
	"github.com/labstack/echo/v5"
)

var e *echo.Echo

// Start launches Caesium's API.
func Start(ctx context.Context, bus event.Bus) error {
	e = echo.New()

	// health
	e.GET("/health", Health)

	// metrics
	metrics.Register()
	e.Use(echoprometheus.NewMiddleware("caesium"))
	e.GET("/metrics", echoprometheus.NewHandler())

	// REST
	bind.All(e.Group("/v1"), bus)

	// GraphQL
	e.GET("/gql", gql.Handler())

	// UI
	RegisterUI(e)

	sc := echo.StartConfig{
		Address: fmt.Sprintf(":%v", env.Variables().Port),
	}
	return sc.Start(ctx, e)
}
