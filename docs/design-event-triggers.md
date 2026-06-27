# Design: Event-Driven Triggers

> Status: Work Stream 1 (HTTP webhook triggers) is shipped. Work Streams 2 (event-based routing) and 3 (trigger chaining) are the project's current top-priority next feature (roadmap P0) and are now decomposed into the active exec-plan [`exec-plans/active/event-trigger-routing.md`](exec-plans/active/event-trigger-routing.md). WS1 operator behaviour is covered by the HTTP-trigger fields in [job-schema-reference.md](job-schema-reference.md).

## Problem statement

Caesium's trigger system supports cron (fully implemented) and HTTP (shipped: dedicated webhook routes, authentication, payload param extraction, payload limits, per-IP rate limiting, and operator-authenticated manual/API fire). Two gaps remain:

- **No event-based routing.** A webhook path maps 1:1 to a set of jobs; there is no way to route an event to different jobs based on its type or payload content.
- **No trigger chaining.** One job's completion cannot trigger another. The internal event bus carries lifecycle events but has no mechanism to route them to triggers.
- **No event persistence / observability** for ingested external events.

This design addresses these in three work streams: WS1 (HTTP webhook trigger — **shipped**), WS2 (event-based triggers with content filtering), WS3 (trigger chaining via internal lifecycle events).

## What shipped (WS1 — HTTP webhook triggers)

A webhook request to `POST /v1/hooks/*` is resolved to the matching trigger(s) and fired with payload-extracted params. Implemented in `api/rest/controller/webhook/webhook.go` and `internal/trigger/http/`:

- **Receiver**: `POST /v1/hooks/:path`; the controller resolves the path via the trigger service's `ListByPath` (JSON-field lookup on `configuration.path`). Multiple triggers may share a path — each is validated and fired independently.
- **Signature auth**: `hmac-sha256` (default), `hmac-sha1`, `bearer`, `basic`; secret resolved from `secret://` URIs; constant-time comparison; timestamp-based replay protection.
- **Param extraction**: JSONPath `paramMapping` from the JSON body into run params, merged over `defaultParams` (webhook values win).
- **`FireWithParams`** on the trigger interface forwards merged params to `job.Run()`.
- **Abuse controls**: per-IP rate limiting (`CAESIUM_WEBHOOK_RATE_LIMIT_PER_MINUTE`/`_BURST`) and a body cap (`CAESIUM_WEBHOOK_MAX_BODY_SIZE`).
- **Operator manual fire**: `POST /v1/triggers/:id/fire` with optional params, gated by `CAESIUM_MANUAL_TRIGGER_API_KEY`.

Not yet shipped from the original WS1 scope: durable webhook event logging (the `webhook_events` table + retention) — folded into the observability phase below.

## Current architecture (foundation for WS2/WS3)

- **Trigger interface** (`internal/trigger/trigger.go`): `Listen(ctx)`, `Fire(ctx)`, `FireWithParams(ctx, params)`, `ID()`.
- **Executor** (`internal/executor/executor.go`): a `sync.Map` of active triggers; a 60s loop queues cron triggers; HTTP triggers are queued on webhook arrival or manual fire. Event triggers need a reactive model (below), not timer polling.
- **Event bus** (`internal/event/bus.go`): in-memory pub/sub with `Publish(Event)` / `Subscribe(ctx, Filter)` (filter on `JobID`, `RunID`, `Types`). Already publishes lifecycle events (`run_completed`, `run_failed`, …) and persists to `internal/event/store.go` for SSE catchup. **WS3 rides on this existing stream** — it is the substrate for chaining, currently consumed only by SSE/metrics/lineage/callbacks.

Only `cron` and `http` trigger types exist today (`internal/models/trigger.go`); WS2 adds `event`.

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

Response: `{ "event_id": "...", "matched_triggers": 2, "runs_started": 2 }`. The endpoint persists the event to `ingested_events`, evaluates all event triggers, fires matches with extracted params, and returns the match/run counts. Reuse the webhook controller's auth + rate-limit middleware.

### 2.4 Event matching

An event matches a pattern when: the event `type` matches (exact or glob, e.g. `webhook.*`); `source`, if specified, matches exactly; and every `filter` key-value matches the corresponding event-data field (dot-notation for nested fields). Matching is eager on arrival — no queue or polling.

```go
// internal/trigger/event/matcher.go
type EventPattern struct {
    Type   string            `json:"type"`
    Source string            `json:"source,omitempty"`
    Filter map[string]string `json:"filter,omitempty"` // dot-path -> expected value
}

func (p *EventPattern) Matches(evt *IngestedEvent) bool {
    if !matchGlob(p.Type, evt.Type) { return false }
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
```

Retention: prune older than `CAESIUM_EVENT_RETENTION` (default 7d). Events are for observability/debugging, not replay — the event-trigger contract is fire-and-forget.

### 2.6 Executor integration

Event triggers react to events rather than polling on a timer. Register them as subscribers on the event bus and the ingestion path, dispatched by a singleton **event router**.

```go
// internal/trigger/event/event.go
type EventTrigger struct {
    id       uuid.UUID
    patterns []EventPattern
    params   map[string]string  // paramMapping
    defaults map[string]string  // defaultParams
}
func (e *EventTrigger) FireWithParams(ctx context.Context, params map[string]string) error {
    // Same as the HTTP trigger: list jobs, merge params, run each.
}
```

```go
// internal/trigger/event/router.go — loads event triggers on startup, subscribes to the bus,
// and exposes Route(event) for the ingestion API and webhook controller.
func (r *Router) Route(ctx context.Context, evt *IngestedEvent) []uuid.UUID {
    r.mu.RLock(); defer r.mu.RUnlock()
    var matched []uuid.UUID
    for id, trig := range r.triggers {
        for _, pattern := range trig.patterns {
            if pattern.Matches(evt) {
                params := mergeParams(trig.defaults, extractParams(evt.Data, trig.params))
                go trig.FireWithParams(ctx, params)
                matched = append(matched, id)
                break
            }
        }
    }
    return matched
}
```

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
func (r *Router) FireWithParams(ctx context.Context, params map[string]string) error {
    depth, _ := strconv.Atoi(params["_trigger_depth"])
    if depth >= r.maxDepth { return ErrTriggerChainDepthExceeded }
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
| `POST` | `/v1/events` | proposed (WS2) |
| `GET` | `/v1/events/ingested` | proposed (WS2 observability) |
| `GET` | `/v1/triggers/:id/events` | proposed (WS2 observability) |

CLI (proposed): `caesium event push --type … --source … --data '{}'`; `caesium trigger events <alias>`. (`caesium trigger fire`/`list` ship with WS1.)

### Configuration

| Variable | Default | Status |
|----------|---------|--------|
| `CAESIUM_WEBHOOK_MAX_BODY_SIZE` | `1MB` | ✅ shipped |
| `CAESIUM_WEBHOOK_RATE_LIMIT_PER_MINUTE` | `120` | ✅ shipped |
| `CAESIUM_WEBHOOK_RATE_LIMIT_BURST` | `20` | ✅ shipped |
| `CAESIUM_MANUAL_TRIGGER_API_KEY` | unset | ✅ shipped |
| `CAESIUM_WEBHOOK_EVENT_RETENTION` | `7d` | proposed (webhook event log) |
| `CAESIUM_EVENT_RETENTION` | `7d` | proposed (WS2) |
| `CAESIUM_MAX_TRIGGER_DEPTH` | `10` | proposed (WS3) |

### Metrics (proposed for WS2/WS3)

`caesium_events_ingested_total{type,source}`, `caesium_event_trigger_matches_total{trigger_id,event_type}`, `caesium_trigger_chain_depth` (histogram), `caesium_trigger_chain_rejected_total`. (WS1's `caesium_webhook_received_total`/`caesium_webhook_auth_failures_total` ship with the receiver.)

### Dependencies

JSONPath extraction is already vendored for WS1 param mapping; WS2/WS3 reuse it. No mandatory infrastructure dependencies — the feature runs entirely in the existing binary on dqlite.

---

## Remaining implementation plan

### Phase 2: Event ingestion & routing (P0)

1. `internal/trigger/event/event.go` — `EventTrigger` implementing the Trigger interface (`TriggerTypeEvent` in `internal/models/trigger.go`).
2. `internal/trigger/event/matcher.go` — pattern matching (type glob, source filter, content filter).
3. `internal/trigger/event/router.go` — singleton router that loads event triggers and dispatches matches.
4. `api/rest/controller/event/ingest.go` — `POST /v1/events` (reusing webhook auth/rate-limit middleware).
5. `ingested_events` table, model, GORM migration.
6. Executor: register event triggers alongside cron triggers; add `ListByEventPattern` to the trigger service.
7. Linter rules for event-trigger configuration.
8. Tests: matcher unit tests; integration test for event → match → run (fan-out to two jobs).

### Phase 3: Trigger chaining (P1)

9. Internal event bridge: router subscribes to lifecycle events, converts to `IngestedEvent`, enriches with `job_alias`.
10. Static cycle detection at lint/apply; runtime `_trigger_depth` guard with `CAESIUM_MAX_TRIGGER_DEPTH`.
11. Tests: A→B chain; cycle-detection (static + depth).

### Phase 4: Observability & CLI (P1)

12. Durable webhook event log (`webhook_events` table) + ingested-event pruners with retention.
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
    image: etl:latest
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
    image: etl:latest
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
    image: deploy:latest
    command: ["deploy.sh"]
```
