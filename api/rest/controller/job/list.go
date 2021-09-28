package job

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/labstack/echo/v4"
)

func List(c echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	jobs, err := job.Service().List(req)
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	return c.JSON(http.StatusOK, jobs)
}

func parseListRequest(c echo.Context) (req *job.ListRequest, err error) {
	req = &job.ListRequest{
		TriggerID: c.QueryParam("trigger_id"),
	}

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
