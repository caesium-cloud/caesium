package cache

import (
	"net/http"

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

	store := cache.NewStore(db.Connection())
	entries, err := store.ListByJob(id)
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

	return c.JSON(http.StatusOK, map[string]interface{}{
		"entries": items,
	})
}
