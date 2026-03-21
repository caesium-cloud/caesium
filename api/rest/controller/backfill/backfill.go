package backfill

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	backfillstore "github.com/caesium-cloud/caesium/internal/backfill"
	internalJob "github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	croncfg "github.com/caesium-cloud/caesium/internal/trigger/cron"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// PostRequest is the body for POST /v1/jobs/:id/backfill.
type PostRequest struct {
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	MaxConcurrent int       `json:"max_concurrent,omitempty"`
	Reprocess     string    `json:"reprocess,omitempty"`
}

// cancelFuncs stores in-memory cancel functions for same-instance wakeups.
// Cross-instance cancellation is coordinated through the backfill row in the
// database so any replica can request cancellation safely.
var (
	cancelFuncsMu sync.Mutex
	cancelFuncs   = make(map[uuid.UUID]context.CancelFunc)
)

func Post(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	var req PostRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if req.Start.IsZero() || req.End.IsZero() {
		return echo.NewHTTPError(http.StatusBadRequest, "start and end are required")
	}

	if !req.End.After(req.Start) {
		return echo.NewHTTPError(http.StatusBadRequest, "end must be after start")
	}

	j, err := jsvc.Service(ctx).Get(jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if j.Paused {
		return echo.NewHTTPError(http.StatusConflict, "job is paused")
	}

	trigger, err := tsvc.Service(ctx).Get(j.TriggerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "job trigger not found")
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if trigger.Type != models.TriggerTypeCron {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "backfill requires a cron trigger")
	}

	schedule, loc, err := croncfg.ParseSchedule(trigger.Configuration)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid cron expression in trigger").Wrap(err)
	}

	reprocess := req.Reprocess
	if reprocess == "" {
		reprocess = string(models.ReprocessNone)
	}
	switch models.ReprocessPolicy(reprocess) {
	case models.ReprocessNone, models.ReprocessFailed, models.ReprocessAll:
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "reprocess must be one of: none, failed, all")
	}

	maxConcurrent := req.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	b := &models.Backfill{
		ID:            uuid.New(),
		JobID:         jobID,
		Status:        string(models.BackfillStatusRunning),
		Start:         req.Start.UTC(),
		End:           req.End.UTC(),
		MaxConcurrent: maxConcurrent,
		Reprocess:     reprocess,
	}

	if err := backfillstore.Default().Create(b); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	bCtx, cancel := context.WithCancel(context.Background())

	cancelFuncsMu.Lock()
	cancelFuncs[b.ID] = cancel
	cancelFuncsMu.Unlock()

	go func() {
		defer func() {
			cancelFuncsMu.Lock()
			delete(cancelFuncs, b.ID)
			cancelFuncsMu.Unlock()
			cancel()
		}()
		internalJob.RunBackfill(bCtx, b, j, schedule, loc)
	}()

	return c.JSON(http.StatusAccepted, b)
}

func List(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err := jsvc.Service(ctx).Get(jobID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	backfills, err := backfillstore.Default().List(jobID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, backfills)
}

func Get(c *echo.Context) error {
	backfillID, err := uuid.Parse(c.Param("backfill_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	b, err := backfillstore.Default().Get(backfillID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, b)
}

func Cancel(c *echo.Context) error {
	backfillID, err := uuid.Parse(c.Param("backfill_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	b, err := backfillstore.Default().Get(backfillID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if b.Status != string(models.BackfillStatusRunning) {
		return echo.NewHTTPError(http.StatusConflict, "backfill is not running")
	}

	cancelFuncsMu.Lock()
	cancel, ok := cancelFuncs[backfillID]
	cancelFuncsMu.Unlock()

	if err := backfillstore.Default().RequestCancel(backfillID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if ok {
		cancel()
	}

	updated, err := backfillstore.Default().Get(backfillID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusOK, updated)
}
