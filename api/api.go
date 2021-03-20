package api

import (
	"fmt"

	"github.com/caesium-cloud/caesium/api/gql"
	"github.com/caesium-cloud/caesium/api/rest/v1"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo-contrib/prometheus"
	"github.com/labstack/echo/v4"
)

// Start launches Caesium's API.
func Start() error {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// health
	e.GET("/health", Health)

	// metrics
	prometheus.NewPrometheus("caesium", nil).Use(e)

	// REST
	rest.Bind(e.Group("/v1"))

	// GraphQL
	e.GET("/gql", gql.Handler())

	return e.Start(fmt.Sprintf(":%v", env.Variables().Port))
}
