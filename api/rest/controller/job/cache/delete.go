package cache

import (
	"net/http"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// DeleteJob invalidates all cache entries for a job.
func DeleteJob(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	store := cache.NewStore(db.Connection())
	if err := store.InvalidateJob(id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.NoContent(http.StatusNoContent)
}

// DeleteTask invalidates cache entries for a specific task within a job.
func DeleteTask(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	taskName := c.Param("task_name")
	if taskName == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "task_name is required")
	}

	store := cache.NewStore(db.Connection())
	if err := store.Invalidate(id, taskName); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.NoContent(http.StatusNoContent)
}
