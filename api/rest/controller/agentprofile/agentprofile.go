// Package agentprofile is the REST surface for the AgentProfile resource
// (docs/design-agent-in-the-loop.md, agent-in-the-loop-remediation Stream
// E2). It mirrors api/rest/controller/notification's channel CRUD.
package agentprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/agentprofile"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// allowedOrderColumns is the allowlist of columns that may appear in order_by.
var allowedOrderColumns = map[string]struct{}{
	"name":       {},
	"image":      {},
	"engine":     {},
	"created_at": {},
	"updated_at": {},
}

func List(c *echo.Context) error {
	req, err := parseListRequest(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	profiles, err := svc.New(c.Request().Context()).List(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	views := make([]profileView, len(profiles))
	for i := range profiles {
		views[i] = toView(profiles[i])
	}
	return c.JSON(http.StatusOK, views)
}

func Get(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := svc.New(c.Request().Context()).Get(id)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, toView(*p))
}

func Create(c *echo.Context) error {
	req := &svc.CreateRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := svc.New(c.Request().Context()).Create(req)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusCreated, toView(*p))
}

func Update(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &svc.UpdateRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	p, err := svc.New(c.Request().Context()).Update(id, req)
	if err != nil {
		return serviceError(err)
	}
	return c.JSON(http.StatusOK, toView(*p))
}

func Delete(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if err := svc.New(c.Request().Context()).Delete(id); err != nil {
		return serviceError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// profileView is the API response for an AgentProfile. SecretRefs holds only
// secret:// URIs — never resolved values — so unlike notification channel
// config it needs no redaction (the model comment on
// internal/models.AgentProfile documents this invariant).
type profileView struct {
	ID         uuid.UUID              `json:"id"`
	Name       string                 `json:"name"`
	Image      string                 `json:"image"`
	Engine     models.AtomEngine      `json:"engine"`
	Limits     map[string]interface{} `json:"limits,omitempty"`
	SecretRefs map[string]string      `json:"secret_refs,omitempty"`
	Budgets    map[string]interface{} `json:"budgets,omitempty"`
	Playbook   map[string]interface{} `json:"playbook,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
}

func toView(p models.AgentProfile) profileView {
	view := profileView{
		ID:        p.ID,
		Name:      p.Name,
		Image:     p.Image,
		Engine:    p.Engine,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	if len(p.Limits) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(p.Limits, &m); err == nil {
			view.Limits = m
		}
	}
	if len(p.SecretRefs) > 0 {
		var m map[string]string
		if err := json.Unmarshal(p.SecretRefs, &m); err == nil {
			view.SecretRefs = m
		}
	}
	if len(p.Budgets) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(p.Budgets, &m); err == nil {
			view.Budgets = m
		}
	}
	if len(p.Playbook) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(p.Playbook, &m); err == nil {
			view.Playbook = m
		}
	}
	return view
}

func serviceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case errors.Is(err, svc.ErrProfileNameConflict):
		return echo.NewHTTPError(http.StatusConflict, "conflict").Wrap(err)
	case errors.Is(err, svc.ErrInvalidProfile):
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}

// maxListLimit caps the page size a client may request, so an oversized
// limit can't force an unbounded scan/allocation.
const maxListLimit = 1000

func parseListRequest(c *echo.Context) (*svc.ListRequest, error) {
	req := &svc.ListRequest{}

	if limit := c.QueryParam("limit"); limit != "" {
		v, err := strconv.ParseUint(limit, 10, 32)
		if err != nil {
			return nil, err
		}
		if v > maxListLimit {
			v = maxListLimit
		}
		req.Limit = v
	}

	if offset := c.QueryParam("offset"); offset != "" {
		v, err := strconv.ParseUint(offset, 10, 32)
		if err != nil {
			return nil, err
		}
		req.Offset = v
	}

	if orderBy := c.QueryParam("order_by"); orderBy != "" {
		clauses, err := parseSafeOrderBy(orderBy)
		if err != nil {
			return nil, err
		}
		req.OrderBy = clauses
	}

	return req, nil
}

// parseSafeOrderBy validates and sanitizes order_by terms against the
// allowlist, mirroring api/rest/controller/notification's implementation.
func parseSafeOrderBy(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tokens := strings.Fields(part)
		col := strings.ToLower(tokens[0])
		if _, ok := allowedOrderColumns[col]; !ok {
			return nil, fmt.Errorf("invalid order_by column: %q", tokens[0])
		}
		dir := "asc"
		if len(tokens) > 1 {
			switch strings.ToLower(tokens[1]) {
			case "asc":
				dir = "asc"
			case "desc":
				dir = "desc"
			default:
				return nil, fmt.Errorf("invalid order_by direction: %q", tokens[1])
			}
		}
		if len(tokens) > 2 {
			return nil, fmt.Errorf("invalid order_by term: %q", part)
		}
		result = append(result, col+" "+dir)
	}
	return result, nil
}
