package notification

import (
	"net/http"

	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func ListPolicies(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	policies, err := svc.New(c.Request().Context()).ListPolicies(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, policies)
}

func GetPolicy(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := svc.New(c.Request().Context()).GetPolicy(id)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, p)
}

func CreatePolicy(c *echo.Context) error {
	req := &svc.CreatePolicyRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := svc.New(c.Request().Context()).CreatePolicy(req)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusCreated, p)
}

func UpdatePolicy(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &svc.UpdatePolicyRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := svc.New(c.Request().Context()).UpdatePolicy(id, req)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, p)
}

func DeletePolicy(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if err := svc.New(c.Request().Context()).DeletePolicy(id); err != nil {
		return serviceError(err)
	}
	return c.NoContent(http.StatusNoContent)
}
