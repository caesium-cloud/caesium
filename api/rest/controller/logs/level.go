package logs

import (
	"net/http"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

type levelResponse struct {
	Level string `json:"level"`
}

type setLevelRequest struct {
	Level string `json:"level"`
}

// GetLevel returns the current server log level.
func GetLevel(c *echo.Context) error {
	return c.JSON(http.StatusOK, levelResponse{
		Level: log.GetLevel().String(),
	})
}

// SetLevel changes the server log level at runtime.
func SetLevel(c *echo.Context) error {
	var req setLevelRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}

	if err := log.SetLevel(req.Level); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	log.Info("log level changed", "level", req.Level)

	return c.JSON(http.StatusOK, levelResponse{
		Level: log.GetLevel().String(),
	})
}
