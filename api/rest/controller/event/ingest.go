package event

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	eventsvc "github.com/caesium-cloud/caesium/api/rest/service/event"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/bodylimit"
	freshnesspkg "github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/datatypes"
)

type ingestRequest struct {
	Type   string          `json:"type"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

type ingestResponse struct {
	// EventID is unique per accepted request. Event ingestion is at-least-once:
	// retrying the same payload can create another event and route another run.
	EventID         uuid.UUID `json:"event_id"`
	MatchedTriggers int       `json:"matched_triggers"`
	RunsStarted     int       `json:"runs_started"`
	// Delivered counts the jobs the event was offered to (whatever their
	// start outcome). Skipped counts the matching triggers' jobs outside the
	// caller's scope (namespaces where it lacks runner, or aliases outside a
	// job-scoped key), and SkippedNamespaces names those of their namespaces
	// the caller may read, so a partial delivery is never silent
	// (identity-and-access A6). An unconfined caller is never skipped.
	Delivered         int      `json:"delivered"`
	Skipped           int      `json:"skipped"`
	SkippedNamespaces []string `json:"skipped_namespaces"`
}

var routeEvent = func(ctx context.Context, evt *models.IngestedEvent, opts ...triggerevent.RouteOption) (*triggerevent.RouteResult, error) {
	return triggerevent.DefaultRouter().Route(ctx, evt, opts...)
}

var observeEventArrival = func(ctx context.Context, evt *models.IngestedEvent, opts ...freshnesspkg.ObserveOption) error {
	_, err := freshnesspkg.DefaultArrivalObserver().Observe(ctx, evt, opts...)
	return err
}

var eventIngestRateLimiters = authmw.NewIPRateLimiters(15*time.Minute, eventIngestRateLimitConfig)

// Ingest accepts client-supplied events using at-least-once semantics. A
// successful response means this request was persisted and routed; repeating an
// identical request may create another event and start another run.
//
// Delivery is confined to the caller's write scope for runner
// (authmw.WriteConfinement): the namespaces where it holds runner unless it
// holds runner cluster-wide, and a job-scoped key's exact aliases. The router
// carries the scope into run admission, and the arrival observer advances only
// datasets whose producers the scope covers. The deprecated
// CAESIUM_EVENT_INGEST_API_KEY is checked by authmw.BeginLegacySecret; it
// never widens a principal's scope.
func (ctrl *Controller) Ingest(c *echo.Context) (err error) {
	if !eventIngestRateLimiters.Allow(c.RealIP()) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "rate limit exceeded")
	}
	legacy, err := authmw.BeginLegacySecret(c, ctrl.auditor, iauth.LegacyEventIngest, env.Variables().EventIngestAPIKey)
	if err != nil {
		return err
	}
	defer func() { legacy.Finish(err) }()

	principal := authmw.GetPrincipal(c)
	namespaces, aliases, err := authmw.WriteConfinement(principal, models.RoleRunner)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions").Wrap(err)
	}
	scope := jobdef.NewTenancyScope(namespaces, aliases)

	body, err := readIngestBody(c.Request().Body)
	switch {
	case errors.Is(err, errIngestRequestTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large")
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	req := &ingestRequest{}
	if err := json.Unmarshal(body, req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	data := req.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	if !json.Valid(data) {
		return echo.NewHTTPError(http.StatusBadRequest, "data must be valid json")
	}

	evt := &models.IngestedEvent{
		Type:   strings.TrimSpace(req.Type),
		Source: strings.TrimSpace(req.Source),
		Data:   datatypes.JSON(data),
	}
	if evt.Type == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "type is required")
	}

	result, err := routeEvent(c.Request().Context(), evt, triggerevent.WithDeliveryScope(scope))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if result == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error")
	}
	// The routing transaction committed: record what it reached on the
	// shared-secret audit row before anything later can fail.
	legacy.SetNamespaces(routedNamespaces(result.Delivered))
	legacy.Set("event_id", result.EventID.String())
	legacy.Set("delivered", len(result.Delivered))
	legacy.Set("runs_started", runsStarted(result))

	var observeOpts []freshnesspkg.ObserveOption
	if scope != nil {
		observeOpts = append(observeOpts, freshnesspkg.WithArrivalScope(scope.Allows))
	}
	if err := observeEventArrival(c.Request().Context(), evt, observeOpts...); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	metrics.EventsIngestedTotal.WithLabelValues("ingest").Inc()

	return c.JSON(http.StatusAccepted, ingestResponse{
		EventID:           result.EventID,
		MatchedTriggers:   len(result.MatchedTriggers),
		RunsStarted:       runsStarted(result),
		Delivered:         len(result.Delivered),
		Skipped:           len(result.Withheld),
		SkippedNamespaces: skippedNamespaces(principal, aliases, result.Withheld),
	})
}

// routedNamespaces is the sorted set of the jobs' namespaces.
func routedNamespaces(jobs []triggerevent.RoutedJob) []string {
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job.Namespace)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// skippedNamespaces names the namespaces of withheld jobs that the caller may
// read (viewer there). A job-scoped key reads only its own aliases, so it is
// told none. The result is never nil, so it always encodes as a list.
func skippedNamespaces(principal *iauth.Principal, aliases []string, withheld []triggerevent.RoutedJob) []string {
	out := []string{}
	if principal == nil || aliases != nil {
		return out
	}
	for _, namespace := range routedNamespaces(withheld) {
		if iauth.HasRole(principal.RoleIn(namespace), models.RoleViewer) {
			out = append(out, namespace)
		}
	}
	return out
}

func (ctrl *Controller) ListIngested(c *echo.Context) error {
	req, err := eventsvc.ListRequestFromValues(c.QueryParams())
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	events, err := ingestedEventService(c.Request().Context()).ListIngested(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	return c.JSON(http.StatusOK, events)
}

func eventIngestRateLimitConfig() (int, int) {
	vars := env.Variables()
	return vars.WebhookRateLimitPerMinute, vars.WebhookRateLimitBurst
}

var errIngestRequestTooLarge = bodylimit.ErrTooLarge

func readIngestBody(body io.Reader) ([]byte, error) {
	data, err := bodylimit.Read(body, env.Variables().WebhookMaxBodySize.Int64())
	if err != nil {
		return nil, err
	}
	return data, nil
}

func runsStarted(result *triggerevent.RouteResult) int {
	if result == nil {
		return 0
	}
	var total int
	for _, match := range result.MatchedTriggers {
		total += len(match.RunsStarted)
	}
	return total
}

// ingestedEventService builds the event service for a list request; a seam
// for the collection-guard test, which serves the real handler over a test
// database.
var ingestedEventService = eventsvc.New
