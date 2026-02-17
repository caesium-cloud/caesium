package trigger

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/labstack/echo/v5"
)

func List(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	triggers, err := trigger.Service(c.Request().Context()).List(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, triggers)
}

func parseListRequest(c *echo.Context) (req *trigger.ListRequest, err error) {
	req = &trigger.ListRequest{}

	if limit := c.QueryParam("limit"); limit != "" {
		if req.Limit, err = strconv.ParseUint(limit, 10, 64); err != nil {
			return nil, err
		}
	}

	if offset := c.QueryParam("offset"); offset != "" {
		if req.Offset, err = strconv.ParseUint(offset, 10, 64); err != nil {
			return nil, err
		}
	}

	if orderBy := c.QueryParam("order_by"); orderBy != "" {
		req.OrderBy = strings.Split(orderBy, ",")
	}

	return
}
