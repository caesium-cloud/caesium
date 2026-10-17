package connector

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	service "github.com/caesium-cloud/caesium/api/rest/service/connector"
	jobsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/auth"
	core "github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type Controller struct{ runtime *service.Runtime }

func New(runtime *service.Runtime) *Controller { return &Controller{runtime: runtime} }
func readPrincipal(c *echo.Context) (string, error) {
	p := authmw.GetPrincipal(c)
	if p == nil || !auth.HasRole(p.ClusterRole(), models.RoleViewer) {
		return "", echo.ErrForbidden
	}
	// A persisted agent marker never grants access to connector surfaces, even
	// if its claim is malformed or lacks the incident UUID required at minting.
	var scope struct {
		Agent json.RawMessage `json:"agent"`
	}
	if len(p.Scope) > 0 {
		if json.Unmarshal(p.Scope, &scope) != nil || (len(scope.Agent) > 0 && string(scope.Agent) != "null") {
			return "", echo.ErrForbidden
		}
	}
	claim, err := auth.DecodeAgentClaim(p.Scope)
	if err != nil || claim != nil {
		return "", echo.ErrForbidden
	}
	scoped, err := auth.IsScoped(p.Scope)
	if err != nil || scoped {
		return "", echo.ErrForbidden
	}
	if p.KeyID != nil {
		return string(p.Kind) + ":" + p.KeyID.String(), nil
	}
	if p.UserID != nil {
		return string(p.Kind) + ":" + p.UserID.String(), nil
	}
	return "", echo.ErrForbidden
}
func readError(err error) error {
	switch {
	case errors.Is(err, core.ErrReadBudget), errors.Is(err, core.ErrBusy):
		return echo.NewHTTPError(http.StatusTooManyRequests, "connector read budget exhausted")
	case errors.Is(err, core.ErrInvalidReference), errors.Is(err, service.ErrInvalidRequest):
		return echo.NewHTTPError(http.StatusBadRequest, "invalid connector read request")
	case errors.Is(err, core.ErrNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case errors.Is(err, core.ErrPayloadTooLarge):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "connector metadata budget exceeded")
	default:
		return echo.NewHTTPError(http.StatusServiceUnavailable, "connector unavailable")
	}
}
func pageSize(c *echo.Context) (int, error) {
	value := c.QueryParam("page_size")
	if value == "" {
		return 0, nil
	}
	size, err := strconv.Atoi(value)
	if err != nil || size < 1 {
		return 0, service.ErrInvalidRequest
	}
	return size, nil
}
func (s *Controller) Connections(c *echo.Context) error {
	principal, err := readPrincipal(c)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	out, err := s.runtime.Connections(c.Request().Context(), principal)
	if err != nil {
		return readError(err)
	}
	return c.JSON(http.StatusOK, out)
}
func (s *Controller) List(c *echo.Context) error {
	principal, err := readPrincipal(c)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	size, err := pageSize(c)
	if err != nil {
		return readError(err)
	}
	raw, err := s.runtime.List(c.Request().Context(), principal, c.Param("connection_id"), c.QueryParam("workflow_type"), c.QueryParam("cursor"), size)
	if err != nil {
		return readError(err)
	}
	return c.Blob(http.StatusOK, "application/json", raw)
}
func (s *Controller) Detail(c *echo.Context) error {
	principal, err := readPrincipal(c)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	raw, err := s.runtime.Detail(c.Request().Context(), principal, c.Param("connection_id"), c.Param("execution_id"))
	if err != nil {
		return readError(err)
	}
	return c.Blob(http.StatusOK, "application/json", raw)
}
func (s *Controller) History(c *echo.Context) error {
	principal, err := readPrincipal(c)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	size, err := pageSize(c)
	if err != nil {
		return readError(err)
	}
	raw, err := s.runtime.History(c.Request().Context(), principal, c.Param("connection_id"), c.Param("execution_id"), c.QueryParam("cursor"), size)
	if err != nil {
		return readError(err)
	}
	return c.Blob(http.StatusOK, "application/json", raw)
}
func (s *Controller) Relationships(c *echo.Context) error {
	principal, err := readPrincipal(c)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	size, err := pageSize(c)
	if err != nil {
		return readError(err)
	}
	raw, err := s.runtime.Relationships(c.Request().Context(), principal, c.Param("connection_id"), c.Param("execution_id"), c.QueryParam("cursor"), size)
	if err != nil {
		return readError(err)
	}
	return c.Blob(http.StatusOK, "application/json", raw)
}

// RunLinks retains ordinary job/run scope. A scoped caller sees no external
// identity, provider, payload, or link, even when the local run is authorized.
func (s *Controller) RunLinks(c *echo.Context) error {
	ctx := c.Request().Context()
	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.ErrBadRequest
	}
	if _, err := jobsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return readError(err)
	}
	run, err := runsvc.New(ctx).Get(runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return readError(err)
	}
	if run.JobID != jobID {
		return echo.ErrNotFound
	}
	principal, permission := readPrincipal(c)
	if permission != nil {
		return c.JSON(http.StatusOK, map[string]any{"executions": []any{}})
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	rows, err := s.runtime.RunLinks(ctx, principal, jobID, runID)
	if err != nil {
		return readError(err)
	}
	if rows == nil {
		rows = []service.Correlation{}
	}
	return c.JSON(http.StatusOK, map[string]any{"executions": rows})
}
