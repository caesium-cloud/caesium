package api

import (
	"context"
	"fmt"

	"github.com/caesium-cloud/caesium/api/gql"
	"github.com/caesium-cloud/caesium/api/rest/bind"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo-contrib/prometheus"
	"github.com/labstack/echo/v4"
)

var e *echo.Echo

// Start launches Caesium's API.
func Start() error {
	e = echo.New()
	e.HideBanner = true
	e.HidePort = true

	// health
	e.GET("/health", Health)

	// metrics
	prometheus.NewPrometheus("caesium", nil).Use(e)

	// REST
	bind.All(e.Group("/v1"))

	// GraphQL
	e.GET("/gql", gql.Handler())

	return e.Start(fmt.Sprintf(":%v", env.Variables().Port))
}

func Shutdown() error {
	if e != nil {
		return e.Shutdown(context.Background())
	}

	return nil
}
