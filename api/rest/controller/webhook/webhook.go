package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/bodylimit"
	eventstore "github.com/caesium-cloud/caesium/internal/event"
	freshnesspkg "github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/runlife"
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

const webhookReceiptRecordTimeout = 10 * time.Second

// triggerFailure captures a per-trigger auth failure during the matching loop.
type triggerFailure struct {
	triggerID string
	reason    string
}

var routeWebhookEvent = func(ctx context.Context, evt *models.IngestedEvent) (*triggerevent.RouteResult, error) {
	return triggerevent.DefaultRouter().Route(ctx, evt)
}

var observeWebhookArrival = func(ctx context.Context, evt *models.IngestedEvent) error {
	_, err := freshnesspkg.DefaultArrivalObserver().Observe(ctx, evt)
	return err
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

	// Admit receipt persistence and every HTTP job before the event bridge can
	// commit any durable work. Request cancellation does not end these contexts.
	receiptCtx, releaseReceipt, err := runlife.FromContext(c.Request().Context()).Reserve(c.Request().Context())
	if err != nil {
		return webhookAdmissionError(path, err)
	}
	receiptTransferred := false
	defer func() {
		if !receiptTransferred {
			releaseReceipt()
		}
	}()
	var launches []func() int
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for _, acceptedTrigger := range accepted {
		launch, release, err := reserveHTTPTriggerJobs(c.Request().Context(), acceptedTrigger, runner)
		if err != nil {
			return webhookAdmissionError(path, err)
		}
		launches = append(launches, launch)
		releases = append(releases, release)
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
		if err := observeWebhookArrival(c.Request().Context(), ingested); err != nil {
			log.Warn("webhook arrival observer failed", "path", path, "event_id", ingested.ID, "error", err)
		}
	}

	httpRunsStarted := 0
	for _, launch := range launches {
		httpRunsStarted += launch()
	}
	releases = nil

	receipt := acceptedWebhookReceipt(path, ingested.Source, accepted, httpRunsStarted, result, err)
	recordWebhookReceiptAsync(receiptCtx, path, receipt, releaseReceipt)
	receiptTransferred = true

	return c.JSON(http.StatusAccepted, webhookReceiptResponse(receipt))
}

func webhookAdmissionError(path string, err error) error {
	if errors.Is(err, runlife.ErrMissing) {
		log.Error("webhook run supervisor wiring is missing", "path", path, "error", err)
	}
	return echo.NewHTTPError(http.StatusServiceUnavailable, "service unavailable").Wrap(err)
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
	launch, release, err := reserveHTTPTriggerJobs(ctx, acceptedHTTPTrigger{
		trigger:     trig,
		httpTrigger: httpTrigger,
		params:      params,
		jobs:        jobs,
	}, runner)
	if err != nil {
		return err
	}
	defer release()
	launch()
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

func reserveHTTPTriggerJobs(ctx context.Context, accepted acceptedHTTPTrigger, runner Runner) (func() int, func(), error) {
	if runner == nil {
		runner = DefaultRunner
	}
	if accepted.httpTrigger == nil || accepted.trigger == nil {
		return func() int { return 0 }, func() {}, nil
	}
	var launches []func()
	var releases []func()
	releaseAll := func() {
		for _, release := range releases {
			release()
		}
	}
	for _, j := range accepted.jobs {
		if j == nil || j.Paused {
			continue
		}
		workCtx, release, err := runlife.FromContext(ctx).Reserve(ctx)
		if err != nil {
			releaseAll()
			return nil, nil, err
		}
		capturedJob := j
		capturedParams := cloneStringMap(accepted.httpTrigger.MergeParams(accepted.params))
		releases = append(releases, release)
		launches = append(launches, func() {
			go func() {
				defer release()
				if err := runner(workCtx, capturedJob, capturedParams); err != nil {
					log.Error("job run failure", "id", capturedJob.ID, "error", err)
				}
			}()
		})
	}
	launched := false
	cleanup := func() {
		if !launched {
			releaseAll()
		}
	}
	return func() int {
		launched = true
		for _, launch := range launches {
			launch()
		}
		return len(launches)
	}, cleanup, nil
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

func acceptedWebhookReceipt(path, source string, accepted []acceptedHTTPTrigger, httpRunsStarted int, result *triggerevent.RouteResult, routeErr error) *models.WebhookEvent {
	receipt := &models.WebhookEvent{
		ID:                   uuid.New(),
		Path:                 path,
		Source:               source,
		Status:               "accepted",
		HTTPTriggersAccepted: len(accepted),
		HTTPRunsStarted:      httpRunsStarted,
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

func recordWebhookReceiptAsync(ctx context.Context, path string, receipt *models.WebhookEvent, release func()) {
	if receipt == nil {
		release()
		return
	}
	captured := *receipt
	// Capture the recorder synchronously before launching the goroutine: it's a
	// package-level var that tests stub + restore, so reading it inside the
	// goroutine races the test's Cleanup (the -race detector flags it).
	recorder := recordWebhookReceipt
	go func() {
		defer release()
		recordCtx, cancel := context.WithTimeout(ctx, webhookReceiptRecordTimeout)
		defer cancel()
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

var errRequestTooLarge = bodylimit.ErrTooLarge

func readWebhookBody(body io.Reader) ([]byte, error) {
	data, err := bodylimit.Read(body, env.Variables().WebhookMaxBodySize.Int64())
	if err != nil {
		return nil, err
	}
	return data, nil
}

func cloneStringMap[K comparable, V any](in map[K]V) map[K]V {
	if len(in) == 0 {
		return nil
	}
	return maps.Clone(in)
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
				Metadata: map[string]any{
					"path":   path,
					"reason": f.reason,
				},
			}); err != nil {
				log.Warn("failed to write webhook audit log", "error", err)
			}
		}
	}
}
