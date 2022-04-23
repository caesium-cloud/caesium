package trigger

import (
	"fmt"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v4"
)

func Put(c echo.Context) error {
	req := &trigger.CreateRequest{}

	if err := c.Bind(req); err != nil {
		return err
	}

	if req.Type == "" {
		return echo.ErrBadRequest.SetInternal(fmt.Errorf("type is required"))
	}

	trig, err := trigger.Service(c.Request().Context()).Create(req)
	if err != nil {
		log.Error("failed to create trigger", "error", err)
		return echo.ErrInternalServerError.SetInternal(err)
	}

	return c.JSON(http.StatusCreated, trig)
}
