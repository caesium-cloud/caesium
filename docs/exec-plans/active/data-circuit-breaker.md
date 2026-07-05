# Data Circuit Breaker — Dataset Holds & Statistical Assertions

Last updated: 2026-07-03

Caesium's data contracts today are **structural**: `outputSchema` is validated
post-task (`internal/run/schema_validation.go` `ValidateTaskOutputSchema`, called
from both executors — `internal/job/job.go:1072` and
`internal/worker/runtime_executor.go:595`), violations persist on the `TaskRun`
(`SchemaViolations`), and `metadata.schemaValidation: warn|fail` decides whether
the task goes red. That catches *shape* problems, never the failures that poison
consumers: a truncated feed with a perfectly valid schema, a join key that goes
40% null, a watermark stalled on yesterday's data. Bad *data* propagates one hop
further with every downstream trigger, and failing every consumer buries the root
cause under N pages.

This plan ships [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md):
a circuit breaker on the **dataset**, not the run. A new `##caesium::metrics`
stdout marker (beside `##caesium::output` in `pkg/task/output.go`) lets a step
self-report what it observed; a post-task **assertion evaluator** (a sibling to
schema validation) records metrics, computes rolling baselines server-side in Go,
and on a `hold`-mode violation opens a `DatasetHold` and fires **one** alert at
the source; a **run-admission gate** in the run store (`internal/run/store.go`
`admit()` at :711, in the same transaction that inserts the run) skips downstream
jobs that `consume` a held dataset with reason `dataset_hold:<ns>/<name>` instead
of feeding them poison; and holds release on human ack (fail-closed under
`CAESIUM_AUTH_MODE=none`) or the next clean producer run. The whole feature is
gated by `CAESIUM_DATA_ASSERTIONS_ENABLED` (default `false`) — off means no
evaluator, no gate, no routes.

Per the `CLAUDE.md` end-to-end gate, every new CLI verb (`caesium dataset
list/holds/release/metrics`) and REST endpoint (`GET /v1/datasets*`, `POST
/v1/datasets/holds/:id/release`) ships with a `test/` integration scenario that
drives the real surface against a live server with
`CAESIUM_DATA_ASSERTIONS_ENABLED=true`, capturing `--json` stdout separately via
`runCLIStdout`. A green unit test that hand-feeds the evaluator proves the
evaluator, never the wiring.

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

When this plan and [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md)
disagree, **the design doc wins on INTENT and SCOPE** — what the marker, the
evaluator, the hold model, the admission gate, and the release semantics must do.
No item may add a NEW marker, model, event type, config knob, endpoint, or CLI
verb beyond what the design enumerates without first amending the design doc and
this Source-Of-Truth Note. Strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) — the Phase-4 Data-Plane Differentiators
table (§ "Data circuit breaker") — and the roadmap wins on priority/status
disagreements. The job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go); this plan adds
`produces` (with `assertions`), `consumes`, and `onViolation`/`release`/
`onUpstreamHold` metadata — a schema change (new structs + `Validate()`), gated
so the fields are inert when `CAESIUM_DATA_ASSERTIONS_ENABLED=false`.

**Shared `DatasetDeclaration` registry — coordinate with `freshness-scheduling.md`.**
The design is explicit that the declared-dataset registry (`produces:` keyed
`(namespace, name)`) is the **same YAML and model substrate** as
[`design-freshness-scheduling.md`](../../design-freshness-scheduling.md); "whichever
design lands first creates it, the other extends it." To keep the two plans from
colliding on the base table, the canonical names are freshness's: the model is
**`DatasetDeclaration`** in `internal/models/dataset_declaration.go` (table
`dataset_declarations`), and the jobdef `produces`/`consumes` entries key the
dataset identity on **`name`** (not `dataset`) — matching
[`freshness-scheduling.md`](../completed/freshness-scheduling.md) Stream A and the
[`contract-enforcement.md`](contract-enforcement.md) plan, both of which use `name`.
The [`freshness-scheduling`](../completed/freshness-scheduling.md) plan's Stream A owns the
`dataset_declarations` registry model + the `Metadata.Datasets` jobdef block; this
plan's Stream A owns the assertion/`consumes` half of the same registry. **Neither
ships a private copy of the base registry table, and neither introduces a second
`Dataset` model.** Whichever plan's registry item merges first creates the
`DatasetDeclaration` model + jobdef `produces` scaffold; the second plan's item
extends it (adds columns / fields) rather than redefining it. If a
Stream-A item here finds the base registry already merged by freshness, it
**extends** — see the Sequencing section's cross-plan note. The
[`contract-enforcement`](contract-enforcement.md) plan reads the same `consumes`
edges for its compatibility graph but does not define registry structure.

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from
[`design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md) alongside
its Phase-4 companions (`contract-enforcement`, `freshness-scheduling`,
`agent-in-the-loop-remediation`); the first wave is the next eligible run of the
`exec-plan-wave` skill against this doc. The design's four phases (Phase 0
Observe → Phase 1 Assert → Phase 2 Break the circuit → Phase 3 Ergonomics) map
onto the streams below; Phase 3 (park disposition, agent-action integration,
freshness/backtest reach) is recorded as deferred.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Observability substrate — `##caesium::metrics` marker, `DatasetDeclaration`/`DatasetMetric` models, jobdef `produces`/`consumes`/`assertions` schema + lint, metrics persistence in both executors, baseline read, master env gate (Phase 0) | **P0** | Not started |
| B | Assertion evaluator — `run.EvaluateDataAssertions` with rolling baselines, cold-start warn-only, `warn`/`fail` dispatch, `DataViolation` persistence (Phase 1) | **P0** | Not started |
| C | Circuit breaker — `DatasetHold` model + partial-unique guard, hold-open path, downstream admission gate, release (clean-run + fail-closed ack), bus events + alert-once (Phase 2) | **P0** | Not started |
| D | Operator surface — `GET /v1/datasets*` reads + `caesium dataset list/holds/release/metrics` CLI | P1 | Not started |
| E | Console UI — hold badges on the lineage graph, ack/release panel + baseline sparkline, nav active-holds count | P1 | Not started |
| H-1 | Integration harness — `CAESIUM_DATA_ASSERTIONS_ENABLED=true` on the live server + a metrics-emitting script image | — | Not started |
| N-1 | Docs — roadmap Phase-4 flip, design banner, `metrics`/`produces`/`assertions`/`consumes` schema references + examples, README index | — | Not started |
| (Phase 3) | `park` disposition + release-drain, agent-action incident class, freshness/backtest integration | — | **Deferred** — recorded follow-on |

## Streams

### Stream A — Observability substrate: marker, models, schema, metrics persistence (Phase 0)

The container-native data plane every other stream builds on: the fourth stdout
marker, the two Phase-0 tables plus the declared registry, the jobdef schema for
`produces`/`consumes`/`assertions`, and metrics persistence wired into both
executors' post-task pipelines. Largest blast radius (`pkg/task/output.go`,
`internal/models/`, `pkg/jobdef/definition.go`, both executors), so it merges
first. Phase 0 is enforcement-free: it seeds baselines while the evaluator (B) and
breaker (C) are reviewed.

- [ ] A1. Add the `##caesium::metrics` marker to `pkg/task/output.go`: a fourth
      marker beside `output`, `output-ref`, and `branch`, parsed in the same
      single-pass `parseMarkers` scan (marker stem check ordered so `metrics`
      doesn't collide with `output`). Payload is a flat JSON object; `dataset`
      selects the declared dataset (omitted ⇒ the step's sole declared dataset,
      error if ambiguous); values are JSON numbers or RFC3339 strings; multiple
      lines merge last-write-wins per `(dataset, metric)`. Metrics get their own
      `MaxMetricsBytes = 16 KiB` cap (separate from the 64 KiB `MaxOutputBytes` so
      a chatty emitter can't evict real outputs); malformed lines are skipped
      leniently like malformed output lines.
      Files: `pkg/task/output.go` (+ `output_test.go`).
- [ ] A2. Add the `DatasetDeclaration` declared-registry model (canonical name
      shared with `freshness-scheduling` Stream A), the `DatasetMetric` model,
      and register both in the `models.All` slice (`internal/models/models.go`).
      `DatasetMetric` — task-run ref (`CASCADE` like `LineageDataset`), namespace,
      name, metric, float value (watermarks stored as epoch seconds), created-at;
      pruned past `CAESIUM_DATASET_METRIC_RETENTION` (default `2160h`/90d) by an
      env-gated background pruner started in `cmd/start/start.go` (mirror an
      existing pruner). The base registry model is **`DatasetDeclaration`** (table
      `dataset_declarations`) — namespace, name, declaring job/step, spec JSON;
      nullable tenant column from day one (design open-question 4). **Coordinate
      with `freshness-scheduling` Stream A on the base registry table** (see the
      Source-Of-Truth Note): if freshness's `dataset_declarations` registry has
      already merged, this item **extends** `internal/models/dataset_declaration.go`
      with the assertion/spec columns rather than creating a second `Dataset`
      registry; if this item merges first, it creates `DatasetDeclaration` under the
      canonical name. `DatasetMetric` (baseline samples) is this plan's own table.
      These are catalog/observability tables, **not** hot per-run tables — do NOT
      add them to `hotPathModels()` / `hotTables`.
      Files: `internal/models/dataset_declaration.go` (create-or-extend, canonical
      name shared with freshness Stream A), new
      `internal/models/dataset_metric.go`, `internal/models/models.go`,
      `pkg/env/env.go`, `cmd/start/start.go`.
      Depends on: A1 (the marker the metrics come from).
- [ ] A3. Add the jobdef schema for `produces`/`consumes`: a step-level
      `produces []ProducedDataset{name, assertions{rowCount, nullRate,
      freshness, custom[]}, onViolation ∈ warn|fail|hold, release ∈ auto|manual}`
      and `consumes [name...]`, plus `metadata.onUpstreamHold ∈
      skip|run` (default `skip`). The base `produces`/`consumes` block keyed on
      `name` (plus `freshness`/`watermark`) is owned by `freshness-scheduling` Stream
      A; this item adds the `assertions`/`onViolation`/`release` fields to
      `ProducedDataset` — it does not rename the identity key or fork the struct. Wire the structs + `Validate()` + the dual
      `Step`/`rawStep` declaration + `UnmarshalYAML` in `pkg/jobdef/definition.go`,
      `pkg/jobdef/schema.go`, and `internal/jobdef/runtime/spec.go`. `caesium job
      lint` validates dataset names, `consumes` resolvability against declared
      producers, assertion syntax, and flags declared datasets never observed in
      lineage (`internal/lineage/`). **Cache-hash note:** `produces`/`consumes`/
      `assertions` are post-task evaluation and admission concerns — they do NOT
      change what the container executes, so they are **not** added to
      `internal/cache/hash.go` `HashInput` (mirroring triggers). Document the one
      edge in the item: a cache-short-circuited task emits no fresh metrics, so the
      evaluator treats a cached task as "no new sample" (no assertion, no baseline
      write) — not a violation.
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`,
      `internal/jobdef/runtime/spec.go`, `cmd/job/` (lint path),
      `internal/lineage/` (declared-vs-observed check).
      Depends on: A2 (registry identity the schema references).
- [ ] A4. Persist emitted metrics in the post-task pipeline: add a
      `run.EvaluateDataAssertions(...)` call site beside
      `run.ValidateTaskOutputSchema` in **both** executors (`internal/job/job.go`
      ~1072, `internal/worker/runtime_executor.go` ~595), gated on
      `CAESIUM_DATA_ASSERTIONS_ENABLED`. In Phase 0 the function's only job is to
      persist emitted metrics as `DatasetMetric` rows (including undeclared metrics
      — free baseline history for assertions added later); Stream B fills in the
      evaluation logic behind the same seam (new file in `internal/run/`, so B does
      NOT re-edit the executors). Replay-quarantined runs (`TaskRun.Quarantine`)
      are excluded completely — no metrics recorded.
      Files: new `internal/run/data_assertions.go`, `internal/job/job.go`,
      `internal/worker/runtime_executor.go`.
      Depends on: A1 + A2 + A3.
- [ ] A5. Add the master env gate `CAESIUM_DATA_ASSERTIONS_ENABLED` (default
      `false`) to the `Environment` struct in `pkg/env/env.go`, and surface it as a
      field on the `Features` struct in `api/rest/service/system/system.go` (so
      `GET /system/features` reports it and the UI can hide gated surfaces). Add the
      baseline read API — median + p10/p90 computed on read over the last
      `CAESIUM_BASELINE_WINDOW` (default `20`) clean (non-held, non-quarantined,
      succeeded) `DatasetMetric` rows per metric; no materialized baseline table.
      Files: `pkg/env/env.go`, `api/rest/service/system/system.go`, new
      `internal/run/baseline.go` (compute-on-read helper).
      Depends on: A2.

### Stream B — Assertion evaluator: warn / fail (Phase 1)

The evaluator proper, feature-complete for teams that only want red runs. Fills in
the `run.EvaluateDataAssertions` seam A4 created (in `internal/run/`, not the
executors), so it never re-touches the executor call sites.

- [ ] B1. Implement `run.EvaluateDataAssertions`: load rolling baselines (via A5's
      compute-on-read helper), evaluate each declared assertion — `min`/`max`
      absolute bounds enforce from run one; `deltaFromBaseline` compares against the
      median; a **missing declared metric is itself a violation** (a step that stops
      emitting `rowCount` must not silently pass). **Cold start:** below
      `CAESIUM_BASELINE_MIN_SAMPLES` (default `5`) samples, `deltaFromBaseline`
      assertions are **warn-only** (recorded, shown as "seeding", never holding);
      absolute bounds still enforce. Persist violations as a `DataViolation` shape
      parallel to `SchemaViolations` (a `SaveDataViolations` mirroring
      `SaveSchemaViolations` at `internal/run/store.go:2139`). Dispatch: `warn` logs
      + persists (like `schemaValidation: warn`); `fail` returns an error the
      executors escalate exactly as schema `fail` mode (red run). `hold` is a no-op
      here — Stream C wires it. Add `CAESIUM_BASELINE_WINDOW`,
      `CAESIUM_BASELINE_MIN_SAMPLES` to `pkg/env/env.go` and
      `caesium_data_assertions_total{result}` to `internal/metrics/metrics.go`
      (both edit sites: the `var (...)` block + `Register()`).
      Files: `internal/run/data_assertions.go`, `internal/run/store.go`
      (`SaveDataViolations` + `DataViolations` column), `pkg/env/env.go`,
      `internal/metrics/metrics.go`.
      Depends on: A4 + A5.

### Stream C — Circuit breaker: hold, admission gate, release, events (Phase 2 headline)

The headline drop: the dataset actually breaks the circuit. Builds on B's
evaluator (adds the `hold` disposition) and gates run admission in the store.

- [ ] C1. Add the `DatasetHold` model + register in `models.All`: namespace/name;
      `Status` (`active|released`) with a **partial-unique guard so at most one
      active hold per dataset**, enforced the way concurrency admission is (one
      conditional INSERT, leader-safe under dqlite's Raft serialization); held-by
      job/run/task refs; violation JSON (assertion, observed, bound, baseline
      snapshot); occurrence count; released-at/by/reason + release-run ref;
      tolerance entries; nullable tenant + optional partition key reserved (design
      open-questions 3–4). Wire the `hold` disposition into
      `run.EvaluateDataAssertions`: on a `hold` violation the task **succeeds** (the
      work is done) but the evaluator opens the hold via **idempotent upsert** — one
      active hold per dataset; repeat violations append occurrences rather than
      re-alerting. Where a declared name matches observed lineage rows, attach
      `internal/lineage/impact.go` `QueryImpact` blast-radius data to the hold. Add
      `caesium_dataset_holds_total{reason}` + `caesium_dataset_holds_active`.
      Files: new `internal/models/dataset_hold.go`, `internal/models/models.go`,
      `internal/run/data_assertions.go`, `internal/metrics/metrics.go`.
      Depends on: B1.
- [ ] C2. Add the downstream **run-admission gate** in the run store: before
      `admit()` (`internal/run/store.go:711`), resolve the consuming job's declared
      `consumes` list and query active `DatasetHold` rows **in the same transaction**
      that inserts the run. On a hit the default disposition is **skip-with-reason** —
      the run is created directly in terminal `skipped` status with `SkipReason:
      dataset_hold:<ns>/<name>`, reusing the concurrency-skip machinery
      (`admissionSkipped` / `skipReason`, :741) so run history shows *why* nothing
      ran. Honor the per-job `metadata.onUpstreamHold: skip|run` override (default
      `skip`). Emit `run_held_upstream` (non-paging). Every path into a run (cron,
      event/chained triggers, HTTP, manual, queue dequeue) funnels through this
      store, so one decision covers all. Mid-run task starts do NOT re-check holds
      in v1 (documented limitation). Add `caesium_runs_held_upstream_total`.
      Files: `internal/run/store.go`, `internal/metrics/metrics.go`.
      Depends on: C1 (the `DatasetHold` rows it reads) + A3 (the `consumes` list).
- [ ] C3. Add release semantics. **Clean run** — when a producing task finishes
      with all assertions passing, the evaluator releases any active hold on that
      dataset (`release_reason: clean_run`, recording the clearing run) in the same
      transaction that records the metrics; a clean run only releases holds on
      datasets it re-produced. **Human ack** — `POST /v1/datasets/holds/:id/release`
      with reason + optional per-assertion tolerance windows
      (`--tolerate <assertion>=<dur>`), audited via `AuditLog`
      (`internal/models/audit_log.go`). **Fail-closed:** Caesium defaults to
      `CAESIUM_AUTH_MODE=none` (`pkg/env/env.go:169`) which attaches no auth
      middleware, so with no auth mode active the release endpoint is **disabled
      (403 naming the precondition)** and holds release only through the `clean_run`
      path; `ReleasedBy` then always records an authenticated principal. Emit
      `dataset_released`.
      The `/v1/datasets/holds/:id/release` route extends the base dataset REST
      package owned by [`freshness-scheduling.md`](../completed/freshness-scheduling.md) Stream E
      — see Stream D's cross-plan ownership note; this item adds a handler file and
      appends one route line, it does not create the package.
      Files: `internal/run/data_assertions.go` (clean-run release),
      new `api/rest/controller/dataset/release.go`,
      `api/rest/service/dataset/`, `api/rest/bind/bind.go`.
      Depends on: C1.
- [ ] C4. Add the bus event types `dataset_held`, `dataset_released`,
      `run_held_upstream` to `internal/event/bus.go`, flowing through the existing
      persisted-event store and the notification subscriber
      (`internal/notification/subscriber.go`) so policies route them like any
      lifecycle event. **Alert-once is structural:** `dataset_held` is the
      page-worthy event, emitted exactly once per hold (repeat violations increment
      the occurrence counter in C1 without re-emitting); `run_held_upstream`
      defaults to no-notify. Wire the emit calls at the hold-open (C1),
      release (C3), and gate-skip (C2) sites.
      Files: `internal/event/bus.go`, `internal/notification/subscriber.go`.
      Depends on: C1 + C2 + C3 (the three emit sites).

#### Deferred — Phase 3 ergonomics & reach

Recorded as a follow-on, **not** part of this plan's acceptance criteria: the
`park` run disposition with release-drain (the dequeuer is concurrency-driven
today and needs its own leader-gated draining wiring); the agent-in-the-loop
`data_quality_hold` incident class + `release_hold` action
([`design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md)); freshness
integration (a held dataset is not fresh —
[`design-freshness-scheduling.md`](../../design-freshness-scheduling.md)); and
backtest evaluation of assertions over historical metrics
([`design-backtesting.md`](../../design-backtesting.md)). Draft these against the
Stream C/D endpoints once this plan completes.

### Stream D — Operator surface: dataset REST reads + CLI

The read side of the registry/hold surface plus the operator CLI. The release
`POST` is owned by C3 (it's release *semantics*); D owns the reads and the CLI
that drives all four endpoints.

**Cross-plan ownership (base `cmd/dataset/` + `/v1/datasets` package):** the base
`caesium dataset` Cobra group, the `api/rest/controller/dataset/` +
`api/rest/service/dataset/` package, and the base `/v1/datasets` route family are
owned by [`freshness-scheduling.md`](../completed/freshness-scheduling.md) **Stream E**. The
items below (and C3's release route) **extend** that package — adding
hold/metrics/release handlers and the `/v1/datasets/holds*` +
`/v1/datasets/:ns/:name/metrics` routes and the `holds`/`release`/`metrics`
subcommands — rather than creating a second package or Cobra group. If
freshness Stream E has not merged yet, whichever dataset-surface item lands first
creates the package skeleton + the `cmds`-slice / `Protected()` registration under
these canonical paths; the other extends it. The two plans' `bind.go` /
`cmd/execute.go` dataset edits must not land in the same wave (see Sequencing).

- [ ] D1. Add the dataset read endpoints: `GET /v1/datasets` (registry + hold
      status), `GET /v1/datasets/holds?status=active`, and
      `GET /v1/datasets/:ns/:name/metrics` (baseline series). Route lines appended
      in `Protected()` (`api/rest/bind/bind.go`). Extends the base dataset package
      owned by freshness Stream E (see the cross-plan note above) — `GET
      /v1/datasets` is created once by whichever plan lands first, then reused.
      Files: `api/rest/controller/dataset/`, `api/rest/service/dataset/`,
      `api/rest/bind/bind.go`.
      Depends on: A2 (models) + A5 (baseline read) + C1 (`DatasetHold`).
- [ ] D2. Add the circuit-breaker subcommands to the `caesium dataset` CLI group —
      `holds [--status active] [--json]`,
      `release <ns>/<name> [--reason …] [--tolerate <assertion>=<dur>]`,
      `metrics <ns>/<name> --metric rowCount [--json]` — extending the base
      `cmd/dataset/` group owned by freshness Stream E (which contributes
      `list`/`status`/`advance`). If that group does not exist yet, this item
      creates it + appends it to the `cmds` slice in `cmd/execute.go` under the
      canonical path; otherwise it adds subcommand files to the existing group.
      `--json` goes to **stdout, clean and parseable** via `cmd.OutOrStdout()`
      (asserted in tests via `runCLIStdout`, never the stream-merging capture); a
      timed-out HTTP client + bearer API-key header like the shipped
      `cmd/event`/`cmd/trigger` groups.
      Files: `cmd/dataset/` (extend or create), `cmd/execute.go`.
      Depends on: D1 + C3 (the release endpoint `release` drives).

### Stream E — Console UI: hold badges, ack/release, baseline sparkline

The web surface. UI-gated by the `Features` flag A5 exposes. All UI changes run
the `ui/**` conditional gate.

- [ ] E1. Mark held dataset nodes on the lineage graph
      (`ui/src/features/jobs/LineageGraph.tsx`) — distinct from run-status colors
      and replay-quarantine styling — and shade the downstream cone (the blast
      radius at a glance). Join the active-holds count to the nav badges
      (`ui/src/features/jobs/useNavCounts.ts`); gate-skipped runs render the held
      dataset as their skip reason, deep-linking to the hold. Add the API methods to
      `ui/src/lib/api.ts` and any route to `ui/src/router.tsx`.
      Files: `ui/src/features/jobs/LineageGraph.tsx`,
      `ui/src/features/jobs/useNavCounts.ts`, `ui/src/lib/api.ts`,
      `ui/src/router.tsx`.
      Depends on: D1 (the reads it renders).
- [ ] E2. Add the hold ack/release flow and assertion display: a hold panel
      (violated assertion, observed vs bound, producing-run link, occurrence count)
      with a Release action (reason required, optional tolerance picker); and
      per-assertion results on `RunDetailPage`/`TaskDetailPanel` beside today's
      schema-violation display, each `deltaFromBaseline` row getting a **baseline
      sparkline** (last-N values, band, current point) so "10× normal" is visible,
      not inferred.
      Files: new `ui/src/features/datasets/` (hold panel + sparkline),
      `ui/src/features/jobs/RunDetailPage.tsx`,
      `ui/src/features/jobs/TaskDetailPanel.tsx`, `ui/src/lib/api.ts`.
      Depends on: D1 + C3 (release) + E1 (shared api.ts additions).

## Harness Strengthening

- [ ] H-1. Ensure the integration server exercises the real assertion path: set
      `CAESIUM_DATA_ASSERTIONS_ENABLED=true` on the `just integration-up` /
      `just integration-test` server (mirror the lineage
      `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the `CLAUDE.md` gate calls out), pass
      the same env through CI, and add a small **metrics-emitting script image**
      (echoes `##caesium::metrics`) the Stream A/B/C scenarios drive so they hit the
      live surface rather than an internal call. Add a low
      `CAESIUM_BASELINE_MIN_SAMPLES` / short `CAESIUM_DATASET_METRIC_RETENTION` if a
      scenario needs a tight window.
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers,
      `build/` (or `test/`) metrics-emitting fixture image.

## Navigational / Organizational Improvements

- [ ] N-1. Flip the `docs/roadmap.md` Phase-4 Data-Plane Differentiators entry for
      "Data circuit breaker" to Shipped (and update the Execution-Priority /
      Completed-Features rows as appropriate); update the
      [`design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md)
      `> Status:` banner (Brainstorm/Design → shipped, per-phase). Document the
      `##caesium::metrics` marker and the `produces`/`assertions`/`consumes`/
      `onViolation`/`onUpstreamHold` fields in `docs/job-schema-reference.md`,
      `docs/job-definitions.md`, and `docs/caesium-job-llm-reference.md`; add a
      circuit-breaker example under `docs/examples/` (pinned image). Index this plan
      in `docs/README.md` — **backtick/inline-code form**, not a clickable
      subdirectory link (the `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail
      rejects those). Runs last, after the runtime ships.
      Files: `docs/roadmap.md`, `docs/design-data-circuit-breaker.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–E (runs last).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — the marker, models, jobdef schema, metrics
  persistence, and master gate. B (evaluator), C (breaker), D (surface), and E
  (UI) all consume it. A merges first (largest blast radius).
- **Stream B** depends on A4 (the evaluator seam) + A5 (baseline read); it fills in
  the `internal/run/data_assertions.go` logic without re-touching the executors.
- **Stream C** depends on B1 (adds the `hold` disposition to the evaluator);
  C2 additionally depends on A3 (`consumes`); C4 depends on C1+C2+C3 (its three
  emit sites), so C sequences C1 → (C2, C3) → C4.
- **Stream D** depends on A2/A5/C1 (reads) and C3 (the release endpoint the CLI
  drives) — runs after C.
- **Stream E** depends on D1 (the reads it renders) + C3; runs after D.
- **H-1** is independent (justfile/CI/harness) and supports the A/B/C integration
  scenarios; land it in the first wave so the substrate's end-to-end gate has a
  live, enabled surface to drive.
- **N-1** runs last, after A–E ship, so the roadmap/schema/design docs reflect
  reality.

**Cross-PLAN order (shared `DatasetDeclaration` registry):** the base declared-registry table
+ jobdef `produces` scaffold is shared with
[`freshness-scheduling`](../completed/freshness-scheduling.md) Stream A. Whichever plan's Stream-A
registry item merges first **creates** the registry model; the second **extends**
it (adds columns / jobdef sub-fields) rather than redefining the table (see the
Source-Of-Truth Note). Coordinate at wave-dispatch so the two Stream-A registry
items are not in flight in the same wave; the later one rebases onto the merged base.

**Cross-PLAN order (shared `cmd/dataset/` group + `/v1/datasets` package):** this
plan's C3/D1/D2 add handlers/subcommands/routes to the same dataset REST package +
Cobra group that [`freshness-scheduling`](../completed/freshness-scheduling.md) **Stream E**
owns (`api/rest/controller/dataset/`, `api/rest/service/dataset/`, `cmd/dataset/`,
the `/v1/datasets` route family, the `cmds`-slice registration). Whichever plan's
dataset-surface item merges first **creates** the package skeleton + registration
under the canonical paths; the other **extends** it (adds files, appends route
lines). Never land this plan's `bind.go` / `cmd/execute.go` dataset edits in the
same wave as a freshness Stream-E `bind.go` / `cmd/execute.go` edit — sequence them
(the later item rebases onto the merged base).

**Suggested waves:**
- **W1 = A (A1 → A2 → A3 → A4 → A5) + H-1.** A is one near-strict chain (marker,
  then models, then schema, then persistence seam, then gate/baseline).
- **W2 = B.** Unblocked once A's seam + baseline read are in.
- **W3 = C (C1 → (C2, C3) → C4).** The breaker on top of B's evaluator.
- **W4 = D (D1 → D2) + E (E1 → E2) + N-1.**

**Within-stream order:** A1 → A2 → A3 → A4, with A5 parallel to A3/A4 after A2.
C1 → C2 and C1 → C3 (parallel), then C4. D1 → D2. E1 → E2.

**Cross-stream file conflicts:**

- `internal/run/data_assertions.go` — A4 *creates* it (persist-only), B1 fills in
  evaluation, C1 adds the `hold` disposition, C3 adds clean-run release. All
  **sequential across waves** (A → B → C), never same-wave; no parallel edit.
- `internal/run/store.go` — B1 (`SaveDataViolations` + `DataViolations` column) and
  C2 (admission gate) both edit it; B (W2) before C (W3), so no same-wave collision.
- `internal/models/models.go` — A2 (`DatasetDeclaration`, `DatasetMetric`) and C1
  (`DatasetHold`) append to the order-sensitive `All` slice; A2 (W1) before C1 (W3).
- `pkg/jobdef/definition.go` — **A3 only in this plan** (the dual `Step`/`rawStep`
  declaration + `Validate()`). **Cross-plan true-conflict** with
  `freshness-scheduling` Stream A (which also edits `definition.go` for `datasets`);
  sequence the two plans' schema items, do not run them in the same wave.
- `pkg/env/env.go` — A2 (`CAESIUM_DATASET_METRIC_RETENTION`), A5
  (`CAESIUM_DATA_ASSERTIONS_ENABLED`), B1 (`CAESIUM_BASELINE_WINDOW`,
  `CAESIUM_BASELINE_MIN_SAMPLES`) all add fields; additive across waves, shared
  `validate()` — flag for a clean rebase, not a hand-merge.
- `internal/metrics/metrics.go` — B1 (`data_assertions_total`), C1 (`holds_total`
  + `holds_active`), C2 (`runs_held_upstream_total`) each add a collector (two edit
  sites: the `var (...)` block + `Register()`). B is W2, C is W3 — no same-wave
  overlap; C1 + C2 are the one same-wave additive overlap in W3, call it out.
- `api/rest/bind/bind.go` — C3 (release route) and D1 (read routes) append to
  `Protected()`; C3 (W3) before D1 (W4). Additive.
- `cmd/execute.go` — D2 appends one command group to `cmds`. Additive.
- `internal/event/bus.go` + `internal/notification/subscriber.go` — C4 only.
- `ui/src/lib/api.ts`, `ui/src/router.tsx` — E1 + E2 append; import blocks conflict
  if truly concurrent, so sequence E1 → E2 (already a dependency).
- `cmd/start/start.go` — A2 (metric-retention pruner) only.
- **No `internal/cache/hash.go` change:** `produces`/`consumes`/`assertions` are
  post-task / admission concerns and do NOT affect the step execution hash (A3),
  so the cache key is untouched.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (C3, D):** an integration scenario in `test/`
  that drives the **real surface** against the live server (with
  `CAESIUM_DATA_ASSERTIONS_ENABLED=true`) — a metrics-emitting run that opens a
  hold, a downstream trigger that produces a `skipped` run with reason
  `dataset_hold:<ns>/<name>`, `caesium dataset release` reopening the gate, a clean
  rerun auto-releasing — asserting observed output. A unit test that hand-feeds the
  evaluator proves the evaluator, not the wiring; both are required.
- **Machine-readable CLI (`--json` on `caesium dataset`):** assert stdout is clean
  and parseable, captured **separately** from stderr via `runCLIStdout`.
- **New metric (B1, C1, C2):** assert via `internal/metrics/testutil` in a
  `*_test.go`; the collector must also appear in `Register()`.
- **Job-schema change (A3):** `caesium job lint --path docs/examples/` green on the
  new `produces`/`assertions`/`consumes` example; a bad assertion / unresolvable
  `consumes` rejected at lint.
- **UI changes (E):** `just ui-lint && just ui-test && just ui-e2e` — an e2e that
  drives the hold badge + release panel against a live backend.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the observability substrate** is live: a step emits
   `##caesium::metrics` (capped at 16 KiB, last-write-wins), the metrics persist as
   `DatasetMetric` rows against the declared `DatasetDeclaration` registry, `produces`/
   `consumes`/`assertions` lint clean, and the master `CAESIUM_DATA_ASSERTIONS_ENABLED`
   gate is reported by `GET /system/features`. Closed by a `test/` integration
   scenario: a metrics-emitting run persists metrics readable via
   `GET /v1/datasets/:ns/:name/metrics`.
2. **Stream B — the assertion evaluator** works: a declared assertion in `warn`
   mode records a `DataViolation` without failing the task; `fail` mode goes red
   exactly like `schemaValidation: fail`; a missing declared metric is a violation;
   cold-start `deltaFromBaseline` is warn-only below `CAESIUM_BASELINE_MIN_SAMPLES`;
   `caesium_data_assertions_total{result}` registered. Closed by integration
   scenarios for warn / fail / cold-start.
3. **Stream C — the circuit breaker** breaks the circuit: a `hold`-mode violation
   opens exactly one `DatasetHold` + one `dataset_held` event (repeat violations
   append occurrences, no re-alert), a downstream job that `consumes` the held
   dataset is admitted directly to `skipped` with reason
   `dataset_hold:<ns>/<name>` + a `run_held_upstream` event, and the hold releases
   on a clean producer rerun **and** (when an auth mode is active) on
   `POST /v1/datasets/holds/:id/release` — with that endpoint returning 403 under
   `CAESIUM_AUTH_MODE=none`. Closed by hold-open, downstream-skip, clean-run-release,
   and fail-closed-403 integration scenarios; holds/active metrics registered.
4. **Stream D — the operator surface** ships: `GET /v1/datasets`,
   `/v1/datasets/holds`, `/v1/datasets/:ns/:name/metrics` read the live registry/holds,
   and `caesium dataset list/holds/release/metrics` drive the real endpoints,
   `--json` asserted via `runCLIStdout` (clean stdout captured separately from stderr).
5. **Stream E — the Console UI** surfaces holds: held dataset nodes are badged on
   the lineage graph with the downstream cone shaded, the active-holds count joins
   the nav badges, the ack/release panel drives the release endpoint, and
   `deltaFromBaseline` assertions render a baseline sparkline — each gated by a
   Playwright e2e against a live backend.
6. **H-1 — the integration server** runs with `CAESIUM_DATA_ASSERTIONS_ENABLED=true`
   and a metrics-emitting fixture image, so the Stream A/B/C scenarios drive the
   live binary in CI, not an internal call.
7. **N-1 — docs reflect reality:** the `docs/roadmap.md` Phase-4 "Data circuit
   breaker" entry flipped to Shipped, the design-doc `> Status:` banner updated, the
   `metrics` marker + `produces`/`assertions`/`consumes` fields documented in the
   schema references with a working `docs/examples/` manifest, and this plan indexed
   in `docs/README.md` (backtick form).
8. **Cross-cutting:** `docs/roadmap.md`, `docs/design-data-circuit-breaker.md`, and
   this plan's per-stream `## Progress` entries reflect every shipped stream and
   match the merged PRs; the shared `DatasetDeclaration` registry stays a single table
   coordinated with `freshness-scheduling` (no duplicate registry). (Phase 3
   ergonomics remain explicitly deferred — not a gate here.)

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
   `<Imperative subject> (data-circuit-breaker <wave>-<stream>)` — e.g.
   `Add the ##caesium::metrics marker (data-circuit-breaker W1-α)`. GitHub appends
   `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md) —
  the design of record. Source of truth for intent and scope.
- [`docs/roadmap.md`](../../roadmap.md) Phase 4 Data-Plane Differentiators — the
  "Data circuit breaker" entry this plan closes.
- [`freshness-scheduling.md`](../completed/freshness-scheduling.md) — **shares the `DatasetDeclaration`
  registry table + jobdef `produces` substrate**; coordinate on who creates vs.
  extends (see Source-Of-Truth Note).
- [`contract-enforcement.md`](contract-enforcement.md) — the static/PR-time half of
  the contract story; reads the same `consumes` edges. This plan is the runtime
  breaker for what static analysis cannot see (the *values*).
- [`agent-in-the-loop-remediation.md`](../completed/agent-in-the-loop-remediation.md) —
  `dataset_held` becomes a `data_quality_hold` incident class + `release_hold`
  action in the deferred Phase 3.
- [`docs/job-schema-reference.md`](../../job-schema-reference.md),
  `docs/job-definitions.md`, `docs/caesium-job-llm-reference.md` — the schema docs
  N-1 extends with `produces`/`assertions`/`consumes`.
- `pkg/task/output.go` (`parseMarkers`), `internal/run/schema_validation.go`,
  `internal/run/store.go` (`admit()`), `internal/lineage/impact.go`
  (`QueryImpact`), `internal/event/bus.go` — the shipped substrates this plan
  builds on.
