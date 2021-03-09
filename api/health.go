package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

var startedAt time.Time

func init() {
	startedAt = time.Now()
}

// HealthResponse defines the data the Health
// REST endpoint returns.
type HealthResponse struct {
	Status Status        `json:"status"`
	Uptime time.Duration `json:"uptime"`
}

// Health is used to determine if Caesium is healthy.
// The response also includes the uptime.
func Health(c echo.Context) error {
	return c.JSON(
		http.StatusOK,
		HealthResponse{
			Status: Healthy,
			Uptime: time.Now().Sub(startedAt),
		},
	)
}

// Status enumerates the health statues of Caesium.
type Status string

const (
	// Healthy implies Caesium is having no major issues.
	Healthy Status = "healthy"
)
