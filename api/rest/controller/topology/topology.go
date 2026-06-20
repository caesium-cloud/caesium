// Package topology implements the historical DAG topology endpoints
// (data-plane-memory B2).
//
//	GET /v1/jobs/:id/topology             → latest snapshot
//	GET /v1/jobs/:id/topology?snapshot=<hash>  → snapshot by content hash
//	GET /v1/jobs/:id/topology?commit=<sha>     → snapshot by git commit
//	GET /v1/jobs/:id/topology/history          → all snapshots (newest first)
package topology

import (
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	topsvc "github.com/caesium-cloud/caesium/api/rest/service/topology"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Get handles GET /v1/jobs/:id/topology.
//
// Query parameters (mutually exclusive; first match wins):
//   - ?snapshot=<content-hash> — return the snapshot with that topology hash.
//   - ?commit=<git-sha>        — return the most-recent snapshot for that commit.
//   - (none)                   — return the latest (most-recent) snapshot.
func Get(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	// Verify the job exists before querying the snapshot table.
	if _, err = jsvc.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	svc := topsvc.Service(ctx)
	var snap interface{}

	switch {
	case c.QueryParam("snapshot") != "":
		snap, err = svc.ByContentHash(id, c.QueryParam("snapshot"))
	case c.QueryParam("commit") != "":
		snap, err = svc.ByGitCommit(id, c.QueryParam("commit"))
	default:
		snap, err = svc.Latest(id)
	}

	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, snap)
}

// History handles GET /v1/jobs/:id/topology/history.
// Returns all dag_snapshots for the job, newest first.
func History(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = jsvc.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	snaps, err := topsvc.Service(ctx).List(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"snapshots": snaps,
	})
}
