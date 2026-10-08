package run

import (
	"context"
	"errors"
	"net/http"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Retry retries a failed run, preserving cached/succeeded task results and
// re-executing only failed/skipped/pending tasks.
func Retry(c *echo.Context) error {
	ctx := c.Request().Context()

	jobID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	j, err := retryGetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	runEntry, err := retryGetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if runEntry.JobID != jobID {
		return echo.ErrNotFound
	}

	workCtx, releaseWork, err := runlife.FromContext(ctx).Reserve(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error()).Wrap(err)
	}
	cancelCtx, releaseCancel := job.RegisterRunCancel(workCtx, runID)
	transferred := false
	release := func() { releaseCancel(); releaseWork() }
	defer func() {
		if !transferred {
			release()
		}
	}()
	r, err := retryFromFailure(runID)
	if err != nil {
		if committed, ok := errors.AsType[*runstorage.RunCommittedError](err); ok && committed.RunID == runID && committed.JobID == jobID && runEntry.ID == runID {
			// Retry preserves durable params. The preloaded row supplies only
			// launch identity/params; execution reloads the committed task resets.
			fallback := &runstorage.JobRun{ID: runID, JobID: jobID, Status: runstorage.StatusRunning, Params: runEntry.Params, Quarantine: runEntry.Quarantine}
			retryLaunch(cancelCtx, j, fallback, release)
			transferred = true
			log.Warn("job retry committed but readback failed; executing the admitted retry",
				"job_id", jobID, "run_id", runID, "error", err)
			return c.JSON(http.StatusAccepted, fallback)
		}
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	retryLaunch(cancelCtx, j, r, release)
	transferred = true

	return c.JSON(http.StatusAccepted, r)
}

var (
	retryGetJob      = func(ctx context.Context, id uuid.UUID) (*models.Job, error) { return jsvc.Service(ctx).Get(id) }
	retryGetRun      = func(ctx context.Context, id uuid.UUID) (*runstorage.JobRun, error) { return runsvc.New(ctx).Get(id) }
	retryFromFailure = func(id uuid.UUID) (*runstorage.JobRun, error) { return runstorage.Default().RetryFromFailure(id) }
	retryLaunch      = launchWholeRunRetry
)

func launchWholeRunRetry(ctx context.Context, j *models.Job, r *runstorage.JobRun, release func()) {
	go func() {
		defer release()
		runCtx := runstorage.WithContext(ctx, r.ID)
		if err := runExecution(runCtx, j, r.Params); err != nil {
			log.Error("job retry run failure", "id", j.ID, "run_id", r.ID, "error", err)
		}
	}()
}
