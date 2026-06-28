# Design: Event-Driven Triggers

> Status: Work Stream 1 (HTTP webhook triggers), Work Stream 2 (event-based routing), and Work Stream 3 (trigger chaining) are shipped. The reconciliation plan is `exec-plans/completed/event-trigger-routing.md`; operator-facing trigger fields are covered by [job-schema-reference.md](job-schema-reference.md).

## Problem statement

Caesium's trigger system supports cron, HTTP, and event triggers. HTTP shipped dedicated webhook routes, authentication, payload param extraction, payload limits, per-IP rate limiting, and operator-authenticated manual/API fire. WS2/WS3 closed the remaining gaps:

- **Event-based routing.** Jobs can match external, webhook-derived, and internal events by type/source/content.
- **Trigger chaining.** One job's completion can trigger another through the lifecycle bus.
- **Event persistence / observability.** Ingested events and event-trigger matches are persisted for inspection and CLI/API reads.

This design records the shipped three work streams: WS1 (HTTP webhook trigger), WS2 (event-based triggers with content filtering), and WS3 (trigger chaining via internal lifecycle events).

## What shipped (WS1 — HTTP webhook triggers)

A webhook request to `POST /v1/hooks/*` is resolved to the matching trigger(s) and fired with payload-extracted params. Implemented in `api/rest/controller/webhook/webhook.go` and `internal/trigger/http/`:

- **Receiver**: `POST /v1/hooks/:path`; the controller resolves the path via the trigger service's `ListByPath` (JSON-field lookup on `configuration.path`). Multiple triggers may share a path — each is validated and fired independently.
- **Signature auth**: `hmac-sha256` (default), `hmac-sha1`, `bearer`, `basic`; secret resolved from `secret://` URIs; constant-time comparison; timestamp-based replay protection.
- **Param extraction**: JSONPath `paramMapping` from the JSON body into run params, merged over `defaultParams` (webhook values win).
- **Concrete `FireWithParams`** on the HTTP trigger forwards merged params to `job.Run()`. The shared trigger interface stays minimal: `Listen(ctx)`, `Fire(ctx)`, and `ID()`.
- **Abuse controls**: per-IP rate limiting (`CAESIUM_WEBHOOK_RATE_LIMIT_PER_MINUTE`/`_BURST`) and a body cap (`CAESIUM_WEBHOOK_MAX_BODY_SIZE`).
- **Operator manual fire**: `POST /v1/triggers/:id/fire` with optional params, gated by `CAESIUM_MANUAL_TRIGGER_API_KEY`.

Durable webhook/event observability is handled by the event-routing observability work: webhook traffic is bridged into the event router, ingested events and trigger matches are persisted, and the `caesium event push` / `caesium trigger events` CLI reads and writes those records.

## Current architecture (foundation for WS2/WS3)

- **Trigger interface** (`internal/trigger/trigger.go`): `Listen(ctx)`, `Fire(ctx)`, `ID()`. Parameterized firing is intentionally concrete-trigger behavior: HTTP and event triggers expose their own `FireWithParams` methods, and the event router holds concrete `*EventTrigger` values.
- **Executor** (`internal/executor/executor.go`): a `sync.Map` of active triggers; a 60s loop queues cron triggers; HTTP triggers are queued on webhook arrival or manual fire. Event triggers need a reactive model (below), not timer polling.
- **Event bus** (`internal/event/bus.go`): in-memory pub/sub with `Publish(Event)` / `Subscribe(ctx, Filter)` (filter on `JobID`, `RunID`, `Types`). It publishes lifecycle events (`run_completed`, `run_failed`, …) and persists to `internal/event/store.go` for SSE catchup. **WS3 rides on this existing stream**: the event router subscribes to lifecycle events, converts them to ingested events with `source: caesium`, and routes matching event triggers.

The shipped trigger types are `cron`, `http`, and `event` (`internal/models/trigger.go` and `pkg/jobdef/definition.go`).

---

## Work Stream 2: Event-Based Triggers

### 2.1 Concept

An event trigger fires a job when a matching event arrives. Events come from external sources (the webhook receiver or a new ingestion API) or internal lifecycle events (via the existing bus). Unlike HTTP triggers (path → jobs, 1:1), event triggers match by **type and content**, enabling many-to-many routing.

### 2.2 Trigger configuration

```yaml
trigger:
  type: event
  configuration:
    events:
      - type: "webhook"           # match incoming webhook events
        source: "github"          # optional: filter by source identifier
        filter:                   # optional: content-based filter
          action: "completed"
          "workflow_run.conclusion": "success"
      - type: "run_completed"     # match internal lifecycle events
        filter:
          job_alias: "extract-pipeline"
    paramMapping:
      commit: "$.head_sha"
      branch: "$.workflow_run.head_branch"
    defaultParams:
      triggered_by: "event"
```

### 2.3 Event ingestion API

```
POST /v1/events
{ "type": "deployment", "source": "github-actions",
  "data": { "environment": "production", "commit": "abc123", "actor": "cryan" } }
```

Response: `{ "event_id": "...", "matched_triggers": 2, "runs_started": 2 }`. The endpoint requires `CAESIUM_EVENT_INGEST_API_KEY`, persists the event to `ingested_events`, evaluates all event triggers, fires matches with extracted params, and returns the match/run counts. It reuses the webhook abuse controls where applicable, but does **not** reuse webhook signature auth: webhook auth is path/signature based, while `/v1/events` is pathless API ingestion.

### 2.4 Event matching

An event matches a pattern when: the event `type` matches (exact or glob, e.g. `webhook.*`); `source`, if specified, matches exactly; and every `filter` key-value matches the corresponding event-data field (dot-notation for nested fields). Matching is eager on arrival — no queue or polling.

```go
// internal/trigger/event/matcher.go
type EventPattern struct {
    Type   string            `json:"type"`
    Source string            `json:"source,omitempty"`
    Filter map[string]string `json:"filter,omitempty"` // dot-path -> expected value
}

func (p EventPattern) Matches(evt *models.IngestedEvent) bool {
    if !matchesEventType(p.Type, evt.Type) { return false }
    if p.Source != "" && p.Source != evt.Source { return false }
    for path, expected := range p.Filter {
        actual, err := extractField(evt.Data, path)
        if err != nil || fmt.Sprintf("%v", actual) != expected { return false }
    }
    return true
}
```

### 2.5 Event persistence

```sql
CREATE TABLE ingested_events (
    id         TEXT PRIMARY KEY,
    type       TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT '',
    data       TEXT NOT NULL,           -- JSON
    created_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_ingested_events_type ON ingested_events(type);
CREATE INDEX idx_ingested_events_created ON ingested_events(created_at);

CREATE TABLE event_trigger_matches (
    id           TEXT PRIMARY KEY,
    event_id     TEXT NOT NULL,
    trigger_id   TEXT NOT NULL,
    runs_started TEXT,
    skipped      BOOLEAN NOT NULL DEFAULT false,
    skip_reason  TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    matched_at   TIMESTAMP NOT NULL
);
CREATE INDEX idx_event_trigger_matches_event ON event_trigger_matches(event_id);
CREATE INDEX idx_event_trigger_matches_trigger ON event_trigger_matches(trigger_id, matched_at);
```

Retention: prune older than `CAESIUM_EVENT_RETENTION` (default 7d). Events are for observability/debugging, not replay — the event-trigger contract is fire-and-forget. `GET /v1/triggers/:id/events` reads the durable `event_trigger_matches` table joined to `ingested_events`; it never recomputes current trigger patterns over historical events, because that would lie after trigger edits.

### 2.6 Executor integration

Event triggers react to events rather than polling on a timer. Register them as subscribers on the event bus and the ingestion path, dispatched by a singleton **event router**.

```go
// internal/trigger/event/event.go
type EventTrigger struct {
    id     uuid.UUID
    config Config // events, paramMapping, defaultParams
}
func (e *EventTrigger) FireWithParams(ctx context.Context, params map[string]string) ([]FireOutcome, error) {
    // List jobs, merge defaultParams with extracted params, apply _trigger_depth,
    // start run records, and launch the matched jobs.
}
```

```go
// internal/trigger/event/router.go — loads event triggers on startup, subscribes to the bus,
// and exposes Route(event) for the ingestion API, webhook controller, and lifecycle bridge.
func (r *Router) Route(ctx context.Context, evt *IngestedEvent) (*RouteResult, error) {
    result := &RouteResult{EventType: evt.Type, Source: evt.Source}
    return result, db.Transaction(func(tx *gorm.DB) error {
        persistIngestedEvent(tx, evt)
        for _, trig := range r.matchingTriggers(evt) { // concrete *EventTrigger values
            params := withLifecycleTriggerDepth(evt, trig.ExtractEventParams(evt))
            outcomes, err := trig.FireWithParams(ctx, params)
            result.MatchedTriggers = append(result.MatchedTriggers, summarize(trig.ID(), outcomes, err))
        }
        persistEventTriggerMatches(tx, evt.ID, result.MatchedTriggers)
        return nil
    })
}
```

The `Router.Route` boundary is deliberately both the persistence and firing boundary: `ingested_events` and one `event_trigger_matches` row per matched trigger are written in the same transaction as the run records created by `FireWithParams`; actual job execution is launched after commit.

---

## Work Stream 3: Trigger Chaining

### 3.1 Concept

One job's completion triggers another — implemented as a special case of WS2 where the event source is the internal lifecycle bus.

### 3.2 Configuration

```yaml
# Pipeline B triggers when Pipeline A completes successfully
trigger:
  type: event
  configuration:
    events:
      - type: "run_completed"
        filter:
          job_alias: "pipeline-a"
    paramMapping:
      upstream_run_id: "$.run_id"
```

The bus already publishes `run_completed` with `JobID`/`RunID`; the router (WS2) subscribes to internal events and evaluates triggers against them.

### 3.3 Internal event bridging

```go
func (r *Router) bridgeInternalEvents(ctx context.Context) {
    ch, _ := r.bus.Subscribe(ctx, event.Filter{Types: []event.Type{
        event.TypeRunCompleted, event.TypeRunFailed, event.TypeRunTerminal,
    }})
    for evt := range ch {
        ingested := &IngestedEvent{Type: string(evt.Type), Source: "caesium", Data: evt.Payload}
        if job, err := lookupJob(ctx, evt.JobID); err == nil {
            enrichData(ingested, "job_alias", job.Alias)  // so triggers can filter by alias
        }
        r.Route(ctx, ingested)
    }
}
```

### 3.4 Cycle detection

Chaining risks infinite loops (A→B→A). Two guards:

1. **Static**: on `caesium job apply`/`lint`, build a trigger-dependency graph and reject cycles.
2. **Runtime**: track chain depth via a `_trigger_depth` param; reject when it exceeds `CAESIUM_MAX_TRIGGER_DEPTH` (default 10).

```go
func (e *EventTrigger) FireWithParams(ctx context.Context, params map[string]string) ([]FireOutcome, error) {
    depth, _ := strconv.Atoi(params["_trigger_depth"])
    if depth >= e.maxTriggerDepth { return ErrTriggerChainDepthExceeded }
    params["_trigger_depth"] = strconv.Itoa(depth + 1)
    // ... fire jobs
}
```

---

## Schema, API, and configuration changes

### YAML schema (`pkg/jobdef/definition.go`)

No structural change — `Trigger.Configuration` is already a flexible map (`Type` adds `event` alongside `cron`/`http`). Validation is type-specific. Add event-trigger lint rules: `events` required with ≥1 pattern; each pattern needs a `type`; `filter` values must be strings; `paramMapping` values must be valid JSONPath.

### API changes

| Method | Path | Status |
|--------|------|--------|
| `POST` | `/v1/hooks/*` | ✅ shipped (WS1) |
| `POST` | `/v1/triggers/:id/fire` | ✅ shipped (WS1, operator-authenticated) |
| `POST` | `/v1/events` | shipped (WS2, keyed by `CAESIUM_EVENT_INGEST_API_KEY`) |
| `GET` | `/v1/events/ingested` | shipped (WS2 observability) |
| `GET` | `/v1/triggers/:id/events` | shipped (WS2 observability, backed by `event_trigger_matches`) |

WS1 manual fire is the operator-authenticated REST endpoint `POST /v1/triggers/:id/fire`. The event-trigger CLI shipped by this plan is `caesium event push --type … --source … --data '{}'` for `POST /v1/events` ingestion and `caesium trigger events <alias>` for `GET /v1/triggers/:id/events` inspection.

### Configuration

| Variable | Default | Status |
|----------|---------|--------|
| `CAESIUM_WEBHOOK_MAX_BODY_SIZE` | `1MB` | ✅ shipped |
| `CAESIUM_WEBHOOK_RATE_LIMIT_PER_MINUTE` | `120` | ✅ shipped |
| `CAESIUM_WEBHOOK_RATE_LIMIT_BURST` | `20` | ✅ shipped |
| `CAESIUM_MANUAL_TRIGGER_API_KEY` | unset | ✅ shipped |
| `CAESIUM_EVENT_INGEST_API_KEY` | unset | shipped (WS2 ingestion API) |
| `CAESIUM_WEBHOOK_EVENT_RETENTION` | `7d` | shipped (webhook event log) |
| `CAESIUM_EVENT_RETENTION` | `7d` | shipped (WS2 ingested event log) |
| `CAESIUM_MAX_TRIGGER_DEPTH` | `10` | shipped (WS3 runtime chain guard) |

### Metrics (WS2/WS3)

`caesium_events_ingested_total{type,source}`, `caesium_event_trigger_matches_total{trigger_id,event_type}`, `caesium_trigger_chain_depth` (histogram), `caesium_trigger_chain_rejected_total`. (WS1's `caesium_webhook_received_total`/`caesium_webhook_auth_failures_total` ship with the receiver.)

### Dependencies

JSONPath extraction is already vendored for WS1 param mapping; WS2/WS3 reuse it. No mandatory infrastructure dependencies — the feature runs entirely in the existing binary on dqlite.

---

## Shipped implementation record

### Phase 2: Event ingestion & routing (P0)

1. `internal/trigger/event/event.go` — `EventTrigger` implementing the Trigger interface (`TriggerTypeEvent` in `internal/models/trigger.go`).
2. `internal/trigger/event/matcher.go` — pattern matching (type glob, source filter, content filter).
3. `internal/trigger/event/router.go` — singleton router that loads event triggers and dispatches matches.
4. `api/rest/controller/event/ingest.go` — `POST /v1/events` keyed by `CAESIUM_EVENT_INGEST_API_KEY`.
5. `ingested_events` and `event_trigger_matches` tables, models, GORM migration.
6. Executor: register event triggers alongside cron triggers; add `ListByEventPattern` to the trigger service.
7. Linter rules for event-trigger configuration.
8. Tests: matcher unit tests; integration test for event → match → run (fan-out to two jobs).

### Phase 3: Trigger chaining (P1)

9. Internal event bridge: router subscribes to lifecycle events, converts to `IngestedEvent`, enriches with `job_alias`.
10. Static cycle detection at lint/apply; runtime `_trigger_depth` guard with `CAESIUM_MAX_TRIGGER_DEPTH`.
11. Tests: A→B chain; cycle-detection (static + depth).

### Phase 4: Observability & CLI (P1)

12. Durable webhook/event logging plus ingested-event pruners with retention.
13. `GET /v1/events/ingested`, `GET /v1/triggers/:id/events`.
14. CLI: `caesium event push`, `caesium trigger events`.
15. Prometheus metrics above; UI: event-trigger visualization in the DAG view, event log on trigger detail.

---

## Risks & mitigations

| Risk | Mitigation |
|------|------------|
| Infinite trigger chains | Runtime `_trigger_depth` guard + static cycle detection at lint time. |
| Event-bus backpressure dropping internal events for chaining | The bus drops on a full channel (non-blocking) — for chaining, increase the router subscriber's buffer, log dropped events as warnings, surface in metrics. |
| JSONPath injection via malicious payloads | Extraction always yields strings; run params are env vars (not executed); validate extracted values against parameter schemas where available. |
| Path/pattern collisions (multiple triggers match) | Allowed — fire all matches; document that paths/patterns should be unique per job. |
| Webhook flood (already mitigated for WS1) | Body cap + per-IP rate limiting (shipped). |

---

## Examples

### S3 object arrival (event trigger, WS2)

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: process-upload
trigger:
  type: event
  configuration:
    events:
      - type: "s3:ObjectCreated"
        filter:
          "detail.bucket.name": "incoming-data"
          "detail.object.key_prefix": "raw/"
    paramMapping:
      bucket: "$.detail.bucket.name"
      key: "$.detail.object.key"
steps:
  - name: process
    image: alpine:3.23
    command: ["process.sh"]
```

### Trigger chaining — Pipeline B follows Pipeline A (WS3)

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: pipeline-b
trigger:
  type: event
  configuration:
    events:
      - type: "run_completed"
        source: "caesium"
        filter:
          job_alias: "pipeline-a"
    paramMapping:
      upstream_run: "$.run_id"
steps:
  - name: downstream-work
    image: debian:12-slim
    command: ["continue.sh"]
```

### GitHub push webhook (HTTP trigger, WS1 — shipped)

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: deploy-on-push
trigger:
  type: http
  configuration:
    path: "github/push"
    secret: "secret://env/GITHUB_WEBHOOK_SECRET"
    signatureScheme: hmac-sha256
    signatureHeader: X-Hub-Signature-256
    paramMapping:
      branch: "$.ref"
      commit: "$.after"
      actor: "$.pusher.name"
  defaultParams:
    environment: staging
steps:
  - name: deploy
    image: alpine:3.23
    command: ["deploy.sh"]
```
