// Package dataset implements the dataset freshness and circuit-breaker REST surface.
package dataset

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	svc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Controller serves the dataset endpoints.
type Controller struct{}

// New constructs a dataset Controller.
func New() *Controller {
	return &Controller{}
}

// List handles GET /v1/datasets.
func (ctrl *Controller) List(c *echo.Context) error {
	params := svc.ListParams{
		Status: strings.TrimSpace(c.QueryParam("status")),
	}
	if err := parsePagination(c, &params.Limit, &params.Offset); err != nil {
		return err
	}

	result, err := svc.New(c.Request().Context()).List(params)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

// Get handles GET /v1/datasets/:ns/:name.
func (ctrl *Controller) Get(c *echo.Context) error {
	namespace, name := datasetPath(c)
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request")
	}

	options := svc.GetOptions{IncludeHold: true}
	if c.QueryParams().Has("include_hold") {
		include, err := strconv.ParseBool(c.QueryParam("include_hold"))
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid include_hold")
		}
		options.IncludeHold = include
	}
	result, err := svc.New(c.Request().Context()).GetWithOptions(namespace, name, options)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

// Derivations handles GET /v1/datasets/:ns/:name/derivations.
func (ctrl *Controller) Derivations(c *echo.Context) error {
	namespace, name := datasetPath(c)
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request")
	}

	params := svc.DerivationsParams{
		Namespace: namespace,
		Name:      name,
	}
	if err := parsePagination(c, &params.Limit, &params.Offset); err != nil {
		return err
	}

	result, err := svc.New(c.Request().Context()).Derivations(params)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

type advanceRequest struct {
	Watermark string `json:"watermark"`
}

// Advance handles POST /v1/datasets/:ns/:name/advance.
func (ctrl *Controller) Advance(c *echo.Context) error {
	namespace, name := datasetPath(c)
	if name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request")
	}

	var body advanceRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	watermark := strings.TrimSpace(body.Watermark)
	if watermark == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "watermark is required")
	}

	result, err := svc.New(c.Request().Context()).Advance(svc.AdvanceParams{
		Namespace: namespace,
		Name:      name,
		Watermark: watermark,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

func datasetPath(c *echo.Context) (string, string) {
	namespace, name := c.Param("ns"), c.Param("name")
	// Echo's default router uses RawPath when present and leaves its parameters
	// escaped. Otherwise it uses the already-decoded Path. Decode exactly once:
	// unconditional unescaping would confuse a literal %2F name with a slash.
	if c.Request().URL.RawPath != "" {
		if decoded, err := url.PathUnescape(namespace); err == nil {
			namespace = decoded
		}
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
	}
	return svc.NamespaceFromPath(namespace), strings.TrimSpace(name)
}

func parsePagination(c *echo.Context, limit, offset *int) error {
	if raw := strings.TrimSpace(c.QueryParam("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid limit")
		}
		*limit = n
	}
	if raw := strings.TrimSpace(c.QueryParam("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid offset")
		}
		*offset = n
	}
	return nil
}
