package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
)

type Runner func(context.Context, *models.Job, map[string]string) error

type JobLister interface {
	List(*jsvc.ListRequest) (models.Jobs, error)
}

type TriggerLister interface {
	ListByPath(string) (models.Triggers, error)
}

func Receive(c *echo.Context) error {
	ctx := c.Request().Context()
	return ReceiveWithServices(c, triggersvc.Service(ctx), jsvc.Service(ctx), DefaultRunner)
}

func ReceiveWithServices(c *echo.Context, trigSvc TriggerLister, jobSvc JobLister, runner Runner) error {
	path := normalizeHookPath(c.Param("*"))

	triggers, err := trigSvc.ListByPath(path)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if len(triggers) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "no trigger registered for path")
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	var accepted int
	for _, trig := range triggers {
		httpTrigger, err := triggerhttp.New(trig)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}

		params, err := httpTrigger.ExtractWebhookParams(c.Request().Context(), c.Request(), body)
		switch {
		case errors.Is(err, triggerhttp.ErrInvalidSignature):
			continue
		case err != nil:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}

		if err := FireHTTPTrigger(c.Request().Context(), jobSvc, trig, params, runner); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		accepted++
	}

	if accepted == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid signature")
	}

	return c.JSON(http.StatusAccepted, nil)
}

func FireHTTPTrigger(ctx context.Context, jobSvc JobLister, trig *models.Trigger, params map[string]string, runner Runner) error {
	if trig == nil {
		return fmt.Errorf("trigger is required")
	}
	if trig.Type != models.TriggerTypeHTTP {
		return fmt.Errorf("trigger %v is not http", trig.ID)
	}
	if runner == nil {
		runner = DefaultRunner
	}
	httpTrigger, err := triggerhttp.New(trig)
	if err != nil {
		return err
	}

	req := &jsvc.ListRequest{TriggerID: trig.ID.String()}
	jobs, err := jobSvc.List(req)
	if err != nil {
		return err
	}

	log.Info("running jobs", "count", len(jobs), "trigger_id", trig.ID)

	for _, j := range jobs {
		if j.Paused {
			log.Info("skipping paused job", "id", j.ID)
			continue
		}

		capturedJob := j
		capturedParams := cloneStringMap(httpTrigger.MergeParams(params))
		go func() {
			runCtx := context.WithoutCancel(ctx)
			if err := runner(runCtx, capturedJob, capturedParams); err != nil {
				log.Error("job run failure", "id", capturedJob.ID, "error", err)
			}
		}()
	}

	return nil
}

func DefaultRunner(ctx context.Context, j *models.Job, params map[string]string) error {
	return job.New(j, job.WithParams(params)).Run(ctx)
}

func normalizeHookPath(path string) string {
	return models.NormalizedTriggerPath(strings.TrimSpace(path))
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
