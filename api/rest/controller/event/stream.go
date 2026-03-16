package event

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type Controller struct {
	bus   event.Bus
	store *event.Store
}

func New(bus event.Bus) *Controller {
	return &Controller{
		bus:   bus,
		store: event.NewStore(db.Connection()),
	}
}

func (ctrl *Controller) Stream(c *echo.Context) error {
	ctx := c.Request().Context()

	filter, err := buildFilter(c)
	if err != nil {
		return err
	}

	var live <-chan event.Event
	if ctrl.bus != nil {
		live, err = ctrl.bus.Subscribe(ctx, filter)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}

	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
	c.Response().Header().Set(echo.HeaderConnection, "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no")

	flusher, ok := c.Response().(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming not supported")
	}

	lastSent, err := parseLastSequence(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid Last-Event-ID")
	}

	if _, err := fmt.Fprintf(c.Response(), ": ping\n\n"); err != nil {
		return nil
	}
	flusher.Flush()

	if ctrl.store != nil {
		latestBeforeCatchup, err := ctrl.store.LatestSequence(ctx)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to read event cursor").Wrap(err)
		}

		for {
			backlog, err := ctrl.store.ListSince(ctx, lastSent, 500, filter)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "failed to read event backlog").Wrap(err)
			}
			if len(backlog) == 0 {
				break
			}

			for _, evt := range backlog {
				if evt.Sequence == 0 || evt.Sequence > latestBeforeCatchup || evt.Sequence <= lastSent {
					continue
				}
				if err := writeEvent(c, evt); err != nil {
					return nil
				}
				flusher.Flush()
				lastSent = evt.Sequence
			}

			if lastSent >= latestBeforeCatchup {
				break
			}
		}
	}

	// Use a fixed high-water mark for deduplication between backlog and
	// live events.  Do NOT advance it from live events — events may arrive
	// on the bus out-of-sequence-order when publishers are delayed (e.g.
	// run_started commits at seq N but reaches the bus after task_started
	// at seq N+2).  Advancing lastSent from live events would incorrectly
	// discard the lower-sequenced event.
	catchupHighWater := lastSent

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
			flusher.Flush()
		case evt, ok := <-live:
			if !ok {
				return nil
			}
			if evt.Sequence > 0 && evt.Sequence <= catchupHighWater {
				continue
			}
			if err := writeEvent(c, evt); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func buildFilter(c *echo.Context) (event.Filter, error) {
	filter := event.Filter{}

	if jobIDStr := c.QueryParam("job_id"); jobIDStr != "" {
		id, err := uuid.Parse(jobIDStr)
		if err != nil {
			return filter, echo.NewHTTPError(http.StatusBadRequest, "invalid job_id")
		}
		filter.JobID = id
	}

	if runIDStr := c.QueryParam("run_id"); runIDStr != "" {
		id, err := uuid.Parse(runIDStr)
		if err != nil {
			return filter, echo.NewHTTPError(http.StatusBadRequest, "invalid run_id")
		}
		filter.RunID = id
	}

	if typesStr := c.QueryParam("types"); typesStr != "" {
		for _, part := range strings.Split(typesStr, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			filter.Types = append(filter.Types, event.Type(part))
		}
	}

	return filter, nil
}

func parseLastSequence(c *echo.Context) (uint64, error) {
	value := strings.TrimSpace(c.Request().Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(c.QueryParam("cursor"))
	}
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func writeEvent(c *echo.Context, evt event.Event) error {
	data, err := json.Marshal(evt)
	if err != nil {
		c.Logger().Error("failed to marshal event for SSE stream", "error", err)
		return nil
	}

	if evt.Sequence > 0 {
		if _, err := fmt.Fprintf(c.Response(), "id: %d\n", evt.Sequence); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", evt.Type, data)
	return err
}
