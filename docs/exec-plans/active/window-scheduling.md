# Deadline-Window Scheduling — The Time Loop (Plan 4 of the Closed-Loop Arc)

Last updated: 2026-09-05

> **Arc position: Plan 4 of
> [`closed-loop-arc.md`](closed-loop-arc.md) — the *time* loop, explicitly
> OPTIONAL.** This plan is **not an arc gate**: it runs only if the arc still has
> momentum after Plan 3 (backtesting), and not running it does not block the
> arc's acceptance (arc criterion 5). It was re-cut on 2026-09-05 from the
> original three-phase (P0/P1/P2) shape to **P0 only** over the arc's shared
> stats substrate: Streams A, B, C, E1, H-1 and N-1 are the acceptance scope;
> D1 is parked unless the arc has momentum; D2 and E2 are parked by arc
> decision (full text preserved under `#### Parked` below). The arc's shared
> conventions 1–8 apply by link and are not restated here.

Cron makes users encode a *guess* about the best start time when what they
actually hold is a *constraint* about the finish time. Forty nightly jobs pinned
at `0 0 * * *` spike the cluster at midnight then idle for hours; "run at 02:30"
silently rots into a missed deadline when the job slows; and a movable batch job
is nailed to a minute chosen with no view of cluster load, spot price, or grid
carbon. Caesium already *detects* a blown deadline (`metadata.sla.completedBy`,
`internal/notification/watcher.go` `scanCompletedBySLA`) but nothing uses the
deadline to decide when to *start*. This plan ships the P0 of the design of
record in [`design-window-scheduling.md`](../../design-window-scheduling.md): a
job declares an execution **window** plus a completion **deadline** (`window
00:00 → 05:00, finish by 06:00`), and Caesium picks the start moment from
predicted duration (p95 from history) and priority — with a hard rule that
force-starts at the latest safe moment regardless of signals. In arc terms
(the arc's loop table, "Time" row): **observe** per-step duration history from
the shared stats substrate; **judge** p95 against the declared window +
deadline; **act** by parking the fire as a durable `run_queue` row and
force-starting at `deadline − p95 − buffer`; **explain** every park and
force-start through `caesium why`.

The feature is a **queueing/scheduling policy over machinery that already
exists**, not an autoscaler: parked runs are rows in the durable dqlite
`run_queue` (`internal/models/run_queue.go` `RunQueue`), released through the
same atomic admission path triggers already use (`internal/run/store.go`
`Store.AdmitRun` → `Store.admit`), leader-gated by the same `dqlite.IsLocalLeader`
check the run-queue dequeuer uses (`internal/runqueue/dequeuer.go`
`Dequeuer.DrainOnce`). No windows declared ⇒ nothing paid. The original
decomposition followed the design's own phasing — **P0** — window trigger
type, parking columns, leader-gated scheduler with `earliest`/`latest`
objectives, p95 predictor + cold-start floor, force-start, derived
`completedBy` SLA, `caesium job window`, REST, integration tests; **P1** —
running-count load gate; **P2** — pluggable static/event cost-carbon signals,
the `cheapest` objective, and the UI window bar. The 2026-09-05 re-cut keeps
P0 as the plan's scope and parks P1/P2 (see Stream D and Stream E).

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

**Arc rule (highest precedence on scope and ordering):**
[`closed-loop-arc.md`](closed-loop-arc.md) is the program-level source of
truth. When this plan and the arc disagree on *why* something is in scope,
on cross-plan ordering, or on whether this plan runs at all, **the arc wins**;
when they disagree on *how* a stream is built, this plan and its design doc
win. This plan inherits the arc's shared conventions by link and does not
restate them: **1** feature gate (`CAESIUM_WINDOW_SCHEDULING_ENABLED` gates
startup wiring, route mounting, **and** job-definition validation of the
`window` trigger type), **2** harness (flag on every self-server lane), **3**
explainability (one persisted-event item, one `why` scenario), **4** agent
actions (none in this plan's P0 — see the parked "agent proposes window
changes" note in the arc's loop table), **5** stats substrate (the predictor
reads `TaskRun`/`JobRun` timestamps and Plan 2 A1's stats columns; no second
stats table), **6** symbol citations, **7** docs/N-1 shape including the loop
tour, **8** wave hygiene.

[`docs/design-window-scheduling.md`](../../design-window-scheduling.md) is
**authoritative for INTENT and SCOPE within P0** — when this plan and the
design doc disagree on how a P0 stream is built, the design doc wins, and the
plan is reconciled to match. Where the design describes P1/P2 machinery (load
gate, signal sources, `cheapest`, the run-history window bar) the arc's
parking decision wins over the design's phasing. The design is still a
brainstorm/design-status banner, so N-1 flips it to "active — this plan" when
the first runtime item merges. Strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) (Phase 5 arc table, row 4 "The time loop
(optional tail)"; the Phase 4 Data-Plane Differentiators table row still links
here); the roadmap wins on priority/status disagreements. The
job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go):
`Trigger.Configuration` is already a flexible `map[string]any` (the `Trigger`
struct), so `window` — exactly like the shipped `event` and `freshness` types —
adds **type-specific configuration validation, not a structural schema
change**; if an item finds it needs a struct change, stop and reconcile against
the design before proceeding. The pattern A2 mirrors is **the gate only**: the
`freshnessFeatureEnabled()` check (`strconv.ParseBool` over
`os.Getenv("CAESIUM_FRESHNESS_ENABLED")`) inside `validateTrigger`'s freshness
case. A2 does **not** mirror `ValidateTriggerSpec`'s refusal of a bare
trigger-API create: that refusal exists because *"a freshness trigger derives
its runs from the job's declared dataset graph … which lives on the definition
rather than the trigger"* (`pkg/jobdef/definition.go`, the doc comment on
`ValidateTriggerSpec`), and a `window` trigger's configuration
(`cron`/`deadline`/`close`/`timezone`/`buffer`/`objective`) is entirely
self-contained on the trigger. **Window triggers are therefore creatable
through `POST /v1/triggers`** like `cron`/`http`/`event`; nothing in the design
asks otherwise, and A2 must not add a `TriggerWindow` arm to that refusal.

Code-verified facts that shape the streams: (1) `run_queue` is a
catalog-resident table, **not** a hot per-run shard table (`hotTables` in
`pkg/db/router.go` lists `job_runs`/`task_runs`/`callback_runs`/
`execution_events`; `hotPathModels()` in `pkg/db/db.go` returns those four plus
`run_checkpoints` — the two lists differ, and neither contains `run_queue`),
so the nullable window columns (four scheduling instants plus B4's park provenance) need **no** hot-table router edit and no new
`models.All` entry (`&RunQueue{}` is already registered in
`internal/models/models.go`). (2) `TriggerTypeEvent`/`TriggerTypeFreshness`
and the executor's per-type listing loop already exist
(`internal/models/trigger.go` `TriggerType` constants;
`internal/executor/executor.go` `Start` builds the `reqs` list and
`queueTriggers` switches on type), so `window` is an additive third listing
request, not new machinery. A third verified fact added at the re-cut:
(3) the shipped release path a trigger fire takes is `job.New(j,
job.WithParams(params)).Run(ctx)` (`internal/trigger/cron/cron.go`
`Cron.fireAt`), which itself calls `Store.AdmitRun` and, when the job has no
concurrency policy (`AdmitRun` returns `handled=false`), falls through to
`Store.Start` (`internal/job/job.go`, the `resolveRun` closure inside `Run`).
The scheduler's "release" therefore means *launching through `job.New`*, not
calling `AdmitRun` directly — otherwise a window job without a `concurrency:`
block would never start (the freshness evaluator's
`Evaluator.derive` records `!handled` as "admission declined" for exactly this
reason; the window scheduler must not inherit that shape).

**Explicit design amendment (arc § Shared conventions: "a child plan's item
that contradicts one of these must say so explicitly and why").**
[`design-window-scheduling.md`](../../design-window-scheduling.md) still states
**"Release = `store.AdmitRun(...)` with the parked row's params, priority, and
logical date"**. That is wrong for jobs without a `concurrency:` block, per
fact (3). This plan overrides it: **release = `job.New(...).Run(ctx)`**, and
**N-1 reconciles the design text** (not only its status banner) so the design
of record and this plan agree. Until N-1 lands, this paragraph is the
authority on the release mechanism.

Three further verified properties of `job.New(...).Run` that B2/B4 must design
around — none of them is visible from the design doc's `AdmitRun` framing:

- **`Run` returns no run ID.** `type Job interface { Run(ctx context.Context)
  error }` (`internal/job/job.go`); `beforeComplete` is documented in the
  `job` struct as *"an unexported test seam"*. There is **no** exported way to
  learn the run `Run` created, so B4's "persist the release event against the
  run" needs a new seam (see B4).
- **`ErrRunQueued` is swallowed.** In `Run`, `retryOnContention(resolveRun)`'s
  error branch returns `nil` for both `run.ErrRunSkipped` and
  `run.ErrRunQueued` (`internal/job/job.go`). A queued admission is therefore
  **indistinguishable from a successful start** through `Run` — B2 must not
  claim to observe it there.
- **`resolveRun` has a third branch nobody planned for.** After `AdmitRun`
  returns `handled=false`, `resolveRun` calls `store.FindRunning(j.id)`; if a
  run is already running it returns **that existing run** — in distributed
  mode via `store.Get(running.ID)`, and in **local** mode after
  `store.ResetInFlightTasks(running.ID)` — and never reaches `store.Start`. A
  forced window release fired while the previous window's run is still in
  flight would silently re-enter the *old* run instead of starting the
  deadline-safe one, and the default/auth lanes run in **local** execution mode
  (arc convention 2). B2 must gate on this rather than discover it.

## Progress (as of 2026-09-05)

No implementation waves have shipped yet. The plan was published 2026-07-03
from the `design-window-scheduling.md` design of record and **re-cut
2026-09-05 as Plan 4 (optional tail) of the closed-loop arc**: P0 scope only,
predictor rebased onto the shared stats substrate, feature gate extended to
jobdef validation, a `why`-provenance item added (B4), D2/E2 parked, all
line-number citations replaced with symbols. **Adversarial review 2026-09-05**
corrected four release-path/ownership errors in that re-cut: B1 now *adopts*
Plan 2 D1's `HistorySource` instead of defining it; B2 no longer claims
`ErrRunQueued` is observable through `Job.Run` (it is swallowed) and adds an
admission-outcome seam plus an in-flight guard for `resolveRun`'s
`FindRunning`/`ResetInFlightTasks` branch; B4's `window_parked` moves off the
hot `execution_events` table onto the catalog `run_queue` row; and the
dequeuer-priority claim, the `ValidateTriggerSpec` mirror, and Open Question 6
are corrected/closed. The first wave is the next
eligible run of the `exec-plan-wave` skill against this doc — **only after
Plan 3 closes and the arc decides to continue** (arc § Sequence). Leaf items
eligible for the first wave are **A1**, **A2**, **B1**, and **H-1** (no unmet
`Depends on:` edges; B1's read of Plan 2 A1's columns is soft — see
Sequencing).

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Window trigger type (validation gated behind `CAESIUM_WINDOW_SCHEDULING_ENABLED`), YAML schema + lint satisfiability, DST open-resolution, `run_queue` parking columns, implicit `completedBy` SLA | **P0** | Not started |
| B | Window scheduler engine — p95 predictor over the shared stats substrate, leader-gated loop, force-start, `earliest`/`latest` objectives, restart-safety reconciler, bus events + metrics, `why` provenance (B4) | **P0** | Not started |
| C | Operator surface — `GET /v1/jobs/:id/window` + `GET /v1/window/parked` REST, `caesium job window` CLI | **P0** | Not started |
| D | Load + cost/carbon signal gating — D1 running-count gate (P1) **parked unless arc momentum**; D2 pluggable static/event signals + `cheapest` objective (P2) **parked (arc decision 2026-09-05)** | P1 / P2 | **Parked** — listed, not an acceptance gate |
| E | Frontend — E1 planned-start badge + rationale (P0); E2 run-history window bar (P2) **parked (arc decision 2026-09-05)** | P0 (E1) / P2 (E2) | Not started (E1) / **Parked** (E2) |
| H-1 | Integration harness — enable `CAESIUM_WINDOW_SCHEDULING_ENABLED` on every lane that runs its own server; mid-window restart scenario on the default lane | — | Not started |
| N-1 | Docs — roadmap row, design banner, generator-driven schema reference, example manifest, README index, `docs/tour-time-loop.md`, arc dashboard row | — | Not started |

## Streams

### Stream A — Window trigger, schema, DST resolution & parking columns (P0 foundation)

The declarative + persistence substrate every other stream builds on: the
`window` trigger type, its YAML configuration validation and lint-time
satisfiability check, DST-correct open resolution, the nullable `run_queue`
columns that make a parked run a durable row, and the implicit `completedBy` SLA
so a blown window deadline alerts through the shipped watcher with zero new
machinery. Largest blast radius (schema + models + admission read path), so it
merges first. Mirror the shipped `event`-trigger config-validation shape
(`pkg/jobdef/definition.go` `validateEventTriggerConfiguration`, dispatched from
`validateTrigger`) and the cron schedule/timezone parsing this trigger reuses
(`internal/trigger/cron/cron.go` `ParseSchedule`, `extractLocation`,
`Cron.nextTick`).

- [ ] A1. Extend `run_queue` with the nullable window columns —
      `WindowOpen`, `WindowClose`, `WindowDeadline` (all `*time.Time`, resolved
      UTC instants) and `LogicalDate *time.Time` — **plus the park-provenance
      pair B4 needs** (`ParkedAt *time.Time` and a `ParkRationale string`, or
      one nullable JSON `window_meta` column; decide in the A1 PR and record
      it). Land these in **A1's** migration, not later: `execution_events` is
      a hot-shard table that cannot hold a RunID-less job-scoped park event
      (see B4), so the catalog-resident `run_queue` row *is* the park's
      provenance store, and adding the column in W3 would mean a second
      migration. Plus a unique partial index
      `(job_id, logical_date)` for idempotent parking. Make the two populations
      disjoint via `window_deadline`: the **existing** dequeuer keeps draining
      only concurrency-overflow rows — add `AND window_deadline IS NULL` to its
      distinct-`job_id` listing (`internal/runqueue/dequeuer.go`
      `Dequeuer.DrainOnce`, the `Where("claimed_by = ''")` pluck) and to the
      raw claim SQL in `Store.DequeueNextRun` (`internal/run/store.go`, the
      `UPDATE run_queue ... WHERE job_id = ? AND claimed_by = ''` subselect) —
      so the dequeuer never touches window rows and the window scheduler
      (Stream B) owns them. Also exclude window rows from the three other
      `run_queue` readers that would otherwise miscount them: the queue-depth
      cap in `Store.enqueueRunTx` (`maxDepth`), the `caesium_run_queue_depth`
      gauge in `Store.observeRunQueueDepth`, and the `GET /v1/jobs/:id/queue`
      listing (`api/rest/service/job/job.go` `jobService.Queue`); leave
      `Dequeuer.reclaimStaleClaims` population-agnostic (a stale scheduler
      claim on a window row *should* be reclaimed — this is the "claim columns
      + stale reclaim cover leader death mid-release" property the design
      relies on) and leave `jobService.CancelQueuedRun` working on window rows
      (deleting the row is how an operator cancels a parked run — decide in
      the PR whether C1 exposes it, see Open Questions). Add a typed
      `ParkWindowRun(...)` store method that inserts a parked row
      (`claimed_by=''`, window columns set) idempotently against the unique
      index. No `models.All` change (`&RunQueue{}` is already registered) and
      **no** hot-table router edit (`run_queue` is catalog-resident — see the
      Source-Of-Truth Note).
      Files: `internal/models/run_queue.go`, `internal/runqueue/dequeuer.go`,
      `internal/run/store.go`, `api/rest/service/job/job.go`.
- [ ] A2. Add `TriggerTypeWindow`/`TriggerWindow` and window trigger config
      validation, **gated behind `CAESIUM_WINDOW_SCHEDULING_ENABLED` exactly
      like the `freshness` trigger (arc convention 1)**: accept `window` in the
      `TriggerType` constants (`internal/models/trigger.go`) and in the jobdef
      trigger constants + `validateTrigger` switch (`pkg/jobdef/definition.go`;
      the constants `TriggerCron`/`TriggerHTTP`/`TriggerEvent`/`TriggerFreshness`
      live in `definition.go`, not `schema.go` — the 2026-07 draft's
      `schema.go` citation was stale). Add a `windowSchedulingFeatureEnabled()`
      helper mirroring `freshnessFeatureEnabled()` (`strconv.ParseBool` over
      `os.Getenv("CAESIUM_WINDOW_SCHEDULING_ENABLED")` — `pkg/jobdef` reads the
      raw env so `caesium job lint` and the server validate identically without
      importing `pkg/env`), and have `validateTrigger` return
      `trigger.type "window" requires CAESIUM_WINDOW_SCHEDULING_ENABLED=true`
      when it is off — so with the flag off the type is **rejected at apply
      and at lint**, not merely un-scheduled (off means inert: no schema
      surface). Then a new `validateWindowTriggerConfiguration` (require a
      valid 5-field `cron` per the existing `robfig/cron` parser used by
      `cron.ParseSchedule`/`extractExpression`; `deadline` present as `HH:MM`;
      optional `close < deadline`; optional `timezone` resolvable via
      `time.LoadLocation` as `extractLocation` does; optional `buffer`
      parseable; `objective ∈ {earliest,latest,cheapest}` default `earliest` —
      `cheapest` **validates but is rejected with "objective cheapest requires
      the parked P2 signal gate"** until D2 ships, so a manifest written for P2
      fails loudly rather than silently behaving as `earliest`). Add the
      **lint satisfiability** check (`open + buffer ≤ deadline` modulo
      rollover; `close < deadline`) surfaced through `caesium job lint`
      (`cmd/job/lint.go` calls `Definition.Validate()`; the same env var must be
      set in the CLI process to lint a window job, as with `freshness`). Add
      the `WindowSchedulingEnabled bool` field
      (`envconfig:"WINDOW_SCHEDULING_ENABLED" default:"false"`) to the
      `Environment` struct in `pkg/env/env.go` and surface it as
      `window_scheduling_enabled` on the `Features` struct in
      `api/rest/service/system/system.go` (the `FreshnessEnabled` precedent) —
      the one flag every later item (B2 wiring, C1 routes, E1 badge) reads.
      Add `window` as a third listing request in the executor loop
      (`internal/executor/executor.go` `Start`'s `reqs` slice and the
      `queueTriggers` switch), appended **only when
      `env.Variables().WindowSchedulingEnabled`** — the `cron.go`
      `env.Variables().FreshnessEnabled` check is the precedent for
      env-conditional trigger behavior.
      Files: `internal/models/trigger.go`, `pkg/jobdef/definition.go`,
      `pkg/env/env.go`, `api/rest/service/system/system.go`,
      `internal/executor/executor.go`, `cmd/job/lint.go`.
- [ ] A3. Implement the `internal/trigger/window` package satisfying the shared
      three-method `Trigger` interface (`Listen`/`Fire`/`ID`,
      `internal/trigger/trigger.go` `Trigger`): `Listen` waits for the next
      window *open* exactly as cron waits for its tick (`Cron.Listen` →
      `time.After(time.Until(next))` → `Cron.fireAt`) but on fire **parks a
      durable `run_queue` row** (via A1's `ParkWindowRun`) stamped with logical
      date, resolved open, effective close, and deadline — it does NOT launch
      the job; the in-process `time.After` is only a prompt-parking
      optimization, never a correctness dependency. Resolve open through the
      cron schedule in the configured location and derive `deadline`/`close`
      via `time.Date` on the logical date in that location. Carry the cron
      trigger's run-param shape (`Cron.scheduledRunParams`: `logical_date` +
      configured default params) into the parked row's `Params` so the
      released run is indistinguishable from a cron-fired one downstream.
      **DST policies (tested):** spring-forward gap → first valid instant after
      it; ambiguous fall-back → first (earlier-UTC, conservative) occurrence;
      DST-collapsed window (`open ≥ forceAt`) → park and immediately mark for
      force with a `window_collapsed` warning.
      Files: new `internal/trigger/window/window.go` (+ `window_test.go`), reuses
      `internal/trigger/cron` schedule/timezone parsing (`ParseSchedule`).
      Depends on: A1 + A2.
- [ ] A4. Derive an implicit `sla.completedBy` from the window deadline at apply
      time when the job declares no SLA, so a blown deadline alerts through the
      shipped watcher (`internal/notification/watcher.go` `scanCompletedBySLA`
      → `emitCompletedBySLAEvent`, `sla_missed`) with zero new machinery —
      deadline *enforcement before the fact* is Stream B; deadline *alerting
      after the fact* is reused verbatim. Wire the derivation into the
      importer's SLA marshaling (`internal/jobdef/importer.go` `marshalSLA`, called
      from `Importer.upsertJobAndTriggerTx` for both the create and the
      update branch). Note the shipped `resolveCompletedBy` is UTC-only `HH:MM`
      (`watcher.go` `resolveCompletedBy`; `SLAConfig.CompletedBy` in
      `pkg/jobdef/definition.go` documents the UTC contract); the derived value
      is a best-effort UTC mapping of the tz-aware window deadline (tz-aware
      enforcement lives in Stream B, which does not reuse this resolver).
      Files: `internal/jobdef/importer.go`, `pkg/jobdef/definition.go` (SLA
      derivation helper).
      Depends on: A2.

### Stream B — Window scheduler engine (P0 core)

The engine that turns parked rows into timely starts: a new `internal/windowsched`
package structured like the run-queue dequeuer — ticker, `DrainOnce`, and the
**same leadership gate** (`runqueue.Config.LeaderCheck` → `dqlite.IsLocalLeader`,
checked at the top of `Dequeuer.DrainOnce`; wired in `cmd/start/start.go`
inside the `vars.RunQueueEnabled || vars.RunQueueDequeuerEnabled` block) — so
only the leader releases window rows and two nodes never double-start a parked
run. Release goes through normal atomic admission (`Store.AdmitRun` →
`Store.admit`, the conditional `insertRunIfSlotTx`) by launching via
`job.New(...)` exactly as `Cron.fireAt` does — the scheduler holds **no new
write authority** and never bypasses concurrency (see Source-Of-Truth Note
fact 3 for why it must not call `AdmitRun` directly). Depends on Stream A (the
trigger, columns, and `ParkWindowRun`).

- [ ] B1. Add the duration predictor
      (`internal/windowsched/predictor.go`): `P95(jobID)` over the last N
      (`CAESIUM_WINDOW_P95_SAMPLES`, default 20) succeeded, non-quarantined
      runs. **Read from the existing run-history substrate — no second stats
      table (arc convention 5):** run wall time is `JobRun.CompletedAt −
      JobRun.StartedAt` (`internal/models/run.go` `JobRun`; the same
      `completed_at − started_at` shape the stats service computes in
      `api/rest/service/stats/stats.go` `Service.durationExpr`, tz/driver-aware),
      and the per-step breakdown is `TaskRun.StartedAt`/`TaskRun.CompletedAt`
      (`internal/models/run.go` `TaskRun` — both exist **today**). When the
      Plan 2 A1 stats columns are present on `TaskRun` (`PeakMemoryBytes`,
      `CPUSeconds`, `StatsSource`, `OOMKilled`, `AppliedResources`,
      `EscalationLevel` — [`resource-right-sizing.md`](resource-right-sizing.md)
      Stream A), the predictor reads them through the **same reader** so an
      escalated-and-retried attempt (`EscalationLevel > 0`) or an OOM-killed
      attempt is not mistaken for a normal-duration sample. **Read history
      through the quantile-parameterised `HistorySource` reader Plan 2 D1
      defines** — `internal/rightsizing/`, per arc convention 5 ("Plan 2 D1
      defines the quantile-parameterised `HistorySource` reader … Plan 4 B1
      adopts `HistorySource`") and the arc's synergy row ("Plan 2, Stream D
      defines; Plan 4, Stream B adopts") — parameterised to **p95** where
      right-sizing passes p99. B1 **does not define** that interface: Plan 2 D1
      already states "this item is the definition site and Plan 4 B1 imports
      it", and its reader already exposes the quantile as a parameter and
      filters `TaskRun.CacheHit` / quarantined rows. **If Plan 2 has not
      shipped when B1 lands** (Plan 4 is the optional tail, so this should not
      happen — but the wave must not block on it): land a timestamp-only
      implementation *of the same interface shape* in
      `internal/windowsched/predictor.go`, name it `HistorySource`, and say in
      the PR body that Plan 2 D1 absorbs it — do not fork a second query shape.
      The dependency on Plan 2 A1 is **soft**: absent
      those columns the predictor is complete over timestamps alone; present,
      it filters better. **This predictor is also the designated home for the
      predictive at-risk ETA from the removed SLA-management design** (deleted
      2026-09-06 — `git show 2459109:docs/design-sla-management.md`; arc §
      Parked: "the predictive-ETA engine folds into Plan 4's predictor"): expose `ETA(runID)` = `StartedAt + P95(jobID)` alongside
      `P95` so B2's `window_deadline_at_risk` and any later at-risk alerting
      share one quantile engine. Do **not** import that design's EWMA model,
      escalation chains, `internal/sla/` package, `caesium sla` verbs, or
      `sla_*_notified` columns — a safety margin wants a conservative upper
      quantile, not a smoothed mean (the design doc's own "why p95, not EWMA"
      rationale). **Exclude cache-short-circuited runs** (a fully cached run
      finishes in near-zero wall time): mixing them into the sample skews p95
      *downward*, so the scheduler would pick a `latest` start too late and
      blow the deadline the first time the run actually executes. Filter them
      out (a run whose every `TaskRun` has `CacheHit` set records no real task
      duration) so p95 reflects genuine execution cost; if that leaves fewer
      than the cold-start minimum, degrade as below. **Cold-start policy
      (required):** fewer than 3 completed runs → no p95 → caller degrades to
      `forceAt = windowOpen` (starts at open, i.e. cron behavior);
      `metadata.runTimeout` (`Metadata.RunTimeout` in `pkg/jobdef/definition.go`),
      when set, caps the assumed duration. Injectable clock, fully unit-tested
      (p95, cold-start floor, runTimeout cap, cache-hit exclusion, escalated-
      attempt exclusion when the columns exist).
      Files: new `internal/windowsched/predictor.go` (+ `predictor_test.go`),
      importing `internal/rightsizing`'s `HistorySource` (Plan 2 D1) — no new
      reader unless Plan 2 has not shipped, per the fallback above.
- [ ] B2. Implement the leader-gated scheduler loop
      (`internal/windowsched/scheduler.go`): ticker + `DrainOnce`, evaluate parked
      rows ordered `priority DESC, (deadline − now − p95) ASC`, compute
      `forceAt = min(windowClose, windowDeadline − p95 − buffer)`, and the P0 gate
      chain — `now ≥ forceAt` → release **FORCED** (unconditional); `now <
      windowOpen` → skip; `objective==earliest` → release **PLANNED**;
      `objective==latest` → skip until `forceAt`. Release = claim the row
      (`claimed_by`/`claimed_at`, the dequeuer's claim shape), then launch
      `job.New(j, job.WithParams(params), job.WithPriorityOverride(...),
      job.WithTriggerID(&job.TriggerID)).Run(ctx)` with the parked row's
      params/priority/logical date — so the run passes `Store.AdmitRun` →
      `Store.admit` like a cron fire and the run row's `TriggerType` resolves
      to `window` through the trigger JOIN in `Store.loadRunWithDB`. Forced
      releases are stamped priority `high` (`job.WithPriorityOverride` takes a
      **string**, `jobdef.PriorityHigh`; `run.priorityValue` maps it to
      `models.RunQueue.Priority`'s **int** `PriorityHighValue`,
      `internal/run/priority.go`) so that **if admission queues the released
      run**, the ordinary concurrency-overflow row `Store.enqueueRunTx` writes
      sorts ahead in `Store.DequeueNextRun`'s `ORDER BY priority DESC,
      created_at ASC`. **This is not about the parked window row**: A1 makes
      the two populations disjoint (`AND window_deadline IS NULL`), so the
      dequeuer can never drain a window row at any priority — the overflow row
      it sorts is a *second*, window-column-free row created downstream of the
      release. Delete the parked row on a successful launch, release the claim
      on failure.
      **In-flight guard (Source-Of-Truth Note fact 3, third bullet):** before
      launching, check `Store.FindRunning(jobID)`; if a run for the job is
      already in flight, **skip the release** and record the rationale
      (`"deadline at risk: previous run still in flight"`), emitting
      `window_deadline_at_risk` — do **not** launch, because `resolveRun`
      would attach to the existing run (and in local mode
      `ResetInFlightTasks` it) rather than start the deadline-safe one.
      Unit-test the skip, and cover it in the B2 integration probe.
      Record a one-sentence **rationale** string per decision (a gate chain,
      not a weighted score). Emit bus events `window_planned` /
      `window_forced` / `window_deadline_at_risk` + `window_collapsed` as new
      `Type` constants in `internal/event/bus.go` (beside `TypeSLAMissed`), and
      add `window_forced` + `window_deadline_at_risk` to `notifiableTypes` in
      `internal/notification/subscriber.go` so notification policies can route
      them; add collectors `caesium_window_runs_planned_total`,
      `caesium_window_runs_forced_total`, `caesium_window_deadline_at_risk_total`,
      and a `caesium_window_parked` gauge to `internal/metrics/metrics.go` (both
      the `var (...)` block **and** the `prometheus.MustRegister` list in
      `Register()`). Add the remaining `CAESIUM_WINDOW_` env fields
      `CHECK_INTERVAL` (`15s`), `DEFAULT_BUFFER` (`10m`), `P95_SAMPLES` (`20`)
      to the `Environment` struct (`pkg/env/env.go`) — `SCHEDULING_ENABLED`
      itself lands in A2 (arc convention 1). Wire the scheduler into
      `cmd/start/start.go` behind `vars.WindowSchedulingEnabled`, mirroring the
      dequeuer's `runAsync` + `LeaderCheck: dqlite.IsLocalLeader` composition
      (and the `vars.FreshnessEnabled` block immediately below it).
      **Sub-step — the admission-outcome seam (required; B4 depends on it):**
      `Job.Run` today is `Run(ctx) error` and **swallows `run.ErrRunQueued`**
      (returns `nil`), so neither the created run ID nor "queued rather than
      started" is observable (Source-Of-Truth Note fact 3). Add **one**
      exported seam to `internal/job` that reports the admission outcome —
      e.g. a `job.WithAdmissionObserver(func(run uuid.UUID, outcome
      job.AdmissionOutcome))` option invoked from `Run` after `resolveRun`,
      carrying `started | queued | skipped | attached` and the run ID when one
      exists — and have B2 branch on it: `queued` ⇒ emit
      `window_deadline_at_risk`; `started` ⇒ carry the run ID into B4's event
      persist. Do **not** infer the queued case by racing a re-read of
      `run_queue`. This edit puts `internal/job/job.go` on the cross-stream and
      **cross-plan** conflict lists (see § Sequencing and the arc's
      `internal/job/job.go` row) — the seam is additive and must not change
      `resolveRun`'s existing branches.
      Files: new `internal/windowsched/scheduler.go` (+ test), `cmd/start/start.go`,
      `internal/job/job.go` (admission-outcome seam), `pkg/env/env.go`,
      `internal/metrics/metrics.go`, `internal/event/bus.go`
      (event types), `internal/notification/subscriber.go` (`notifiableTypes`).
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
      forceAt`. Mirrors cron catchup (`Cron.fireCatchup`, which enumerates
      missed slots with `job.EnumerateLogicalDates` from `internal/job/backfill.go`
      since `Store.LatestSuccessfulCronRun`) but driven from the durable table,
      not process memory (no in-process timer holds a parked run); reuse
      `EnumerateLogicalDates` rather than re-deriving slot enumeration.
      Regression guard: `forceAt` is recomputed every tick from live p95, so if
      duration grows past remaining slack the force rule fires next tick.
      Files: new `internal/windowsched/reconciler.go` (+ test).
      Depends on: B2.
- [ ] B4. **`why` provenance (arc convention 3 — this plan's one
      explainability item).** Persist every park / planned-release /
      force-start decision as an execution event a run can be explained by,
      and render it in `caesium why`. On release, B2's `window_planned` /
      `window_forced` event is appended to the execution-event store
      (`internal/event/store.go`, the store `Store.appendRunStartedEventTx`
      already writes `run_started` through) **with `RunID` set** — the run ID
      comes from **B2's admission-outcome seam**, not from `Job.Run`, which
      returns only an `error` (`type Job interface { Run(ctx context.Context)
      error }`; `beforeComplete` is an unexported test seam). B4 must not
      invent a second way to learn the run ID; if B2's seam is not yet merged,
      B4 is blocked. Payload carries `decision`
      (`planned|forced`), `rationale`, `p95_seconds`, `samples`,
      `window_open`, `window_close`, `deadline`, `force_at`, `parked_at`,
      `logical_date`, `objective`.
      **The park itself is NOT an `execution_events` row.** `execution_events`
      is a hot-shard table (`hotTables` in `pkg/db/router.go`,
      `hotPathModels()` in `pkg/db/db.go`) and
      `Router.Database(DatabaseRoleHot, uuid.Nil)` returns
      `"hot database route requires a run ID"` — a `JobID`-only execution
      event is **unroutable** once sharding is on. So persist the park
      provenance **on the catalog-resident parked row itself**: A1's window
      columns gain a `ParkRationale`/`ParkedAt` pair (or a small JSON
      `window_meta` column) on `models.RunQueue`, written by A3's
      `ParkWindowRun`, which is what `GET /v1/window/parked` (C1) reads to show
      *when* and *why* a row parked. `window_parked` still exists as a **bus**
      event type in `internal/event/bus.go` for notification routing; it is
      simply not written to the execution-event store. (If a future item wants
      the park in `execution_events`, it must wait until a run exists and
      backfill it with that `RunID` — do not write a RunID-less hot row.)
      Extend `WhyTrigger` (`internal/run/why.go`) with a `Window *WhyWindow`
      block populated in `Store.loadTrigger` by reading the run's
      `window_planned`/`window_forced` event through the existing
      `eventStore.ListSince(ctx, 0, 1, event.Filter{RunID, Types})` shape
      that already enriches trigger type/alias from `run_started`; fold a
      one-line sentence into the CLI's trigger **table** section
      (`cmd/why/why.go` `renderTable`). **`--json` does not go through the CLI
      struct**: the package doc says the command renders "either a
      human-readable summary table (default) or the raw machine-readable JSON
      (--json)", and the local `explanation` struct is documented as "…the rest
      round-trips via --json (which prints the server body verbatim)". The
      window block therefore reaches `--json` **automatically** once
      `WhyTrigger.Window` is on the server payload — the marshalling of
      `run.WhyExplanation` by `api/rest/controller/why` `Get` needs **no
      change** (it returns the service result as-is; confirm in the PR and add
      the file only if that turns out false). Table sentence of the form **"started at 03:12 because p95=2h10m and deadline=06:00
      (forced; buffer 10m; 18 samples)"** — or "started at window open
      (cold start: 1 sample < 3)" for the degraded path. The run-level
      sentence is rendered for any `--task` on that run. The `why` verb is
      **task-scoped and stays that way** (`why <run-id> --task <task> --job-id
      <job-id>`, `GET /v1/jobs/:id/runs/:run_id/why`; arc convention 3 and arc
      § Parked, which lists a run-level `why` verb as parked) — **B4 adds no
      verb and no route**. **One integration scenario**
      (`test/window_test.go`) asserts the sentence in `caesium why --json`
      stdout captured via `runCLIStdout`, for both a `planned` and a `forced`
      release.
      Files: `internal/windowsched/scheduler.go` (event append on release),
      `internal/event/bus.go` (`window_parked`), `internal/run/why.go`
      (`WhyTrigger.Window`, `loadTrigger`), `cmd/why/why.go` (`renderTable`),
      `test/window_test.go`.
      Depends on: B2 + C2 (the scenario drives the CLI and parked surface).

### Stream C — Operator surface: REST + CLI (P0)

The read surface over the engine so operators can see and explain a run's plan.
Mirror the shipped `caesium job queue` precedent (`cmd/job/queue.go`) for
clean-stdout `--json`, and add the two REST reads alongside the existing
job-scoped routes in `api/rest/bind/bind.go` (`Protected()` group — the
`g.GET("/jobs/:id/queue", jobqueue.List)` neighbourhood), **mounted only when
`env.Variables().WindowSchedulingEnabled`** (arc convention 1; the
`contractsvc.Enabled()` guard around `/contracts/graph` is the in-file
precedent for a conditional mount). Depends on Stream B for the
plan/rationale/predictor state these surfaces report.

- [ ] C1. Add `GET /v1/jobs/:id/window` (window config, p95 + sample count,
      derived `latest_safe_start`, current plan `planned|parked|forced|none`,
      rationale, sampled signals) and `GET /v1/window/parked` (all parked rows,
      each with its `window_parked` provenance from B4 when present),
      as a new `api/rest/controller/window/` + `api/rest/service/window/` pair
      bound in `api/rest/bind/bind.go` behind the flag. The service reads
      parked `run_queue` rows (`window_deadline IS NOT NULL`) and calls the
      Stream B predictor + forceAt derivation for the derived fields.
      Files: new `api/rest/controller/window/`, new `api/rest/service/window/`,
      `api/rest/bind/bind.go`.
      Depends on: B2.
- [ ] C2. Add the `caesium job window <alias>` CLI subcommand (its own
      `init()` calling `Cmd.AddCommand`, mirroring `cmd/job/queue.go`'s
      `init` — no shared `cmd/job/job.go` edit): human table (window span,
      predicted p95 + samples, latest safe start, plan, why) plus `--json`
      emitting the REST payload on **stdout** via `cmd.OutOrStdout()` (machine
      output separated from logs — the CLAUDE.md rule and the `cmd/why/why.go`
      precedent; capture via `runCLIStdout` in tests, never the stream-merging
      `runCLIRaw`).
      Files: new `cmd/job/window.go` (+ its `init`).
      Depends on: C1.

### Stream D — Load + cost/carbon signal gating (P1 + P2) — PARKED

The elasticity gates that make `cheapest` real, shipped after the P0 engine.
One signal interface, three implementations selected by env, all zero-dependency
and fail-open (a dead feed must never strand a job past its valley). Extends the
Stream B gate chain — sequences after B2.

**Arc decision 2026-09-05:** the arc's § Parked records "window scheduling
P1/P2 (load gate, cost/carbon signal sources, weighted scoring)" as *still
wanted, not this arc*. D1 stays listed below as **parked unless arc momentum**
— it is the cheapest of the three and the only one whose signal (running
count) the server already has — but it is **not** an acceptance criterion of
this plan. D2 is parked outright. Neither is picked up by `exec-plan-wave`
unless the arc doc's parking entry is revised first.

- [ ] D1. **(P1 — parked unless arc momentum)** Add the load gate: count
      running runs/tasks (the `Store.CountActive` pattern,
      `internal/run/store.go`, `status = running AND quarantine <> true AND
      backfill_id IS NULL`) against
      `CAESIUM_WINDOW_LOAD_MAX_RUNNING` (`0` = off), inserted into the scheduler's
      gate chain (all-green-required) with a rationale string (`"parked: cluster
      at 41/32 running tasks"`). Add the env field to `pkg/env/env.go`.
      Files: `internal/windowsched/scheduler.go` (gate-chain hook), new
      `internal/windowsched/signal.go` (load counter), `pkg/env/env.go`.
      Depends on: B2.

#### Parked (arc decision 2026-09-05)

Full text preserved; checkboxes kept; **excluded from Acceptance Criteria**.
Re-activation requires editing the arc doc's § Parked entry first.

- [ ] D2. **(P2 — parked)** Add the pluggable cost/carbon signal source: one interface with
      a **static calendar** (`CAESIUM_WINDOW_SIGNAL_CALENDAR`, JSON
      day-of-week/hour → relative cost score, zero I/O) and an **event-ingested
      feed** (operators POST `signal.cost`/`signal.carbon` through the shipped
      `POST /v1/events` pipeline; a subscriber persists the latest value per
      signal with `CAESIUM_WINDOW_SIGNAL_TTL`, default `1h`; expired ⇒ gate green,
      fail-open — Caesium never calls a price API itself). Add the `cheapest`
      objective + the forecast-minimum gate (`signal(now) ≤ min over [now,
      forceAt]`) to the scheduler gate chain, and the two env fields. (Until
      this ships, A2 rejects `objective: cheapest` at validation with an
      explicit "requires the parked P2 signal gate" error.)
      Files: `internal/windowsched/signal.go`, `internal/windowsched/scheduler.go`
      (gate-chain hook), `pkg/env/env.go`, `internal/event/` (signal subscriber).
      Depends on: D1 + B2.

**Weighted scoring vs. gate chain** (design Open Question 1) stays parked with
D2: an explainable gate chain with a recorded rationale is the P0 contract;
tunable coefficients for heterogeneous fleets are reconsidered only if D2 is
un-parked.

### Stream E — Frontend (P0 badge; P2 window bar parked)

The web surface driving the Stream C REST endpoint. The capability is gated:
E1 reads `window_scheduling_enabled` from the `Features` struct A2 adds
(`api/rest/service/system/system.go`; the UI's `features?.agent_remediation_enabled`
check in `ui/src/features/incidents/IncidentDetailPage.tsx` is the consumption
precedent, and the `Features` type in `ui/src/lib/api.ts` gains the field).

- [ ] E1. **(P0)** Add the job-detail planned-start badge on the trigger summary
      (`parked · starts ≈02:00 · forced 04:58`) with the rationale as a
      tooltip/expando, driving `GET /v1/jobs/:id/window`. Add the API method in
      `ui/src/lib/api.ts`; surface within the existing jobs feature
      (`ui/src/features/jobs/JobDetailPage.tsx`); render nothing when the
      feature flag is off.
      Files: `ui/src/features/jobs/`, `ui/src/lib/api.ts`.
      Depends on: C1.

#### Parked (arc decision 2026-09-05)

Full text preserved; checkbox kept; **excluded from Acceptance Criteria**.

- [ ] E2. **(P2 — parked)** Add the run-detail/history horizontal window bar per run —
      window span, planned start, actual start (colored planned/forced), actual
      end, deadline tick — so an operator sees at a glance how much elasticity was
      used. (When un-parked, the per-run data comes from B4's persisted
      `window_planned`/`window_forced` event, not a new column.)
      Files: `ui/src/features/jobs/` (run-detail view — `RunDetailPage.tsx` /
      `RunTimeline.tsx`).
      Depends on: E1.

## Harness Strengthening

- [ ] H-1. Enable the window path on **every lane that runs its own server**
      (arc convention 2 — lanes that start their own server silently go red when a
      flag is added to only the default lane): set
      `CAESIUM_WINDOW_SCHEDULING_ENABLED=true` (and a low
      `CAESIUM_WINDOW_CHECK_INTERVAL`, e.g. `500ms`, so seconds-scale windows
      resolve inside a test timeout; `CAESIUM_WINDOW_LOAD_MAX_RUNNING` only if
      D1 is un-parked) in the `justfile` server blocks `integration-up`
      (the default lane `integration-test` drives), `integration-up-distributed`,
      `integration-up-owner-memory`, `integration-up-infra`, `integration-up-agent`,
      `integration-test-podman` (inline `docker run`), `ui-e2e` and
      `ui-e2e-auth` (inline server), and `k8s-distributed` (helm
      `config.extraEnv[N]`); in `.github/workflows/ci.yml` the `ui-e2e` and
      `ui-e2e-auth` jobs' inline `docker run` blocks and the
      `podman-integration-test` job's server block; and in
      `helm/caesium/ci/test-values-k8s.yaml` (the `helm-integration-test`
      kind lane) beside `CAESIUM_FRESHNESS_ENABLED` — grep that variable to
      find every site; it is the exact list. Carry the flag through the `test/`
      harness (`IntegrationTestSuite.SetupSuite` reads its env; add a
      `windowSchedulingEnabled()` skip-guard so lanes without the flag skip
      rather than fail). **The mid-window server-restart scenario runs on the
      default lane** (`just integration-test` / CI `build-and-integration-test`):
      the test container joins the server's network namespace
      (`--network=container:caesium-server-test`) and has the Docker socket, so
      a restart must be driven from the recipe, not from inside the suite —
      add a `just integration-test-window-restart` step (or a phase inside
      `integration-test`) that runs
      `-run 'TestIntegrationTestSuite/TestWindowRestartPark'` (parks a
      long-window job), `docker restart caesium-server-test`, waits for
      health, then `-run 'TestIntegrationTestSuite/TestWindowRestartResumes'`
      (asserts the row survived and the run force-started) — `-run` patterns
      must be suite-qualified or they match nothing. Config-gated features must
      be enabled here or CI proves nothing (the CLAUDE.md lineage-flag
      precedent).
      Files: `justfile`, `.github/workflows/ci.yml`,
      `helm/caesium/ci/test-values-k8s.yaml`, `test/integration_test.go`
      (harness helpers), `test/window_test.go`.

## Navigational / Organizational Improvements

- [ ] N-1. Docs close-out (arc convention 7 — all of the following in one PR,
      last): flip the [`docs/roadmap.md`](../../roadmap.md) rows — the Phase 5
      arc table row 4 ("The time loop (optional tail)") to shipped and the
      Phase 4 "Deadline-window scheduling" table row from a bare design link
      to the completed plan link; update the
      [`design-window-scheduling.md`](../../design-window-scheduling.md)
      `> Status:` banner from brainstorm/design to "active — this plan" (on the
      first runtime merge) and then to shipped-P0 with P1/P2 parked (the
      `TestPlanningAndHistoricalDocsCarryStatusBanner` guardrail in
      `internal/guardrails/guardrails_test.go` pins the banner); **reconcile
      the design's release-mechanism text in the same PR** — its "**Release** =
      `store.AdmitRun(...)` … with the parked row's params, priority, and
      logical date" (and the matching `start = store.AdmitRun(...)` label in
      its ASCII flow) becomes `job.New(...).Run(ctx)`, per the explicit design
      amendment in this plan's Source-Of-Truth Note; drop the stale
      `store.go:1044` line-number citation while there (arc convention 6);
      document
      the `window` trigger fields (`cron`/`deadline`/`close`/`timezone`/`buffer`/
      `objective`) by **updating the generator** in
      `internal/jobdef/report/report.go` (the "Supported trigger types" line and
      a new `### Window Trigger` section beside `### Freshness Trigger`) and
      regenerating — `docs/job-schema-reference.md` is generated and
      `TestGeneratedSchemaReferenceIsCurrent` rejects hand edits — plus
      `docs/job-definitions.md` and `docs/caesium-job-llm-reference.md` by
      hand; add a `window`-trigger example under `docs/examples/`
      (`window-deadline.job.yaml`, image pinned to `alpine:3.23`, and referenced
      from `docs/job-definitions.md` — `TestJobDefinitionsDocReferencesEveryExampleManifest`
      requires it); write **`docs/tour-time-loop.md`** — the 10-minute
      walkthrough: enable the flag, apply the example with a seconds-scale
      window, watch it park in `caesium job window` / the Console badge, watch
      it force-start, read `caesium why` — and index both the tour and this
      plan in `docs/README.md` (the plan in backtick/inline-code form: the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects clickable
      subdirectory links — PR #245 precedent; the tour is a top-level doc and
      **must** be indexed or the same guardrail fails); and **tick the arc
      dashboard row** "4 window-scheduling" in
      [`closed-loop-arc.md`](closed-loop-arc.md) (waves shipped, last PR) in
      the same PR, then move this plan to `docs/exec-plans/completed/` and
      repoint the arc's links. Runs last, after the runtime ships.
      Files: `docs/roadmap.md`, `docs/design-window-scheduling.md` (status
      banner **and** the release-mechanism text/flow label),
      `internal/jobdef/report/report.go` (+ regenerated
      `docs/job-schema-reference.md`), `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/window-deadline.job.yaml`,
      new `docs/tour-time-loop.md`, `docs/README.md`,
      `docs/exec-plans/active/closed-loop-arc.md`.
      Depends on: A, B, C, E1, H-1 (runs last).

## Sequencing & Dependencies

**Cross-plan gate (arc § Sequence):** this plan starts only after Plan 3
(`backtesting.md`) closes and the arc decides to continue. Its
`pkg/jobdef/definition.go` edit (A2) is a true-conflict site shared with
1-A3, 2-B1, 3-A3 — one plan per wave; its `internal/run/store.go` edits (A1)
follow 0-A and 1-B1/C2 in plan order (arc file-conflict table). **B2 also
edits `internal/job/job.go`** (the admission-outcome seam) — that arc row
currently lists only 0-A3/A4, 1-A4 and 2-A3/B2/C1; 4-B2 belongs on it and must
not share a wave with those items.

**Cross-stream order:**

- **Stream A is the foundation** — B, C, D, and E all consume the `window`
  trigger type, the parking columns, or `ParkWindowRun`. A merges first (largest
  blast radius: schema + models + admission read path).
- **Stream B** depends on A (B2 needs A1's columns/`ParkWindowRun` + A3's parking
  trigger; B1 the predictor is structurally independent and can start in the
  first wave). B2 → B3; B4 after B2 **and** C2 (its scenario drives the CLI).
  **B1's dependency on Plan 2 A1 is soft**: B1 reads `JobRun`/`TaskRun`
  timestamps that exist today and *optionally* the stats columns Plan 2 A1
  adds; it must compile and pass with or without them (feature-detect via the
  model, not via build tags). **B1 adopts Plan 2 D1's `HistorySource`** (arc
  convention 5) rather than defining it; only if Plan 2 has not shipped does
  B1 land a timestamp-only implementation of the same interface shape, to be
  absorbed by Plan 2 D1.
- **Stream C** depends on B2 (it reports the scheduler's plan/rationale +
  predictor state). C1 → C2.
- **Stream D** (parked) would depend on B2 (both items extend the B2 gate
  chain). D1 → D2. Not scheduled unless the arc's parking entry is revised.
- **Stream E** depends on C1 (the REST endpoint it renders). E1 → E2 (E2
  parked).
- **H-1** is independent (justfile/CI/helm values/test harness); land it in the
  first wave so the engine's end-to-end gate has a live, enabled surface to
  drive on every lane.
- **N-1** runs last, after A–C, E1 and H-1 ship, so roadmap/schema/design docs
  reflect reality, the design banner is flipped, the tour exists, and the arc
  dashboard row is ticked.

**Suggested waves (L, ~2–3 waves per the arc):**
- **W1 = A (A1 → A2 → A3, A4) + B1 (predictor) + H-1.** A is the foundation; B1
  and H-1 are structurally independent leaf items.
- **W2 = B2 → B3, then C1 → C2.** Unblocked once A ships. B2 is the engine;
  B3 the reconciler; C the read surface.
- **W3 = B4 (why provenance) + E1**, then **N-1** last. D1 only if the arc
  un-parks it; D2/E2 not scheduled.

**Within-stream order:** A1 + A2 (parallel; different files) → A3 (needs both);
A4 after A2. B1 → B2 → B3; B4 after B2 + C2. C1 → C2. D1 → D2 (parked). E1 →
E2 (E2 parked).

**Cross-stream file conflicts:**

- `internal/windowsched/scheduler.go` — B2 *creates* it; B4 appends the
  release-time event persist; **D1** (load gate) and **D2** (cost gate +
  `cheapest`) would edit its gate chain. Sequence **B2 → B4 → D1 → D2**; never
  the same wave. B3's reconciler is a **separate file** (`reconciler.go`)
  precisely to stay off this seam.
- `pkg/env/env.go` — A2 (`SCHEDULING_ENABLED`), B2 (`CHECK_INTERVAL`/
  `DEFAULT_BUFFER`/`P95_SAMPLES`), D1 (`LOAD_MAX_RUNNING`), D2
  (`SIGNAL_CALENDAR`/`SIGNAL_TTL`) all append fields to the single
  `Environment` struct. Additive across waves (A2 in W1, B2 in W2, D later);
  flag for a clean rebase. Arc rule: one plan's item per wave in this file.
- `internal/metrics/metrics.go` — only B2 adds collectors here (two edit sites:
  the `var (...)` block + `Register()`); no other stream touches it, so no
  same-wave overlap.
- `internal/run/store.go` — A1 adds the `DequeueNextRun` / `enqueueRunTx` /
  `observeRunQueueDepth` `window_deadline IS NULL` guards **and**
  `ParkWindowRun`; B2 only *calls* the existing admission path (through
  `job.New`, no store edit); D1 only *reads* the `CountActive` count pattern.
  A1 owns all store.go edits — no cross-stream collision.
- `internal/run/why.go` + `cmd/why/why.go` — only B4 edits them.
  `api/rest/controller/why/why.go` — **expected no change** (`Get` returns the
  service result verbatim and `--json` prints the server body verbatim); B4
  confirms in its PR.
- `internal/job/job.go` — **B2 only**, for the admission-outcome seam (the run
  ID + `queued|started|skipped|attached` outcome `Run` does not expose today).
  This is a **cross-plan** true-conflict site: the arc's file-conflict table
  lists `internal/job/job.go` for 0-A3/A4, 1-A4 and 2-A3/B2/C1, so 4-B2 must
  not share a wave with any of those items (the arc row needs 4-B2 added — see
  this plan's note to the arc). Additive: do not alter `resolveRun`'s existing
  branches.
- `cmd/start/start.go` — only B2 adds startup wiring (the scheduler goroutine).
  Single writer.
- `internal/event/bus.go` — B2 (window bus event types) and B4
  (`window_parked`) both add constants; same stream, sequential. D2's signal
  subscriber (parked) would add under `internal/event/` too.
- `internal/notification/subscriber.go` — only B2 touches `notifiableTypes`.
- `api/rest/bind/bind.go` — C1 adds two routes (single stream, additive append).
- `api/rest/service/system/system.go` — only A2 adds the `Features` field.
- `api/rest/service/job/job.go` — only A1 (`jobService.Queue` exclusion).
- `ui/src/lib/api.ts` / `ui/src/features/jobs/` — E1 + E2 both append; sequence
  E1 → E2 (same stream; E2 parked).
- `justfile`, `.github/workflows/ci.yml`, `helm/caesium/ci/test-values-k8s.yaml`
  — only H-1; arc rule: one plan's H-1 per wave.
- `internal/models/models.go` — **no change**: `&RunQueue{}` is already
  registered and A1 adds columns, not a new model.
- `pkg/db/router.go` / `pkg/db/db.go` — **no change**: `run_queue` is
  catalog-resident, not a hot shard table (see Source-Of-Truth Note).
- `internal/cache/hash.go` — **no change**: triggers do not participate in the
  step execution hash (`HashInput` folds `RunParams`, image, command, env,
  predecessor hashes/outputs — never the trigger configuration), so the cache
  key is untouched. Note the parked row's `logical_date` param *does* enter
  `RunParams` exactly as a cron fire's does — same behavior, no new field.
- `pkg/jobdef/definition.go` — only A2 (window config validation + gate) + A4
  (SLA derivation helper) edit it, same stream; no cross-stream collision
  inside this plan (cross-plan: one plan per wave, see the arc table).

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (C1, C2) and the scheduler (B2, B3):** an
  integration scenario in `test/window_test.go` that drives the **real surface**
  against the live server (with `CAESIUM_WINDOW_SCHEDULING_ENABLED=true` from
  H-1): apply a job with a seconds-scale window; assert it parks (via
  `GET /v1/window/parked` and `caesium job window`), force-starts by
  latest-safe-start, and completes; **restart the server mid-window** (the
  H-1 two-phase recipe on the default lane) — the parked run survives (durable
  row) and still starts; a cold-start job starts at window open; **and a
  release fired while a previous run is still in flight is skipped with
  `window_deadline_at_risk`, not attached to the running run** (the local-mode
  `FindRunning`/`ResetInFlightTasks` branch). A unit test
  that hand-computes `forceAt` proves the arithmetic, not the wiring — both are
  required.
- **`why` provenance (B4):** the scenario asserts `caesium why --json` (stdout
  via `runCLIStdout`) carries the window sentence for a planned and a forced
  release.
- **Machine-readable CLI (`--json`, C2, B4):** assert stdout is clean and
  parseable, captured **separately** from stderr via `runCLIStdout` (never the
  merging `runCLIRaw`).
- **New metrics (B2):** assert via `internal/metrics/testutil` in a `*_test.go`;
  each collector must also appear in `Register()`.
- **Job-schema validation (A2):** `caesium job lint --path docs/examples/` green
  on the new `window`-trigger example **with the flag set in the CLI
  environment**, and the same lint rejecting the example with the flag unset
  (gate proof); an unsatisfiable window (`open + buffer > deadline`) rejected
  at lint; `objective: cheapest` rejected while D2 is parked; and a unit test
  that `ValidateTriggerSpec` **accepts** a bare `window` trigger with the flag
  on (unlike `freshness`, which it refuses for dataset-graph reasons).
- **Feature-gate inertness (A2, B2, C1):** with the flag off — no scheduler
  goroutine launched (`cmd/start`), `GET /v1/window/parked` is 404 (not
  mounted), and a `window` trigger is rejected at apply.
- **`ui/**` changes (E1):** `just ui-lint && just ui-test && just ui-e2e`.
- **Docs (N-1):** `just unit-test` green on `TestGeneratedSchemaReferenceIsCurrent`,
  `TestJobDefinitionsDocReferencesEveryExampleManifest`,
  `TestDocsREADMEIndexesEveryTopLevelDoc`,
  `TestPlanningAndHistoricalDocsCarryStatusBanner`, and
  `TestPinnedContainerImageVersionsAreConsistent` (the example uses
  `alpine:3.23`).
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema/design banner/arc dashboard)
  refreshed in the same PR.

## Acceptance Criteria

**Arc-level statement:** this plan is **optional** (arc criterion 5). Not
running it does not block the arc. If it runs, the arc's bar is: *a
`trigger: {type: window}` job parks, is released by the predictor, and
force-starts at the deadline-safe moment across a server restart.* The
criteria below are **P0 only**; parked items (D1 unless un-parked, D2, E2)
are not gates.

The plan is done when **all** of these hold:

1. **Stream A — the window trigger + parking substrate** is live: a `window`
   trigger validates and lints (including satisfiability) **only when
   `CAESIUM_WINDOW_SCHEDULING_ENABLED=true`** and is rejected otherwise, a
   fired window parks a durable `run_queue` row with the window columns
   set, the dequeuer (and the queue-depth cap/gauge and `GET /v1/jobs/:id/queue`)
   ignore window rows (`window_deadline IS NULL` disjoint population), DST
   edge cases resolve per the tested policies, and a window job with no SLA
   gets an implicit `completedBy`. Closed by unit tests for
   validation/gate/lint/DST + the parking path exercised in the Stream B/C
   integration scenario.
2. **Stream B — the scheduler engine** is a runtime feature: a leader-gated loop
   releases parked runs through normal admission (`job.New` →
   `Store.AdmitRun`), `earliest`/`latest` objectives work, force-start fires at
   `deadline − p95 − buffer` (cold-start degrades to window open), the p95
   reads the existing `JobRun`/`TaskRun` timestamps (and the Plan 2 stats
   columns when present) with no new table, a release whose admission **queues**
   (or whose job already has a run in flight) is detected through B2's
   admission-outcome seam and skipped/flagged rather than silently attaching to
   the old run, the reconciler re-parks missed
   opens after a restart, and the
   `window_planned`/`window_forced`/`window_deadline_at_risk` metrics increment.
   Closed by a `test/` integration scenario: park → force-start → complete →
   survive a mid-window restart, green in CI on the default lane.
3. **Stream B4 — explainability:** `caesium why` on a window-released run
   renders "started at … because p95=… and deadline=…" from a persisted event,
   asserted by an integration scenario over `runCLIStdout` (arc criterion 7).
4. **Stream C — the operator surface** is live: `GET /v1/jobs/:id/window` and
   `GET /v1/window/parked` return the plan + rationale + derived
   `latest_safe_start` (mounted only with the flag on), and `caesium job window`
   renders them with clean, parseable `--json` stdout (asserted via
   `runCLIStdout`). Closed by integration scenarios hitting the live server +
   the CLI binary.
5. **Stream D — signal gating:** *(explicitly recorded as parked — not a
   gate).* If D1 is un-parked by an arc decision, its criterion is: the
   running-count load gate parks over the ceiling, closed by an integration
   scenario. D2's criterion (the `cheapest` objective starts in the forecast
   valley from a static calendar or an event-ingested feed, failing open on an
   expired signal) is preserved here for the record and is not a gate.
6. **Stream E — frontend (E1)** ships: the job-detail planned-start badge
   renders the plan + rationale, driving the Stream C endpoint, hidden when
   the feature is off; `just ui-e2e` green. *(E2 — the run-history window bar
   showing elasticity used — explicitly recorded as parked, not a gate.)*
7. **H-1 — every self-server lane** runs with
   `CAESIUM_WINDOW_SCHEDULING_ENABLED=true` (the list in H-1), so the Stream
   B/C scenarios drive the live binary in CI on every lane, not an internal
   call, and the restart scenario runs on the default lane.
8. **N-1 — docs reflect reality:** the `docs/roadmap.md` rows point at this
   plan, the `design-window-scheduling.md` `> Status:` banner is flipped, the
   `window` trigger fields are documented across the schema references (via
   the generator) with a working `docs/examples/` manifest,
   `docs/tour-time-loop.md` exists and is indexed, this plan is indexed in
   `docs/README.md`, and the arc dashboard row for Plan 4 is ticked.
9. **Cross-cutting:** `docs/roadmap.md`, `docs/design-window-scheduling.md`,
   `closed-loop-arc.md`'s dashboard, and this plan's per-stream `## Progress`
   entries reflect every shipped stream and match the merged PRs.

## How To Pick Up Work

1. Read [`closed-loop-arc.md`](closed-loop-arc.md) first, then this file
   end-to-end, so you understand the arc gate (this plan runs only after
   Plan 3, only with momentum), the streams, their interdependencies, and
   which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`). Do not pick D2 or E2 (parked), or
   D1 unless the arc's § Parked entry has been revised.
3. Re-grep every symbol this plan cites before editing (arc convention 6 —
   the plan cites symbols, not lines; the 2026-07 draft drifted 50–300 lines
   before this re-cut).
4. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
5. Run the verification block under `## Verification (Run For Every PR)`.
6. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
7. Open the PR with title format
   `<Imperative subject> (window-scheduling <wave>-<stream>)` — e.g.
   `Add the window trigger type and parking columns (window-scheduling W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`closed-loop-arc.md`](closed-loop-arc.md) — the umbrella arc; this is its
  Plan 4 (optional tail). Its loop table ("Time" row), § Synergies ("Window
  scheduling specced its own p95 predictor" → Plan 4 Stream B reads Plan 2
  A1's columns), § Shared conventions 1–8, § Cross-plan sequencing (the
  `definition.go` / `store.go` conflict rows), and arc acceptance criterion 5
  govern this plan.
- [`resource-right-sizing.md`](resource-right-sizing.md) **Stream A (A1)** —
  Plan 2's `TaskRun` stats columns (`PeakMemoryBytes`, `CPUSeconds`,
  `StatsSource`, `OOMKilled`, `AppliedResources`, `EscalationLevel`) are the
  shared run-history substrate B1 reads when present. **`HistorySource` is
  defined by Plan 2 D1, in `internal/rightsizing/`, and adopted by B1** — arc
  convention 5 and the arc Synergies row ("Plan 2, Stream D defines; Plan 4,
  Stream B adopts"); Plan 2 D1's own text says "this item is the definition
  site and Plan 4 B1 imports it" and asks that this plan's text be corrected
  at its next re-cut — done here. B1 passes p95 where right-sizing passes p99.
  Soft dependency — see Sequencing for the not-yet-shipped fallback.
- the removed `docs/design-sla-management.md` (`git show 2459109:docs/design-sla-management.md`) —
  **superseded design** (arc § Parked designs: breach detection already shipped in
  `internal/notification/watcher.go`, freshness SLOs cover the declarative
  half). Its predictive-ETA engine folds into this plan's B1 predictor
  (`ETA(runID)`); its EWMA model, escalation chains, `internal/sla/` package,
  `caesium sla` verbs and REST surface are **not** imported.
- [`docs/design-window-scheduling.md`](../../design-window-scheduling.md) — the
  design of record and source of truth for how P0 streams are built.
- [`docs/roadmap.md`](../../roadmap.md) Phase 5 (closed-loop arc table, row 4)
  and Phase 4 Data-Plane Differentiators (Deadline-window scheduling row) —
  the strategic entries this plan promotes from design to in-progress.
- [`docs/exec-plans/completed/freshness-scheduling.md`](../completed/freshness-scheduling.md) —
  the sibling temporal-scheduling initiative; freshness policies compile down to a
  rolling window + deadline, so they compose as layers (freshness decides *what
  deadline*, this plan decides *when inside the window to start*) over the same
  run-history + `run_queue` substrate. Its `CAESIUM_FRESHNESS_ENABLED` gating of
  `trigger.type: freshness` in `validateTrigger` is the pattern A2 mirrors.
- [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md),
  [`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md) — companion
  designs: right-sizing shares the run-history substrate and the signal-source
  interface; dynamic fan-out is the spatial slice to this temporal one.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the
  job-definition contract the `window` trigger extends with type-specific
  configuration validation (`validateTrigger`, `freshnessFeatureEnabled` — the
  gate A2 mirrors; `ValidateTriggerSpec`, whose bare-trigger-API refusal is
  freshness-specific and is **not** mirrored, see the Source-Of-Truth Note).
- `internal/runqueue/dequeuer.go` (`Dequeuer.DrainOnce`, `reclaimStaleClaims`),
  `internal/run/store.go` (`AdmitRun`/`admit`/`DequeueNextRun`/`CountActive`/
  `LatestSuccessfulCronRun`), `internal/trigger/cron/` (`Cron.fireAt`,
  `fireCatchup`, `ParseSchedule`), `internal/job/job.go` (`New`, `WithParams`,
  `WithPriorityOverride`, `WithTriggerID`), `internal/job/backfill.go`
  (`EnumerateLogicalDates`), `internal/run/why.go` (`WhyTrigger`,
  `Store.loadTrigger`), `pkg/dqlite` (`IsLocalLeader`) — the shipped queueing,
  admission, cron-parsing, launch, explain, and leadership machinery this plan
  schedules over.

## Open Questions

Carried from the design (numbered as there) plus those raised by the re-cut;
none blocks W1.

1. **Weighted scoring vs. gate chain** — parked with D2 (see Stream D).
2. **Separate `window_queue` table?** Reusing `run_queue` shares claim
   machinery but overloads one table; split if window-specific state grows.
   The re-cut adds four extra `run_queue` read sites that must exclude window
   rows (A1) — if that list grows again, that is the signal to split.
3. **Global vs. per-job load ceiling** — parked with D1; per-namespace fairness
   belongs to roadmap §3.1, not here.
4. **Quantile choice** — heavy-tailed jobs may want p99; B1's `HistorySource`
   should make the quantile a parameter so right-sizing (p99) and windows
   (p95) share the reader without sharing the number.
5. **Backfills** — backfill runs bypass ordinary concurrency accounting
   (`Store.admit` returns `admissionNoPolicy` when `model.BackfillID != nil`);
   should backfilled logical dates respect windows? Leaning immediate execution
   (operator intent is explicit).
6. **Run-level `why` — settled, not open.** The verb is task-scoped:
   `caesium why <run-id> --task <task> --job-id <job-id>` with route
   `GET /v1/jobs/:id/runs/:run_id/why` (arc convention 3; the arc's Synergies
   row adds "no plan adds a run-level `why` verb unless one item says so"; arc
   § Parked lists "a run-level `caesium why` verb (today's is task-scoped)").
   B4 renders the window sentence in the trigger block of any task's
   explanation and **adds no verb and no route**. *(The 2026-09-05 re-cut
   listed this as an arc/plan conflict citing a `GET /v1/runs/:id/why` form;
   that route appears nowhere in the arc doc — the citation was wrong and the
   question is closed.)*
7. **Cancelling a parked run.** A1 leaves `DELETE /v1/jobs/:id/queue/:queue_id`
   (`jobService.CancelQueuedRun`) able to delete a window row. Is that the
   operator surface for "un-park", or should C1 add `DELETE /v1/window/parked/:id`
   so the queue endpoint stays concurrency-only? Decide in A1/C1; do not ship
   both.
8. **Mid-window restart mechanics.** The default lane's test container shares
   the server's network namespace; `docker restart` of the server may or may
   not preserve that join. H-1 proposes a two-phase recipe from the host shell;
   if the join survives, an in-suite helper over the mounted Docker socket is
   simpler. Verify on the first H-1 PR.
9. **`window_deadline_at_risk` as an incident input.** The design names it a
   natural agent-in-the-loop input ("reschedule within window" playbook verb).
   Arc convention 4 (agent actions) is out of this plan's P0; the arc's loop
   table lists "agent proposes window changes when deadlines drift" as *later*.
   Not scheduled here.
