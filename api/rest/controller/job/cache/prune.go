package cache

import (
	"net/http"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/labstack/echo/v5"
)

// Prune removes expired cache entries across all jobs.
func Prune(c *echo.Context) error {
	store := cache.NewStore(db.Connection())
	count, err := store.Prune()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"pruned": count,
	})
}
