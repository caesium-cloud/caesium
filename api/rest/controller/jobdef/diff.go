package jobdef

import (
	"net/http"

	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/labstack/echo/v5"
)

type DiffRequest struct {
	Definitions []schema.Definition `json:"definitions"`
}

type DiffResponse struct {
	Added    []jobdiff.JobSpec `json:"added"`
	Removed  []jobdiff.JobSpec `json:"removed"`
	Modified []jobdiff.Update  `json:"modified"`
}

func Diff(c *echo.Context) error {
	var req DiffRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	desired := make(map[string]jobdiff.JobSpec)
	for i := range req.Definitions {
		def := &req.Definitions[i]
		if err := def.Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		desired[def.Metadata.Alias] = jobdiff.FromDefinition(def)
	}

	ctx := c.Request().Context()
	specs, err := jobdiff.LoadDatabaseSpecs(ctx, db.Connection())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load database specs").Wrap(err)
	}

	result := jobdiff.Compare(desired, specs)

	// ensure non-nil slices for JSON serialization
	added := result.Creates
	if added == nil {
		added = []jobdiff.JobSpec{}
	}
	removed := result.Deletes
	if removed == nil {
		removed = []jobdiff.JobSpec{}
	}
	modified := result.Updates
	if modified == nil {
		modified = []jobdiff.Update{}
	}

	resp := DiffResponse{
		Added:    added,
		Removed:  removed,
		Modified: modified,
	}

	return c.JSON(http.StatusOK, resp)
}
