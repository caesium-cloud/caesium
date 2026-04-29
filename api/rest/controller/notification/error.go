package notification

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	svc "github.com/caesium-cloud/caesium/api/rest/service/notification"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// allowedOrderColumns is the allowlist of columns that may appear in order_by.
var allowedOrderColumns = map[string]struct{}{
	"name":       {},
	"type":       {},
	"enabled":    {},
	"created_at": {},
	"updated_at": {},
}

func serviceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case errors.Is(err, svc.ErrChannelNameConflict),
		errors.Is(err, svc.ErrPolicyNameConflict):
		return echo.NewHTTPError(http.StatusConflict, "conflict").Wrap(err)
	case errors.Is(err, svc.ErrInvalidChannel),
		errors.Is(err, svc.ErrInvalidPolicy):
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}

func parseListRequest(c *echo.Context) (*svc.ListRequest, error) {
	req := &svc.ListRequest{}

	if limit := c.QueryParam("limit"); limit != "" {
		v, err := strconv.ParseUint(limit, 10, 64)
		if err != nil {
			return nil, err
		}
		req.Limit = v
	}

	if offset := c.QueryParam("offset"); offset != "" {
		v, err := strconv.ParseUint(offset, 10, 64)
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

// parseSafeOrderBy validates and sanitizes order_by terms against the allowlist.
// Accepted format per term: "column" or "column asc" or "column desc".
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
