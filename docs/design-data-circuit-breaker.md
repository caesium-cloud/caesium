# Design: Data Circuit Breaker — Dataset Holds & Statistical Assertions

> Status: Brainstorm/Design — proposal for runtime data-quality assertions and
> dataset-level circuit breaking ("holds"). No implementation yet. Companions:
> [`design-contract-enforcement.md`](design-contract-enforcement.md) (static
> half of the contract story), [`design-freshness-scheduling.md`](design-freshness-scheduling.md)
> (shared declared-dataset registry), [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md)
> (holds become an incident class and a gated action).

## Problem

Caesium's data contracts today are structural: `outputSchema` is validated
post-task (`internal/run/schema_validation.go`), violations persist on the
`TaskRun`, and `metadata.schemaValidation: warn|fail` decides whether the
task goes red. That catches *shape* problems — not the failures that
actually poison consumers: the extract "succeeds" on a truncated vendor file
(900 rows instead of 10 million, schema perfectly valid); a join key goes
40% null after a refactor; the watermark stalls on yesterday's data.

Three properties make this failure class worse than a red run. **Bad data
propagates further than bad runs**: a failed task stops its own DAG, but a
*successful* task that emitted garbage feeds every lineage-downstream job on
the next trigger, one hop further with every consumer that runs. **Failing
the world duplicates alerts**: making every consumer validate its inputs and
fail turns one bad extract into N red runs and N pages, burying the root
cause under its own symptoms (the noise pattern agent-in-the-loop added
`suppress_downstream_alerts` to fight after the fact). And **silent poison
is worst**: nobody is paged, and the bad numbers reach a dashboard.

The failure is not run-shaped, it is *dataset*-shaped. The missing primitive
is a circuit breaker on the dataset: when what a step *produced* looks
statistically wrong, mark the **dataset** held, let downstream jobs skip
instead of consuming poison, alert **once at the source**, and release on
human ack or the next clean run — a native primitive for scenario 3 of
`design-agent-in-the-loop.md` (bad values) and the runtime complement of the
PR-time checks in `design-contract-enforcement.md`.

## Terminology: "hold", deliberately not "quarantine"

Caesium already has a quarantine concept and it means something else:
[`design-quarantined-replay.md`](design-quarantined-replay.md) defines **run
quarantine** — a replay `JobRun`/`TaskRun` marked non-authoritative — and
that marker is load-bearing in persisted columns (`JobRun.Quarantine`,
`TaskRun.Quarantine`, `ExecutionEvent.Quarantine`), the live-bus field
(`event.Event.Quarantine`), and `quarantine IS NOT TRUE` query predicates.
Overloading it would be a correctness hazard — a reviewer seeing
`Quarantine` must be able to assume "non-authoritative replay run"
everywhere. This feature therefore uses **hold** exclusively: `DatasetHold`
model, `dataset_held`/`dataset_released` events, `caesium dataset holds`
CLI. A dataset is *held*; a replay run is *quarantined*; the two never
share a column, an event field, or a YAML key.

## Fit with Design Principles

1. **Container-native.** Assertions evaluate from what the step *prints* —
   a new `##caesium::metrics` stdout marker beside `##caesium::output`
   (`pkg/task/output.go`). No SDK, no agent, no connection to the data.
2. **Declarative and GitOps-first.** Assertions live in the job YAML on the
   producing step, linted by `caesium job lint`, diffable in PRs.
3. **Zero-dependency.** Holds, metrics, and the registry are small dqlite
   tables; baselines are percentiles computed server-side in Go — no stats
   engine or time-series store.
4. **Smart by default.** Opt-in declaration; once declared, baselines, hold
   propagation, downstream skip, and single-source alerting need no glue.
5. **Data engineering first.** The "stop the line" primitive every data team
   builds badly on top of orchestrators that only understand runs.

## Trust model (read this before the YAML)

Caesium has **no data plane**: it cannot count rows, it schedules containers
and reads their stdout. A row-count assertion works because *the step emits
the count it observed* — the same trust boundary as `##caesium::output` and
the `output-ref` digests. Stated honestly, the breaker catches **accidents,
not adversaries**: a malicious or buggy image can print flattering metrics.
The flip side is universality — any tool that can `echo` participates (dbt
post-hooks, Spark counters, a `psql` wrapper). Verifying metrics against
actual data is a job for an auditing *step*, not for Caesium.

## Overview

```
 task exits ─▶ parse markers (##caesium::output / ::metrics, pkg/task)
                 ▼
   assertion evaluator (post-task, beside schema validation)
     │ pass ─▶ record metrics, update baseline, release own hold
     ├─ violation, onViolation: warn|fail ─▶ existing schema semantics
     └─ violation, onViolation: hold ─▶ DatasetHold + dataset_held event
                 │                                 └─▶ ONE alert (policies)
                 ▼
   downstream run-admission gate (run store, same tx as admit):
     job consumes held dataset? ─▶ skip with reason, non-paging event
   release: human ack (CLI/UI/REST) or next clean producer run
            ─▶ dataset_released, gate reopens
```

## YAML

Assertions attach to a **declared produced dataset** on the step. The
`produces:` block is the declared-dataset registry — explicitly the *same*
YAML and model substrate as [`design-freshness-scheduling.md`](design-freshness-scheduling.md),
one registry keyed `(namespace, name)` read there for watermarks and here for
holds; neither design ships a private copy.

```yaml
steps:
  - name: load-transactions
    image: etl/load:1.4
    produces:
      - dataset: warehouse/transactions_daily  # registry identity
        assertions:
          # min is an absolute floor (always on); deltaFromBaseline means
          # |value − median(last N clean runs)| must be ≤ 50% of the median.
          rowCount: {min: 1000, deltaFromBaseline: 50%}
          nullRate: {metric: null_rate_customer_id, max: 0.02}
          freshness: {watermark: max_event_time, maxLag: 26h}  # RFC3339 metric
          custom:
            - {metric: dedup_ratio, max: 0.05}  # any scalar the step emits
        onViolation: hold                       # hold | warn | fail
  - name: publish-report
    image: etl/report:2.0
    consumes:
      - dataset: warehouse/transactions_daily  # admission gate input
```

The step self-reports on stdout (one line, JSON, same parse pass as the other
markers in `pkg/task/output.go` `parseMarkers`):

```
##caesium::metrics {"dataset":"warehouse/transactions_daily","rowCount":10400312,"null_rate_customer_id":0.0003,"max_event_time":"2026-07-03T01:12:00Z","dedup_ratio":0.001}
```

Semantics: `warn` records the violation (like `schemaValidation: warn`);
`fail` fails the task (red-run semantics); `hold` lets the task **succeed** —
the work is done, and failing it would just invite a retry of the same data —
but holds the dataset. A missing declared metric is itself a violation — a
step that stops emitting `rowCount` must not silently pass. `caesium job
lint` validates dataset names, `consumes` resolvability, and syntax.

## Scenario walkthroughs

**1. Truncated feed / bad values.** Nightly load writes 10M rows but 3 bad
rows upstream corrupted the dedup pass — `dedup_ratio` comes back `0.31`
against `max: 0.05`. The task succeeds; the evaluator opens a `DatasetHold`
on `warehouse/transactions_daily` and fires **one** alert naming the dataset,
producing run, and violated assertion with observed-vs-bound values.
Overnight, four downstream jobs trigger on their crons; each is
admission-gated and skips with reason
`dataset_hold:warehouse/transactions_daily` (non-paging). Morning page count:
one, at the source, with the blast radius attached.

**2. False positive: month-end.** On July 31 the row count legitimately
lands 10× baseline; `deltaFromBaseline: 50%` trips. The operator checks the
baseline sparkline, agrees it is seasonal, and acks:
`caesium dataset release warehouse/transactions_daily --reason "month-end" --tolerate rowCount.deltaFromBaseline=72h`.
The hold releases immediately; the named assertion is snoozed on that dataset
for 72h so follow-up runs don't re-trip. Tolerance windows are the v1 answer
to seasonality — seasonal baseline *models* are out of scope.

**3. Clean rerun auto-releases.** The vendor re-ships the file; the operator
retries the producing run. All assertions pass, so the evaluator releases
the hold (`release_reason: clean_run`, recording which run cleared it) and
downstream flows on the next trigger — no ack required. A clean run only
releases holds on datasets it re-produced, never unrelated ones.

## Backend design

### `##caesium::metrics` marker (`pkg/task/output.go`)

A fourth marker beside `output`, `output-ref`, and `branch`, parsed in the
same single-pass `parseMarkers` scan. Payload: flat JSON object; `dataset`
selects the declared dataset (omitted ⇒ the step's sole declared dataset,
error if ambiguous); values are JSON numbers or RFC3339 strings; multiple
lines merge last-write-wins per (dataset, metric). Metrics get their own cap
(`MaxMetricsBytes = 16 KiB`, separate from the 64 KiB `MaxOutputBytes` so a
chatty metrics emitter cannot evict real outputs, or vice versa). Malformed
lines are skipped leniently like malformed output lines — safe because a
declared assertion whose metric never arrives is itself a violation.

### Assertion evaluator (post-task pipeline)

Runs exactly where schema validation runs today — after marker capture, in
both executors (`internal/job/job.go` calls `run.ValidateTaskOutputSchema` at
~1072; `internal/worker/runtime_executor.go` at ~595). A sibling
`run.EvaluateDataAssertions(...)`: (1) persists emitted metrics as
`DatasetMetric` rows — including undeclared metrics, free baseline history
for assertions added later; (2) loads rolling baselines; (3) evaluates and
persists violations (a `DataViolation` shape parallel to `SchemaViolations`);
(4) dispatches `onViolation` — `warn` logs + persists, `fail` returns an
error the executors escalate exactly as schema `fail` mode, `hold` opens the
hold via idempotent upsert (one *active* hold per dataset; repeat violations
append occurrences rather than re-alerting). Replay-quarantined runs
(`TaskRun.Quarantine`) are excluded completely — no metrics, no baselines,
no holds opened or released; a what-if never trips or clears the breaker.

### Data model (`internal/models/`)

- `DatasetHold` — namespace/name; `Status` (`active|released`) with a
  partial-unique guard so at most one active hold per dataset, enforced the
  way concurrency admission is (one conditional INSERT, leader-safe under
  dqlite's Raft serialization); held-by job/run/task refs; violation JSON
  (assertion, observed, bound, baseline snapshot); occurrence count;
  released-at/by/reason + release-run ref; tolerance entries.
- `DatasetMetric` — task-run ref (CASCADE like `LineageDataset`), namespace,
  name, metric, float value (watermarks as epoch seconds), created-at;
  pruned past `CAESIUM_DATASET_METRIC_RETENTION` (default 90d).
- `Dataset` — the declared registry row (namespace, name, declaring job/step,
  spec JSON). **Shared table with freshness scheduling**; whichever design
  lands first creates it, the other extends it.

Baselines are computed on read — median + p10/p90 over the last N clean
(non-held, non-quarantined, succeeded) runs' `DatasetMetric` rows,
`N = CAESIUM_BASELINE_WINDOW` (default 20); no materialized baseline table,
since the window is ≤20 small rows per metric. **Cold start:** below
`CAESIUM_BASELINE_MIN_SAMPLES` (default 5) samples, `deltaFromBaseline`
assertions are **warn-only** (recorded, shown as "seeding", never holding);
absolute bounds enforce from run one. Enabling is safe by construction.

### Dataset identity

Today a "dataset" exists only as *observed* lineage rows — `LineageDataset`
keyed `(namespace, name, direction)` per task run
(`internal/models/lineage_dataset.go`), with names heuristically derived
from path-like output values or synthesized as `alias.step.output`
(`internal/lineage/mapper.go`). Heuristic names are too unstable to hang
holds on; the declared registry fixes identity. Where a declared name matches
observed lineage rows, the impact graph (`internal/lineage/impact.go`
`QueryImpact`) attaches blast-radius data to the hold; lint flags declared
datasets never observed in lineage.

### Downstream admission gate

The check belongs at **run admission**, not task start. Admission is already
the durable, leader-safe decision point: `Store.admit()`
(`internal/run/store.go:711`) resolves concurrency inside one transaction via
an atomic conditional insert, and every path into a run — cron, event/chained
triggers (`internal/trigger/event`), HTTP, manual, queue dequeue — funnels
through run creation in the store. One decision, recorded durably on the run;
no per-node state, no cross-node TOCTOU. A task-start gate is rejected for v1
— a hold landing mid-run would strand a half-executed DAG, and admission
already bounds exposure to one in-flight run.

Mechanics: before `admit()`, the store resolves the consuming job's declared
`consumes` list and queries active `DatasetHold` rows **in the same
transaction** that inserts the run. On a hit, the default disposition is
**skip-with-reason**: the run is created directly in terminal `skipped`
status with `SkipReason: dataset_hold:<ns>/<name>`, reusing the
concurrency-skip machinery so run history shows *why* nothing ran (an
invisible non-run would be silent-poison's evil twin), and emits
`run_held_upstream`. A per-job override, `metadata.onUpstreamHold: skip|run`
(default `skip`), lets a consumer opt out (e.g. a monitoring job that *wants*
held data). A third disposition — `park` in the durable `run_queue`, draining
on release — is deferred to Phase 3: the dequeuer is concurrency-driven today
and hold-release draining needs its own leader-gated wiring. Trigger rules
are unaffected: the gate acts at run granularity, before any task exists, so
a gate-skipped run looks to trigger chaining exactly like a
concurrency-skipped run does today. Mid-run task starts do not re-check
holds in v1 (documented limitation).

### Release semantics

**Human ack** — `POST /v1/datasets/holds/:id/release` with reason and
optional per-assertion tolerance windows; audited (`AuditLog`). **Clean run**
— when a producing task finishes with all assertions passing, the evaluator
releases any active hold on that dataset (`release_reason: clean_run`) in the
same transaction that records the metrics; configurable per dataset
(`release: auto|manual`, default `auto`). Release is level-triggered —
downstream jobs simply pass the gate on their next trigger; Caesium never
retroactively fires skipped runs in v1 (operators can retry them).

**Auth honesty.** Caesium defaults to `CAESIUM_AUTH_MODE=none`
(`pkg/env/env.go:169`), so by default release is an unauthenticated POST and
`ReleasedBy` records `anonymous`. Unlike agent-in-the-loop's approval gates
(where an unauthenticated approve route would let the agent approve itself),
a hold is a *safety* device against accidents, not a security boundary — the
feature is not hard-gated on auth. Deployments wanting an enforced ack chain
set an auth mode; `CAESIUM_HOLD_RELEASE_REQUIRE_AUTH=true` additionally 403s
release when no authenticated principal is present.

### Events, notifications, REST, env

New bus types (`internal/event/bus.go`): `dataset_held`, `dataset_released`,
`run_held_upstream`, flowing through the existing persisted-event store and
notification subscriber (`internal/notification/subscriber.go`) so policies
route them like any lifecycle event. Alert-once is structural: `dataset_held`
is the page-worthy event, emitted exactly once per hold (repeat violations
increment the occurrence counter without re-emitting); `run_held_upstream`
defaults to no-notify. Prometheus: `caesium_dataset_holds_total{reason}`,
`caesium_dataset_holds_active`, `caesium_data_assertions_total{result}`,
`caesium_runs_held_upstream_total`.

- REST: `GET /v1/datasets` (registry + freshness/hold status),
  `GET /v1/datasets/holds?status=active`, `GET /v1/datasets/:ns/:name/metrics`
  (baseline series), `POST /v1/datasets/holds/:id/release`.
- Env: `CAESIUM_DATA_ASSERTIONS_ENABLED` (default `false`, master gate — off
  means no evaluator, no gate, no routes; reported by `GET /system/features`),
  `CAESIUM_BASELINE_WINDOW=20`, `CAESIUM_BASELINE_MIN_SAMPLES=5`,
  `CAESIUM_DATASET_METRIC_RETENTION=2160h`, `CAESIUM_HOLD_RELEASE_REQUIRE_AUTH=false`.

## CLI

```
caesium dataset list                                  # registry + status
caesium dataset holds [--status active] [--json]
caesium dataset release <ns>/<name> [--reason ...] [--tolerate <assertion>=<dur>]
caesium dataset metrics <ns>/<name> --metric rowCount [--json]
```

`--json` goes to stdout, clean and parseable — asserted by integration tests
via `runCLIStdout`, never the stream-merging capture.

## Frontend (Caesium Console)

1. **Hold badges on the lineage graph.** `LineageGraph.tsx` marks held
   dataset nodes (distinct from run-status colors and replay-quarantine
   styling) and shades the downstream cone — the blast radius at a glance.
2. **Ack/release flow.** A hold panel (violated assertion, observed vs
   bound, producing-run link, occurrence count) with Release: reason
   required, optional tolerance picker. Active-holds count joins the nav
   badges (`useNavCounts.ts`); gate-skipped runs render the held dataset as
   their skip reason, deep-linking to the hold.
3. **Assertions on the run surface.** `RunDetailPage`/`TaskDetailPanel` show
   per-assertion results beside today's schema-violation display; each
   `deltaFromBaseline` row gets a **baseline sparkline** (last-N values,
   band, current point) so "10× normal" is visible, not inferred.

## Interplay

- **[`design-freshness-scheduling.md`](design-freshness-scheduling.md)** —
  same `Dataset` registry; a held dataset is **not fresh** regardless of
  watermark, so a poisoned-but-recent partition never satisfies a freshness
  trigger or advances downstream scheduling.
- **[`design-contract-enforcement.md`](design-contract-enforcement.md)** —
  static/PR-time enforcement there; this is the runtime breaker for what
  static analysis cannot see, the *values*. One contract story, two gates.
- **[`design-agent-in-the-loop.md`](design-agent-in-the-loop.md)** —
  `dataset_held` becomes an incident class (`data_quality_hold`) whose triage
  bundle carries the violation, baseline, and impact graph; `release_hold`
  joins the action catalog at tier 2 (tier 3 with tolerance windows);
  `suppress_downstream_alerts` becomes largely unnecessary for this class.
- **[`design-backtesting.md`](design-backtesting.md)** — assertions evaluate
  in backtests too, over metrics historical runs emitted: free regression
  signal ("this change would have tripped the breaker on 3 of the last 30
  days"), with holds never opened from backtest runs.

## Testing

Per the repo's end-to-end gate, every CLI command and endpoint above ships
with an integration test in `test/` driving the real surface. A
metrics-emitting script image covers: passing run records metrics/baselines;
violating `hold` run opens exactly one hold + one `dataset_held` event; the
downstream trigger produces a skipped run with the hold reason;
`caesium dataset release` (stdout-clean `--json` via `runCLIStdout`) reopens
the gate; clean rerun auto-releases; cold-start runs warn-only; `fail` mode
goes red like schema `fail`; disabled gate is inert.
`CAESIUM_DATA_ASSERTIONS_ENABLED=true` is set in `just integration-up` so the
path executes in CI. Distributed parity (worker path via
`runtime_executor.go`; leader-safe gate under concurrent admission) and
replay isolation (a quarantined replay never trips or releases holds) get
their own scenarios.

## Phasing

- **Phase 0 — Observe.** Marker parsing, `DatasetMetric` persistence,
  registry table, baseline read API, metrics CLI/UI sparkline. No
  enforcement; seeds baselines while the rest is reviewed.
- **Phase 1 — Assert.** Evaluator with `warn|fail`, violations persisted,
  lint. Feature-complete for teams that only want red runs.
- **Phase 2 — Break the circuit.** `DatasetHold`, admission gate, release
  (ack + clean-run), events/notifications, holds CLI/UI. The headline drop.
- **Phase 3 — Ergonomics & reach.** `park` disposition with release-drain,
  agent actions + incident class, freshness integration, backtest evaluation.

## Non-goals (v1)

- **No data-plane access.** Caesium never connects to warehouses, reads
  files, or samples rows. Metrics are step-emitted, full stop.
- **No anomaly ML, no seasonal baseline models.** Rolling percentile windows
  plus manual tolerance windows; month-end is an ack, not a Fourier term.
- **No row-level holds.** Setting rows aside is the pipeline's job (cf. the
  `badRowPolicy` pattern in agent-in-the-loop scenario 3); Caesium holds
  the dataset pointer, never data.
- **No retroactive un-skip** of gate-skipped runs on release, and **not a
  data catalog** — the registry stores identity + contract, not
  ownership/glossary/discovery metadata.

## Open questions

1. **Identity unification.** Should declared registry names *replace* the
   heuristic lineage names for declaring steps (mapper prefers `produces`
   over path-derivation), so holds and impact queries share one spine?
   Leaning yes — it also fixes lineage-name instability.
2. **Multi-producer datasets.** Does a clean run of producer B release a
   hold opened by producer A? Proposal: no — only the holder's clean run.
3. **Partition-scoped holds.** Holding `transactions_daily/2026-07-03`
   rather than the whole dataset needs partition identity in the registry —
   likely arriving with freshness scheduling's watermark work; the hold
   schema should reserve an optional partition key now.
4. **Tenancy.** When multi-tenancy (roadmap §3.1) lands, holds and the
   registry scope per namespace; models carry a nullable tenant column from
   day one, as agent-in-the-loop already commits to.
