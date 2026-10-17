package cache

import (
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func List(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	// Cache entries are history: each one lists the run that filled it, so a
	// confined caller sees only entries filled in namespaces it may read (B4).
	ctx := authz.WithHistoryScope(c.Request().Context(), authmw.GetPrincipal(c))
	store := cache.NewStore(db.Connection())
	entries, err := store.ListByJobContext(ctx, id)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	type entryResponse struct {
		Hash      string  `json:"hash"`
		TaskName  string  `json:"task_name"`
		Result    string  `json:"result"`
		RunID     string  `json:"run_id"`
		TaskRunID string  `json:"task_run_id"`
		CreatedAt string  `json:"created_at"`
		ExpiresAt *string `json:"expires_at,omitempty"`
	}

	items := make([]entryResponse, 0, len(entries))
	for _, e := range entries {
		resp := entryResponse{
			Hash:      e.Hash,
			TaskName:  e.TaskName,
			Result:    e.Result,
			RunID:     e.RunID.String(),
			TaskRunID: e.TaskRunID.String(),
			CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if e.ExpiresAt != nil {
			s := e.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
			resp.ExpiresAt = &s
		}
		items = append(items, resp)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"entries": items,
	})
}
