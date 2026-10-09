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
	"github.com/caesium-cloud/caesium/internal/runlife"
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

	j, err := backfillGetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if j.Paused {
		return echo.NewHTTPError(http.StatusConflict, "job is paused")
	}

	trigger, err := backfillGetTrigger(ctx, j.TriggerID)
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
		Namespace:     models.NamespaceOrDefault(j.Namespace),
		Status:        models.BackfillStatusRunning,
		Start:         req.Start.UTC(),
		End:           req.End.UTC(),
		MaxConcurrent: maxConcurrent,
		Reprocess:     models.ReprocessPolicy(reprocess),
	}

	workCtx, release, err := runlife.FromContext(ctx).Reserve(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error()).Wrap(err)
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	if err := backfillCreate(b); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	bCtx, cancel := context.WithCancel(workCtx)

	cancelFuncsMu.Lock()
	cancelFuncs[b.ID] = cancel
	cancelFuncsMu.Unlock()

	go func() {
		defer release()
		defer func() {
			cancelFuncsMu.Lock()
			delete(cancelFuncs, b.ID)
			cancelFuncsMu.Unlock()
			cancel()
		}()
		backfillExecute(bCtx, b, j, schedule, loc)
	}()

	transferred = true
	return c.JSON(http.StatusAccepted, b)
}

func List(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err := backfillGetJob(ctx, jobID); err != nil {
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

	if b.Status != models.BackfillStatusRunning {
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

var (
	backfillGetJob     = func(ctx context.Context, id uuid.UUID) (*models.Job, error) { return jsvc.Service(ctx).Get(id) }
	backfillGetTrigger = func(ctx context.Context, id uuid.UUID) (*models.Trigger, error) { return tsvc.Service(ctx).Get(id) }
	backfillCreate     = func(b *models.Backfill) error { return backfillstore.Default().Create(b) }
	backfillExecute    = internalJob.RunBackfill
)
