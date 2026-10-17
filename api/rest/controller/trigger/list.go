package trigger

import (
	"net/http"
	"strconv"

	"github.com/caesium-cloud/caesium/api/internal/orderby"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/labstack/echo/v5"
)

// triggerListOrderColumns are the columns ?order_by= may sort the trigger list
// by; a raw expression reached GORM's Order verbatim (identity-and-access B4).
var triggerListOrderColumns = map[string]struct{}{
	"id": {}, "alias": {}, "type": {}, "created_at": {}, "updated_at": {},
}

func List(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	triggers, err := triggerListService(c.Request().Context()).List(req)
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
		if req.OrderBy, err = orderby.Parse(orderBy, triggerListOrderColumns); err != nil {
			return nil, err
		}
	}

	return
}

// triggerListService builds the trigger service for a list request; a seam
// for the collection-guard test, which serves the real handler over a test
// database.
var triggerListService = trigger.Service
