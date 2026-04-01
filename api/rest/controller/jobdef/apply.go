package jobdef

import (
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/labstack/echo/v5"
)

type ApplyRequest struct {
	Definitions []schema.Definition `json:"definitions"`
	Force       bool                `json:"force,omitempty"`
	Prune       bool                `json:"prune,omitempty"`
}

type ApplyResponse struct {
	Applied int `json:"applied"`
	Pruned  int `json:"pruned,omitempty"`
}

func Apply(c *echo.Context) error {
	var req ApplyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	if len(req.Definitions) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "definitions are required")
	}

	importer := jobdef.NewImporter(db.Connection())
	ctx := c.Request().Context()
	applied := 0
	aliases := make([]string, 0, len(req.Definitions))

	for i := range req.Definitions {
		def := &req.Definitions[i]
		if err := def.Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}

		aliases = append(aliases, def.Metadata.Alias)
		if _, err := importer.ApplyWithOptions(ctx, def, &jobdef.ApplyOptions{Force: req.Force}); err != nil {
			if errors.Is(err, jobdef.ErrDuplicateJob) || errors.Is(err, jobdef.ErrProvenanceConflict) || errors.Is(err, jobdef.ErrJobRunning) {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		applied++
	}

	pruned := 0
	if req.Prune {
		count, err := importer.PruneMissing(ctx, aliases, nil)
		if err != nil {
			if errors.Is(err, jobdef.ErrJobRunning) {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		pruned = count
	}

	return c.JSON(http.StatusOK, ApplyResponse{Applied: applied, Pruned: pruned})
}
