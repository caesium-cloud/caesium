package queue

import (
	"errors"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func List(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	svc := jsvc.Service(ctx)
	j, err := svc.Get(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	// The queue read is audited; the job just loaded names its namespace, so
	// the audit row gets it without a second lookup (identity-and-access A3).
	authz.NoteNamespace(c, j.Namespace)

	// Queued requests keep the namespace they were enqueued in: a confined
	// caller sees only those of namespaces it may read (identity-and-access
	// D1), exactly as it sees a moved job's history.
	historyCtx := authz.WithHistoryScope(ctx, authmw.GetPrincipal(c))
	rows, err := jsvc.Service(historyCtx).Queue(id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, rows)
}
