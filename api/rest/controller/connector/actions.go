package connector

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	service "github.com/caesium-cloud/caesium/api/rest/service/connector"
	"github.com/caesium-cloud/caesium/internal/auth"
	core "github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/operations"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type messageRequest struct {
	BindingName    string         `json:"binding_name"`
	BindingVersion string         `json:"binding_version"`
	Input          map[string]any `json:"input"`
}
type queryRequest struct {
	BindingName    string `json:"binding_name"`
	BindingVersion string `json:"binding_version"`
}

func messagePrincipal(c *echo.Context, write bool) (*operations.OperationPrincipal, error) {
	if _, err := readPrincipal(c); err != nil {
		return nil, err
	}
	p := authmw.GetPrincipal(c)
	if write && !auth.HasRole(p.ClusterRole(), models.RoleOperator) {
		return nil, echo.ErrForbidden
	}
	out := &operations.OperationPrincipal{Kind: core.PrincipalKind(p.Kind), Subject: p.Subject, Role: string(p.ClusterRole()), Scope: append([]byte(nil), p.Scope...)}
	switch p.Kind {
	case auth.PrincipalAPIKey:
		if p.KeyID == nil || *p.KeyID == uuid.Nil {
			return nil, echo.ErrForbidden
		}
		out.StableID = p.KeyID.String()
	case auth.PrincipalUser:
		if p.UserID == nil || *p.UserID == uuid.Nil {
			return nil, echo.ErrForbidden
		}
		out.StableID = p.UserID.String()
	default:
		return nil, echo.ErrForbidden
	}
	return out, nil
}
func messageError(err error) error {
	switch {
	case errors.Is(err, operations.ErrOperationForbidden):
		return echo.ErrForbidden
	case errors.Is(err, core.ErrInvalidPayload), errors.Is(err, operations.ErrSensitiveOperation):
		return echo.NewHTTPError(http.StatusBadRequest, "invalid connector action input")
	case errors.Is(err, service.ErrMessageBinding), errors.Is(err, core.ErrInvalidOperation), errors.Is(err, core.ErrIdempotencyMismatch), errors.Is(err, operations.ErrOperationChanged), errors.Is(err, operations.ErrOperationConfirmation):
		return echo.NewHTTPError(http.StatusConflict, "connector binding, request or operation revision conflicts")
	case errors.Is(err, core.ErrRetentionLimit):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "connector operation retention budget exceeded")
	default:
		return readError(err)
	}
}
func decodeMessage(c *echo.Context, target any) error {
	req := c.Request()
	// The complete public envelope, not merely input, must fit the hard ceiling.
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), req.Body, core.MaxPageMetadataBytes))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid connector message body")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid connector message body")
	}
	return nil
}
func (s *Controller) Query(c *echo.Context) (resultErr error) {
	p, err := messagePrincipal(c, false)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	serviceCalled := false
	defer func() {
		if !serviceCalled && resultErr != nil {
			if auditErr := s.runtime.AuditRefusal(auth.ActionConnectorStatusQuery, p, c.Param("connection_id"), c.Param("execution_id"), c.Param("query_id"), resultErr); auditErr != nil {
				resultErr = messageError(auditErr)
			}
		}
	}()
	var request queryRequest
	if err := decodeMessage(c, &request); err != nil {
		return err
	}
	serviceCalled = true
	raw, err := s.runtime.Query(c.Request().Context(), p, c.Param("connection_id"), c.Param("execution_id"), c.Param("query_id"), request.BindingName, request.BindingVersion)
	if err != nil {
		return messageError(err)
	}
	return c.Blob(http.StatusOK, "application/json", raw)
}
func operationResponse(c *echo.Context, result service.OperationResult, err error, lookup bool) error {
	if err != nil && !errors.Is(err, operations.ErrOperationPending) && !errors.Is(err, operations.ErrOperationProvider) {
		return messageError(err)
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	if result.Pending && !lookup {
		status = http.StatusAccepted
	}
	if result.Conflict {
		status = http.StatusConflict
	}
	return c.JSON(status, result)
}
func (s *Controller) Submit(c *echo.Context) (resultErr error) {
	p, err := messagePrincipal(c, true)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	serviceCalled := false
	defer func() {
		if !serviceCalled && resultErr != nil {
			if auditErr := s.runtime.AuditRefusal(auth.ActionConnectorActionSubmit, p, c.Param("connection_id"), c.Param("execution_id"), c.Param("action_id"), resultErr); auditErr != nil {
				resultErr = messageError(auditErr)
			}
		}
	}()
	keys := c.Request().Header.Values("Idempotency-Key")
	if len(keys) != 1 || strings.TrimSpace(keys[0]) == "" || len(keys[0]) > 256 {
		return echo.NewHTTPError(http.StatusBadRequest, "one nonempty Idempotency-Key is required")
	}
	var request messageRequest
	if err := decodeMessage(c, &request); err != nil {
		return err
	}
	if request.Input == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "input must be a JSON object")
	}
	serviceCalled = true
	result, err := s.runtime.Submit(c.Request().Context(), p, c.Param("connection_id"), c.Param("execution_id"), c.Param("action_id"), keys[0], request.BindingName, request.BindingVersion, request.Input)
	return operationResponse(c, result, err, false)
}
func (s *Controller) Operations(c *echo.Context) error {
	p, err := readPrincipal(c)
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
	raw, err := s.runtime.Operations(c.Request().Context(), p, c.Param("connection_id"), c.Param("execution_id"), c.QueryParam("cursor"), size)
	if err != nil {
		return messageError(err)
	}
	return c.Blob(http.StatusOK, "application/json", raw)
}
func (s *Controller) Lookup(c *echo.Context) error {
	p, err := messagePrincipal(c, false)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	id, err := uuid.Parse(c.Param("operation_id"))
	if err != nil || id == uuid.Nil {
		return echo.ErrBadRequest
	}
	result, err := s.runtime.Lookup(c.Request().Context(), p, c.Param("connection_id"), id)
	return operationResponse(c, result, err, true)
}
func (s *Controller) Resubmit(c *echo.Context) (resultErr error) {
	p, err := messagePrincipal(c, true)
	if err != nil {
		return err
	}
	if s.runtime == nil {
		return readError(core.ErrEpochMismatch)
	}
	serviceCalled := false
	defer func() {
		if !serviceCalled && resultErr != nil {
			if auditErr := s.runtime.AuditRefusal(auth.ActionConnectorActionResubmit, p, c.Param("connection_id"), c.Param("execution_id"), "", resultErr); auditErr != nil {
				resultErr = messageError(auditErr)
			}
		}
	}()
	id, err := uuid.Parse(c.Param("operation_id"))
	if err != nil || id == uuid.Nil {
		return echo.ErrBadRequest
	}
	var request struct {
		Confirmed bool   `json:"confirmed"`
		Revision  uint64 `json:"revision"`
	}
	if err := decodeMessage(c, &request); err != nil {
		return err
	}
	if !request.Confirmed || request.Revision == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "explicit confirmation and receipt revision are required")
	}
	serviceCalled = true
	result, err := s.runtime.Resubmit(c.Request().Context(), p, c.Param("connection_id"), id, request.Revision, request.Confirmed)
	return operationResponse(c, result, err, false)
}
