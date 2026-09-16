package run

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

const (
	// defaultRunListPageSize / maxRunListPageSize mirror the ListPartitions
	// convention (see partitionPageBounds in partitions.go): a documented
	// default and ceiling instead of an unbounded query or a limit that gets
	// silently clamped.
	defaultRunListPageSize = 100
	maxRunListPageSize     = 1000

	// runListHeaderTotalCount / runListHeaderNextOffset carry the pagination
	// contract on RESPONSE HEADERS rather than in the JSON body. The body
	// must stay a bare run array: ui/src/lib/api.ts's getJobRuns decodes it
	// as JobRun[], and every test/*.go helper (fetchRuns, listJobRunSummaries,
	// tryFetchRuns, ...) decodes `/v1/jobs/:id/runs` straight into
	// []runResponse. Wrapping the body in an envelope the way ListPartitions
	// does would silently break all of them; headers add the continuation
	// contract without changing what already parses today.
	runListHeaderTotalCount = "X-Caesium-Total-Count"
	runListHeaderNextOffset = "X-Caesium-Next-Offset"
)

func List(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = jsvc.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	limit, offset, err := runListPageBounds(c.QueryParam("limit"), c.QueryParam("offset"))
	if err != nil {
		return err
	}

	runs, total, err := runsvc.New(ctx).List(id, limit, offset)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	c.Response().Header().Set(runListHeaderTotalCount, strconv.FormatInt(total, 10))
	if next := nextRunListOffset(offset, len(runs), total); next != nil {
		c.Response().Header().Set(runListHeaderNextOffset, strconv.Itoa(*next))
	}

	return c.JSON(http.StatusOK, runs)
}

// runListPageBounds parses and validates the page window the same way
// partitionPageBounds does: an unparseable or out-of-range limit is a 400
// rather than a silent fallback — a client that asked for 5000 runs and got
// 100 without being told has an incomplete view it believes is complete.
// Absent params default to defaultRunListPageSize, matching how the other
// list endpoints in this package behave when no limit is given.
func runListPageBounds(limitParam, offsetParam string) (limit, offset int, err error) {
	limit = defaultRunListPageSize
	if raw := strings.TrimSpace(limitParam); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed <= 0 || parsed > maxRunListPageSize {
			return 0, 0, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("limit must be an integer between 1 and %d", maxRunListPageSize))
		}
		limit = parsed
	}
	if raw := strings.TrimSpace(offsetParam); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 0 {
			return 0, 0, echo.NewHTTPError(http.StatusBadRequest, "offset must be a non-negative integer")
		}
		offset = parsed
	}
	return limit, offset, nil
}

// nextRunListOffset returns the offset a client should request next, or nil
// when this page is the last one — the same null-means-done contract
// nextPartitionOffset uses.
func nextRunListOffset(offset, returned int, total int64) *int {
	next := offset + returned
	if returned == 0 || int64(next) >= total {
		return nil
	}
	return &next
}
