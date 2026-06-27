# Event-Driven Trigger Routing — Event Triggers + Chaining (WS2 + WS3)

Last updated: 2026-06-27

Caesium's trigger system today supports `cron` (timer-polled) and `http` (the
shipped WS1 webhook receiver: dedicated `/v1/hooks/*` routes, signature auth,
JSONPath param extraction, per-IP rate limiting, operator manual fire). Two gaps
remain, and they are the roadmap's single **P0** item (§1.2): a webhook path maps
1:1 to its jobs, so there is **no content-based routing** (one endpoint fanning
out to different jobs by event type/payload), and **no trigger chaining** (one
job's completion firing another). The internal event bus already carries
lifecycle events (`run_completed`, `run_failed`, …) and persists them for SSE,
but nothing routes them to triggers.

This plan ships the two remaining work streams from
[`design-event-triggers.md`](../../design-event-triggers.md): **WS2** — a reactive
`event` trigger type with a content matcher, a singleton router, a
`POST /v1/events` ingestion API, and durable event observability; and **WS3** —
trigger chaining as a special case of WS2 where the event source is the internal
lifecycle bus, guarded by static cycle detection (at lint/apply) and a runtime
chain-depth limit. Every new CLI verb and REST endpoint ships with an integration
test in `test/` that drives the real surface against a live server (per the
`CLAUDE.md` end-to-end-coverage gate); a green unit test that hand-feeds the
matcher proves the matcher, never the wiring.

The **UI surface** the design sketches (event-trigger visualization in the DAG
view, an event log on the trigger-detail page — design Phase 4 item 15) is **out
of scope here** and recorded as a deferred follow-on (see the Deferred note under
`## Streams`), the same way the data-plane causal verbs shipped CLI+REST-first and
got a dedicated [Data-Plane Memory UI](../completed/data-plane-memory-ui.md) plan
afterward.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work backlog,
`## Sequencing & Dependencies` captures cross-stream order, and
`## Acceptance Criteria` lists the gates that close out the entire plan. Any
agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies are
   satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).

## Source-Of-Truth Note

This plan implements WS2 + WS3 of [`design-event-triggers.md`](../../design-event-triggers.md).
**The design doc is authoritative for INTENT and SCOPE** (what WS2/WS3 must do).
**But where this plan records an explicit correction or amendment below — each
verified against the real code — the PLAN's corrected contract wins**, and the
design is reconciled to match in N-1. Adversarial review found the design
misdescribes the *current* code in places (e.g. it lists `FireWithParams` on the
shared `Trigger` interface, but the real `internal/trigger.Trigger` exposes only
`Listen`/`Fire`/`ID` — `FireWithParams` is concrete-only) and under-specifies others,
so **W1/W2 implementers follow this plan's corrected contracts, NOT the stale design
text, for the enumerated items.** No item may add a NEW trigger type, endpoint, or
config knob *beyond these recorded amendments* without first amending the design.
Strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) §1.2 (the roadmap wins on priority/status
disagreements). The job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go); the design states
`Trigger.Configuration` is already a flexible map, so `event` adds **type-specific
validation, not a structural schema change** — if an item finds it needs a struct
change, stop and reconcile against the design before proceeding.

**Amendments to the design (surfaced by adversarial review of this plan, folded
back into `design-event-triggers.md` by N-1):** (1) `POST /v1/events` gets a
dedicated `CAESIUM_EVENT_INGEST_API_KEY` — the design said "reuse webhook auth,"
but webhook auth is per-path signature validation with no analogue for a path-less
endpoint; (2) a durable `event_trigger_matches` table backs `GET /v1/triggers/:id/events`
— the design implied recomputation, which would lie after trigger edits; (3) the
webhook receiver bridges into the event router so content routing reaches
`/v1/hooks/*` traffic (the design states the router serves both the ingestion API
*and* the webhook controller, but didn't enumerate the bridge as work); (4) the
shared `Trigger` interface is `Listen`/`Fire`/`ID` only — `EventTrigger` adds its own
concrete `FireWithParams` and the router holds concrete `*EventTrigger` (the design's
interface snippet is wrong about the current code); (5) `Router.Route` returns a
structured `RouteResult` through one persist-and-fire boundary (so `event_trigger_matches`
is written atomically), not a fire-and-return-`[]uuid.UUID`. These five are the
contracts W1/W2 follow; N-1 folds all of them into `design-event-triggers.md`.

## Progress (as of 2026-06-27)

No implementation waves have shipped yet. WS1 (HTTP webhook triggers) shipped
previously and is the foundation this plan builds on (`internal/trigger/http/`,
`api/rest/controller/webhook/`, the `internal/event` bus + store). The first wave
is the next eligible run of the `exec-plan-wave` skill against this doc — the
leaf-eligible streams are **A** (the engine) and **H-1** (the integration harness).
(Adversarial review moved D1 out of the first wave: it collides with A1 + B3 on
`webhook.go`/`models.go`/`start.go`.)

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Event-trigger evaluation engine — `event` type, matcher, `EventTrigger`, singleton router, executor + startup wiring | **P0** | Not started |
| B | Event ingestion + observability REST API — keyed `POST /v1/events`, the webhook→event bridge, durable-match reads (`GET /v1/events/ingested`, `GET /v1/triggers/:id/events`) | **P0** | Not started |
| C | Trigger chaining — internal lifecycle-bus bridge + static & runtime cycle detection | P1 | Not started |
| D | Webhook event log + trigger CLI — durable `webhook_events`, `caesium event push`, `caesium trigger events` | P1 | Not started |
| H-1 | Integration harness — exercise the event path on the live integration server | — | Not started |
| N-1 | Docs — roadmap §1.2 flip, design banner, schema reference, examples, README | — | Not started |
| (UI) | Event-trigger visualization + event log (design Phase 4 #15) | — | **Deferred** — candidate for a follow-on UI plan |

## Streams

### Stream A — Event-trigger evaluation engine (WS2 core)

The reactive substrate every other stream builds on: a new `event` trigger type,
a content matcher, the `EventTrigger` (implementing the existing `Trigger`
interface in `internal/trigger/trigger.go`), and a **singleton router** that loads
event triggers and dispatches matches — registered in the executor
(`internal/executor/executor.go`, today a `sync.Map` + 60s cron loop) and wired at
startup. Largest blast radius, so it merges first. Mirror the shipped HTTP-trigger
package (`internal/trigger/http/`) for the `FireWithParams` → list-jobs → merge →
`job.Run()` shape.

- [ ] A1. Add the `ingested_events` GORM model + a typed store + a retention
      pruner — **and the `event_trigger_matches` model** (`event_id`, `trigger_id`,
      `matched_at`, `runs_started`, skip/error metadata), the **durable truth source**
      the observability reads (B2) join against. (Recomputing matches from the
      *current* trigger config would lie after a trigger is edited/deleted, and can't
      report truthful `runs_started` — so the match must be recorded at routing time,
      not re-derived.) Register both models in the `All` slice; add
      `CAESIUM_EVENT_RETENTION` (default `7d`). Events/matches are for
      observability/debugging, NOT replay (fire-and-forget). The pruner is an
      env-gated background goroutine started in `cmd/start/start.go` (mirror an
      existing pruner).
      Files: new `internal/models/ingested_event.go`, new
      `internal/models/event_trigger_match.go`, `internal/models/models.go`,
      new `internal/event/ingest_store.go` (or `internal/trigger/event/store.go`),
      `pkg/env/env.go`, `cmd/start/start.go`.
- [ ] A2. Implement the event matcher: `EventPattern.Matches(evt)` — type match
      (exact or glob, e.g. `webhook.*`), optional exact `source`, and every
      `filter` key (dot-path into the event data) equal to its expected string
      value; plus `extractField` for nested dot-path lookups. Pure logic, fully
      unit-tested (glob, source mismatch, nested filter, missing field, type
      coercion).
      Files: new `internal/trigger/event/matcher.go` (+ `matcher_test.go`).
      Depends on: A1 (the `IngestedEvent` type).
- [ ] A3. Add `TriggerTypeEvent` to `internal/models/trigger.go` and the
      `EventTrigger`. **Interface contract (verified against the code):** the shared
      `internal/trigger.Trigger` interface has ONLY `Listen(ctx)`, `Fire(ctx)`,
      `ID()` — `FireWithParams` exists only on the *concrete* HTTP trigger, NOT the
      interface. So `EventTrigger` satisfies `Trigger` (Listen/Fire/ID) **and**
      exposes its own `FireWithParams(ctx, params)` (merge `defaultParams` under
      JSONPath-extracted `paramMapping`, list the trigger's jobs, run each — same as
      the HTTP trigger); the router holds **concrete `*EventTrigger` values** so it
      calls `FireWithParams` directly — do NOT widen the shared interface (cron
      stays unchanged). Add **event-trigger config
      validation**: `events` present with ≥1 pattern; each pattern has a `type`;
      `filter` values are strings; `paramMapping` values are valid JSONPath.
      Files: `internal/models/trigger.go`, new `internal/trigger/event/event.go`,
      `pkg/jobdef/definition.go` (type-specific trigger validation).
      Depends on: A2.
- [ ] A4. Add the singleton event `Router` (`internal/trigger/event/router.go`):
      `Route(ctx, evt)` returns a structured **`RouteResult`** (the matched triggers
      + each trigger's fire outcome — run id / skipped / error), **NOT a bare
      `[]uuid.UUID`**: all three event sources (B1 ingestion, B3 webhook, C1
      lifecycle) go through ONE **persist-and-fire boundary** so `ingested_events` +
      `event_trigger_matches` are written **atomically with the routing outcome**
      (B2's durable truth) — `Route` either persists the match rows itself or returns
      enough for the caller to persist *before* launching jobs. Register event
      triggers in the executor alongside cron/http; add an event-trigger lookup
      (`ListByEventPattern` / load-all) to the trigger service. Add
      `caesium_event_trigger_matches_total{trigger_id,event_type}`.
      **Router lifecycle (load-bearing — do NOT load only at startup):** event
      triggers are created/updated/deleted via `caesium job apply` and the trigger
      Create/Update/Delete service, so the router must **reload/invalidate on every
      trigger mutation, before the API returns** — otherwise it misses triggers
      created after boot and keeps firing deleted/changed configs until restart.
      (HTTP triggers stay fresh via per-request `ListByPath`; the router has no
      per-request resolve, so it owns an explicit refresh hook.) **Pin the hook to
      ALL mutation seams, not just the REST controller:** git sync BYPASSES the REST
      layer — `cmd/start/start.go` launches `git.Watch` with an internal `Importer`,
      and `git_sync` calls `importer.ApplyWithOptions` per definition + `PruneMissing`
      (soft-deletes) — so the refresh must fire after the trigger-service
      Create/Update/Delete commits AND after `Importer.ApplyWithOptions` /
      `PruneMissing` commit, so REST apply, CLI-via-REST, and git sync all hit the
      same seam.
      Files: new `internal/trigger/event/router.go`, `internal/executor/executor.go`,
      `cmd/start/start.go`, the trigger service (`api/rest/service/trigger/` + its
      Create/Update/Delete), the jobdef apply paths (`internal/jobdef/` Importer
      `ApplyWithOptions`/`PruneMissing` + `api/rest/controller/jobdef/`),
      `internal/metrics/metrics.go`.
      Depends on: A3.
      Acceptance probe (integration): apply an event trigger AFTER boot → ingest a
      matching event → the run fires (apply-then-ingest); and update/delete a trigger
      takes effect without a restart.

### Stream B — Event ingestion + observability REST API (WS2 surface)

The HTTP surface over the engine: the ingestion endpoint that drives the router,
and the observability reads. Reuse the shipped webhook controller's auth +
rate-limit middleware (`api/rest/controller/webhook/`) — do NOT invent a new auth
path. There is already an `api/rest/controller/event/` package (SSE) and an
`api/rest/controller/trigger/` package (List/Post/Get/Patch/Fire bound in
`api/rest/bind/bind.go` lines ~127-133) — extend those, don't fork them.

- [ ] B1. Add `POST /v1/events` (`{type, source, data}`): **authenticate via a
      dedicated `CAESIUM_EVENT_INGEST_API_KEY`** (mirroring the shipped
      `CAESIUM_MANUAL_TRIGGER_API_KEY` gate on `POST /v1/triggers/:id/fire` — the
      webhook signature auth is *per-path/per-trigger* and does NOT fit a path-less
      ingestion endpoint, so it cannot be "reused" as-is; this is an explicit amend
      of the design, which left ingestion auth unspecified) + the per-IP rate-limit
      middleware. Persist the event to `ingested_events`; call `router.Route`;
      **persist one `event_trigger_matches` row per matched trigger transactionally
      with the routing outcome** (so B2 reads truth); fire matched triggers; return
      truthful `{event_id, matched_triggers, runs_started}`. Add
      `caesium_events_ingested_total{type,source}`.
      Files: new `api/rest/controller/event/ingest.go`, `api/rest/service/event/`,
      `api/rest/bind/bind.go`, `pkg/env/env.go`, `internal/metrics/metrics.go`.
      Depends on: A4.
- [ ] B2. Add the observability reads: `GET /v1/events/ingested` (filter by
      `type`/`source`/time window, bounded+paginated) and `GET /v1/triggers/:id/events`
      (read the `event_trigger_matches` table joined to `ingested_events` — the
      durable match record, **NOT** a re-evaluation of the current patterns, which
      would lie after a trigger edit).
      Files: `api/rest/controller/event/`, `api/rest/controller/trigger/`,
      `api/rest/service/event/`, `api/rest/bind/bind.go`.
      Depends on: A1 (both models) + B1 (which writes the match rows).
- [ ] B3. **Bridge the webhook receiver into the event router** — without this,
      content-based routing never reaches the shipped `/v1/hooks/*` traffic and the
      P0 problem stays half-solved (the design exposes the router to *both* the
      ingestion API AND the webhook controller). After the receiver's auth/rate-limit,
      convert the receipt to an `IngestedEvent` (`type:"webhook"`, source from the
      path/header) and route it through the same persist-and-fire boundary as B1.
      **Ordering is load-bearing — the real receiver launches the HTTP-trigger job
      goroutines BEFORE the 202** (`api/rest/controller/webhook/webhook.go:89`,
      `:126-139`), so a bridge failure *after* that point would either 5xx (an
      external webhook retry then double-fires the already-launched HTTP job) or 202
      (silently dropping event routing + match observability). Fix the order:
      persist the ingested event + match-intent **before** the irreversible HTTP
      fire, OR key the webhook receipt idempotently so a retry can't double-fire.
      **Define the response when bridge persistence/routing fails**, and the
      double-fire contract: an HTTP trigger and an event trigger may both match one
      webhook — both fire (dedupe only *within* the event-trigger set), documented.
      Files: `api/rest/controller/webhook/webhook.go` (reorder around the existing
      `FireHTTPTrigger`; routes through the shared persist-and-fire boundary from A4).
      Depends on: A4.

### Stream C — Trigger chaining (WS3)

Chaining is WS2 with the event source being the internal lifecycle bus
(`internal/event/bus.go`, which already publishes `run_completed`/`run_failed` and
persists via `internal/event/store.go`). Builds directly on Stream A's router —
**same file (`router.go`), so it sequences after A**.

- [ ] C1. Bridge internal lifecycle events into the router: subscribe to
      `TypeRunCompleted`/`TypeRunFailed`/`TypeRunTerminal`, convert each to an
      `IngestedEvent{Source:"caesium"}`, enrich with `job_alias` (so triggers can
      `filter: {job_alias: …}`), and `Route` it. Raise the subscriber buffer and
      log+meter dropped events (the bus drops on a full channel; chaining must not
      silently lose completions).
      Files: `internal/trigger/event/router.go`, `internal/event/` (bus subscribe).
      Depends on: A4.
- [ ] C2. Cycle detection — **static (BATCH-level)**: build the trigger-dependency
      graph across **all definitions being applied PLUS the existing DB triggers**
      and reject cycles (A→B→A) **before any definition is persisted**. This must live
      in the **batch validation path** (`internal/jobdef/` collect/validate), NOT the
      single-`Definition` validator in `pkg/jobdef/definition.go` — that sees one job
      at a time and apply persists definitions one-by-one, so a cross-job cycle can't
      be proven there and apply could partially write one side. Wire it into CLI
      `caesium job lint`, the REST lint controller, the REST apply controller
      (pre-write), and git sync/apply. **Runtime**: a `_trigger_depth` param
      incremented per chain hop, rejecting with `ErrTriggerChainDepthExceeded` past
      `CAESIUM_MAX_TRIGGER_DEPTH` (default 10). Add `caesium_trigger_chain_depth`
      (histogram) + `caesium_trigger_chain_rejected_total`.
      Files: `internal/trigger/event/router.go` (runtime depth), `internal/jobdef/`
      (batch cycle validator), `api/rest/controller/jobdef/` (apply + lint),
      `cmd/job/lint.go`, `pkg/env/env.go`, `internal/metrics/metrics.go`.
      Depends on: C1.
      Test: a cyclic batch is rejected with **no partial persistence**.

### Stream D — Webhook event log + trigger CLI (WS2/WS3 observability)

The durable webhook log the design folded out of WS1, plus the operator CLI. D1 is
functionally independent of the engine, but it **shares files** with A1
(`models.go`/`start.go`) and B3 (`webhook.go`), so it lands in **W3** (after those),
NOT the first wave; D2 calls the Stream B endpoints.

- [ ] D1. Add the durable `webhook_events` log: model + `All` registration + an
      env-gated pruner with `CAESIUM_WEBHOOK_EVENT_RETENTION` (default `7d`);
      record each webhook receipt in the receiver.
      Files: new `internal/models/webhook_event.go`, `internal/models/models.go`,
      `pkg/env/env.go`, `cmd/start/start.go`, `api/rest/controller/webhook/webhook.go`.
      Depends on: A1 + B3 (shared `models.go`/`start.go` and `webhook.go` — sequence
      after them per the collision matrix; not a functional dependency).
- [ ] D2. Add the trigger/event CLI: `caesium event push --type --source --data '{}'`
      (`POST /v1/events`) and `caesium trigger events <alias>`
      (`GET /v1/triggers/:id/events`), as new top-level Cobra command groups
      appended to the `cmds` slice in `cmd/execute.go`.
      Files: new `cmd/event/`, new `cmd/trigger/`, `cmd/execute.go`.
      Depends on: B1 + B2.

#### Deferred — Event-trigger UI (design Phase 4 item 15)

Surfacing event triggers in the web UI (the `event` trigger type in the DAG/trigger
view; an event log on the trigger-detail page driving `GET /v1/triggers/:id/events`)
is **deferred to a follow-on UI plan**, mirroring how the data-plane causal verbs
shipped CLI+REST-first and earned a dedicated
[Data-Plane Memory UI](../completed/data-plane-memory-ui.md) plan. It is not part of
this plan's acceptance criteria. When this backend plan completes, draft that UI
plan against the Stream B endpoints.

## Harness Strengthening

- [ ] H-1. Ensure the integration server exercises the real event path: set
      `CAESIUM_EVENT_INGEST_API_KEY` (so the Stream A/B/C scenarios can authenticate
      `POST /v1/events`) and a low `CAESIUM_MAX_TRIGGER_DEPTH` if the chain tests need
      a tight bound, on the `just integration-up` / `just integration-test` server,
      so the scenarios drive the live surface rather than an internal call. If event
      triggers land behind an enable flag, set it here (mirror the lineage
      `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the `CLAUDE.md` gate calls out).
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [ ] N-1. Flip `docs/roadmap.md` §1.2 from "WS2/WS3 proposed" to **Shipped**;
      update the [`design-event-triggers.md`](../../design-event-triggers.md)
      `> Status:` banner (WS2/WS3 shipped) **and reconcile all FIVE design
      amendments into the doc body** (the `CAESIUM_EVENT_INGEST_API_KEY` auth knob,
      the `event_trigger_matches` table, the webhook→event bridge, the corrected
      `Trigger` interface — `Listen`/`Fire`/`ID` only, `FireWithParams` concrete on
      `EventTrigger` — and the `Router.Route` → `RouteResult` persist-and-fire
      boundary; see the Source-Of-Truth Note); document the `event` trigger fields
      (events/patterns/filter/paramMapping) in `docs/job-schema-reference.md`,
      `docs/job-definitions.md`, and `docs/caesium-job-llm-reference.md`; add an
      `event`-trigger and a chaining example under `docs/examples/`; index this plan
      in `docs/README.md`. **Use backtick/inline-code form for any
      `exec-plans/...` reference in `docs/README.md`** — the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects clickable
      subdirectory links (PR #245 hit exactly this). Runs last.
      Files: `docs/roadmap.md`, `docs/design-event-triggers.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–D (runs last, after the runtime ships).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B (all three items), C, and D2 consume the
  router, the match table, or the ingestion endpoint it backs. A merges first
  (largest blast radius).
- **Stream B**: B1 (ingestion) and B3 (webhook bridge) depend on A4 (the router);
  B2 depends on A1 (both models) + B1 (which writes the match rows it reads).
- **Stream C** depends on A4 — C1/C2 extend the *same* `internal/trigger/event/router.go`,
  so C runs **after** A, not in parallel with it.
- **Stream D1** (durable webhook log) shares `webhook.go` with **B3** and
  `models.go`/`start.go`/`env.go` with **A1** — so it runs **after** B3 + A1, NOT
  in the first wave (see conflicts below). **D2** depends on B1 + B2.
- **H-1** is independent (justfile/CI/test harness) and supports the A/B/C
  integration scenarios; land it in the first wave so the engine's end-to-end gate
  has a live, authenticated surface to drive.
- **N-1** runs last, after A–D ship, so the roadmap/schema/design docs reflect
  reality (and the three design amendments are reconciled).

**Suggested waves (revised after adversarial review):**
- **W1 = A (A1→A2→A3→A4) + H-1.** A is one strict chain; D1 is *deferred to W3*
  because it collides with A1 + B3 on shared files.
- **W2 = B (B1, B2, B3) + C (C1→C2).** Both unblocked once A's router is in. B and
  C touch *different* core files (B → controllers/webhook; C → router/jobdef); they
  share only the additive `env.go` + `metrics.go` append sites.
- **W3 = D1 + D2 + N-1.** D1 now lands after B3's `webhook.go` edits and A1's
  `models.go`/`start.go` edits; D2 after B's endpoints; N-1 last.

**Within-stream order:** A1 → A2 → A3 → A4 (strict — model+match table, then
matcher, then trigger+validation, then router). B1 → B2 (B2 reads B1's match rows);
B3 parallel to B1 (different files). C1 → C2. D1 standalone within D; D2 after B.

**Cross-stream file conflicts:**

- `internal/trigger/event/router.go` — A4 *creates* it; C1, C2 *extend* it. **Sequence
  A → C** (already a dependency). B1/B3 only *call* `router.Route` from their
  controllers — they do NOT edit `router.go`, so B and C don't collide here.
- `api/rest/controller/webhook/webhook.go` — **B3** (bridge to router) and **D1**
  (durable receipt log) both edit the receiver. **Sequence B3 (W2) → D1 (W3)**; never
  the same wave.
- `internal/models/models.go` — A1 (`ingested_events` + `event_trigger_matches`) and
  D1 (`webhook_events`) append to the `All` slice; A1 lands in W1, D1 in W3, so no
  same-wave collision.
- `cmd/start/start.go` — A1 (event/match pruners + router start) and D1 (webhook-log
  pruner) both add startup wiring; A1 (W1) before D1 (W3).
- `pkg/env/env.go` — A1 (`CAESIUM_EVENT_RETENTION`), B1 (`CAESIUM_EVENT_INGEST_API_KEY`),
  C2 (`CAESIUM_MAX_TRIGGER_DEPTH`), D1 (`CAESIUM_WEBHOOK_EVENT_RETENTION`) all add
  fields. Additive across waves; within W2, B1 + C2 append on different lines — flag
  for a clean rebase.
- `internal/metrics/metrics.go` — A4 (W1), then B1 + C2 (both W2) each add a collector
  (two edit sites: the `var (...)` block + `Register()`). B1 + C2 are the one
  genuine same-wave additive overlap in W2 — call it out to the W2 agents.
- `api/rest/bind/bind.go` — B1 + B2 add routes (same stream B). `cmd/execute.go` — D2
  appends command groups (single stream). Additive.
- `internal/jobdef/` batch validator — C2 (static cycle detection) lives here, NOT in
  `pkg/jobdef/definition.go` (the single-`Definition` validator can't see cross-job
  cycles). A3's event-config validation *does* live in `pkg/jobdef/definition.go` —
  different file from C2, so no A3↔C2 collision.
- No `internal/cache/hash.go` change: triggers do not participate in the step
  execution hash, so the cache key is untouched.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (A, B, C, D):** an integration scenario in
  `test/` that drives the **real surface** — `POST /v1/events` against the live
  server, or the CLI binary via the `s.runCLI*` helpers — and asserts observed
  output (matched/run counts, the fired run, the chain result). A unit test that
  hand-builds an `IngestedEvent` and calls the matcher proves the matcher, not the
  wiring — both are required.
- **New metric (A4, B1, C2):** assert via `internal/metrics/testutil` in a
  `*_test.go`; the collector must also appear in `Register()`.
- **Job-schema validation (A3, C2):** `caesium job lint --path docs/examples/`
  green on the new `event`-trigger + chaining examples; a cyclic chain rejected at
  lint.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the event-trigger engine** is a runtime feature: an `event`
   trigger with content patterns loads on startup, and a matching event fires its
   jobs with extracted params. Closed by a `test/` integration scenario:
   ingest → match → fan-out run to two jobs, green in CI.
2. **Stream B — the ingestion + observability API + webhook bridge** is live:
   `POST /v1/events` (keyed by `CAESIUM_EVENT_INGEST_API_KEY`) returns truthful
   `{event_id, matched_triggers, runs_started}`; a webhook to `/v1/hooks/*` also
   fans out to matching event triggers (content routing reaches webhook traffic);
   and `GET /v1/triggers/:id/events` reads the durable `event_trigger_matches` table
   (still truthful after a trigger is edited). Closed by integration scenarios for
   both the ingestion endpoint and the webhook bridge, hitting the live server.
3. **Stream C — trigger chaining** works: one job's `run_completed` fires a
   downstream job via the internal bridge, a static cycle is rejected at
   `caesium job lint`, and a runtime chain past `CAESIUM_MAX_TRIGGER_DEPTH` is
   rejected with the depth metric incremented. Closed by an A→B chain scenario +
   a cycle-detection scenario (static and depth), green in CI.
4. **Stream D — webhook log + CLI** ships: webhook receipts persist to
   `webhook_events` and prune on retention; `caesium event push` and
   `caesium trigger events` drive the real endpoints, asserted via `runCLIStdout`
   (clean, parseable stdout captured separately from stderr).
5. **H-1 — the integration server** exercises the event path (ingestion auth
   configured / enable flag set), so the Stream A/C scenarios run against the live
   binary in CI, not an internal call.
6. **N-1 — docs reflect reality:** `docs/roadmap.md` §1.2 flipped to Shipped, the
   design-doc `> Status:` banner updated, the `event` trigger fields documented in
   the schema references with working `docs/examples/` manifests, and this plan
   indexed in `docs/README.md`.
7. **Cross-cutting:** `docs/roadmap.md`, `docs/design-event-triggers.md`, and this
   plan's per-stream `## Progress` entries reflect every shipped stream and match
   the merged PRs. (The Event-trigger UI remains explicitly deferred to a follow-on
   plan — not a gate here.)

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (event-trigger-routing <wave>-<stream>)` — e.g.
   `Add the event-trigger evaluation engine (event-trigger-routing W1-α)`. GitHub
   appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-event-triggers.md`](../../design-event-triggers.md) — the design
  of record (WS1 shipped; WS2/WS3 are this plan). Source of truth.
- [`docs/roadmap.md`](../../roadmap.md) §1.2 Event-Driven Trigger Routing — the P0
  strategic entry this plan closes.
- [`docs/job-schema-reference.md`](../../job-schema-reference.md),
  `docs/job-definitions.md`, `docs/caesium-job-llm-reference.md` — the trigger
  schema docs N-1 extends with the `event` type.
- [Data-Plane Memory UI](../completed/data-plane-memory-ui.md) — precedent for the
  deferred follow-on UI plan (causal verbs shipped CLI+REST-first, UI after).
- `internal/trigger/http/`, `api/rest/controller/webhook/`, `internal/event/`
  (bus + store) — the shipped WS1 surfaces this plan builds on.
