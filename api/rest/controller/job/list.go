package job

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func List(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	if aliases := middleware.GetAllowedJobAliases(c); len(aliases) > 0 {
		req.Aliases = aliases
	}

	jobs, err := job.Service(c.Request().Context()).List(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	resp := make([]*JobResponse, 0, len(jobs))
	for _, entry := range jobs {
		item := &JobResponse{Job: entry}
		latest, latestErr := runsvc.New(c.Request().Context()).Latest(entry.ID)
		if latestErr != nil && !errors.Is(latestErr, gorm.ErrRecordNotFound) {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(latestErr)
		}
		item.LatestRun = latest
		resp = append(resp, item)
	}

	return c.JSON(http.StatusOK, resp)
}

func parseListRequest(c *echo.Context) (req *job.ListRequest, err error) {
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
