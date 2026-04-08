package trigger

import (
	"errors"
	"net/http"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func triggerServiceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case errors.Is(err, triggersvc.ErrTriggerAliasConflict):
		return echo.NewHTTPError(http.StatusConflict, "conflict").Wrap(err)
	case errors.Is(err, triggersvc.ErrInvalidTriggerRequest):
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}
