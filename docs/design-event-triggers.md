# Design: Event-Driven Triggers

> Status: Proposed. This document covers the full trigger overhaul — completing the HTTP trigger implementation and adding event-driven routing.

## Problem Statement

Caesium's trigger system has two types: cron (fully implemented) and HTTP (partially implemented). The HTTP trigger exists as a model and fires jobs when `PUT /v1/triggers/:id` is called, but it lacks the capabilities needed for real-world use:

- **No webhook receiver endpoint.** External systems cannot POST to Caesium. The only way to fire an HTTP trigger is the internal `PUT /v1/triggers/:id` API, which requires knowing the trigger's UUID.
- **No payload handling.** The HTTP trigger ignores the request body entirely. Webhook payloads from external systems (GitHub, Slack, CI/CD, S3 notifications) cannot be extracted into job parameters.
- **No request authentication.** The `secret` configuration field exists in the schema and examples but is never validated. Anyone who can reach the API can fire any trigger.
- **No routing.** The `path` configuration field is documented but not implemented. All jobs associated with a trigger fire unconditionally — there is no content-based filtering.
- **No trigger chaining.** One job's completion cannot trigger another job. The internal event bus handles lifecycle events but has no mechanism to route them to triggers.
- **No event persistence.** External events are fire-and-forget. There is no event log, no replay, and no deduplication.

This design addresses all of the above in three work streams:

1. **WS1**: Complete the HTTP webhook trigger (path routing, payload extraction, authentication)
2. **WS2**: Add event-based triggers with content filtering
3. **WS3**: Add trigger chaining via internal lifecycle events

---

## Current Architecture

### Trigger Interface

```go
// internal/trigger/trigger.go
type Trigger interface {
    Listen(ctx context.Context)
    Fire(ctx context.Context) error
    ID() uuid.UUID
}
```

### Executor

The executor (`internal/executor/executor.go`) maintains a `sync.Map` of active triggers. A background loop runs every 60 seconds and queues all cron triggers. HTTP triggers are only queued when `PUT /v1/triggers/:id` is called from the REST controller.

### HTTP Trigger (current)

`internal/trigger/http/http.go` — 72 lines. When `Listen()` is called, it immediately calls `Fire()`, which lists all jobs for the trigger and runs each non-paused job asynchronously. No payload, no params, no auth.

### Event Bus

`internal/event/bus.go` — in-memory pub/sub with `Publish(Event)` and `Subscribe(ctx, Filter)`. Filter supports `JobID`, `RunID`, and `Types`. Used by the SSE endpoint (`GET /v1/events`) and internal subscribers (metrics, lineage, callbacks). Events are also persisted to the event store (`internal/event/store.go`) for SSE catchup on reconnect.

---

## Work Stream 1: Complete HTTP Webhook Trigger

### 1.1 Webhook Receiver Endpoint

Add a new route that accepts external HTTP requests at user-defined paths:

```
POST /v1/hooks/:path
```

Where `:path` matches the `path` field in the trigger's configuration. This is a catch-all route that the webhook controller resolves to the correct trigger(s).

**Route registration** in `api/rest/bind/bind.go`:

```go
// webhooks
{
    ctrl := webhook.New(bus)
    g.POST("/hooks/*", ctrl.Receive)
}
```

**Controller** (`api/rest/controller/webhook/webhook.go`):

```go
func (w *WebhookController) Receive(c echo.Context) error {
    path := c.PathParam("*")  // everything after /hooks/

    // 1. Look up triggers by path
    triggers, err := triggerSvc.ListByPath(path)
    if err != nil || len(triggers) == 0 {
        return c.JSON(404, map[string]string{"error": "no trigger registered for path"})
    }

    // 2. Read and parse request body
    body, err := io.ReadAll(c.Request().Body)
    if err != nil {
        return c.JSON(400, map[string]string{"error": "failed to read body"})
    }

    // 3. For each matching trigger: validate signature, extract params, fire
    // Process all triggers independently — an auth failure on one does not
    // block others bound to the same path (path collisions are intentional).
    var accepted int
    for _, trig := range triggers {
        cfg := trig.ParsedHTTPConfig()

        // Validate signature if secret is configured.
        // Resolve secret:// URIs to their actual values before comparison.
        if cfg.Secret != "" {
            resolvedSecret := resolveSecret(cfg.Secret)
            if !validateSignature(c.Request(), body, resolvedSecret, cfg.SignatureScheme, cfg.SignatureHeader) {
                log.Warn("webhook signature validation failed", "trigger_id", trig.ID, "path", path)
                continue // skip this trigger, keep processing others
            }
        }

        // Extract parameters from payload
        params := extractParams(body, cfg.ParamMapping)

        // Queue the trigger with extracted params
        executor.QueueWithParams(ctx, trig, params)
        accepted++
    }

    if accepted == 0 {
        return c.JSON(401, map[string]string{"error": "invalid signature"})
    }

    return c.JSON(202, map[string]string{"status": "accepted"})
}
```

### 1.2 Trigger Configuration Schema

Extend the HTTP trigger configuration to support the full feature set:

```yaml
trigger:
  type: http
  configuration:
    path: "deploy/production"           # maps to POST /v1/hooks/deploy/production
    secret: "secret://env/WEBHOOK_SECRET"  # supports secret:// URIs
    signatureScheme: hmac-sha256        # hmac-sha256 (default), hmac-sha1, bearer, basic
    signatureHeader: X-Hub-Signature-256  # header containing the signature (default varies by scheme)
    paramMapping:                         # extract fields from JSON body into run params
      branch: "$.ref"                    # JSONPath expression
      commit: "$.after"
      actor: "$.sender.login"
  defaultParams:
    environment: production
```

**Go struct** (new file: `internal/trigger/http/config.go`):

```go
type HTTPConfig struct {
    Path            string            `json:"path"`
    Secret          string            `json:"secret,omitempty"`
    SignatureScheme string            `json:"signatureScheme,omitempty"` // hmac-sha256, hmac-sha1, bearer, basic
    SignatureHeader string            `json:"signatureHeader,omitempty"`
    ParamMapping    map[string]string `json:"paramMapping,omitempty"`   // param name -> JSONPath
}
```

### 1.3 Signature Validation

Support common webhook signing schemes:

| Scheme | How It Works | Used By |
|--------|-------------|---------|
| `hmac-sha256` | HMAC-SHA256 of body with shared secret, compared to signature header | GitHub, Stripe, generic |
| `hmac-sha1` | HMAC-SHA1 of body | GitHub (legacy), Bitbucket |
| `bearer` | `Authorization: Bearer <secret>` header matches configured secret | Generic |
| `basic` | `Authorization: Basic <base64>` header decoded and matched | Generic |

Implementation: `internal/trigger/http/auth.go`

```go
func validateSignature(req *http.Request, body []byte, secret, scheme string, header string) bool {
    switch scheme {
    case "hmac-sha256", "":
        return validateHMAC(req.Header.Get(header), body, secret, sha256.New)
    case "hmac-sha1":
        return validateHMAC(req.Header.Get(header), body, secret, sha1.New)
    case "bearer":
        return req.Header.Get("Authorization") == "Bearer "+secret
    case "basic":
        user, pass, ok := req.BasicAuth()
        return ok && subtle.ConstantTimeCompare([]byte(user+":"+pass), []byte(secret)) == 1
    }
    return false
}
```

### 1.4 Parameter Extraction

JSONPath expressions extract values from the webhook payload into run parameters. These merge with `defaultParams` (webhook values take precedence).

Implementation: `internal/trigger/http/params.go`

Use a lightweight JSONPath library (e.g., `github.com/PaesslerAG/jsonpath`) or a simple dot-notation extractor for the initial implementation:

```go
func extractParams(body []byte, mapping map[string]string) map[string]string {
    params := make(map[string]string)
    var payload interface{}
    if err := json.Unmarshal(body, &payload); err != nil {
        return params
    }
    for paramName, jsonPath := range mapping {
        if val, err := jsonpath.Get(jsonPath, payload); err == nil {
            params[paramName] = fmt.Sprintf("%v", val)
        }
    }
    return params
}
```

### 1.5 Passing Parameters to Job Runs

The `Fire()` method on the HTTP trigger must be updated to accept and forward parameters:

```go
// Updated trigger interface
type Trigger interface {
    Listen(ctx context.Context)
    Fire(ctx context.Context) error
    FireWithParams(ctx context.Context, params map[string]string) error
    ID() uuid.UUID
}
```

The `FireWithParams` method merges extracted params with `defaultParams` and passes them to `job.Run()`. The existing `Fire()` remains for backward compatibility (fires with `defaultParams` only).

### 1.6 Trigger Path Index

Add a database index and service method for path-based lookup:

```go
// trigger service
func (t *triggerService) ListByPath(path string) (models.Triggers, error) {
    var triggers models.Triggers
    q := t.db.WithContext(t.ctx).
        Where("type = ?", models.TriggerTypeHTTP).
        Where("json_extract(configuration, '$.path') = ?", path)
    return triggers, q.Find(&triggers).Error
}
```

For dqlite/SQLite this uses `json_extract`. Add a functional index if performance is a concern:

```sql
CREATE INDEX idx_trigger_http_path ON triggers(json_extract(configuration, '$.path'))
    WHERE type = 'http';
```

### 1.7 Event Logging

Persist incoming webhook events for observability and debugging:

```sql
CREATE TABLE webhook_events (
    id          TEXT PRIMARY KEY,
    trigger_id  TEXT NOT NULL,
    path        TEXT NOT NULL,
    method      TEXT NOT NULL,
    headers     TEXT,          -- JSON
    body        TEXT,
    params      TEXT,          -- JSON, extracted params
    status      TEXT NOT NULL, -- accepted, rejected, failed
    error       TEXT,
    created_at  TIMESTAMP NOT NULL
);

CREATE INDEX idx_webhook_events_trigger ON webhook_events(trigger_id);
CREATE INDEX idx_webhook_events_created ON webhook_events(created_at);
```

Retention: prune webhook events older than `CAESIUM_WEBHOOK_EVENT_RETENTION` (default: 7 days).

---

## Work Stream 2: Event-Based Triggers

### 2.1 Concept

An event trigger fires a job when a matching event arrives. Events can come from:

1. **External sources** via the webhook receiver (WS1) or a new event ingestion API
2. **Internal lifecycle events** (job completed, task failed, etc.) via the existing event bus

This is distinct from the HTTP trigger in that:
- HTTP triggers map a webhook path 1:1 to a set of jobs
- Event triggers match events by **type and content**, allowing many-to-many routing

### 2.2 Trigger Configuration

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

### 2.3 Event Ingestion API

A new endpoint for pushing arbitrary events into Caesium:

```
POST /v1/events
Content-Type: application/json

{
    "type": "deployment",
    "source": "github-actions",
    "data": {
        "environment": "production",
        "commit": "abc123",
        "actor": "cryan"
    }
}
```

Response:

```json
{
    "event_id": "evt_abc123",
    "matched_triggers": 2,
    "runs_started": 2
}
```

This endpoint:
1. Persists the event to the `ingested_events` table
2. Evaluates all event triggers against the event
3. Fires matching triggers with extracted parameters
4. Returns the number of matches and runs started

### 2.4 Event Matching

Event triggers declare a list of event patterns. An event matches a pattern if:

1. The event `type` matches the pattern `type` (exact match or glob: `webhook.*`)
2. If `source` is specified, the event source matches (exact match)
3. If `filter` is specified, every key-value pair in the filter matches the corresponding field in the event data (dot-notation paths supported for nested fields)

Matching is evaluated eagerly on event arrival — there is no queue or polling loop.

```go
// internal/trigger/event/matcher.go
type EventPattern struct {
    Type   string            `json:"type"`
    Source string            `json:"source,omitempty"`
    Filter map[string]string `json:"filter,omitempty"` // dot-path -> expected value
}

func (p *EventPattern) Matches(evt *IngestedEvent) bool {
    if !matchGlob(p.Type, evt.Type) {
        return false
    }
    if p.Source != "" && p.Source != evt.Source {
        return false
    }
    for path, expected := range p.Filter {
        actual, err := extractField(evt.Data, path)
        if err != nil || fmt.Sprintf("%v", actual) != expected {
            return false
        }
    }
    return true
}
```

### 2.5 Event Persistence

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

Retention: prune events older than `CAESIUM_EVENT_RETENTION` (default: 7 days). Events are for observability and debugging, not replay — the event-trigger contract is fire-and-forget (matched events immediately trigger runs).

### 2.6 Executor Integration

The executor currently only auto-queues cron triggers. Event triggers require a different execution model — they don't poll on a timer, they react to incoming events.

**Approach**: Register event triggers as subscribers on the event bus and the new event ingestion path.

```go
// internal/trigger/event/event.go
type EventTrigger struct {
    id       uuid.UUID
    patterns []EventPattern
    params   map[string]string  // paramMapping
    defaults map[string]string  // defaultParams
}

func (e *EventTrigger) Listen(ctx context.Context) {
    // Subscribe to both internal events and ingested events
    // The event router (below) handles dispatching to us
    <-ctx.Done()
}

func (e *EventTrigger) Fire(ctx context.Context) error { /* ... */ }

func (e *EventTrigger) FireWithParams(ctx context.Context, params map[string]string) error {
    // Same as HTTP trigger: list jobs, merge params, run each
}
```

**Event router** (new component: `internal/trigger/event/router.go`):

A singleton that:
1. Loads all event triggers from the database on startup
2. Subscribes to the internal event bus for lifecycle events
3. Exposes `Route(event)` for the event ingestion API and webhook controller
4. On each event: evaluate all registered patterns, fire matching triggers

```go
type Router struct {
    triggers map[uuid.UUID]*EventTrigger
    mu       sync.RWMutex
    bus      event.Bus
}

func (r *Router) Route(ctx context.Context, evt *IngestedEvent) []uuid.UUID {
    r.mu.RLock()
    defer r.mu.RUnlock()

    var matched []uuid.UUID
    for id, trig := range r.triggers {
        for _, pattern := range trig.patterns {
            if pattern.Matches(evt) {
                params := extractParams(evt.Data, trig.params)
                mergedParams := mergeParams(trig.defaults, params)
                go trig.FireWithParams(ctx, mergedParams)
                matched = append(matched, id)
                break // one match per trigger is enough
            }
        }
    }
    return matched
}
```

---

## Work Stream 3: Trigger Chaining

### 3.1 Concept

Trigger chaining allows one job's completion to trigger another job. This is implemented as a special case of event triggers (WS2) where the event source is the internal lifecycle event bus.

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

This works because the existing event bus already publishes `run_completed` events with `JobID` and `RunID`. The event router (WS2) subscribes to internal events and evaluates triggers against them.

### 3.3 Internal Event Bridging

The event router subscribes to the internal event bus and converts lifecycle events into the same `IngestedEvent` format used by external events:

```go
func (r *Router) bridgeInternalEvents(ctx context.Context) {
    // Subscribe to terminal run events
    filter := event.Filter{
        Types: []event.Type{
            event.TypeRunCompleted,
            event.TypeRunFailed,
            event.TypeRunTerminal,
        },
    }

    ch, _ := r.bus.Subscribe(ctx, filter)

    for evt := range ch {
        // Convert internal event to ingested event format
        ingested := &IngestedEvent{
            Type:   string(evt.Type),
            Source: "caesium",
            Data:   evt.Payload,
        }

        // Enrich with job metadata
        if job, err := lookupJob(ctx, evt.JobID); err == nil {
            enrichData(ingested, "job_alias", job.Alias)
        }

        r.Route(ctx, ingested)
    }
}
```

### 3.4 Cycle Detection

Trigger chaining introduces the risk of infinite loops (A triggers B triggers A). The router must detect and prevent cycles:

1. **Static analysis**: On `caesium job apply` / `caesium job lint`, build a graph of trigger dependencies and reject cycles
2. **Runtime guard**: Track the trigger chain depth per run via a `CAESIUM_TRIGGER_DEPTH` parameter. Reject runs where depth exceeds `CAESIUM_MAX_TRIGGER_DEPTH` (default: 10).

```go
func (r *Router) FireWithParams(ctx context.Context, params map[string]string) error {
    depth, _ := strconv.Atoi(params["_trigger_depth"])
    if depth >= r.maxDepth {
        log.Warn("trigger chain depth exceeded", "depth", depth, "max", r.maxDepth)
        return ErrTriggerChainDepthExceeded
    }
    params["_trigger_depth"] = strconv.Itoa(depth + 1)
    // ... fire jobs
}
```

---

## YAML Schema Changes

### Updated Trigger Definition (`pkg/jobdef/definition.go`)

```go
type Trigger struct {
    Type          string            `yaml:"type" json:"type"`                     // cron, http, event
    Configuration map[string]any    `yaml:"configuration" json:"configuration"`
    DefaultParams map[string]string `yaml:"defaultParams" json:"defaultParams"`
}
```

No structural changes needed — the `Configuration` field is already a flexible map. Validation is type-specific.

### Validation Rules

Add to the linter (`internal/jobdef/lint/`):

**HTTP triggers**:
- `path` is required and must be a valid URL path segment (no leading slash, no query params)
- `secret` should use `secret://` URI syntax (warn if plaintext)
- `signatureScheme` must be one of: `hmac-sha256`, `hmac-sha1`, `bearer`, `basic`
- `paramMapping` values must be valid JSONPath expressions

**Event triggers**:
- `events` is required and must contain at least one event pattern
- Each pattern must have a `type` field
- `filter` values must be strings
- `paramMapping` values must be valid JSONPath expressions

---

## API Changes

### New Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/hooks/*` | Webhook receiver (WS1) |
| `POST` | `/v1/events` | Event ingestion (WS2) |
| `GET` | `/v1/events/ingested` | List ingested events (observability) |
| `GET` | `/v1/triggers/:id/events` | List events that matched this trigger |

### Updated Endpoints

| Method | Path | Change |
|--------|------|--------|
| `PUT` | `/v1/triggers/:id` | Accept optional JSON body as run params |

### CLI Changes

```bash
caesium trigger fire <trigger-alias> [--param key=value ...]   # Manual fire with params
caesium trigger list                                           # List all triggers with status
caesium trigger events <trigger-alias> [--limit 50]            # Recent events for a trigger
caesium event push --type <type> --source <source> --data '{}'  # Push an event
```

---

## New Dependencies

| Dependency | Purpose | Notes |
|------------|---------|-------|
| `github.com/PaesslerAG/jsonpath` | JSONPath parameter extraction | Lightweight, no transitive deps. Could also use a simple dot-notation extractor initially. |

No mandatory infrastructure dependencies. The feature runs entirely within the existing Caesium binary using dqlite.

---

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CAESIUM_WEBHOOK_EVENT_RETENTION` | `7d` | How long to keep webhook event logs |
| `CAESIUM_EVENT_RETENTION` | `7d` | How long to keep ingested events |
| `CAESIUM_MAX_TRIGGER_DEPTH` | `10` | Maximum trigger chain depth before rejection |
| `CAESIUM_WEBHOOK_MAX_BODY_SIZE` | `1MB` | Maximum accepted webhook payload size |

---

## Metrics

New Prometheus metrics:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `caesium_webhook_received_total` | Counter | `path`, `status` | Webhooks received by path and status (accepted/rejected/failed) |
| `caesium_webhook_auth_failures_total` | Counter | `path`, `scheme` | Authentication failures |
| `caesium_events_ingested_total` | Counter | `type`, `source` | Events ingested via API |
| `caesium_event_trigger_matches_total` | Counter | `trigger_id`, `event_type` | Event-trigger match count |
| `caesium_trigger_chain_depth` | Histogram | — | Distribution of trigger chain depths |
| `caesium_trigger_chain_rejected_total` | Counter | — | Chains rejected due to depth limit |

---

## Implementation Plan

### Phase 1: HTTP Webhook Completion (P0)

1. **`internal/trigger/http/config.go`**: Parse full HTTP config (path, secret, signatureScheme, signatureHeader, paramMapping)
2. **`internal/trigger/http/auth.go`**: Signature validation (HMAC-SHA256, HMAC-SHA1, bearer, basic)
3. **`internal/trigger/http/params.go`**: JSONPath parameter extraction from webhook payloads
4. **`api/rest/controller/webhook/webhook.go`**: Webhook receiver endpoint (`POST /v1/hooks/*`)
5. **Trigger service**: Add `ListByPath(path)` method with JSON field lookup
6. **Updated `Fire` method**: `FireWithParams(ctx, params)` on the trigger interface
7. **`internal/trigger/http/http.go`**: Rewrite to use parsed config, merge defaultParams with extracted params
8. **Webhook event logging**: `webhook_events` table, write on every received webhook
9. **Linter rules**: Validate HTTP trigger configuration during `caesium job lint`
10. **Tests**: Unit tests for auth, param extraction, path matching. Integration test with a sample GitHub webhook payload.

### Phase 2: Event Ingestion & Routing (P0)

11. **`internal/trigger/event/event.go`**: Event trigger type implementing the Trigger interface
12. **`internal/trigger/event/matcher.go`**: Pattern matching (type glob, source filter, content filter)
13. **`internal/trigger/event/router.go`**: Singleton router that loads event triggers and dispatches matches
14. **`api/rest/controller/event/ingest.go`**: Event ingestion endpoint (`POST /v1/events`)
15. **`ingested_events` table**: Schema, model, GORM migration
16. **Executor update**: Register event triggers in the executor alongside cron triggers
17. **Linter rules**: Validate event trigger configuration
18. **Tests**: Unit tests for matcher, integration test for end-to-end event → trigger → run

### Phase 3: Trigger Chaining (P1)

19. **Internal event bridge**: Router subscribes to lifecycle events, converts to ingested event format
20. **Job metadata enrichment**: Include `job_alias` in bridged events so triggers can filter by alias
21. **Cycle detection (static)**: Build trigger dependency graph during lint/apply, reject cycles
22. **Cycle detection (runtime)**: `_trigger_depth` parameter tracking, configurable max depth
23. **Tests**: Integration test for A → B chain, cycle detection test

### Phase 4: Observability & CLI (P1)

24. **Webhook event pruner**: Background goroutine, configurable retention
25. **Event pruner**: Same for ingested events
26. **API endpoints**: `GET /v1/events/ingested`, `GET /v1/triggers/:id/events`
27. **CLI commands**: `caesium trigger fire`, `caesium trigger list`, `caesium trigger events`, `caesium event push`
28. **Prometheus metrics**: All metrics listed above
29. **UI updates**: Webhook event log on trigger detail page, event trigger visualization in DAG view

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Webhook flood (DoS via high-frequency POSTs) | Resource exhaustion | `CAESIUM_WEBHOOK_MAX_BODY_SIZE` limit. Per-path rate limiting (Phase 1.3 concurrency work). Log but drop payloads exceeding thresholds. |
| Infinite trigger chains | Runaway execution | Runtime depth guard (`_trigger_depth`). Static cycle detection at lint time. |
| JSONPath injection (malicious payloads) | Unexpected parameter values | JSONPath extraction always produces strings. Run parameters are environment variables — they don't execute. Validate extracted values against parameter schemas when available. |
| Event bus backpressure | Dropped internal events for chaining | Current bus drops events when channel is full (non-blocking). For chaining, this could mean missed triggers. Mitigation: increase channel buffer for the router subscriber, log dropped events as warnings, surface in metrics. |
| Path collisions (two triggers with same path) | Ambiguous routing | Allow it — fire all triggers matching the path. Document that paths should be unique per job. |
| Secret rotation | Downtime during rotation | Support array of secrets (try each in order). Document rotation procedure. |

---

## Examples

### GitHub Push Webhook

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

### S3 Object Arrival

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

### Trigger Chaining (Pipeline B Follows Pipeline A)

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

### CI/CD Completion Webhook

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: post-deploy-checks
trigger:
  type: http
  configuration:
    path: "ci/deploy-complete"
    secret: "secret://env/CI_WEBHOOK_SECRET"
    signatureScheme: bearer
    paramMapping:
      environment: "$.environment"
      version: "$.version"
      deploy_id: "$.id"
steps:
  - name: smoke-test
    image: tests:latest
    command: ["smoke.sh"]
    dependsOn: []
  - name: notify
    image: slack-notify:v1
    command: ["notify.sh"]
    dependsOn: [smoke-test]
```
