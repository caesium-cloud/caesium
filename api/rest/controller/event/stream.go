package event

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

type Controller struct {
	bus event.Bus
}

func New(bus event.Bus) *Controller {
	return &Controller{bus: bus}
}

func (ctrl *Controller) Stream(c echo.Context) error {
	ctx := c.Request().Context()
	jobIDStr := c.QueryParam("job_id")
	runIDStr := c.QueryParam("run_id")
	typesStr := c.QueryParam("types")

	filter := event.Filter{}

	if jobIDStr != "" {
		id, err := uuid.Parse(jobIDStr)
		if err != nil {
			return echo.NewHTTPError(400, "invalid job_id")
		}
		filter.JobID = id
	}

	if runIDStr != "" {
		id, err := uuid.Parse(runIDStr)
		if err != nil {
			return echo.NewHTTPError(400, "invalid run_id")
		}
		filter.RunID = id
	}

	if typesStr != "" {
		typeStrings := strings.Split(typesStr, ",")
		for _, s := range typeStrings {
			filter.Types = append(filter.Types, event.Type(strings.TrimSpace(s)))
		}
	}

	ch, err := ctrl.bus.Subscribe(ctx, filter)
	if err != nil {
		return echo.NewHTTPError(500, err.Error())
	}

	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
	c.Response().Header().Set(echo.HeaderConnection, "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no") // Disable buffering in Nginx

	// Send a comment to keep the connection alive (and for testing connectivity)
	if _, err := fmt.Fprintf(c.Response(), ": ping\n\n"); err != nil {
		return nil
	}
	c.Response().Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := fmt.Fprintf(c.Response(), ": ping\n\n"); err != nil {
				return nil
			}
			c.Response().Flush()
		case e, ok := <-ch:
			if !ok {
				return nil
			}

			data, err := json.Marshal(e)
			if err != nil {
				c.Logger().Errorf("failed to marshal event for SSE stream: %v", err)
				continue
			}

			if _, err := fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", e.Type, data); err != nil {
				return nil
			}
			c.Response().Flush()
		}
	}
}
