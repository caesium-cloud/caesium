package auth

import (
	"net/http"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/labstack/echo/v5"
)

func QueryAudit(c *echo.Context) error {
	req := &auth.AuditQueryRequest{
		Actor:  c.QueryParam("actor"),
		Action: c.QueryParam("action"),
	}

	if since := c.QueryParam("since"); since != "" {
		dur, err := parseDuration(since)
		if err != nil {
			// Try parsing as RFC3339.
			t, err2 := time.Parse(time.RFC3339, since)
			if err2 != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "invalid since parameter").Wrap(err)
			}
			req.Since = &t
		} else {
			t := time.Now().UTC().Add(-dur)
			req.Since = &t
		}
	}

	if until := c.QueryParam("until"); until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid until parameter").Wrap(err)
		}
		req.Until = &t
	}

	if limitStr := c.QueryParam("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid limit").Wrap(err)
		}
		req.Limit = limit
	}

	if offsetStr := c.QueryParam("offset"); offsetStr != "" {
		offset, err := strconv.Atoi(offsetStr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid offset").Wrap(err)
		}
		req.Offset = offset
	}

	entries, err := Dependencies.Auditor.Query(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to query audit log").Wrap(err)
	}

	return c.JSON(http.StatusOK, entries)
}
