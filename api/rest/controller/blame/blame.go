// Package blame implements the DAG blame endpoint:
//
//	GET /v1/jobs/:id/blame[?task=<name>&from=<commit>&to=<commit>]
package blame

import (
	"errors"
	"net/http"

	blamesvc "github.com/caesium-cloud/caesium/api/rest/service/blame"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	blamequery "github.com/caesium-cloud/caesium/internal/blame"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Get handles GET /v1/jobs/:id/blame[?task=<name>&from=<commit>&to=<commit>].
func Get(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	result, err := blamesvc.New(ctx).Blame(jobID, blamequery.Options{
		Task:       c.QueryParam("task"),
		FromCommit: c.QueryParam("from"),
		ToCommit:   c.QueryParam("to"),
	})
	if err != nil {
		switch {
		case errors.Is(err, blamequery.ErrCommitNotFound):
			return echo.ErrNotFound
		case errors.Is(err, blamequery.ErrInvalidRange):
			return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
		default:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
	}

	return c.JSON(http.StatusOK, result)
}
