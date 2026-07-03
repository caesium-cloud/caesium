# Design: Deadline-Window Scheduling (Temporal Elasticity)

> Status: Brainstorm/Design — proposal for window-based scheduling with a
> completion deadline and signal-driven start selection. No implementation
> yet. This is the *temporal* slice of "Dataflow-style elasticity"; the
> spatial slice is [`design-dynamic-fanout.md`](design-dynamic-fanout.md).
> Companions: [`design-sla-management.md`](design-sla-management.md) (§2.3),
> [`design-freshness-scheduling.md`](design-freshness-scheduling.md),
> [`design-resource-right-sizing.md`](design-resource-right-sizing.md).

## Problem

Cron makes users encode a *guess* about the best start time, when what they
actually know is a *constraint* about the finish time:

- **Thundering herd at 00:00.** Every nightly job fires at `0 0 * * *`; the
  cluster spikes, then idles for hours. The cron trigger starts them all in
  the same tick (`internal/trigger/cron/cron.go:133-145` — one goroutine per
  job, no spreading).
- **Cron encodes a guess.** "Run at 02:30" means "upstream lands by ~02:00,
  the report is needed by 06:00, and 02:30 seemed safe." When the job slows
  down, the guess silently rots until the deadline is missed.
- **Cost and carbon vary 3–5× intraday.** Spot prices, tariffs, and grid
  carbon intensity have deep nightly valleys; cron pins a movable job to a
  minute chosen with none of that information.
- **Deadlines are what actually matter.** `metadata.sla.completedBy` already
  alerts when a job hasn't finished by a wall-clock time
  (`internal/notification/watcher.go:136-246`) — but it only *detects* the
  miss; nothing uses the deadline to decide when to *start*.

The proposal: jobs declare an execution **window** plus a completion
**deadline** ("start any time between 00:00 and 05:00, finish by 06:00");
Caesium picks the start moment using predicted duration (p95 from run
history), current cluster load, optional cost/carbon signals, and priority —
with a hard deadline-safety rule that force-starts the run at the latest safe
moment regardless of signals. This is a **queueing/scheduling policy over
machinery that already exists** (durable run queue, priorities, atomic
admission, SLA events) — not an autoscaler, and it does not place tasks on
nodes.

## Fit with Design Principles

1. **Container-native execution.** Nothing changes about *what* runs — only
   *when* a run is initiated.
2. **Declarative and GitOps-first.** The window is YAML on the trigger,
   lintable (`open + p95 + buffer ≤ deadline` satisfiability) and diffable.
3. **Zero-dependency simplicity.** Parked runs are rows in the existing dqlite
   `run_queue`; signals are an env-configured static calendar or events via
   the shipped `POST /v1/events` pipeline — never a mandatory external
   service. Deployments without windows pay nothing.
4. **Smart by default.** The predictor reads history the server already stores
   (`job_runs.started_at`/`completed_at`, `internal/models/run.go:35-36`);
   users declare intent, not schedules.
5. **Data engineering first.** Nightly loads and vendor-drop-then-deadline
   pipelines are the workloads with real windows.

## Overview

```
 window trigger fires at window open (reuses robfig/cron schedule)
        │
        ▼
 park durable row in run_queue (window columns set, claimed_by='')
        │
        ▼                            every CAESIUM_WINDOW_CHECK_INTERVAL
 ┌──────────────────────────────────────────────────────────────────┐
 │ Window Scheduler (leader-gated, same LeaderCheck as dequeuer)    │
 │  for each parked row, priority DESC then slack ASC:              │
 │   now ≥ forceAt?  ──────────────▶ FORCE START (ignore signals)   │
 │   else: load gate → cost/carbon gate → all green? ─▶ start now   │
 │   else: stay parked, record rationale                            │
 └──────────────────────────────────────────────────────────────────┘
        │ start = store.AdmitRun(...)   (normal atomic admission —
        ▼                                concurrency/priority still apply)
 run executes exactly like any cron-triggered run
```

where `forceAt = deadline − p95(duration) − buffer` — the **latest safe
start**.

## YAML

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: nightly-warehouse-load
  priority: high                    # existing field; also orders window release
trigger:
  type: window
  configuration:
    cron: "0 0 * * *"              # cadence + window-open anchor + logical date
    deadline: "06:00"              # run must COMPLETE by this wall-clock time
    close: "05:00"                 # optional hard cap on start time
    timezone: "America/New_York"   # IANA; default UTC
    buffer: "15m"                  # deadline-safety margin; default 10m
    objective: cheapest            # earliest | latest | cheapest (default earliest)
steps:
  - name: load
    image: warehouse-load:latest
    command: ["load.sh"]
```

- `latestStart` is **auto-derived**: `deadline − p95 − buffer`, re-evaluated
  every scheduler tick; `close`, if set, caps it
  (`effectiveLatest = min(close, latestSafeStart)`).
- `objective`: `earliest` (start at open — cron-equivalent plus deadline
  guarantee), `latest` (park until latest-safe-start — freshest inputs at
  deadline), `cheapest` (wait for signal valleys, P1/P2).
- Lint: `deadline` after open (modulo rollover), `close < deadline`, cron
  valid per the existing 5-field parser
  (`internal/trigger/cron/cron.go:61-67`, robfig/cron v1.2.0), timezone via
  `time.LoadLocation` (as `extractLocation`, cron.go:268-287).

The alternative — `type: cron` plus a `window:` sibling block — was rejected:
a cron trigger's contract is "fire at this minute," and an annotation that
turns it into "fire sometime later" would surprise in diffs and the UI.
`TriggerType` is an open string enum (`internal/models/trigger.go:14-18`); the
executor's listing loop needs `window` added
(`internal/executor/executor.go:38-42`).

## Scenarios

1. **Nightly warehouse load spread across the cluster valley.** Forty jobs
   pinned at `0 0 * * *` become `window 00:00 → 05:00, deadline 06:00` with
   the P1 load gate. All forty park at 00:00; the scheduler releases them in
   priority order while running-task count stays under the ceiling, smearing
   the herd across the valley.
2. **Carbon-aware batch.** A weekly rebuild declares `objective: cheapest`
   with a static carbon calendar (or a webhook feed posted into `/v1/events`)
   and starts in the greenest hour of its window — unless the valley never
   comes, in which case the force rule fires anyway.
3. **Deadline force-start.** `deadline 06:00`, p95 = 50m, buffer = 15m;
   signals stay red all night. At 04:55 the scheduler stops caring, forces the
   start, emits `window_forced`; the run completes ~05:45, inside deadline.

## Backend

### Window trigger

`internal/trigger/window` implements the three-method `Trigger` interface
(`Listen`/`Fire`/`ID`, `internal/trigger/trigger.go:10-14`) and reuses cron's
schedule/timezone parsing (`cron.ParseSchedule`, cron.go:222-254). Its
`Listen` waits for the next window *open* exactly as cron waits for its tick
(cron.go:82-104) — but on fire it does **not** launch the job; it inserts a
parked row stamped with the logical date, open, effective close, and deadline
resolved in the configured location. The in-process `time.After` is only an
optimization to park promptly; correctness never depends on it.

### Parked rows: extend `run_queue`

The durable `run_queue` table (`internal/models/run_queue.go:12-21`) already
provides restart durability, a `claimed_by`/`claimed_at` claim protocol with
stale-claim reclaim (`internal/runqueue/dequeuer.go:125-141`), and a
priority-ordered drain index. Add nullable columns:

```go
// models.RunQueue additions (all NULL for ordinary concurrency-queued rows)
WindowOpen     *time.Time // window open (resolved instant, UTC)
WindowClose    *time.Time // effective latest allowed start (hard cap)
WindowDeadline *time.Time // completion deadline
LogicalDate    *time.Time // uniqueIndex (job_id, logical_date): idempotent parking
```

`window_deadline IS NULL / NOT NULL` discriminates two disjoint populations:
the **existing dequeuer** keeps draining only concurrency-overflow rows — its
job listing (`dequeuer.go:110-114`) and `DequeueNextRun`
(`internal/run/store.go:3703-3735`) add `AND window_deadline IS NULL` — while
the new **window scheduler** owns the rest. (The dequeuer already skips jobs
without queue-strategy concurrency, `dequeuer.go:143-150`, so window rows for
jobs with no concurrency block would otherwise never drain.)

Crucially, **no in-process timer holds a parked run**. Every existing wait
(cron `time.After`, watcher/dequeuer tickers) is process-local; a parked run
must survive restarts and leader failover, so its only representation is the
row.

### Window scheduler loop

`internal/windowsched`, structured like the run-queue dequeuer: ticker,
`DrainOnce`, and the same leadership gate — `LeaderCheck` wired to
`dqlite.IsLocalLeader` exactly as the dequeuer is
(`internal/runqueue/dequeuer.go:21,94-103`; `cmd/start/start.go:177-184`).
Only the dqlite leader evaluates and releases window rows, so two nodes never
double-start a parked run; claim columns + stale reclaim cover leader death
mid-release.

Each tick, for parked rows ordered `priority DESC, (deadline − now − p95) ASC`:

```
p95     := predictor.P95(jobID)
forceAt := min(windowClose, windowDeadline − p95 − buffer)
if now ≥ forceAt:              release(FORCED)     // unconditional
else if now < windowOpen:      skip
else if objective == earliest: release(PLANNED)
else if objective == latest:   skip until forceAt (that IS the plan)
else:                          // cheapest: gate chain, all must be green
    load gate:  running task count < ceiling             (P1)
    cost gate:  signal(now) ≤ min over [now, forceAt]    (P2)
    green → release(PLANNED, rationale);  red → stay parked, record rationale
```

Deliberate choice: **a gate chain with recorded rationale, not a weighted
score** — every decision explainable in one sentence (`"parked: cluster at
41/32 running tasks; forecast valley 02:00; force at 04:55"`), the bar the
data-plane-memory verbs set.

**Release** = `store.AdmitRun(jobID, triggerID, ...)`
(`internal/run/store.go:1044`) with the parked row's params, priority, and
logical date — the run passes normal atomic admission (`store.admit`,
store.go:711-778; single conditional `INSERT ... WHERE count(active) <
maxRuns`, store.go:780-809). Never bypasses concurrency; see Safety.

### Duration predictor

`internal/windowsched/predictor.go`: p95 over the last N (default 20)
succeeded, non-quarantined runs — the same duration query shape the stats
service uses (`api/rest/service/stats/stats.go:72-77`,
`completed_at − started_at` over `job_runs`).

- **Why p95, not EWMA:** the SLA design proposes an EWMA predictor for
  *at-risk detection*; a *safety margin* wants a conservative upper quantile.
  If `internal/sla/predictor.go` ships, the window scheduler should consume
  quantiles from that shared package — one engine, two consumers.
- **Cold start (required policy):** fewer than 3 completed runs → no p95 →
  `forceAt = windowOpen`: **the run starts at window open**, degrading to cron
  behavior. Elasticity is earned by history, never assumed.
  `metadata.runTimeout`, when set, caps the assumed duration.
- **Regression guard:** forceAt is recomputed every tick from live history; if
  p95 grows past remaining slack, the force rule fires on the next tick.

### Signal sources (zero-dependency, pluggable)

One interface, three shipped implementations, selected by env:

1. **Static calendar** — `CAESIUM_WINDOW_SIGNAL_CALENDAR`: JSON mapping
   day-of-week/hour to a relative cost score. Zero I/O.
2. **Event-ingested feed** — operators POST `signal.cost` / `signal.carbon`
   events through the shipped ingestion endpoint (`POST /v1/events`, roadmap
   §1.2); a subscriber persists the latest value per signal with a TTL.
   Expired signal ⇒ gate green (fail-open: a dead feed must never strand a
   job past its valley). Caesium never calls out to a price API itself.
3. **Load** — no configuration: count of running runs/tasks (same pattern as
   `CountActive`, `internal/run/store.go:3654-3660`), gated by
   `CAESIUM_WINDOW_LOAD_MAX_RUNNING`.

### Restart safety & missed opens

On becoming leader and on every tick, the scheduler reconciles: compute each
window job's most recent open ≤ now in its timezone; if the window is still
feasible and no run/parked row exists for that logical date (unique index ⇒
idempotent), park it — forcing immediately if `now ≥ forceAt`. This mirrors
cron catchup's watermark approach (`fireCatchup`, cron.go:150-197) but is
driven from the durable table, not process memory.

### Timezones & DST

Window open resolves through the cron schedule evaluated in the configured
location (as `nextTick` does today, cron.go:325-331); `deadline`/`close` via
`time.Date` in the same location on the logical date. robfig/cron v1.2.0 has
no native tz support — Caesium already owns location conversion and this
design inherits it. Policies (tested): spring-forward gap → first valid
instant after it; ambiguous fall-back → first occurrence (earlier UTC —
conservative for a deadline); DST-collapsed window (`open ≥ forceAt`) → park
and immediately force with a `window_collapsed` warning. Note: the shipped
`sla.completedBy` resolver is UTC-only HH:MM (`watcher.go:386-400`); window
deadlines are tz-aware from day one and do not reuse it.

### SLA integration (reuse, don't duplicate)

Deadline *enforcement before the fact* is this design; deadline *alerting
after the fact* is shipped — the watcher already emits `sla_missed` for
duration and completedBy SLAs (`watcher.go:119-128, 136-246`). `job apply`
derives an implicit `sla.completedBy` from the window deadline when the job
declares no SLA, so a blown deadline produces the standard `sla_missed` event
with zero new alerting machinery. New bus/store events: `window_planned`,
`window_forced`, `window_deadline_at_risk` (a forced start whose p95 no longer
fits).

### Models, REST, env

- **Models**: four nullable `run_queue` columns + unique
  `(job_id, logical_date)` partial index; no new tables.
- **REST**: `GET /v1/jobs/:id/window` — config, p95 + sample count, derived
  `latest_safe_start`, current plan (`planned|parked|forced|none`), rationale,
  sampled signals; `GET /v1/window/parked` — all parked rows.
- **Env** (envconfig, `pkg/env/env.go` pattern):
  `CAESIUM_WINDOW_SCHEDULING_ENABLED` (`false`), `_CHECK_INTERVAL` (`15s`),
  `_DEFAULT_BUFFER` (`10m`), `_P95_SAMPLES` (`20`), `_LOAD_MAX_RUNNING`
  (`0` = off), `_SIGNAL_CALENDAR`, `_SIGNAL_TTL` (`1h`).

## CLI

```
$ caesium job window nightly-warehouse-load
Job:               nightly-warehouse-load
Window:            00:00 → 05:00 America/New_York (deadline 06:00)
Predicted p95:     47m  (18 samples)
Latest safe start: 04:58  (deadline − p95 − 15m buffer)
Plan:              parked — waiting for cost valley
Why:               cluster 12/32 running (green); cost signal 0.71 now,
                   forecast minimum 0.24 at 02:00; will start ≈02:00,
                   forced at 04:58 regardless.
```

`caesium job window <alias> --json` emits the REST payload on **stdout**
(machine output separated from logs — the hard-won rule in CLAUDE.md).
Subcommand precedent: `caesium job queue` (`cmd/job/queue.go`).

## Frontend

- **Job detail**: planned-start badge on the trigger summary (`parked · starts
  ≈02:00 · forced 04:58`) with the rationale as tooltip/expando.
- **Run detail / history**: a horizontal window bar per run — window span,
  planned start, actual start (colored planned/forced), actual end, deadline
  tick — showing at a glance how much elasticity was used.

## Safety

**Deadline guarantee (proof sketch).** Let δ = tick interval, α =
admission+claim latency, D = deadline, P = p95, B = buffer. The force rule
releases by `t ≤ D − P − B + δ`; the run starts by `t + α` and, with
probability ≥ the p95 coverage (≈0.95 under stationarity), completes by
`D − B + δ + α`. Choosing `B > δ + α` (defaults: 10m vs 15s + subsecond
admission) yields: **Caesium initiates the start early enough that a
p95-or-faster run completes before the deadline.** It is a *start* guarantee;
completion stays probabilistic — regressions beyond p95 or a saturated cluster
can still miss, which is exactly what the derived `sla_missed` +
`window_deadline_at_risk` events flag.

**Concurrency admission interplay.** A window start — including a forced one —
still passes `store.admit()` (store.go:711): `maxRuns` can queue, skip, or
fail it per the job's declared strategy. Deliberate (admission invariants have
one owner), but it makes the guarantee conditional on admission. Mitigations:
forced releases are stamped priority `high` so the run-queue dequeuer and
distributed claimer drain them first (`ORDER BY priority DESC, created_at
ASC`, store.go:3708-3718; roadmap §1.4); a forced release that lands in the
concurrency queue instead of starting emits `window_deadline_at_risk`.

**Starvation & priority.** Parked rows are evaluated priority-first, so
high-priority jobs claim signal valleys first — but every parked row owns a
force time, so a low-priority job is delayed only until its own
latest-safe-start, never starved past it. Priority shapes *where in the
window* a run lands, never *whether* it runs.

**No new write authority.** The scheduler only inserts runs through the same
admission path triggers use; leader-gating plus the atomic conditional insert
(no cross-node TOCTOU) means the worst failover outcome is a run starting one
tick late, never twice.

## Testing

Per the repo's end-to-end gate (CLAUDE.md): every new CLI command and REST
endpoint ships with an integration test in `test/` driving the real surface.

- **Integration** (`-tags=integration`, with
  `CAESIUM_WINDOW_SCHEDULING_ENABLED=true` added to `just integration-up` —
  config-gated features must be enabled there or CI proves nothing): apply a
  job with a seconds-scale window; assert it parks (visible via
  `GET /v1/window/parked` and `caesium job window`), force-starts by
  latest-safe-start, and completes; assert `--json` stdout is clean and
  parseable via `runCLIStdout` (never the stream-merging `runCLIRaw`); restart
  the server mid-window and assert the parked run survives and still starts;
  cold-start job (no history) starts at window open.
- **Unit**: p95 + cold-start floor; forceAt derivation incl. `close` cap; DST
  cases; gate-chain rationale strings; signal parsing + TTL fail-open;
  reconciler idempotency. Injectable clock; integration tests use short real
  windows, not mocked time.

## Phasing

- **P0 — window + latest-safe-start, no signals.** Trigger type, run_queue
  columns, leader-gated scheduler with `earliest`/`latest` objectives, p95
  predictor + cold-start policy, force-start, derived `completedBy` SLA,
  `caesium job window`, REST, integration tests.
- **P1 — load signal.** Running-count gate + ceiling env; the thundering-herd
  scenario. No new tables.
- **P2 — pluggable cost/carbon.** `cheapest` objective, static calendar +
  event-ingested signals, forecast-minimum gate, UI window bar.

## Non-Goals

- **No autoscaling.** Caesium never adds/removes capacity; it moves work in
  *time* to fit the capacity that exists.
- **No preemption.** A started run is never paused or killed to make room
  (consistent with §1.4's "strictly ordering, never preemptive").
- **No bin-packing scheduler.** Node placement stays with the engines; on
  Kubernetes, quota-aware admission is already delegated to Kueue
  (`pkg/jobdef/definition.go:192-198`).
- **No per-step windows.** The window is a run-level property.

## Relationship to Sibling Designs

- [`design-freshness-scheduling.md`](design-freshness-scheduling.md) —
  freshness targets may *subsume* windows for data-driven jobs: a freshness
  policy compiles down to a rolling window + deadline. They compose as
  layers — freshness decides *what deadline a run must meet*; this design's
  parking/predictor/force machinery decides *when inside the resulting window
  to start*. One substrate, two intent front-ends.
- [`design-dynamic-fanout.md`](design-dynamic-fanout.md) — the spatial
  elasticity slice (how wide); this doc is the temporal slice (when).
- [`design-resource-right-sizing.md`](design-resource-right-sizing.md) — same
  run-history substrate applied to sizing; its cost models should share the
  signal-source interface defined here.
- [`design-sla-management.md`](design-sla-management.md) — shipped subset
  (`watcher.go` breach detection) reused verbatim; its proposed
  predictive-ETA engine should become the shared quantile provider.
- [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) —
  `window_deadline_at_risk` / `window_forced` are incident inputs;
  "reschedule within window" is a natural bounded playbook verb.

## Open Questions

1. **Weighted scoring vs. gate chain** — is an explainable gate chain enough,
   or do heterogeneous fleets need tunable coefficients? Deferred until P2.
2. **Separate `window_queue` table?** Reusing `run_queue` shares claim/reclaim
   machinery but overloads one table behind a discriminator; split if
   window-specific state grows (rationale history, signal snapshots).
3. **Global vs. per-job load ceiling** — a single cluster ceiling is crude;
   per-namespace fairness belongs to roadmap §3.1, not here.
4. **Quantile choice** — heavy-tailed jobs may want p99 or `max(recent)`;
   possibly a `durationQuantile` knob.
5. **Backfills** — backfill runs bypass ordinary concurrency accounting
   (store.go:723-728); should backfilled logical dates respect windows, or
   always run immediately? Leaning immediate (operator intent is explicit).
