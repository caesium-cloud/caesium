package notification

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/caesium-cloud/caesium/api/rest/controller/internal/orderby"
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
		clauses, err := orderby.Parse(orderBy, allowedOrderColumns)
		if err != nil {
			return nil, err
		}
		req.OrderBy = clauses
	}

	return req, nil
}

// parseSafeOrderBy keeps the package-local call surface while delegating the
// grammar to the shared parser.
func parseSafeOrderBy(raw string) ([]string, error) {
	return orderby.Parse(raw, allowedOrderColumns)
}
