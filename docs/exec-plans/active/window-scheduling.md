# Deadline-Window Scheduling — Declare a Deadline, Not a Cron Minute

Last updated: 2026-07-03

Cron makes users encode a *guess* about the best start time when what they
actually hold is a *constraint* about the finish time. Forty nightly jobs pinned
at `0 0 * * *` spike the cluster at midnight then idle for hours; "run at 02:30"
silently rots into a missed deadline when the job slows; and a movable batch job
is nailed to a minute chosen with no view of cluster load, spot price, or grid
carbon. Caesium already *detects* a blown deadline (`metadata.sla.completedBy`,
`internal/notification/watcher.go`) but nothing uses the deadline to decide when
to *start*. This plan ships the design of record in
[`design-window-scheduling.md`](../../design-window-scheduling.md): a job declares
an execution **window** plus a completion **deadline** (`window 00:00 → 05:00,
finish by 06:00`), and Caesium picks the start moment from predicted duration
(p95 from history), cluster load, optional cost/carbon signals, and priority —
with a hard rule that force-starts at the latest safe moment regardless of
signals.

The feature is a **queueing/scheduling policy over machinery that already
exists**, not an autoscaler: parked runs are rows in the durable dqlite
`run_queue` (`internal/models/run_queue.go`), released through the same atomic
admission path triggers already use (`store.AdmitRun`,
`internal/run/store.go:1044`), leader-gated by the same `dqlite.IsLocalLeader`
check the run-queue dequeuer uses (`internal/runqueue/dequeuer.go`). No windows
declared ⇒ nothing paid. The work decomposes along the design's own phasing:
**P0** — window trigger type, parking columns, leader-gated scheduler with
`earliest`/`latest` objectives, p95 predictor + cold-start floor, force-start,
derived `completedBy` SLA, `caesium job window`, REST, integration tests; **P1**
— running-count load gate; **P2** — pluggable static/event cost-carbon signals,
the `cheapest` objective, and the UI window bar.

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
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Source-Of-Truth Note

[`docs/design-window-scheduling.md`](../../design-window-scheduling.md) is
**authoritative for INTENT and SCOPE** — when this plan and the design doc
disagree, the design doc wins, and the plan is reconciled to match. The design is
still a brainstorm/design-status banner, so N-1 flips it to "active — this plan"
when the first runtime item merges. Strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) (Phase 4 Data-Plane Differentiators table,
currently **P3**); the roadmap wins on priority/status disagreements. The
job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go): `Trigger.Configuration`
is already a flexible `map[string]any` (verified `definition.go:139-143`), so
`window` — exactly like the shipped `event` type — adds **type-specific
configuration validation, not a structural schema change**; if an item finds it
needs a struct change, stop and reconcile against the design before proceeding.

Two code-verified facts that shape the streams: (1) `run_queue` is a
catalog-resident table, **not** a hot per-run shard table (`hotTables` in
`pkg/db/router.go:23-28` and `hotPathModels()` in `pkg/db/db.go:281-291` list
only `job_runs`/`task_runs`/`callback_runs`/`execution_events`/`run_checkpoints`),
so the four nullable window columns need **no** hot-table router edit and no new
`models.All` entry (`&RunQueue{}` is already registered,
`internal/models/models.go:10`). (2) `TriggerTypeEvent` and the executor's
`window`-adjacent listing loop already exist (`internal/models/trigger.go:14-18`,
`internal/executor/executor.go:38-42`), so `window` is an additive third listing
request, not new machinery.

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from the
`design-window-scheduling.md` design of record; the first wave is the next
eligible run of the `exec-plan-wave` skill against this doc. Leaf items eligible
for the first wave are **A1**, **A2**, **B1**, and **H-1** (no unmet
`Depends on:` edges).

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Window trigger type, YAML schema + lint satisfiability, DST open-resolution, `run_queue` parking columns, implicit `completedBy` SLA | **P0** | Not started |
| B | Window scheduler engine — p95 predictor, leader-gated loop, force-start, `earliest`/`latest` objectives, restart-safety reconciler, bus events + metrics | **P0** | Not started |
| C | Operator surface — `GET /v1/jobs/:id/window` + `GET /v1/window/parked` REST, `caesium job window` CLI | **P0** | Not started |
| D | Load + cost/carbon signal gating — running-count gate (P1), pluggable static/event signals + `cheapest` objective (P2) | P1/P2 | Not started |
| E | Frontend — planned-start badge + rationale (P0), run-history window bar (P2) | P2 | Not started |
| H-1 | Integration harness — enable `CAESIUM_WINDOW_SCHEDULING_ENABLED` on the live integration server | — | Not started |
| N-1 | Docs — roadmap row, design banner, schema references, example manifest, README index | — | Not started |

## Streams

### Stream A — Window trigger, schema, DST resolution & parking columns (P0 foundation)

The declarative + persistence substrate every other stream builds on: the
`window` trigger type, its YAML configuration validation and lint-time
satisfiability check, DST-correct open resolution, the four nullable `run_queue`
columns that make a parked run a durable row, and the implicit `completedBy` SLA
so a blown window deadline alerts through the shipped watcher with zero new
machinery. Largest blast radius (schema + models + admission read path), so it
merges first. Mirror the shipped `event`-trigger config-validation shape
(`pkg/jobdef/definition.go:497-519`, `validateEventTriggerConfiguration`) and the
cron schedule/timezone parsing this trigger reuses
(`internal/trigger/cron/cron.go:222-254`, `nextTick` at `:325-331`).

- [ ] A1. Extend `run_queue` with four nullable window columns —
      `WindowOpen`, `WindowClose`, `WindowDeadline` (all `*time.Time`, resolved
      UTC instants) and `LogicalDate *time.Time` — plus a unique partial index
      `(job_id, logical_date)` for idempotent parking. Make the two populations
      disjoint via `window_deadline`: the **existing** dequeuer keeps draining
      only concurrency-overflow rows — add `AND window_deadline IS NULL` to its
      job listing (`internal/runqueue/dequeuer.go:109-114`) and to
      `DequeueNextRun` (`internal/run/store.go:3729-3745`) — so the dequeuer never
      touches window rows and the window scheduler (Stream B) owns them. Add a
      typed `ParkWindowRun(...)` store method that inserts a parked row
      (`claimed_by=''`, window columns set) idempotently against the unique index.
      No `models.All` change (`&RunQueue{}` is already registered) and **no**
      hot-table router edit (`run_queue` is catalog-resident — see the
      Source-Of-Truth Note).
      Files: `internal/models/run_queue.go`, `internal/runqueue/dequeuer.go`,
      `internal/run/store.go`.
- [ ] A2. Add `TriggerTypeWindow`/`TriggerWindow` and window trigger config
      validation: accept `window` in `internal/models/trigger.go:14-18` and in the
      jobdef trigger validator (`pkg/jobdef/definition.go:497-519` +
      `pkg/jobdef/schema.go:20-22`) with a new
      `validateWindowTriggerConfiguration` (require a valid 5-field `cron` per the
      existing `robfig/cron` parser at `cron.go:61-67`; `deadline` present as
      `HH:MM`; optional `close < deadline`; optional `timezone` resolvable via
      `time.LoadLocation`; optional `buffer` parseable; `objective ∈
      {earliest,latest,cheapest}` default `earliest`). Add the **lint
      satisfiability** check (`open + buffer ≤ deadline` modulo rollover; `close <
      deadline`) surfaced through `caesium job lint`. Add `window` as a third
      listing request in the executor loop (`internal/executor/executor.go:38-42`).
      Files: `internal/models/trigger.go`, `pkg/jobdef/definition.go`,
      `pkg/jobdef/schema.go`, `internal/executor/executor.go`, `cmd/job/lint.go`.
- [ ] A3. Implement the `internal/trigger/window` package satisfying the shared
      three-method `Trigger` interface (`Listen`/`Fire`/`ID`,
      `internal/trigger/trigger.go:10-14`): `Listen` waits for the next window
      *open* exactly as cron waits for its tick (`cron.go:82-104`) but on fire
      **parks a durable `run_queue` row** (via A1's `ParkWindowRun`) stamped with
      logical date, resolved open, effective close, and deadline — it does NOT
      launch the job; the in-process `time.After` is only a prompt-parking
      optimization, never a correctness dependency. Resolve open through the cron
      schedule in the configured location and derive `deadline`/`close` via
      `time.Date` on the logical date in that location. **DST policies (tested):**
      spring-forward gap → first valid instant after it; ambiguous fall-back →
      first (earlier-UTC, conservative) occurrence; DST-collapsed window
      (`open ≥ forceAt`) → park and immediately mark for force with a
      `window_collapsed` warning.
      Files: new `internal/trigger/window/window.go` (+ `window_test.go`), reuses
      `internal/trigger/cron` schedule/timezone parsing.
      Depends on: A1 + A2.
- [ ] A4. Derive an implicit `sla.completedBy` from the window deadline at apply
      time when the job declares no SLA, so a blown deadline alerts through the
      shipped watcher (`internal/notification/watcher.go:136-246`) with zero new
      machinery — deadline *enforcement before the fact* is Stream B; deadline
      *alerting after the fact* is reused verbatim. Wire the derivation into the
      importer's SLA marshaling (`internal/jobdef/importer.go:413,435,1153`). Note
      the shipped `resolveCompletedBy` is UTC-only `HH:MM`
      (`watcher.go:386-400`); the derived value is a best-effort UTC mapping of
      the tz-aware window deadline (tz-aware enforcement lives in Stream B, which
      does not reuse this resolver).
      Files: `internal/jobdef/importer.go`, `pkg/jobdef/definition.go` (SLA
      derivation helper).
      Depends on: A2.

### Stream B — Window scheduler engine (P0 core)

The engine that turns parked rows into timely starts: a new `internal/windowsched`
package structured like the run-queue dequeuer — ticker, `DrainOnce`, and the
**same leadership gate** (`LeaderCheck` → `dqlite.IsLocalLeader`,
`internal/runqueue/dequeuer.go:21,94-103`, wired in
`cmd/start/start.go:174-184`) — so only the leader releases window rows and two
nodes never double-start a parked run. Release is `store.AdmitRun(...)`
(`internal/run/store.go:1044`) through normal atomic admission
(`store.admit`, `:711-778`) — the scheduler holds **no new write authority** and
never bypasses concurrency. Depends on Stream A (the trigger, columns, and
`ParkWindowRun`).

- [ ] B1. Add the duration predictor
      (`internal/windowsched/predictor.go`): `P95(jobID)` over the last N
      (`CAESIUM_WINDOW_P95_SAMPLES`, default 20) succeeded, non-quarantined runs,
      using the same `completed_at − started_at` duration shape the stats service
      uses (`api/rest/service/stats/stats.go:72-77`, tz/driver-aware `durationExpr`).
      **Cold-start policy (required):** fewer than 3 completed runs → no p95 →
      caller degrades to `forceAt = windowOpen` (starts at open, i.e. cron
      behavior); `metadata.runTimeout`, when set, caps the assumed duration.
      Injectable clock, fully unit-tested (p95, cold-start floor, runTimeout cap).
      Files: new `internal/windowsched/predictor.go` (+ `predictor_test.go`).
- [ ] B2. Implement the leader-gated scheduler loop
      (`internal/windowsched/scheduler.go`): ticker + `DrainOnce`, evaluate parked
      rows ordered `priority DESC, (deadline − now − p95) ASC`, compute
      `forceAt = min(windowClose, windowDeadline − p95 − buffer)`, and the P0 gate
      chain — `now ≥ forceAt` → release **FORCED** (unconditional); `now <
      windowOpen` → skip; `objective==earliest` → release **PLANNED**;
      `objective==latest` → skip until `forceAt`. Release = `store.AdmitRun(...)`
      with the parked row's params/priority/logical date (forced releases stamped
      priority `high` so the dequeuer drains them first,
      `store.go:3737-3745`); record a one-sentence **rationale** string per
      decision (a gate chain, not a weighted score). Emit bus events
      `window_planned` / `window_forced` / `window_deadline_at_risk` (the last
      when a release queues rather than starts) + `window_collapsed`
      (`internal/event/`); add collectors `caesium_window_runs_planned_total`,
      `caesium_window_runs_forced_total`, `caesium_window_deadline_at_risk_total`,
      and a `caesium_window_parked` gauge to `internal/metrics/metrics.go` (both
      the `var (...)` block at `:22` **and** the `Register()` list at `:496-498`).
      Add `CAESIUM_WINDOW_` env fields `SCHEDULING_ENABLED` (`false`),
      `CHECK_INTERVAL` (`15s`), `DEFAULT_BUFFER` (`10m`), `P95_SAMPLES` (`20`) to
      the `Environment` struct (`pkg/env/env.go`). Wire the scheduler into
      `cmd/start/start.go` behind `CAESIUM_WINDOW_SCHEDULING_ENABLED`, mirroring
      the dequeuer's `runAsync` + `LeaderCheck: dqlite.IsLocalLeader` composition.
      Files: new `internal/windowsched/scheduler.go` (+ test), `cmd/start/start.go`,
      `pkg/env/env.go`, `internal/metrics/metrics.go`, `internal/event/` (event
      types).
      Depends on: B1 + A1 + A3.
      Acceptance probe (integration): apply a seconds-scale window job → it parks
      (visible via Stream C surfaces) → force-starts by latest-safe-start →
      completes in deadline.
- [ ] B3. Add the restart-safety / missed-opens reconciler
      (`internal/windowsched/reconciler.go`, a **separate file** from the loop to
      keep Stream D's gate edits conflict-free): on becoming leader and on every
      tick, for each window job compute its most recent open ≤ now in its
      timezone; if still feasible and no run/parked row exists for that logical
      date (unique index ⇒ idempotent), park it — forcing immediately if `now ≥
      forceAt`. Mirrors cron catchup (`fireCatchup`, `cron.go:150-197`) but driven
      from the durable table, not process memory (no in-process timer holds a
      parked run). Regression guard: `forceAt` is recomputed every tick from live
      p95, so if duration grows past remaining slack the force rule fires next
      tick.
      Files: new `internal/windowsched/reconciler.go` (+ test).
      Depends on: B2.

### Stream C — Operator surface: REST + CLI (P0)

The read surface over the engine so operators can see and explain a run's plan.
Mirror the shipped `caesium job queue` precedent (`cmd/job/queue.go`) for
clean-stdout `--json`, and add the two REST reads alongside the existing
job/trigger routes in `api/rest/bind/bind.go` (`Protected()` group,
`bind.go:131-138`). Depends on Stream B for the plan/rationale/predictor state
these surfaces report.

- [ ] C1. Add `GET /v1/jobs/:id/window` (window config, p95 + sample count,
      derived `latest_safe_start`, current plan `planned|parked|forced|none`,
      rationale, sampled signals) and `GET /v1/window/parked` (all parked rows),
      as a new `api/rest/controller/window/` + `api/rest/service/window/` pair
      bound in `api/rest/bind/bind.go`. The service reads parked `run_queue` rows
      (`window_deadline IS NOT NULL`) and calls the Stream B predictor + forceAt
      derivation for the derived fields.
      Files: new `api/rest/controller/window/`, new `api/rest/service/window/`,
      `api/rest/bind/bind.go`.
      Depends on: B2.
- [ ] C2. Add the `caesium job window <alias>` CLI subcommand (its own
      `init()` calling `Cmd.AddCommand`, mirroring `cmd/job/queue.go:176-181` — no
      shared `cmd/job/job.go` edit): human table (window span, predicted p95 +
      samples, latest safe start, plan, why) plus `--json` emitting the REST
      payload on **stdout** (machine output separated from logs — the CLAUDE.md
      rule; capture via `runCLIStdout` in tests, never the stream-merging
      `runCLIRaw`).
      Files: new `cmd/job/window.go` (+ its `init`).
      Depends on: C1.

### Stream D — Load + cost/carbon signal gating (P1 + P2)

The elasticity gates that make `cheapest` real, shipped after the P0 engine.
One signal interface, three implementations selected by env, all zero-dependency
and fail-open (a dead feed must never strand a job past its valley). Extends the
Stream B gate chain — sequences after B2.

- [ ] D1. **(P1)** Add the load gate: count running runs/tasks (the `CountActive`
      pattern, `internal/run/store.go:3680`) against
      `CAESIUM_WINDOW_LOAD_MAX_RUNNING` (`0` = off), inserted into the scheduler's
      gate chain (all-green-required) with a rationale string (`"parked: cluster
      at 41/32 running tasks"`). Add the env field to `pkg/env/env.go`.
      Files: `internal/windowsched/scheduler.go` (gate-chain hook), new
      `internal/windowsched/signal.go` (load counter), `pkg/env/env.go`.
      Depends on: B2.
- [ ] D2. **(P2)** Add the pluggable cost/carbon signal source: one interface with
      a **static calendar** (`CAESIUM_WINDOW_SIGNAL_CALENDAR`, JSON
      day-of-week/hour → relative cost score, zero I/O) and an **event-ingested
      feed** (operators POST `signal.cost`/`signal.carbon` through the shipped
      `POST /v1/events` pipeline; a subscriber persists the latest value per
      signal with `CAESIUM_WINDOW_SIGNAL_TTL`, default `1h`; expired ⇒ gate green,
      fail-open — Caesium never calls a price API itself). Add the `cheapest`
      objective + the forecast-minimum gate (`signal(now) ≤ min over [now,
      forceAt]`) to the scheduler gate chain, and the two env fields.
      Files: `internal/windowsched/signal.go`, `internal/windowsched/scheduler.go`
      (gate-chain hook), `pkg/env/env.go`, `internal/event/` (signal subscriber).
      Depends on: D1 + B2.

### Stream E — Frontend (P0 badge + P2 window bar)

The web surface driving the Stream C REST endpoint. If the capability is gated,
add a field to the `Features` struct
(`api/rest/service/system/system.go`) and check it in the UI.

- [ ] E1. **(P0)** Add the job-detail planned-start badge on the trigger summary
      (`parked · starts ≈02:00 · forced 04:58`) with the rationale as a
      tooltip/expando, driving `GET /v1/jobs/:id/window`. Add the API method in
      `ui/src/lib/api.ts`; surface within the existing jobs feature.
      Files: `ui/src/features/jobs/`, `ui/src/lib/api.ts`.
      Depends on: C1.
- [ ] E2. **(P2)** Add the run-detail/history horizontal window bar per run —
      window span, planned start, actual start (colored planned/forced), actual
      end, deadline tick — so an operator sees at a glance how much elasticity was
      used.
      Files: `ui/src/features/jobs/` (run-detail view).
      Depends on: E1.

## Harness Strengthening

- [ ] H-1. Enable the window path on the live integration server: set
      `CAESIUM_WINDOW_SCHEDULING_ENABLED=true` (and a low `CAESIUM_WINDOW_CHECK_INTERVAL`
      / `CAESIUM_WINDOW_LOAD_MAX_RUNNING` if a scenario needs a tight bound) on the
      `just integration-up` / `just integration-test` server and in CI, so the
      Stream B/C/D scenarios drive the live surface rather than an internal call
      (config-gated features must be enabled here or CI proves nothing — the
      CLAUDE.md lineage-flag precedent). Carry the flag through the `test/` harness
      helpers.
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [ ] N-1. Flip the [`docs/roadmap.md`](../../roadmap.md) Phase 4 "Deadline-window
      scheduling" table row from a bare design link to an in-progress plan link;
      update the [`design-window-scheduling.md`](../../design-window-scheduling.md)
      `> Status:` banner from brainstorm/design to "active — this plan"; document
      the `window` trigger fields (`cron`/`deadline`/`close`/`timezone`/`buffer`/
      `objective`) in `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      and `docs/caesium-job-llm-reference.md`; add a `window`-trigger example under
      `docs/examples/` (`.job.yaml`, pinned image); and index this plan in
      `docs/README.md` in backtick/inline-code form (the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects clickable
      subdirectory links — PR #245 precedent). Runs last, after the runtime ships.
      Files: `docs/roadmap.md`, `docs/design-window-scheduling.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–E (runs last).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B, C, D, and E all consume the `window`
  trigger type, the parking columns, or `ParkWindowRun`. A merges first (largest
  blast radius: schema + models + admission read path).
- **Stream B** depends on A (B2 needs A1's columns/`ParkWindowRun` + A3's parking
  trigger; B1 the predictor is structurally independent and can start in the
  first wave). B2 → B3.
- **Stream C** depends on B2 (it reports the scheduler's plan/rationale +
  predictor state). C1 → C2.
- **Stream D** depends on B2 (both items extend the B2 gate chain). D1 → D2.
- **Stream E** depends on C1 (the REST endpoint it renders). E1 → E2.
- **H-1** is independent (justfile/CI/test harness); land it in the first wave so
  the engine's end-to-end gate has a live, enabled surface to drive.
- **N-1** runs last, after A–E ship, so roadmap/schema/design docs reflect
  reality and the design banner is flipped.

**Suggested waves:**
- **W1 = A (A1 → A2 → A3, A4) + B1 (predictor) + H-1.** A is the foundation; B1
  and H-1 are structurally independent leaf items.
- **W2 = B2 → B3.** Unblocked once A ships. B2 is the engine; B3 the reconciler.
- **W3 = C1 + D1** (both unblocked by B2; different files — controllers/service
  vs. scheduler gate). Then C2 after C1.
- **W4 = D2 + E1**, then **E2** and **N-1** last.

**Within-stream order:** A1 + A2 (parallel; different files) → A3 (needs both);
A4 after A2. B1 → B2 → B3. C1 → C2. D1 → D2. E1 → E2.

**Cross-stream file conflicts:**

- `internal/windowsched/scheduler.go` — B2 *creates* it; **D1** (load gate) and
  **D2** (cost gate + `cheapest`) both edit its gate chain. Sequence **B2 → D1 →
  D2**; never the same wave. B3's reconciler is a **separate file**
  (`reconciler.go`) precisely to stay off this seam.
- `pkg/env/env.go` — B2 (`SCHEDULING_ENABLED`/`CHECK_INTERVAL`/`DEFAULT_BUFFER`/
  `P95_SAMPLES`), D1 (`LOAD_MAX_RUNNING`), D2 (`SIGNAL_CALENDAR`/`SIGNAL_TTL`) all
  append fields to the single `Environment` struct. Additive across waves (B2 in
  W2, D in W3+); flag for a clean rebase.
- `internal/metrics/metrics.go` — only B2 adds collectors here (two edit sites:
  the `var (...)` block + `Register()`); no other stream touches it, so no
  same-wave overlap.
- `internal/run/store.go` — A1 adds the `DequeueNextRun` `window_deadline IS
  NULL` guard **and** `ParkWindowRun`; B2 only *calls* the existing `AdmitRun`
  (no edit); D1 only *reads* the `CountActive` count pattern. A1 owns all
  store.go edits — no cross-stream collision.
- `cmd/start/start.go` — only B2 adds startup wiring (the scheduler goroutine).
  Single writer.
- `internal/event/` — B2 (window bus event types) and D2 (signal subscriber) both
  add here; B2 (W2) before D2 (W3+), different symbols, additive.
- `api/rest/bind/bind.go` — C1 adds two routes (single stream, additive append).
- `ui/src/lib/api.ts` / `ui/src/features/jobs/` — E1 + E2 both append; sequence
  E1 → E2 (same stream).
- `internal/models/models.go` — **no change**: `&RunQueue{}` is already
  registered and A1 adds columns, not a new model.
- `pkg/db/router.go` / `pkg/db/db.go` — **no change**: `run_queue` is
  catalog-resident, not a hot shard table (see Source-Of-Truth Note).
- `internal/cache/hash.go` — **no change**: triggers do not participate in the
  step execution hash, so the cache key is untouched.
- `pkg/jobdef/definition.go` — only A2 (window config validation) + A4 (SLA
  derivation helper) edit it, same stream; no cross-stream collision.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (C1, C2) and the scheduler (B2, B3, D):** an
  integration scenario in `test/` that drives the **real surface** against the
  live server (with `CAESIUM_WINDOW_SCHEDULING_ENABLED=true` from H-1): apply a
  job with a seconds-scale window; assert it parks (via `GET /v1/window/parked`
  and `caesium job window`), force-starts by latest-safe-start, and completes;
  **restart the server mid-window** — the parked run survives (durable row) and
  still starts; a cold-start job starts at window open. A unit test that
  hand-computes `forceAt` proves the arithmetic, not the wiring — both are
  required.
- **Machine-readable CLI (`--json`, C2):** assert stdout is clean and parseable,
  captured **separately** from stderr via `runCLIStdout` (never the merging
  `runCLIRaw`).
- **New metrics (B2):** assert via `internal/metrics/testutil` in a `*_test.go`;
  each collector must also appear in `Register()`.
- **Job-schema validation (A2):** `caesium job lint --path docs/examples/` green
  on the new `window`-trigger example; an unsatisfiable window (`open + buffer >
  deadline`) rejected at lint.
- **`ui/**` changes (E1, E2):** `just ui-lint && just ui-test && just ui-e2e`.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema/design banner) refreshed in the same
  PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the window trigger + parking substrate** is live: a `window`
   trigger validates and lints (including satisfiability), a fired window parks a
   durable `run_queue` row with the four window columns set, the dequeuer ignores
   window rows (`window_deadline IS NULL` disjoint population), DST edge cases
   resolve per the tested policies, and a window job with no SLA gets an implicit
   `completedBy`. Closed by unit tests for validation/lint/DST + the parking path
   exercised in the Stream B/C integration scenario.
2. **Stream B — the scheduler engine** is a runtime feature: a leader-gated loop
   releases parked runs through `store.AdmitRun`, `earliest`/`latest` objectives
   work, force-start fires at `deadline − p95 − buffer` (cold-start degrades to
   window open), the reconciler re-parks missed opens after a restart, and the
   `window_planned`/`window_forced`/`window_deadline_at_risk` metrics increment.
   Closed by a `test/` integration scenario: park → force-start → complete →
   survive a mid-window restart, green in CI.
3. **Stream C — the operator surface** is live: `GET /v1/jobs/:id/window` and
   `GET /v1/window/parked` return the plan + rationale + derived
   `latest_safe_start`, and `caesium job window` renders them with clean,
   parseable `--json` stdout (asserted via `runCLIStdout`). Closed by integration
   scenarios hitting the live server + the CLI binary.
4. **Stream D — signal gating** works: the running-count load gate parks over the
   ceiling (P1), and the `cheapest` objective starts in the forecast valley from a
   static calendar or an event-ingested feed, failing open on an expired signal
   (P2). Closed by integration/unit scenarios for the load gate and both signal
   sources.
5. **Stream E — frontend** ships: the job-detail planned-start badge renders the
   plan + rationale (P0) and the run-history window bar shows elasticity used
   (P2), both driving the Stream C endpoint; `just ui-e2e` green.
6. **H-1 — the integration server** runs with `CAESIUM_WINDOW_SCHEDULING_ENABLED=true`,
   so the Stream B/C/D scenarios drive the live binary in CI, not an internal call.
7. **N-1 — docs reflect reality:** the `docs/roadmap.md` Phase 4 row points at
   this plan, the `design-window-scheduling.md` `> Status:` banner is flipped to
   active, the `window` trigger fields are documented across the schema references
   with a working `docs/examples/` manifest, and this plan is indexed in
   `docs/README.md`.
8. **Cross-cutting:** `docs/roadmap.md`, `docs/design-window-scheduling.md`, and
   this plan's per-stream `## Progress` entries reflect every shipped stream and
   match the merged PRs.

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
   `<Imperative subject> (window-scheduling <wave>-<stream>)` — e.g.
   `Add the window trigger type and parking columns (window-scheduling W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-window-scheduling.md`](../../design-window-scheduling.md) — the
  design of record and source of truth for this plan.
- [`docs/roadmap.md`](../../roadmap.md) Phase 4 Data-Plane Differentiators
  (Deadline-window scheduling row) — the strategic entry this plan promotes from
  design to in-progress.
- [`docs/exec-plans/active/freshness-scheduling.md`](freshness-scheduling.md) —
  the sibling temporal-scheduling initiative; freshness policies compile down to a
  rolling window + deadline, so they compose as layers (freshness decides *what
  deadline*, this plan decides *when inside the window to start*) over the same
  run-history + `run_queue` substrate.
- [`docs/design-sla-management.md`](../../design-sla-management.md),
  [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md),
  [`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md) — companion
  designs: SLA breach detection is reused verbatim, and its proposed predictive
  quantile engine should become the shared provider this plan's predictor
  consumes; right-sizing shares the run-history substrate and should share the
  signal-source interface; dynamic fan-out is the spatial slice to this temporal
  one.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the
  job-definition contract the `window` trigger extends with type-specific
  configuration validation.
- `internal/runqueue/dequeuer.go`, `internal/run/store.go` (`AdmitRun`/`admit`),
  `internal/trigger/cron/`, `pkg/dqlite` (`IsLocalLeader`) — the shipped queueing,
  admission, cron-parsing, and leadership machinery this plan schedules over.
