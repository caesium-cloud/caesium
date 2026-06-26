package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type Controller struct {
	bus       event.Bus
	storeOnce sync.Once
	store     *event.Store
	authSvc   *iauth.Service
}

func New(bus event.Bus) *Controller {
	return &Controller{bus: bus}
}

func (ctrl *Controller) WithAuthService(svc *iauth.Service) *Controller {
	ctrl.authSvc = svc
	return ctrl
}

func (ctrl *Controller) persistentStore() *event.Store {
	ctrl.storeOnce.Do(func() {
		ctrl.store = event.NewStore(db.Connection())
	})
	return ctrl.store
}

func (ctrl *Controller) Stream(c *echo.Context) error {
	ctx := c.Request().Context()

	filter, err := ctrl.buildFilter(c)
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

	if store := ctrl.persistentStore(); store != nil {
		latestBeforeCatchup, err := store.LatestSequence(ctx)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to read event cursor").Wrap(err)
		}

		for {
			backlog, err := store.ListSince(ctx, lastSent, 500, filter)
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

func (ctrl *Controller) buildFilter(c *echo.Context) (event.Filter, error) {
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
		if err := ctrl.authorizeRunQuarantineAccess(c, id); err != nil {
			return filter, err
		}
		filter.IncludeQuarantine = true
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

func (ctrl *Controller) authorizeRunQuarantineAccess(c *echo.Context, runID uuid.UUID) error {
	principal := authmw.GetPrincipal(c)
	if principal == nil {
		// In auth-enabled mode the auth middleware rejects nil-principal requests
		// before this handler runs, so reaching here means auth is disabled (the
		// whole server is intentionally open). Defense-in-depth: if auth IS enabled,
		// still reject rather than relying solely on the middleware.
		if vars := env.Variables(); vars.AuthMode == "api-key" || vars.SSOEnabled() {
			return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
		}
		return nil
	}

	svc := ctrl.authSvc
	if svc == nil {
		svc = iauth.NewService(db.Connection())
	}
	jobAlias, err := svc.JobAliasByRunID(c.Request().Context(), runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if !iauth.CheckScope(principal.Scope, jobAlias) {
		return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	return nil
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
