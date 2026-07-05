package job

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const jobListRunHistoryLimit = 10

type ListJobResponse struct {
	*models.Job
	LatestRun *runstorage.JobRun   `json:"latest_run,omitempty"`
	LastRuns  []job.RunListSummary `json:"last_runs"`
}

func List(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	if aliases := middleware.GetAllowedJobAliases(c); len(aliases) > 0 {
		req.Aliases = aliases
	}

	svc := job.Service(c.Request().Context())
	jobs, err := svc.List(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	jobIDs := make([]uuid.UUID, 0, len(jobs))
	for _, entry := range jobs {
		jobIDs = append(jobIDs, entry.ID)
	}

	runsByJob, err := svc.ListRecentRuns(jobIDs, jobListRunHistoryLimit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	resp := make([]*ListJobResponse, 0, len(jobs))
	for _, entry := range jobs {
		history := runsByJob[entry.ID]
		if history == nil {
			history = []job.RunListSummary{}
		}

		item := &ListJobResponse{
			Job:      entry,
			LastRuns: history,
		}
		if len(history) > 0 {
			item.LatestRun = history[len(history)-1].JobRun()
		}
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
