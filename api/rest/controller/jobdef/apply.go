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
}

type ApplyResponse struct {
	Applied int `json:"applied"`
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

	for i := range req.Definitions {
		def := &req.Definitions[i]
		if err := def.Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}

		if _, err := importer.Apply(ctx, def); err != nil {
			if errors.Is(err, jobdef.ErrDuplicateJob) {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		applied++
	}

	return c.JSON(http.StatusOK, ApplyResponse{Applied: applied})
}
