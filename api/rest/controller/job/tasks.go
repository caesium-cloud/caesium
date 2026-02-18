package job

import (
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Tasks(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = job.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	tasks, err := task.Service(ctx).List(&task.ListRequest{
		JobID:   id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, tasks)
}
