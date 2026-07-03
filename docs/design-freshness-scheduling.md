# Design: Freshness-Driven Scheduling — Schedule on Data, Not Time

> Status: Brainstorm/Design — proposal for dataset freshness SLOs and
> lineage-derived scheduling. No implementation yet. The strategic flagship
> of this design wave: cron becomes the fallback, not the model. Companion
> designs: [`design-window-scheduling.md`](design-window-scheduling.md),
> [`design-data-circuit-breaker.md`](design-data-circuit-breaker.md),
> [`design-contract-enforcement.md`](design-contract-enforcement.md),
> [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md),
> [`design-dynamic-fanout.md`](design-dynamic-fanout.md).

## Problem

A cron expression is a *guess about when data will have arrived*. The guess is
why 3 a.m. pages exist:

- The vendor file usually lands by 03:00, so the extract runs at 03:15. The
  day it lands at 04:30, the extract **fails** and the on-call is paged; the
  "fix" is to wait and press retry. The delayed-file incident class in
  [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) exists
  largely because time-based scheduling turns "not yet" into "error".
- To be safe, the guess is padded: run hourly "just in case", re-run DAGs
  whose inputs haven't changed. Compute is burned proving nothing happened.
- The thing consumers actually care about is never stated anywhere: *how
  stale may this table be?* Nobody consumes "the 03:15 run"; they consume
  `analytics.orders_daily` at most 6h out of date. Today that SLO lives in
  a runbook, if anywhere.

Freshness-driven scheduling inverts the declaration. Jobs declare the
datasets their steps produce and consume, and a freshness SLO on each
output ("at most 6h stale"). Caesium derives execution from that graph:
**run** when upstream data has arrived and my output is stale against its
SLO; **don't run** when nothing changed; **don't page** when upstream is
late — the dataset shows `stale-upstream: waiting on raw.vendor_x`, an
observable state with a reason, not a failed run. Escalation happens on
*SLO risk*, not a step's exit code. Materialize/Dagster-freshness-policies
energy, but container-native (the contract stays stdout markers + YAML, no
SDK) and zero-dependency (dqlite rows, the existing event router and cache
identity).

## Fit with Design Principles

1. **Container-native execution.** Nothing about a step changes: watermarks
   ride the existing `##caesium::output` marker; datasets are declared in
   YAML. No SDK, no library.
2. **Declarative and GitOps-first.** `produces`/`consumes`/`freshness` are
   jobdef fields — linted, diffed, PR-reviewed, reconstructable from
   manifests alone.
3. **Zero-dependency simplicity.** The registry and state machine are dqlite
   tables; arrival signals ride the shipped event ingestion. **No built-in
   S3/SFTP pollers** — external arrival is event push or a documented
   sensor-container pattern.
4. **Smart by default.** Skip-when-fresh composes with the shipped cache:
   the cache makes a no-op run cheap; freshness makes it free (no run).
5. **Data engineering first.** Freshness SLOs on datasets are the native
   language of data teams — what Airflow's `schedule_interval` never became.

## Overview

```
 YAML apply                      arrival signals
 produces/consumes/freshness     POST /v1/events, /v1/hooks/*,
      │                          sensor job, ##caesium::output watermark
      ▼                                  │
 ┌───────────────────┐           ┌───────▼────────────────┐
 │ Dataset registry  │──────────▶│  Dataset state store   │
 │ (declared graph)  │           │  watermark, advanced_at,│
 └───────────────────┘           │  status + reason        │
                                 └───────┬────────────────┘
                                         │ evaluate (leader-gated loop
                                         │ + reactive on advance events)
                                         ▼
              fresh ──────────▶ skip (recorded: "fresh")
              stale ──────────▶ derive run ──▶ _trigger_depth +
              stale-upstream ─▶ wait + at-risk   concurrency admission
              violated ───────▶ freshness_violated event
                                (notifications + agent incident)
```

## What exists today (honest inventory)

- **Observed lineage datasets exist, but only behind OpenLineage.**
  `lineage_datasets` rows (`internal/models/lineage_dataset.go:20-41`) are
  written per task run by the lineage mapper — only when
  `CAESIUM_OPEN_LINEAGE_ENABLED=true` (default `false`,
  `pkg/env/env.go:143`). Dataset identity is *derived*, not declared:
  `buildTaskDatasets` (`internal/lineage/mapper.go:611-698`) promotes
  **path/URI-like structured output values** to datasets or synthesizes
  `<job>.<step>.output` from a declared `outputSchema`; inputs come from
  `inputSchema` keys. Most jobs today declare neither and emit no path-like
  outputs — the observed graph is sparse and appears only *after* runs
  happen. **Freshness cannot bootstrap from observation alone; it needs
  explicit YAML declarations.**
- **Downstream traversal exists.** `QueryImpact`
  (`internal/lineage/impact.go:82`) BFS-walks dataset consumers across job
  boundaries. The declared registry feeds this same shape.
- **Arrival signaling exists.** `POST /v1/events`, `caesium event push`,
  webhook bridging, and the event router's `_trigger_depth` chain guard
  ([`design-event-triggers.md`](design-event-triggers.md)) are shipped.
- **"Nothing changed" is already detectable.** `HashInput`
  (`internal/cache/hash.go:266-287`) folds image digest, command, env,
  predecessor hashes, **predecessor outputs**, and run params into cache
  identity; `##caesium::output-ref` carries content digests
  (`pkg/task/output.go:25-32`) and the value-verified short-circuit
  (`internal/cache/shortcircuit.go`) proves byte-identical outputs.
- **The trigger loop is NOT leader-gated.** `executor.Start`
  (`internal/executor/executor.go:36-59`) is a per-process 60s ticker
  launched unconditionally on every node (`cmd/start/start.go:589-594`); the
  cron trigger fires with no leader check
  (`internal/trigger/cron/cron.go:82-104`). The leader-gated house pattern
  is the run-queue dequeuer (`internal/runqueue/dequeuer.go:21-70`, wired
  with `LeaderCheck: dqlite.IsLocalLeader` at `cmd/start/start.go:183`) —
  the evaluator must follow the dequeuer, or an N-node cluster derives N
  duplicate runs per stale dataset.
- **Admission exists** (`internal/run/store.go:711` `admit`; `AdmitRun` at
  `store.go:1044`). Derived runs pass it like everyone else.

## YAML example

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: orders-daily
  datasets:
    sources:                 # external datasets nobody in Caesium produces
      - name: raw.vendor_x
        expectedEvery: 24h   # cadence expectation; late ⇒ stale-upstream
        arrival:             # event binding — how "it arrived" is signaled
          event:
            type: "s3:ObjectCreated"
            filter: { "detail.bucket.name": "vendor-x-drop" }
          watermark: "$.detail.object.key"  # JSONPath into event payload
trigger:
  type: cron                 # fallback cadence; freshness augments
  configuration:
    expression: "0 */6 * * *"
steps:
  - name: extract
    image: etl:1.4
    datasets:
      consumes: [raw.vendor_x]
      produces:
        - name: staging.orders
          freshness: 8h
          watermark: { key: max_order_ts }  # output key this step emits
  - name: transform
    image: etl:1.4
    datasets:
      consumes: [staging.orders]
      produces:
        - name: analytics.orders_daily
          freshness: 6h        # the SLO consumers actually care about
          maxStaleness: 12h    # hard bound; breach ⇒ freshness_violated
          watermark: { key: max_order_ts }
```

The step emits its watermark through the existing zero-SDK contract:
`echo '##caesium::output {"max_order_ts": "2026-07-03T04:31:00Z"}'`.

### The watermark / advance contract

A run *succeeding* is not the same as its output *advancing* — a run can
succeed and produce nothing new. The contract distinguishes them:

- **With a declared `watermark.key`**: the dataset **advances** only when
  the emitted value changes (and, for RFC3339/numeric values, only when it
  increases — a regression is recorded, never advanced). A successful run
  with an unchanged watermark updates `verified_at` ("checked, nothing new")
  but not `advanced_at`.
- **Without a watermark key** (degraded mode): a successful non-cached run's
  completion time is the watermark — an honest limitation, flagged by lint,
  that conflates "ran" with "advanced".
- **Freshness** is evaluated against `max(advanced_at, verified_at)` — a run
  (or cache-identity check) that *confirms* the output is up to date counts
  as freshening even when no new bytes were produced.
- Each run records the **consumed watermark set**, so "is my output up to
  date with my inputs" is a pure row comparison, not a heuristic.

## Scenarios

### 1. The late vendor file stops being a failure

Today: cron fires at 03:15, `extract.sh` exits non-zero on a missing file,
`task_failed` pages a human. With freshness: at 03:15 no arrival event for
`raw.vendor_x` has been seen, so `staging.orders` is `stale-upstream` — the
evaluator **does not derive a run** (running is provably pointless) and the
cron tick is skipped with a recorded reason; the dataset board shows
*"stale — waiting on raw.vendor_x, last arrived 25h ago"*. At 04:30 the S3
notification hits `/v1/hooks/*`, the arrival advances, the evaluator
reacts, and the chain refreshes. Nobody was paged. If
`analytics.orders_daily` crosses `maxStaleness` first, `freshness_violated`
escalates with the diagnosis — *"12h stale because vendor file never
arrived"* — not a stack trace. The agent design's `data_unavailable`
incident class largely dissolves here.

### 2. Fan-in: run once, when all three upstreams have arrived

`reporting-rollup` consumes three datasets produced by three jobs finishing
at unpredictable times; today that's a cron guess padded late enough to
usually be safe. With freshness the rollup needs no cron: each upstream
advance triggers evaluation, and the derivation fires only when the
rollup's output is stale **and** consumed datasets have advanced past what
the last run consumed. Three upstream completions produce one derived run,
not three (derivations dedupe on the consumed-watermark set).

### 3. Skip-when-fresh saves the compute entirely

`orders-daily` keeps its 6-hourly cron as a safety cadence. The 18:00 tick
finds `analytics.orders_daily` fresh and no upstream advance since the last
run — the tick is skipped and recorded (`skipped_fresh`). Where the shipped
cache would have started a run and cache-hit every task (cheap), freshness
starts nothing (free); when a run does derive unnecessarily, the cache still
does its job — the layers compose rather than compete.

## Backend

### Dataset registry and declarations

New jobdef surface (`pkg/jobdef/definition.go`): `datasets` on `Step`
(`consumes: [name...]`, `produces: [{name, freshness, maxStaleness,
watermark}]`) and `metadata.datasets.sources` for external datasets
(`expectedEvery`, `arrival` binding). Lint: SLO fields parse as durations; a
`consumes` name must be produced in the applied set, declared as a source,
or marked `external: true`; exactly one job produces a given dataset (any
number consume); the declared graph must be acyclic **across jobs** — a
dataset cycle is a derivation cycle (the event-trigger static-cycle check
class).

Apply upserts declarations into a new `dataset_declarations` table: the
*declared* graph, independent of and complementary to the *observed*
`lineage_datasets` graph — declarations bootstrap scheduling before any run
exists; observations (when OpenLineage is on) validate declarations and flag
drift. Freshness does **not** require `CAESIUM_OPEN_LINEAGE_ENABLED`.

### Models (`internal/models/`)

- `DatasetDeclaration` — job/step refs, namespace+name, direction, SLO
  fields, watermark key, arrival binding JSON. Rebuilt on apply.
- `DatasetState` — one row per dataset (natural key namespace+name):
  `watermark`, `advanced_at`, `verified_at`, `status`, `reason`,
  `last_run_id`. Small, hot, updated transactionally.
- `DatasetDerivation` — append-only audit of every evaluator decision:
  dataset, decision (`derived|skipped_fresh|skipped_upstream|
  skipped_admission|skipped_active_run`), consumed-watermark snapshot,
  resulting run id — the row behind "why did/didn't this run".

### Arrival signals (event-driven, never polled by Caesium)

Three ways a dataset advances, all existing surfaces:

1. **Producing step completes** with a watermark output key (the normal
   internal case).
2. **External event**: an ingested event (`POST /v1/events`, keyed by
   `CAESIUM_EVENT_INGEST_API_KEY`, or a `/v1/hooks/*` webhook such as an S3
   notification) matches a source's `arrival` binding; the watermark is
   JSONPath-extracted. `caesium event push` covers scripting;
   `caesium dataset advance` covers operators.
3. **Sensor-container pattern** (documented, not built in): a small cron
   job whose only step polls SFTP/S3/a table and, on new data, emits the
   watermark output for the source dataset it produces. The poller is just
   a container; Caesium stays zero-dependency.

### The freshness evaluator (leader-gated, durable)

New package `internal/freshness`: a single evaluator modeled on the
run-queue dequeuer, **not** the executor trigger loop — constructed with
`LeaderCheck: dqlite.IsLocalLeader` in `cmd/start/start.go`, ticking every
`CAESIUM_FRESHNESS_EVAL_INTERVAL` (default `1m`). Everything it needs
(states, declarations, derivation dedupe) is in dqlite — no in-process
timer is ever the only record of a pending decision, so failover resumes on
the new leader. A reactive fast path subscribes to dataset-advance events
and immediately evaluates the affected downstream slice; the timer loop
remains the correctness backstop.

Per produced dataset with an SLO, the state machine:

- `fresh` — `now - max(advanced_at, verified_at) ≤ freshness`.
- `stale` — SLO window exceeded (or an upstream advanced past the last
  run's consumed watermarks) **and** upstream data is available → derive.
- `stale-upstream` — SLO window exceeded but some consumed dataset has not
  advanced past what the last run consumed → running is pointless; record
  the reason, emit `dataset_freshness_at_risk` once per window.
- `violated` — `maxStaleness` breached → emit `freshness_violated` on the
  event bus (alongside `sla_missed`, `internal/event/bus.go:45`), routed to
  notification policies and — when agent-in-the-loop lands — opening a
  `freshness_violation` incident class with the diagnosis pre-attached.
- `quarantined` — set by the data circuit breaker; never `fresh`, never
  advances consumers, suppresses derivation of runs that would consume it.

### Derivation to run starts

A `stale` decision derives a run for the producing job. Derived runs:

- are stamped `_trigger_depth` exactly like event-chained runs, so a refresh
  cascade rides the existing runaway guard (`CAESIUM_MAX_TRIGGER_DEPTH`) and
  a cycle that slipped past lint still terminates;
- pass concurrency admission (`internal/run/store.go:711`) with the job's
  declared strategy; an admission skip/queue is recorded on the
  `DatasetDerivation` row, never silently dropped;
- are deduped: at most one in-flight derivation per (dataset,
  consumed-watermark set), and none while the producing job already has an
  active or queued run consuming the same watermarks;
- carry params (`logical_date`, `_derived_from_dataset`, consumed
  watermarks) so a step can extract incrementally ("everything since
  `$CAESIUM_PARAM_SINCE_WATERMARK`").

### REST & config

```
GET  /v1/datasets                               list + status filter
GET  /v1/datasets/:ns/:name                     state, SLO, producing job
GET  /v1/datasets/:ns/:name/derivations         decision audit (why/why not)
POST /v1/datasets/:ns/:name/advance             manual arrival (auth-scoped)
```

Env (`pkg/env/env.go`): `CAESIUM_FRESHNESS_ENABLED` (default `false` — no
evaluator goroutine, no routes, declarations lint-only),
`CAESIUM_FRESHNESS_EVAL_INTERVAL` (`1m`),
`CAESIUM_FRESHNESS_MAX_DERIVATIONS_PER_TICK` (blast-radius cap). Metrics:
`caesium_dataset_staleness_seconds{dataset}`,
`caesium_dataset_derivations_total{dataset,decision}`,
`caesium_freshness_violations_total{dataset}`.

## CLI

```
caesium dataset list [--status stale|violated|...] [--json]
caesium dataset status <namespace.name> [--json]  # state, SLO, last decision
caesium dataset advance <namespace.name> --watermark <v>
```

`caesium job lint`/`preview` validate and render the declared graph. Per the
repo testing gate, `--json` output goes to stdout, clean and parseable,
asserted with stdout captured separately from stderr.

## Frontend (Caesium Console)

New feature dir `ui/src/features/datasets/`:

1. **Dataset freshness board** (`/datasets`, nav-level): every declared
   dataset with status chip, staleness age vs SLO bar, producing job, and
   the `stale-upstream` reason — the consumer-facing answer to "is my table
   up to date", readable without understanding DAGs.
2. **Lineage graph colored by freshness**: the existing `LineageGraph`
   component (`ui/src/features/jobs/LineageGraph.tsx`) gains a freshness
   overlay — "everything downstream of vendor-x is amber" at a glance;
   declared edges render before the first run.
3. **Job detail — "why did/didn't this run"**: a derivations panel rendering
   `DatasetDerivation` rows: *"18:00 tick skipped — analytics.orders_daily
   fresh (2h/6h)"*, *"04:31 derived by raw.vendor_x advance"*. Run detail
   shows the consumed-watermark set, linking back to the arrival event.

## Interplay

- **Cron** (precedence, honestly defined): when a job declares both,
  freshness *augments* — the evaluator may derive a run **earlier** than the
  next tick, and a tick is **skipped** when every produced dataset is fresh
  and no consumed watermark advanced (recorded, visible, opt-out via
  `metadata.datasets.skipWhenFresh: false` during trust-building). Cron
  remains the guaranteed upper-bound cadence and the fallback for undeclared
  jobs; in P2 a job may drop cron and declare `trigger: {type: freshness}`.
- **Event triggers**: arrival bindings are event patterns — same matcher,
  same router, same `_trigger_depth`; freshness adds the *state* layer (a
  dataset absorbs N arrival events into one staleness answer).
- **Backfill**: backfill runs write historical partitions and must **never
  advance** a watermark (monotonic guard; derivations ignore backfill runs —
  the same reasoning as cron catchup keying off `LatestSuccessfulCronRun`).
- **Incremental execution**: cache identity keeps a derived-but-unnecessary
  run cheap; the value-verified short-circuit lets a `verified_at` refresh
  be *proven*. Freshness decides whether to start; the cache, what to
  execute.
- **Agent-in-the-loop**: `freshness_violated` becomes an incident class
  whose triage bundle already contains the answer (which upstream, how
  late, lateness history); the delayed-file scenario mostly stops reaching
  the incident manager because no task fails.
- **Window scheduling** ([`design-window-scheduling.md`](design-window-scheduling.md)):
  freshness says IF, windows say WHEN — a derived run outside its window
  parks until it opens; staleness accrued while parked is the window's.
- **Circuit breaker / contracts**: quarantined datasets
  ([`design-data-circuit-breaker.md`](design-data-circuit-breaker.md)) are
  never fresh; contract violations
  ([`design-contract-enforcement.md`](design-contract-enforcement.md)) can,
  per policy, block the advance so downstream never freshens off bad data.
- **Dynamic fan-out** ([`design-dynamic-fanout.md`](design-dynamic-fanout.md)):
  per-partition watermarks are the natural extension, out of v1 scope.

## Testing

Per the repo's end-to-end gate, every CLI command and REST endpoint above
ships with an integration test in `test/` driving the real surface, with
`CAESIUM_FRESHNESS_ENABLED=true` set in `just integration-up`:

- Apply a two-job declared graph; `GET /v1/datasets` and
  `caesium dataset list --json` (stdout captured separately via
  `runCLIStdout`, parseable) show `unknown` → run → `fresh`.
- Arrival: `caesium event push` matching a source binding advances the
  watermark and a derived run starts; an identical second push derives
  nothing.
- Stale-upstream: expire the SLO with no arrival; assert the status, the
  `dataset_freshness_at_risk` emission, and **zero** runs started.
- Skip-when-fresh: a cron tick with fresh outputs records `skipped_fresh`
  and no run; an unchanged-watermark success updates `verified_at` only.
- Cascade + guards: a three-job chain refreshes end-to-end; a declared
  cycle is rejected at lint; a runtime cycle exhausts `_trigger_depth`.
- Evaluator leader-gating unit tests (fake `LeaderCheck`, the dequeuer's
  pattern); disabled-gate inertness. Playwright e2e for the dataset board
  and lineage overlay against a live backend (data-plane-memory-ui
  precedent).

## Phasing

- **P0 — Declarations + observability (no scheduling change).** Jobdef
  fields, lint (incl. cross-job cycle check), registry + state models,
  watermark capture, arrival bindings, evaluator in *observe-only* mode (no
  derivation), `GET /v1/datasets`, `caesium dataset list/status`, dataset
  board + lineage coloring. Day-one value: every dataset shows fresh/stale
  with reason and `freshness_violated` pages carry the diagnosis. Zero
  scheduling risk.
- **P1 — Skip-when-fresh.** Cron ticks consult dataset state and skip
  recorded-fresh work (per-job opt-in). Compute savings; no new run starts.
- **P2 — Full derivation.** Stale ⇒ derived runs through admission +
  `_trigger_depth`; fan-in dedupe; `trigger: {type: freshness}` for purely
  data-derived jobs; cron demoted to optional heartbeat. The headline
  release: schedule on data, not time.

## Non-goals (v1)

- **No built-in pollers.** Caesium never speaks S3/SFTP/JDBC to detect
  arrival; that is event push or a sensor container. Zero-dependency is
  load-bearing.
- **No data-quality judgment.** Freshness is recency, not correctness —
  quality gating is the circuit breaker's job.
- **No partition-level freshness.** One watermark per dataset in v1.
- **Not a data catalog.** The registry stores scheduling metadata only;
  rich catalogs consume our OpenLineage emission.
- **No cross-instance datasets**; namespace scoping waits for roadmap §3.1
  (models carry a nullable `namespace` column from day one).

## Open questions

1. **Watermark ordering.** Is "changed" enough, or must v1 enforce
   monotonic ordering — and what does legitimate reprocessing that lowers
   `max_order_ts` do (a `cache.version`-style `watermark.epoch` bump)?
2. **Shared dataset ownership.** Single-producer is lint-enforced per
   applied set; two repos applying to one server — apply-time conflict, or
   last-writer-wins with a warning?
3. **`verified_at` without a run.** Should P1 skip decisions *actively*
   verify (recompute the would-be `HashInput` server-side) or passively
   trust recorded consumed watermarks? Active is stronger but reconstructs
   hashing outside the worker path — does it stay honest with
   distributed-mode propagation?
4. **Evaluation fan-out.** A hub dataset with hundreds of consumers makes
   each advance a wide evaluation; does the reactive path need a budget?
5. **Snoozing a known-late source.** Can an operator acknowledge
   `stale-upstream` so the at-risk event quiets, sharing the agent design's
   persisted-timer primitive?
