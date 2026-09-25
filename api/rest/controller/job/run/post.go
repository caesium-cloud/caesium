package run

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/api/rest/manualparams"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// PostRequest holds the optional body for POST /v1/jobs/:id/run.
type PostRequest struct {
	Params   map[string]string `json:"params,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

const (
	// IdempotencyKeyHeader makes a run start idempotent: a retry of the same
	// request with the same key returns the original outcome instead of
	// admitting a second run.
	IdempotencyKeyHeader = "Idempotency-Key"
	// IdempotentReplayedHeader is set to "true" on a response that answers a
	// retry from the recorded outcome rather than a new admission.
	IdempotentReplayedHeader = "Idempotent-Replayed"
)

// StartedRunResponse is the 202 body when the start created a run: the run
// itself, plus outcome "created".
type StartedRunResponse struct {
	*runstorage.JobRun
	Outcome runstorage.StartOutcome `json:"outcome"`
}

// StartOutcomeResponse is the 202 body when the start was accepted but did not
// create a run: outcome is queued, skipped, or dropped (a queued start whose
// queue entry was removed before it ran; only a replayed key reports it).
type StartOutcomeResponse struct {
	Outcome runstorage.StartOutcome `json:"outcome"`
	JobID   uuid.UUID               `json:"job_id"`
	// Reason explains a skip: max_concurrency or dataset_hold.
	Reason string `json:"reason,omitempty"`
	// RunID is the terminal skipped run a dataset hold wrote.
	RunID *uuid.UUID `json:"run_id,omitempty"`
	// QueueID is the run_queue entry of a queued or dropped start.
	QueueID *uuid.UUID `json:"queue_id,omitempty"`
}

// Seams the tests replace to drive Post against a test database without
// launching containers.
var (
	postGetJob = func(ctx context.Context, id uuid.UUID) (*models.Job, error) {
		return jsvc.Service(ctx).Get(id)
	}
	postStartRun = func(ctx context.Context, jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error) {
		return runsvc.New(ctx).StartWithResult(jobID, nil, opts...)
	}
	postFindIdempotentStart = func(ctx context.Context, jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, bool, error) {
		return runsvc.New(ctx).FindIdempotentStart(jobID, opts...)
	}
	postLaunchRun = launchRun
)

func Post(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	// Parse optional request body. An empty or absent body is fine; a malformed
	// JSON body returns 400 so the caller gets a clear signal.
	var req PostRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	if err := manualparams.Validate(req.Params); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	idempotencyKey, err := runstorage.ValidateIdempotencyKey(c.Request().Header.Get(IdempotencyKeyHeader))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	startOpts := []runstorage.StartOption{
		runstorage.WithStartParams(req.Params),
		runstorage.WithStartPriority(req.Priority),
		runstorage.WithStartIdempotencyKey(idempotencyKey),
	}

	j, err := postGetJob(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	if j.Paused {
		// A retry of a start admitted before the pause still gets its
		// original answer; only a new start is refused.
		if idempotencyKey != "" {
			result, found, err := postFindIdempotentStart(ctx, j.ID, startOpts...)
			if err != nil {
				return startError(err)
			}
			if found {
				return writeStartResult(c, j.ID, result)
			}
		}
		return echo.NewHTTPError(http.StatusConflict, "job is paused")
	}

	result, err := postStartRun(ctx, j.ID, startOpts...)
	if err != nil {
		return startError(err)
	}
	if result.Outcome == runstorage.StartOutcomeCreated && result.Run != nil && !result.Replayed {
		postLaunchRun(j, result.Run)
	}
	return writeStartResult(c, j.ID, result)
}

func startError(err error) error {
	switch {
	case errors.Is(err, runstorage.ErrInvalidPriority), errors.Is(err, runstorage.ErrInvalidIdempotencyKey):
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	case errors.Is(err, runstorage.ErrIdempotencyKeyReused):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "idempotency key was already used for a different request").Wrap(err)
	case errors.Is(err, runstorage.ErrMaxConcurrentRunsReached):
		return echo.NewHTTPError(http.StatusConflict, "max concurrent runs reached").Wrap(err)
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}

// writeStartResult answers every accepted start with 202 and a body whose
// outcome field says what happened: the run itself when one was created, and a
// StartOutcomeResponse otherwise.
func writeStartResult(c *echo.Context, jobID uuid.UUID, result runstorage.StartResult) error {
	if result.Replayed {
		c.Response().Header().Set(IdempotentReplayedHeader, "true")
	}
	if result.Outcome == "" {
		return c.NoContent(http.StatusAccepted)
	}
	if result.Outcome == runstorage.StartOutcomeCreated && result.Run != nil {
		return c.JSON(http.StatusAccepted, StartedRunResponse{JobRun: result.Run, Outcome: result.Outcome})
	}
	return c.JSON(http.StatusAccepted, StartOutcomeResponse{
		Outcome: result.Outcome,
		JobID:   jobID,
		Reason:  strings.TrimSpace(result.Reason),
		RunID:   result.RunID,
		QueueID: result.QueueID,
	})
}

func launchRun(j *models.Job, r *runstorage.JobRun) {
	go func() {
		// Detached from the request context on purpose (the run outlives the
		// HTTP call), but NOT uncancellable: RegisterRunCancel makes a later
		// CancelRun / concurrency-replace reach this engine's containers.
		cancelCtx, release := job.RegisterRunCancel(context.Background(), r.ID)
		defer release()
		runCtx := runstorage.WithContext(cancelCtx, r.ID)
		if err := job.New(j, job.WithTriggerID(nil), job.WithParams(r.Params)).Run(runCtx); err != nil {
			log.Error("job run failure", "id", j.ID, "run_id", r.ID, "error", err)
		}
	}()
}
