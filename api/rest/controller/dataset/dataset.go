// Package dataset implements the dataset freshness and circuit-breaker REST surface.
package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/dataset"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Controller serves the dataset endpoints.
type Controller struct{}

// newService builds the dataset service for a request; tests swap it to
// serve the handlers over a test database.
var newService = svc.New

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

	result, err := newService(c.Request().Context()).List(params)
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
	result, err := newService(readContext(c)).GetWithOptions(namespace, name, options)
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

	result, err := newService(readContext(c)).Derivations(params)
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

	result, err := newService(c.Request().Context()).Advance(svc.AdvanceParams{
		Namespace: namespace,
		Name:      name,
		Watermark: watermark,
		Principal: authmw.GetPrincipal(c),
	})
	if err != nil {
		if mapped := writeError(err); mapped != nil {
			return mapped
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, result)
}

// writeError maps the refusals of a confined caller's dataset write (B6):
// the in-transaction producer re-check's *authz.Denial goes back unchanged,
// so the auth middleware audits it and answers 403 exactly like its own
// denials; a producer set that kept changing under the write is a 409. Any
// other error is nil here (the caller maps it).
func writeError(err error) error {
	if denial, ok := errors.AsType[*authz.Denial](err); ok {
		return denial
	}
	if errors.Is(err, svc.ErrProducersChanged) {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return nil
}

// readContext is the context a single-dataset read runs under: narrowed to
// the namespaces the caller may read when it is namespace-confined (B6), so
// the service shapes history by owner; unchanged for everyone else.
func readContext(c *echo.Context) context.Context {
	return authz.WithHistoryScope(c.Request().Context(), authmw.GetPrincipal(c))
}

// datasetPath returns the dataset identity the route addresses. It is the
// decoding the auth middleware authorises against (authmw.DatasetPathIdentity),
// so the handler acts on exactly the dataset whose owners were checked: Echo
// leaves parameters escaped when the request has a RawPath, so they are
// decoded exactly once (unconditional unescaping would confuse a literal %2F
// name with a slash), and "_" names the empty dataset namespace.
func datasetPath(c *echo.Context) (string, string) {
	return authmw.DatasetPathIdentity(c)
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
