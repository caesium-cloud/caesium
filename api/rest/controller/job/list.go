package job

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/api/internal/orderby"
	"github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const jobListRunHistoryLimit = 10

// jobListOrderColumns are the columns ?order_by= may sort the job list by. A
// raw expression reached GORM's Order verbatim, so a caller could sort by a
// subquery over rows it may not read (and, for a namespace-confined caller,
// learn about other namespaces through it; identity-and-access B4).
var jobListOrderColumns = map[string]struct{}{
	"id": {}, "alias": {}, "namespace": {}, "trigger_id": {}, "priority": {}, "paused": {},
	"created_at": {}, "updated_at": {},
}

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
	// ?alias= narrows inside the caller's allowlist. It must not replace that
	// allowlist with a wider set, and an alias outside it is an empty list:
	// an empty Aliases slice means "no alias predicate".
	aliases, empty := narrowJobListAliases(middleware.GetAllowedJobAliases(c), req.Aliases)
	if empty {
		return c.JSON(http.StatusOK, []*ListJobResponse{})
	}
	req.Aliases = aliases

	svc := jobListService(c.Request().Context())
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
		if req.OrderBy, err = orderby.Parse(orderBy, jobListOrderColumns); err != nil {
			return nil, err
		}
	}

	// ?namespace= narrows within the caller's allowed set; omitted or "*" does not.
	if namespace, narrow, nerr := authz.ParseNamespaceNarrow(c.QueryParam("namespace")); nerr != nil {
		return nil, nerr
	} else if narrow {
		req.Namespace = namespace
	}

	if alias := strings.TrimSpace(c.QueryParam("alias")); alias != "" {
		req.Aliases = []string{alias}
	}

	return
}

// narrowJobListAliases intersects ?alias= with the caller's job-alias
// allowlist. An empty request keeps the allowlist (or no alias predicate).
// empty is true when every requested alias is outside the allowlist.
func narrowJobListAliases(allowed, requested []string) (aliases []string, empty bool) {
	if len(requested) == 0 {
		if len(allowed) == 0 {
			return nil, false
		}
		return allowed, false
	}
	if len(allowed) == 0 {
		return requested, false
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, alias := range allowed {
		allow[alias] = struct{}{}
	}
	matched := make([]string, 0, len(requested))
	for _, alias := range requested {
		if _, ok := allow[alias]; ok {
			matched = append(matched, alias)
		}
	}
	if len(matched) == 0 {
		return nil, true
	}
	return matched, false
}

// jobListService builds the job service for a list request; a seam for the
// collection-guard test, which serves the real handler over a test database.
var jobListService = job.Service
