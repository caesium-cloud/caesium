package job

import (
	"errors"
	"net/http"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/labstack/echo/v5"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// ManifestContentType is the media type of the exported manifest. It matches
// what `caesium job apply` consumes from disk, so the response body can be
// written straight to a `.job.yaml` file and re-applied.
const ManifestContentType = "application/yaml"

// Manifest reconstructs a live job's authoring manifest and serialises it.
//
// YAML is the default because the manifest's purpose is to be committed and
// re-applied; `?format=json` returns the same pkg/jobdef.Definition as JSON for
// callers that would otherwise parse YAML in the browser. The reconstruction
// itself lives in internal/jobdef (Exporter), the inverse of the Importer that
// `POST /v1/jobdefs/apply` drives, so the two field mappings sit side by side.
func Manifest(c *echo.Context) error {
	ctx := c.Request().Context()

	format := strings.ToLower(strings.TrimSpace(c.QueryParam("format")))
	switch format {
	case "", "yaml", "yml", "json":
	default:
		return echo.NewHTTPError(http.StatusBadRequest, `format must be one of ["yaml","json"]`)
	}

	j, err := jsvc.Service(ctx).GetByIDPrefix(c.Param("id"))
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return echo.ErrNotFound
		case errors.Is(err, jsvc.ErrInvalidJobIDPrefix):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
		case errors.Is(err, jsvc.ErrAmbiguousJobIDPrefix):
			return echo.NewHTTPError(http.StatusConflict, err.Error()).Wrap(err)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	def, err := internaljobdef.NewExporter(db.Connection()).Export(ctx, j.ID)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return echo.ErrNotFound
		case errors.Is(err, internaljobdef.ErrJobChangedDuringExport):
			// A concurrent apply kept landing mid-read. Serving a manifest
			// stitched from two revisions would be worse than telling the
			// caller to ask again, so this is a retryable 409, not a 500.
			return echo.NewHTTPError(http.StatusConflict, err.Error()).Wrap(err)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if format == "json" {
		return c.JSON(http.StatusOK, def)
	}

	encoded, err := yaml.Marshal(def)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.Blob(http.StatusOK, ManifestContentType, encoded)
}
