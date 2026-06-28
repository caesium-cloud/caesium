package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/auth"
	eventstore "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/datatypes"
)

type Runner func(context.Context, *models.Job, map[string]string) error

type JobLister interface {
	List(*jsvc.ListRequest) (models.Jobs, error)
}

type TriggerLister interface {
	ListByPath(string) (models.Triggers, error)
}

type acceptedHTTPTrigger struct {
	trigger     *models.Trigger
	httpTrigger *triggerhttp.HTTP
	params      map[string]string
	jobs        models.Jobs
}

// triggerFailure captures a per-trigger auth failure during the matching loop.
type triggerFailure struct {
	triggerID string
	reason    string
}

var routeWebhookEvent = func(ctx context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
	return triggerevent.DefaultRouter().Route(ctx, evt)
}

var recordWebhookReceipt = func(ctx context.Context, receipt *models.WebhookEvent) error {
	return eventstore.NewWebhookEventStore(db.Connection()).Create(ctx, receipt)
}

// ReceiveWith returns a handler that uses the given auditor for failure logging.
func ReceiveWith(auditor *auth.AuditLogger) func(*echo.Context) error {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		return ReceiveWithServices(c, triggersvc.Service(ctx), jsvc.Service(ctx), auditor, DefaultRunner)
	}
}

func ReceiveWithServices(c *echo.Context, trigSvc TriggerLister, jobSvc JobLister, auditor *auth.AuditLogger, runner Runner, opts ...triggerhttp.Option) error {
	path := normalizeHookPath(c.Param("*"))
	if !webhookRateLimiters.Allow(c.RealIP()) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "rate limit exceeded")
	}

	body, err := readWebhookBody(c.Request().Body)
	switch {
	case errors.Is(err, errRequestTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large")
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	triggers, err := trigSvc.ListByPath(path)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if len(triggers) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "no trigger registered for path")
	}

	accepted := make([]acceptedHTTPTrigger, 0, len(triggers))
	var failures []triggerFailure
	for _, trig := range triggers {
		httpTrigger, err := triggerhttp.New(trig, opts...)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}

		params, err := httpTrigger.ExtractWebhookParams(c.Request().Context(), c.Request(), body)
		switch {
		case errors.Is(err, triggerhttp.ErrInvalidSignature):
			failures = append(failures, triggerFailure{triggerID: trig.ID.String(), reason: "invalid_signature"})
			continue
		case errors.Is(err, triggerhttp.ErrReplayedRequest):
			failures = append(failures, triggerFailure{triggerID: trig.ID.String(), reason: "replayed_request"})
			continue
		case err != nil:
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}

		jobs, err := listTriggerJobs(c.Request().Context(), jobSvc, trig)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		accepted = append(accepted, acceptedHTTPTrigger{
			trigger:     trig,
			httpTrigger: httpTrigger,
			params:      params,
			jobs:        jobs,
		})
	}

	if len(accepted) == 0 {
		recordWebhookAuthFailures(path, c.RealIP(), failures, auditor)
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid signature")
	}

	// A webhook may satisfy both an HTTP trigger and one or more event triggers.
	// That is an explicit fan-out contract: the event bridge is persisted/routed
	// before HTTP jobs are launched when possible, and both trigger families may
	// start runs. The event bridge is at-least-once: retrying the same delivery
	// can route another event-trigger run.
	ingested := webhookIngestedEvent(c, path, body)
	result, err := routeWebhookEvent(c.Request().Context(), ingested)
	switch {
	case err != nil:
		log.Warn("webhook event bridge failed", "path", path, "error", err)
		metrics.EventBridgeFailuresTotal.WithLabelValues("webhook").Inc()
	case result == nil:
		log.Warn("webhook event bridge returned nil result", "path", path)
		metrics.EventBridgeFailuresTotal.WithLabelValues("webhook").Inc()
	default:
		metrics.EventsIngestedTotal.WithLabelValues("webhook").Inc()
	}

	receipt := acceptedWebhookReceipt(path, ingested.Source, accepted, result, err)
	recordWebhookReceiptAsync(c.Request().Context(), path, receipt)

	for _, acceptedTrigger := range accepted {
		launchHTTPTriggerJobs(c.Request().Context(), acceptedTrigger, runner)
	}

	return c.JSON(http.StatusAccepted, webhookReceiptResponse(receipt))
}

func FireHTTPTrigger(ctx context.Context, jobSvc JobLister, trig *models.Trigger, params map[string]string, runner Runner) error {
	httpTrigger, err := newHTTPTrigger(trig)
	if err != nil {
		return err
	}
	jobs, err := listTriggerJobs(ctx, jobSvc, trig)
	if err != nil {
		return err
	}
	launchHTTPTriggerJobs(ctx, acceptedHTTPTrigger{
		trigger:     trig,
		httpTrigger: httpTrigger,
		params:      params,
		jobs:        jobs,
	}, runner)
	return nil
}

func newHTTPTrigger(trig *models.Trigger) (*triggerhttp.HTTP, error) {
	if trig == nil {
		return nil, fmt.Errorf("trigger is required")
	}
	if trig.Type != models.TriggerTypeHTTP {
		return nil, fmt.Errorf("trigger %v is not http", trig.ID)
	}
	httpTrigger, err := triggerhttp.New(trig)
	if err != nil {
		return nil, err
	}
	return httpTrigger, nil
}

func listTriggerJobs(ctx context.Context, jobSvc JobLister, trig *models.Trigger) (models.Jobs, error) {
	if trig == nil {
		return nil, fmt.Errorf("trigger is required")
	}
	if trig.Type != models.TriggerTypeHTTP {
		return nil, fmt.Errorf("trigger %v is not http", trig.ID)
	}
	req := &jsvc.ListRequest{TriggerID: trig.ID.String()}
	jobs, err := jobSvc.List(req)
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

func launchHTTPTriggerJobs(ctx context.Context, accepted acceptedHTTPTrigger, runner Runner) {
	if runner == nil {
		runner = DefaultRunner
	}
	if accepted.httpTrigger == nil || accepted.trigger == nil {
		log.Warn("skipping malformed accepted webhook trigger")
		return
	}

	log.Info("running jobs", "count", len(accepted.jobs), "trigger_id", accepted.trigger.ID)

	for _, j := range accepted.jobs {
		if j == nil {
			log.Warn("skipping nil job", "trigger_id", accepted.trigger.ID)
			continue
		}
		if j.Paused {
			log.Info("skipping paused job", "id", j.ID)
			continue
		}

		capturedJob := j
		capturedParams := cloneStringMap(accepted.httpTrigger.MergeParams(accepted.params))
		go func() {
			runCtx := context.WithoutCancel(ctx)
			if err := runner(runCtx, capturedJob, capturedParams); err != nil {
				log.Error("job run failure", "id", capturedJob.ID, "error", err)
			}
		}()
	}
}

func DefaultRunner(ctx context.Context, j *models.Job, params map[string]string) error {
	return job.New(j, job.WithParams(params)).Run(ctx)
}

func normalizeHookPath(path string) string {
	return models.NormalizedTriggerPath(strings.TrimSpace(path))
}

func AllowRateLimit(ip string) bool {
	return webhookRateLimiters.Allow(ip)
}

func webhookIngestedEvent(c *echo.Context, path string, body []byte) *models.IngestedEvent {
	return &models.IngestedEvent{
		Type:   "webhook",
		Source: webhookEventSource(c, path),
		Data:   webhookEventData(c, path, body),
	}
}

func webhookEventSource(c *echo.Context, path string) string {
	for _, header := range []string{"X-Caesium-Event-Source", "X-Webhook-Source"} {
		if value := strings.TrimSpace(c.Request().Header.Get(header)); value != "" {
			return value
		}
	}
	return path
}

func webhookEventData(c *echo.Context, path string, body []byte) datatypes.JSON {
	if len(body) > 0 && json.Valid(body) {
		return datatypes.JSON(body)
	}
	payload := map[string]string{
		"hook_path":    path,
		"content_type": c.Request().Header.Get(echo.HeaderContentType),
		"raw_body":     string(body),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return datatypes.JSON(`{}`)
	}
	return datatypes.JSON(data)
}

func acceptedWebhookReceipt(path, source string, accepted []acceptedHTTPTrigger, result *triggerevent.RouteResult, routeErr error) *models.WebhookEvent {
	receipt := &models.WebhookEvent{
		ID:                   uuid.New(),
		Path:                 path,
		Source:               source,
		Status:               "accepted",
		HTTPTriggersAccepted: len(accepted),
		HTTPRunsStarted:      launchableWebhookJobCount(accepted),
		HTTPTriggerIDs:       stringJSONList(acceptedWebhookTriggerIDs(accepted)),
		HTTPJobIDs:           stringJSONList(acceptedWebhookJobIDs(accepted)),
	}
	if result != nil {
		receipt.EventID = result.EventID
		receipt.EventMatchedTriggers = len(result.MatchedTriggers)
		receipt.EventRunsStarted = webhookEventRunsStarted(result)
	}
	if routeErr != nil {
		receipt.Error = routeErr.Error()
	}
	return receipt
}

func recordWebhookReceiptAsync(ctx context.Context, path string, receipt *models.WebhookEvent) {
	if receipt == nil {
		return
	}

	captured := *receipt
	// Capture the recorder synchronously before launching the goroutine: it's a
	// package-level var that tests stub + restore, so reading it inside the
	// goroutine races the test's Cleanup (the -race detector flags it).
	recorder := recordWebhookReceipt
	go func() {
		recordCtx := context.WithoutCancel(ctx)
		if recordErr := recorder(recordCtx, &captured); recordErr != nil {
			log.Warn("webhook receipt log failed", "path", path, "error", recordErr)
		}
	}()
}

func webhookReceiptResponse(receipt *models.WebhookEvent) map[string]any {
	if receipt == nil {
		return map[string]any{}
	}
	resp := map[string]any{
		"receipt_id":             receipt.ID,
		"path":                   receipt.Path,
		"source":                 receipt.Source,
		"event_matched_triggers": receipt.EventMatchedTriggers,
		"event_runs_started":     receipt.EventRunsStarted,
		"http_triggers_accepted": receipt.HTTPTriggersAccepted,
		"http_runs_started":      receipt.HTTPRunsStarted,
	}
	if receipt.EventID != uuid.Nil {
		resp["event_id"] = receipt.EventID
	}
	if receipt.Error != "" {
		resp["error"] = receipt.Error
	}
	return resp
}

func acceptedWebhookTriggerIDs(accepted []acceptedHTTPTrigger) []string {
	ids := make([]string, 0, len(accepted))
	for _, item := range accepted {
		if item.trigger != nil && item.trigger.ID != uuid.Nil {
			ids = append(ids, item.trigger.ID.String())
		}
	}
	return ids
}

func acceptedWebhookJobIDs(accepted []acceptedHTTPTrigger) []string {
	ids := make([]string, 0)
	for _, item := range accepted {
		for _, j := range item.jobs {
			if j != nil && !j.Paused && j.ID != uuid.Nil {
				ids = append(ids, j.ID.String())
			}
		}
	}
	return ids
}

func launchableWebhookJobCount(accepted []acceptedHTTPTrigger) int {
	var count int
	for _, item := range accepted {
		for _, j := range item.jobs {
			if j != nil && !j.Paused {
				count++
			}
		}
	}
	return count
}

func webhookEventRunsStarted(result *triggerevent.RouteResult) int {
	if result == nil {
		return 0
	}
	var total int
	for _, match := range result.MatchedTriggers {
		total += len(match.RunsStarted)
	}
	return total
}

func stringJSONList(values []string) datatypes.JSON {
	if len(values) == 0 {
		return nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	return datatypes.JSON(raw)
}

var errRequestTooLarge = errors.New("request body too large")

func readWebhookBody(body io.Reader) ([]byte, error) {
	maxBytes := env.Variables().WebhookMaxBodySize.Int64()
	if maxBytes <= 0 {
		return io.ReadAll(body)
	}

	limited := io.LimitReader(body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errRequestTooLarge
	}
	return data, nil
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

// recordWebhookAuthFailures records metrics and audit entries for webhook auth
// failures. Only called when no trigger on the path accepted the request, so
// failures are not inflated by multi-trigger paths where one trigger succeeds.
func recordWebhookAuthFailures(path, sourceIP string, failures []triggerFailure, auditor *auth.AuditLogger) {
	recordedReasons := make(map[string]struct{})
	for _, f := range failures {
		if _, seen := recordedReasons[f.reason]; seen {
			continue
		}
		recordedReasons[f.reason] = struct{}{}

		metrics.WebhookAuthFailuresTotal.WithLabelValues(path, f.reason).Inc()

		if auditor != nil {
			if err := auditor.Log(auth.AuditEntry{
				Actor:        "webhook",
				Action:       auth.ActionWebhookDenied,
				ResourceType: "trigger",
				ResourceID:   f.triggerID,
				SourceIP:     sourceIP,
				Outcome:      auth.OutcomeDenied,
				Metadata: map[string]interface{}{
					"path":   path,
					"reason": f.reason,
				},
			}); err != nil {
				log.Warn("failed to write webhook audit log", "error", err)
			}
		}
	}
}
