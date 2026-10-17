package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/streaming"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type Controller struct {
	bus       event.Bus
	storeOnce sync.Once
	store     *event.Store
	authSvc   *iauth.Service
}

func New(bus event.Bus) *Controller {
	return &Controller{bus: bus}
}

func (ctrl *Controller) WithAuthService(svc *iauth.Service) *Controller {
	ctrl.authSvc = svc
	return ctrl
}

func (ctrl *Controller) persistentStore() *event.Store {
	ctrl.storeOnce.Do(func() {
		ctrl.store = event.NewStore(db.Connection())
	})
	return ctrl.store
}

func (ctrl *Controller) Stream(c *echo.Context) error {
	ctx := c.Request().Context()

	filter, err := ctrl.buildFilter(c)
	if err != nil {
		return err
	}

	// A namespace-confined subscriber (identity-and-access B4) receives only
	// events of allow-listed types that belong to a run persisted in one of
	// its namespaces: filtered in SQL for the catch-up and per event, on the
	// server, for the live feed. Events naming no run are cluster-only.
	restricted := authz.Restricted(ctx)
	visible := true
	if restricted {
		authz.MarkFiltered(c)
		filter.Types, visible = restrictedStreamFilterTypes(filter.Types)
	}
	runNamespaces := newRunNamespaceCache()

	var live <-chan event.Event
	if ctrl.bus != nil && visible {
		live, err = ctrl.bus.Subscribe(ctx, filter)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}

	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
	c.Response().Header().Set(echo.HeaderConnection, "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no")

	flusher, ok := c.Response().(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming not supported")
	}

	lastSent, err := parseLastSequence(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid Last-Event-ID")
	}

	if _, err := fmt.Fprintf(streaming.Writer(c.Response()), ": ping\n\n"); err != nil {
		return nil
	}
	flusher.Flush()

	if store := ctrl.persistentStore(); store != nil && visible {
		latestBeforeCatchup, err := store.LatestSequence(ctx)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to read event cursor").Wrap(err)
		}
		if streamCatchupCaptured != nil {
			streamCatchupCaptured()
		}

		// The catch-up scans (cursor, latestBeforeCatchup] only. Bounding the
		// query (instead of skipping newer rows client-side) keeps it making
		// progress when the newest events before the mark are filtered out:
		// otherwise a newer matching event would be returned, skipped and
		// re-read forever.
		cursor := lastSent
		for cursor < latestBeforeCatchup {
			backlog, err := store.ListRange(ctx, cursor, latestBeforeCatchup, 500, filter, restricted)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "failed to read event backlog").Wrap(err)
			}
			if len(backlog) == 0 {
				break
			}

			for _, evt := range backlog {
				if evt.Sequence > cursor {
					cursor = evt.Sequence
				}
				if evt.Sequence == 0 || evt.Sequence > latestBeforeCatchup || evt.Sequence <= lastSent {
					continue
				}
				if restricted {
					evt = redactStreamPayload(evt)
				}
				if err := writeEvent(c, evt); err != nil {
					return nil
				}
				flusher.Flush()
				lastSent = evt.Sequence
			}

			if lastSent >= latestBeforeCatchup {
				break
			}
		}
	}

	// Use a fixed high-water mark for deduplication between backlog and
	// live events.  Do NOT advance it from live events — events may arrive
	// on the bus out-of-sequence-order when publishers are delayed (e.g.
	// run_started commits at seq N but reaches the bus after task_started
	// at seq N+2).  Advancing lastSent from live events would incorrectly
	// discard the lower-sequenced event.
	catchupHighWater := lastSent

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := fmt.Fprintf(streaming.Writer(c.Response()), ": ping\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case evt, ok := <-live:
			if !ok {
				return nil
			}
			if evt.Sequence > 0 && evt.Sequence <= catchupHighWater {
				continue
			}
			if restricted {
				if !ctrl.liveEventVisible(ctx, evt, runNamespaces) {
					continue
				}
				evt = redactStreamPayload(evt)
			}
			if err := writeEvent(c, evt); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func (ctrl *Controller) buildFilter(c *echo.Context) (event.Filter, error) {
	filter := event.Filter{}

	if jobIDStr := c.QueryParam("job_id"); jobIDStr != "" {
		id, err := uuid.Parse(jobIDStr)
		if err != nil {
			return filter, echo.NewHTTPError(http.StatusBadRequest, "invalid job_id")
		}
		filter.JobID = id
	}

	if runIDStr := c.QueryParam("run_id"); runIDStr != "" {
		id, err := uuid.Parse(runIDStr)
		if err != nil {
			return filter, echo.NewHTTPError(http.StatusBadRequest, "invalid run_id")
		}
		filter.RunID = id
		if err := ctrl.authorizeRunQuarantineAccess(c, id); err != nil {
			return filter, err
		}
		filter.IncludeQuarantine = true
	}

	if typesStr := c.QueryParam("types"); typesStr != "" {
		for part := range strings.SplitSeq(typesStr, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			filter.Types = append(filter.Types, event.Type(part))
		}
	}

	return filter, nil
}

func (ctrl *Controller) authorizeRunQuarantineAccess(c *echo.Context, runID uuid.UUID) error {
	principal := authmw.GetPrincipal(c)
	if principal == nil {
		// In auth-enabled mode the auth middleware rejects nil-principal requests
		// before this handler runs, so reaching here means auth is disabled (the
		// whole server is intentionally open). Defense-in-depth: if auth IS enabled,
		// still reject rather than relying solely on the middleware.
		if vars := env.Variables(); vars.AuthMode == "api-key" || vars.SSOEnabled() {
			return echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
		}
		return nil
	}

	svc := ctrl.authService()
	jobAlias, err := svc.JobAliasByRunID(c.Request().Context(), runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if !iauth.CheckScope(principal.Scope, jobAlias) {
		return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	// A namespace-confined subscriber may follow only a run persisted in one
	// of its namespaces (B4); the refusal is an audited 403.
	if ctx := c.Request().Context(); authz.Restricted(ctx) {
		_, namespace, err := svc.RunNamespace(ctx, runID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return echo.ErrNotFound
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		if !authz.NamespaceAllowed(ctx, namespace) {
			return &authz.Denial{
				Reason:     "insufficient_role",
				Required:   models.RoleViewer,
				Namespaces: []string{models.NamespaceOrDefault(namespace)},
				Message:    "insufficient permissions",
			}
		}
	}
	return nil
}

// streamCatchupCaptured is a test seam called after the catch-up high-water
// mark is read, before the catch-up scan.
var streamCatchupCaptured func()

// restrictedStreamTypes are the only event types a namespace-confined
// subscriber receives (identity-and-access B4). Each is allowed because every
// producer's payload describes only the run (and its job) the event names,
// which the subscriber may read; the inventory is pinned by
// TestRestrictedStreamTypeProducersAudited, so a new producer of an allowed
// type fails the build until it is audited:
//
//   - run_started: internal/run/store.go admission (the run's own summary:
//     id, job, namespace, status, priority, times); internal/replay/replay.go
//     (no payload).
//   - run_completed / run_failed / run_terminal / run_cancelled /
//     run_retried: internal/run/store.go (the run's own row, as loaded);
//     replay.go run_completed (no payload).
//   - run_timed_out: internal/notification/watcher.go buildRunEventPayload
//     (the run's id, job, alias, labels, status, trigger type, params).
//   - task_*: internal/run/store.go, store_instance.go, fanout.go,
//     store_owner_support.go recordTaskRunEventTx / recordTaskEventTx and the
//     task payloads beside them (the run's own task row with its job's alias
//     and labels); task_ready, task_claimed, task_lease_expired from
//     internal/worker/claimer.go and the store (ids only); replay.go (no
//     payload).
//   - incident_opened: internal/incident/subscriber.go
//     buildIncidentOpenedEvent (the incident's own ids, class, status and
//     dedupe key; its namespace is the failing run's).
//
// Two payload rules hold for every allowed type: events of a run admission
// refused (a held upstream dataset; its task rows name the hold) are
// withheld, and job labels (read from the job's current row, so possibly a
// new owner's) are removed (redactStreamPayload).
//
// Everything else is withheld until its owner confines it: sla_missed and the
// freshness, dataset and contract events name datasets and jobs of other
// namespaces (B5, B6); run_held_upstream names the upstream hold's dataset
// (B6); agent actions, approvals, escalations and incident status changes can
// name other namespaces' jobs or carry no run (A8); job and backfill events
// carry no run; and unknown types are withheld by default.
var restrictedStreamTypes = map[event.Type]bool{
	event.TypeRunStarted:       true,
	event.TypeRunCompleted:     true,
	event.TypeRunFailed:        true,
	event.TypeRunCancelled:     true,
	event.TypeRunTerminal:      true,
	event.TypeRunRetried:       true,
	event.TypeRunTimedOut:      true,
	event.TypeTaskStarted:      true,
	event.TypeTaskSucceeded:    true,
	event.TypeTaskFailed:       true,
	event.TypeTaskSkipped:      true,
	event.TypeTaskRetrying:     true,
	event.TypeTaskReady:        true,
	event.TypeTaskCached:       true,
	event.TypeTaskClaimed:      true,
	event.TypeTaskLeaseExpired: true,
	event.TypeIncidentOpened:   true,
}

// restrictedStreamFilterTypes narrows a confined subscriber's requested types
// to the allow-list. With no types requested it is the whole allow-list. ok is
// false when nothing requested is allowed: the subscriber then receives no
// events at all (an empty Types list would mean "every type").
func restrictedStreamFilterTypes(requested []event.Type) (types []event.Type, ok bool) {
	if len(requested) == 0 {
		for typ := range restrictedStreamTypes {
			types = append(types, typ)
		}
		slices.Sort(types)
		return types, true
	}
	for _, typ := range requested {
		if restrictedStreamTypes[typ] && !slices.Contains(types, typ) {
			types = append(types, typ)
		}
	}
	return types, len(types) > 0
}

// runNamespaceCache remembers the ownership of runs a live stream has seen. A
// run's namespace and admission skip reason never change, so entries never
// go stale; the cache is bounded and simply reset when full.
type runNamespaceCache struct {
	runs map[uuid.UUID]event.RunOwnership
}

const runNamespaceCacheLimit = 4096

func newRunNamespaceCache() *runNamespaceCache {
	return &runNamespaceCache{runs: make(map[uuid.UUID]event.RunOwnership)}
}

// redactStreamPayload removes what an event payload carries about the job's
// CURRENT owner rather than the run: its labels, read from the job row when
// the event is built, so a run that outlives a move would otherwise hand the
// new owner's labels to the old owner. Data-assertion verdicts recorded before
// baselines were namespace-owned lose their baseline evidence
// (run.WithholdUnownedBaselineEvidenceJSON). Confined subscribers only; a
// payload that names labels or verdicts but is not a JSON object is dropped.
func redactStreamPayload(evt event.Event) event.Event {
	if len(evt.Payload) == 0 ||
		(!bytes.Contains(evt.Payload, []byte(`"job_labels"`)) && !bytes.Contains(evt.Payload, []byte(`"data_violations"`))) {
		return evt
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(evt.Payload))
	decoder.UseNumber() // keep every other value byte-for-byte numerically
	if err := decoder.Decode(&payload); err != nil {
		// Names labels but is not a JSON object: fail closed.
		evt.Payload = nil
		return evt
	}
	delete(payload, "job_labels")
	if tasks, ok := payload["tasks"].([]any); ok {
		for _, task := range tasks {
			if fields, ok := task.(map[string]any); ok {
				delete(fields, "job_labels")
			}
		}
	}
	runstorage.WithholdUnownedBaselineEvidenceJSON(payload)
	redacted, err := json.Marshal(payload)
	if err != nil {
		evt.Payload = nil
		return evt
	}
	evt.Payload = redacted
	return evt
}

// liveEventVisible is the per-subscriber filter of the live feed for a
// namespace-confined subscriber: an allow-listed type, naming a streamable run
// (not one admission refused, whose task rows name an upstream hold), whose
// persisted namespace the subscriber may read. A run that cannot be looked up
// (missing, no store, or a lookup error) hides the event.
func (ctrl *Controller) liveEventVisible(ctx context.Context, evt event.Event, cache *runNamespaceCache) bool {
	if !restrictedStreamTypes[evt.Type] || evt.RunID == uuid.Nil {
		return false
	}
	ownership, ok := cache.runs[evt.RunID]
	if !ok {
		store := ctrl.persistentStore()
		if store == nil {
			return false
		}
		resolved, err := store.RunOwnership(ctx, evt.RunID)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) && ctx.Err() == nil {
				log.Warn("event stream: run ownership lookup failed; withholding event", "run_id", evt.RunID, "error", err)
			}
			return false
		}
		if len(cache.runs) >= runNamespaceCacheLimit {
			clear(cache.runs)
		}
		cache.runs[evt.RunID] = resolved
		ownership = resolved
	}
	return ownership.Streamable() && authz.NamespaceAllowed(ctx, ownership.Namespace)
}

func (ctrl *Controller) authService() *iauth.Service {
	if ctrl.authSvc != nil {
		return ctrl.authSvc
	}
	return iauth.NewService(db.Connection())
}

func parseLastSequence(c *echo.Context) (uint64, error) {
	value := strings.TrimSpace(c.Request().Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(c.QueryParam("cursor"))
	}
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func writeEvent(c *echo.Context, evt event.Event) error {
	data, err := json.Marshal(evt)
	if err != nil {
		c.Logger().Error("failed to marshal event for SSE stream", "error", err)
		return nil
	}

	if evt.Sequence > 0 {
		if _, err := fmt.Fprintf(streaming.Writer(c.Response()), "id: %d\n", evt.Sequence); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(streaming.Writer(c.Response()), "event: %s\ndata: %s\n\n", evt.Type, data)
	return err
}
