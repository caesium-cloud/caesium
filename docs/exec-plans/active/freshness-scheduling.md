# Data-Freshness Scheduling — Schedule on Data, Not Time

Last updated: 2026-07-03

Caesium schedules on time today: a cron expression is a *guess about when
data will have arrived*, and the guess is what produces 3 a.m. pages — a late
vendor file turns "not yet" into a failed run, padded cadences burn compute
re-running DAGs whose inputs never changed, and the SLO consumers actually
care about ("how stale may this table be?") lives in a runbook if anywhere.
This plan ships the design of record in
[`design-freshness-scheduling.md`](../../design-freshness-scheduling.md): jobs
**declare** the datasets their steps produce and consume plus a freshness SLO
on each output, and Caesium derives execution from that graph — run when
upstream data has arrived and my output is stale against its SLO, don't run
when nothing changed, and don't page when upstream is late (an observable
`stale-upstream` state with a reason, not a failed run).

The feature is container-native (watermarks ride the existing
`##caesium::output` marker; datasets are declared in YAML — no SDK) and
zero-dependency (a `dataset_declarations`/`dataset_states`/`dataset_derivations`
trio of dqlite tables, the shipped event router for arrival signals, and the
shipped cache identity for "nothing changed"). It reuses four shipped
substrates verified in the repo: observed lineage datasets
(`internal/models/lineage_dataset.go`, behind `CAESIUM_OPEN_LINEAGE_ENABLED`),
downstream traversal (`internal/lineage/impact.go:82` `QueryImpact`), event
ingestion + `_trigger_depth` chaining (`design-event-triggers.md`, shipped),
and cache identity (`internal/cache/hash.go:266` `HashInput`, the value-verified
short-circuit `internal/cache/shortcircuit.go`). It follows the design's
three-phase rollout — **P0** declarations + observability (no scheduling
change), **P1** skip-when-fresh, **P2** full derivation — behind
`CAESIUM_FRESHNESS_ENABLED` (default `false`), so the substrate lands with
zero scheduling risk and behavior turns on incrementally.

The evaluator is **leader-gated** (modeled on the run-queue dequeuer
`internal/runqueue/dequeuer.go`, wired `LeaderCheck: dqlite.IsLocalLeader` at
`cmd/start/start.go:183`), **not** the per-process executor trigger loop
(`internal/executor/executor.go:36`, launched unconditionally on every node at
`cmd/start/start.go:589`) — otherwise an N-node cluster derives N duplicate
runs per stale dataset. Every new CLI verb and REST endpoint below ships with a
`test/` integration scenario driving the real surface against a live server
with `CAESIUM_FRESHNESS_ENABLED=true` (per the `CLAUDE.md` end-to-end gate); a
green unit test that hand-seeds a `DatasetState` proves the state machine, never
the wiring.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work
backlog, `## Sequencing & Dependencies` captures cross-stream order,
and `## Acceptance Criteria` lists the gates that close out the entire
plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies
   are satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every
   PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Source-Of-Truth Note

**When this plan and [`design-freshness-scheduling.md`](../../design-freshness-scheduling.md)
disagree, the design doc wins** — it is authoritative for INTENT and SCOPE
(what freshness declarations, the state machine, the evaluator, and the
three-phase rollout must do). Every file:line anchor in this plan was verified
against the real code at draft time; where the design's prose and the code drift
(e.g. the design references the event router and cache identity as shipped —
both confirmed), the anchors below are the current-code contract an implementer
follows, but no item may add a NEW jobdef field, endpoint, table, or
`CAESIUM_*` knob *beyond what the design enumerates* without first amending the
design doc. Strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) Phase 4 (Data-Plane Differentiators — the
roadmap wins on priority/status disagreements). The YAML contract lands in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) +
`pkg/jobdef/schema.go`; once a `datasets` field is merged there, **the schema
file wins** over any prose in this plan describing its shape. The design's
Non-goals (no built-in pollers, no data-quality judgment, one watermark per
dataset, nullable `namespace` column but no cross-instance datasets in v1) are
hard boundaries — an item that crosses one stops and reconciles first.

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from the
[`design-freshness-scheduling.md`](../../design-freshness-scheduling.md) design
of record (the strategic flagship of the Phase 4 data-plane design wave); the
first wave is the next eligible run of the `exec-plan-wave` skill against this
doc. The design's P0 slice (declarations + observability, no scheduling change)
maps to Streams A–F + H-1 + N-1; P1 (skip-when-fresh) and P2 (full derivation)
are the tail items in Streams C and G, gated behind `CAESIUM_FRESHNESS_ENABLED`
and sequenced last so scheduling behavior only changes after the substrate is
proven.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Jobdef `datasets` schema, declared registry (`dataset_declarations`), cross-job cycle + single-producer lint | **P0** | Not started |
| B | Dataset state substrate — `DatasetState` + `DatasetDerivation` models, state store, watermark advance/verify contract, consumed-watermark capture | **P0** | Not started |
| C | Freshness evaluator — leader-gated durable loop + reactive fast path, observe-only state machine, then P2 derivation to run starts | **P0** → P2 | Not started |
| D | Arrival signals — source `arrival` event-binding bridged through the event router to advance dataset state | **P0** | Not started |
| E | Dataset REST + CLI operator surface — `GET /v1/datasets*`, `POST …/advance`, `caesium dataset list/status/advance` | **P0** | Not started |
| F | Console UI — dataset freshness board, lineage freshness overlay, "why did/didn't this run" derivations panel | P1 | Not started |
| G | Scheduling behavior — skip-when-fresh (P1) + `trigger: {type: freshness}` (P2) | P1 → P2 | Not started |
| H-1 | Integration harness — `CAESIUM_FRESHNESS_ENABLED=true` on the live integration server | — | Not started |
| N-1 | Docs — roadmap Phase 4 flip, design banner, schema references, examples, README | — | Not started |

## Streams

### Stream A — Dataset schema, declared registry & lint (P0 substrate)

The declared graph every other stream reads. A new jobdef surface — `datasets`
on `Step` (`consumes: [name...]`, `produces: [{name, freshness, maxStaleness,
watermark}]`) and `metadata.datasets.sources` (external datasets with
`expectedEvery` + `arrival` binding) — plus a `dataset_declarations` table
upserted on apply (the *declared* graph, complementary to the *observed*
`lineage_datasets` graph, which requires OpenLineage; freshness must NOT require
`CAESIUM_OPEN_LINEAGE_ENABLED`). Largest blast radius on the schema side, so it
merges first. The declaration fields are **scheduling metadata, not execution
inputs** — they do NOT change the container spec or the three engines and MUST
NOT enter `internal/cache/hash.go` (declarations don't bust cache); the watermark
rides the *existing* `##caesium::output` marker, so `pkg/task/output.go` is
untouched.

- [ ] A1. Add the `datasets` jobdef surface: `Step.Datasets` (`consumes`,
      `produces []ProducedDataset{name, freshness, maxStaleness, watermark{key}}`)
      and `Metadata.Datasets.Sources []SourceDataset{name, expectedEvery,
      arrival{event{type,filter}, watermark}, external}`. Wire the dual
      `Step`/`rawStep` declaration + `UnmarshalYAML`, and single-definition
      field validation in `Definition.Validate()`: SLO fields
      (`freshness`/`maxStaleness`/`expectedEvery`) parse as durations, `watermark`
      JSONPath is well-formed, and a `consumes` name is produced/declared/`external`
      **within this definition** (cross-job checks are A3). Reflect the shape in
      `pkg/jobdef/schema.go`. No engine/cache change — datasets are scheduling
      metadata (assert this with a hash-stability unit test: adding `datasets` to a
      step leaves `HashInput.Compute()` unchanged).
      Files: `pkg/jobdef/definition.go` (Step + rawStep + Metadata + Validate +
      UnmarshalYAML), `pkg/jobdef/schema.go`.
- [ ] A2. Add the `DatasetDeclaration` GORM model (job/step refs,
      `namespace` (nullable, day-one per design Non-goals) + `name`, direction,
      SLO fields, watermark key, arrival-binding JSON) and register it in the
      `All` slice; add the declared-registry store (typed CRUD over
      declarations) and upsert the declared graph on **every** apply seam —
      REST apply controller, git-sync importer (`ApplyWithOptions`/`PruneMissing`),
      rebuilt from the manifest set so a removed declaration is pruned. Not a hot
      per-run table (no `hotPathModels()`/`hotTables` entry).
      Files: new `internal/models/dataset_declaration.go`,
      `internal/models/models.go`, new `internal/freshness/registry.go` (the
      declared-registry store), `internal/jobdef/` (Importer apply/prune),
      `api/rest/controller/jobdef/`.
      Depends on: A1.
- [ ] A3. Add the **batch (cross-job) declared-graph lint**: exactly one job
      produces a given dataset (any number consume); a `consumes` name resolves
      to a produced dataset, a declared source, or `external: true` across the
      **whole applied set PLUS existing DB declarations**; and the declared graph
      is acyclic across jobs (a dataset cycle is a derivation cycle — same class
      as the event-trigger static-cycle check). This lives in the **batch**
      validation path (`internal/jobdef/` collect/validate), NOT the
      single-`Definition` validator (which sees one job at a time and can't prove a
      cross-job cycle or single-producer). Wire it into `caesium job lint`, the REST
      lint controller, the REST apply controller (pre-write, no partial persist),
      and git sync/apply.
      Files: `internal/jobdef/` (batch cycle + single-producer validator),
      `api/rest/controller/jobdef/`, `cmd/job/lint.go`.
      Depends on: A2.
      Test: a two-producer set and a cross-job dataset cycle are both rejected at
      `caesium job lint` with **no partial persistence** on apply.

### Stream B — Dataset state substrate & watermark contract (P0 substrate)

The durable truth every scheduling decision reads: one `DatasetState` row per
dataset and an append-only `DatasetDerivation` audit, plus the
watermark/advance contract that distinguishes "a run succeeded" from "the
output advanced". Everything the evaluator needs is in dqlite so failover
resumes on a new leader — no in-process timer is ever the only record of a
pending decision.

- [ ] B1. Add the `DatasetState` model (natural key `namespace`+`name`:
      `watermark`, `advanced_at`, `verified_at`, `status`, `reason`,
      `last_run_id`) and the append-only `DatasetDerivation` model (dataset,
      `decision` enum `derived|skipped_fresh|skipped_upstream|skipped_admission|
      skipped_active_run`, consumed-watermark JSON snapshot, resulting run id) —
      the row behind "why did/didn't this run". Register both in the `All` slice
      (`DatasetState` after `Job`/`JobRun` for its `last_run_id` ref;
      `DatasetDerivation` after it). Neither is a hot per-run table.
      Files: new `internal/models/dataset_state.go`, new
      `internal/models/dataset_derivation.go`, `internal/models/models.go`.
- [ ] B2. Implement the state store + advance/verify contract in
      `internal/freshness/state.go`: `Advance(ns, name, watermark, runID)` moves
      `advanced_at` **only when the watermark value changes** (for
      RFC3339/numeric values, only when it increases — a regression is recorded,
      never advances; the monotonic guard); a successful run with an unchanged
      watermark updates `verified_at`, not `advanced_at`; freshness is evaluated
      against `max(advanced_at, verified_at)`. **Opaque-string watermarks** (git
      SHAs, UUIDs — no orderable relation) cannot use value comparison, so "any
      change advances" would let a *late-finishing older run* clobber a newer
      watermark with a stale value. Gate opaque advances by the **producing run's
      ordering** instead: persist the advancing run's start/completion time (or a
      monotonic sequence) alongside the watermark and only overwrite when the
      incoming run is newer than the one that set the current value; an
      out-of-order opaque write is recorded and dropped, exactly as a numeric
      regression is. Backfill runs MUST NOT advance a
      watermark (monotonic guard — mirror the cron-catchup
      `LatestSuccessfulCronRun` reasoning: derivations ignore backfill runs).
      Pure state logic, fully unit-tested (change vs no-change, monotonic
      regression, RFC3339 vs numeric vs opaque-string, out-of-order opaque write
      dropped, verify-only refresh, backfill-never-advances).
      Files: new `internal/freshness/state.go` (+ `state_test.go`).
      Depends on: B1.
- [ ] B3. Capture watermarks and the **consumed-watermark set** at run
      completion: when a producing step (non-cached success) emits its declared
      `watermark.key` output, call `state.Advance`; when it succeeds with an
      unchanged/absent key, refresh `verified_at` (degraded mode — no watermark
      key — uses completion time, flagged by lint, per the design's honest
      limitation). Record the consumed-watermark snapshot on the run so "is my
      output up to date with my inputs" is a pure row comparison, not a heuristic.
      Hook the existing run-completion/lifecycle path (do NOT poll).
      Files: `internal/freshness/state.go`, the run-completion seam
      (`internal/run/` completion path) + `internal/event/` lifecycle subscribe.
      Depends on: A2 (reads declarations to know which output key is a watermark)
      + B2.

### Stream C — Freshness evaluator: leader-gated loop, then derivation (P0 → P2)

The scheduling brain. A single durable evaluator modeled on the run-queue
dequeuer — **leader-gated, NOT the per-process executor loop** — that runs the
per-dataset state machine. It ships **observe-only** first (P0: computes
`fresh`/`stale`/`stale-upstream`/`violated`, emits events + metrics, derives
**nothing**), then gains derivation (P2). Gated behind `CAESIUM_FRESHNESS_ENABLED`
(default `false`): off means no goroutine, no routes, declarations lint-only.

- [ ] C1. Add the leader-gated evaluator skeleton + observe-only state machine:
      a loop constructed with `LeaderCheck: dqlite.IsLocalLeader` (mirror
      `internal/runqueue/dequeuer.go`) ticking every
      `CAESIUM_FRESHNESS_EVAL_INTERVAL` (default `1m`), env-gated by
      `CAESIUM_FRESHNESS_ENABLED` (default `false`) and capped by
      `CAESIUM_FRESHNESS_MAX_DERIVATIONS_PER_TICK`, wired in `cmd/start/start.go`
      alongside the dequeuer (`runAsync`). Per produced dataset with an SLO,
      compute the status: `fresh` (`now - max(advanced_at, verified_at) ≤
      freshness`), `stale` (SLO exceeded + upstream available), `stale-upstream`
      (SLO exceeded but a consumed dataset hasn't advanced past the last run's
      consumed watermarks — emit `dataset_freshness_at_risk` once per window),
      `violated` (`maxStaleness` breached — emit `freshness_violated` on the bus
      alongside `sla_missed` at `internal/event/bus.go:45`), and `quarantined`
      (set by the circuit breaker; never `fresh`). **Observe-only: record the
      decision on `DatasetDerivation`, derive no runs.** Add
      `caesium_dataset_staleness_seconds{dataset}`,
      `caesium_dataset_derivations_total{dataset,decision}`,
      `caesium_freshness_violations_total{dataset}` (two edit sites in
      `internal/metrics/metrics.go`: the `var (…)` block + `Register()`).
      Files: new `internal/freshness/evaluator.go`, `cmd/start/start.go`,
      `pkg/env/env.go`, `internal/metrics/metrics.go`,
      `internal/event/bus.go` (new `freshness_violated`/`dataset_freshness_at_risk`
      event types).
      Depends on: B2 (state) + B1 (models).
      Acceptance probe (integration): with `CAESIUM_FRESHNESS_ENABLED=true`,
      apply a two-job graph → a dataset transitions `unknown` → run → `fresh`,
      and an expired SLO with no arrival shows `stale-upstream` with **zero** runs
      started.
- [ ] C2. Add the reactive fast path: subscribe to dataset-advance events and
      immediately evaluate the affected downstream slice (reuse the
      `internal/lineage/impact.go:82` `QueryImpact` traversal shape over the
      declared registry), with the timer loop from C1 remaining the correctness
      backstop. Guard the wide-fan-out case (a hub dataset with many consumers)
      with the per-tick derivation cap so a single advance can't storm.
      Files: `internal/freshness/evaluator.go`, `internal/event/` (subscribe).
      Depends on: C1.
- [ ] C3. **P2 — derivation to run starts:** a `stale` decision derives a run
      for the producing job. Derived runs are stamped `_trigger_depth` exactly
      like event-chained runs (so a refresh cascade rides the shipped
      `CAESIUM_MAX_TRIGGER_DEPTH` runaway guard and a cycle past lint still
      terminates); pass concurrency admission (`internal/run/store.go:711`
      `admit` / `AdmitRun` at `store.go:1044`) with the job's declared strategy,
      recording an admission skip/queue on the `DatasetDerivation` row (never
      silently dropped); dedupe to at most one in-flight derivation per (dataset,
      consumed-watermark set) and none while the producing job already has an
      active/queued run consuming the same watermarks (fan-in: three upstream
      advances → one derived run); carry params (`logical_date`,
      `_derived_from_dataset`, consumed watermarks) into the run so a step can
      extract incrementally. Behind `CAESIUM_FRESHNESS_ENABLED` (derivation stays
      off until this ships).
      Files: `internal/freshness/evaluator.go`, `internal/run/` (derived-run
      start via `AdmitRun`).
      Depends on: C2.
      Test: fan-in three-advance → one run; a runtime cycle exhausts
      `_trigger_depth`; an admission-blocked derivation records the skip.

### Stream D — Arrival signals: external event → dataset advance (P0)

The bridge that lets an external arrival advance a dataset without Caesium ever
polling. A source's `arrival` binding is an event pattern — same matcher, same
router, same `_trigger_depth` as the shipped event triggers; freshness adds the
*state* layer (a dataset absorbs N arrival events into one staleness answer).
No built-in S3/SFTP pollers (design Non-goal) — arrival is event push, a
`/v1/hooks/*` webhook, or a sensor container.

- [ ] D1. Bridge matched arrival events into a dataset advance: when an
      ingested event (`POST /v1/events` keyed by `CAESIUM_EVENT_INGEST_API_KEY`,
      or a `/v1/hooks/*` webhook) matches a source's `arrival.event`
      pattern/filter, JSONPath-extract `arrival.watermark` and call
      `state.Advance` for the source dataset, recording the arrival event id so
      run detail can link back to it. Register arrival bindings from the declared
      registry with the shipped event router (reload on the apply seams A2 already
      hits), and reuse the shipped matcher — do NOT fork it. An identical second
      event advances nothing (idempotent on the watermark contract).
      Files: `internal/freshness/` (arrival binding + advance), the event router
      registration seam (`internal/trigger/event/`),
      `internal/freshness/registry.go`.
      Depends on: B2 (advance) + A2 (arrival bindings live on declarations). Event
      router + ingestion are shipped (design-event-triggers).
      Test: `caesium event push` matching a source binding advances the watermark
      and (once C3 is in) a derived run starts; an identical second push derives
      nothing.

### Stream E — Dataset REST + CLI operator surface (P0)

The read/act surface over the state store. This stream **owns the entire
`caesium dataset` Cobra group** (`cmd/dataset/`) and the `datasets` REST routes
(`api/rest/bind/bind.go`), so no other stream creates `cmd/dataset/` or a
`/v1/datasets` route. `--json` output goes to **stdout, clean and parseable**,
captured separately from stderr per the `CLAUDE.md` gate.

- [ ] E1. Add the dataset read + advance REST: `GET /v1/datasets` (list +
      `status` filter, bounded/paginated), `GET /v1/datasets/:ns/:name` (state,
      SLO, producing job), `GET /v1/datasets/:ns/:name/derivations` (the
      `DatasetDerivation` decision audit — why/why-not), and
      `POST /v1/datasets/:ns/:name/advance` (manual arrival, auth-scoped, reusing
      the shipped API-key convention). Controller + service + the route lines in
      `Protected()` (`api/rest/bind/bind.go:57`). The reads serve declared-graph
      datasets before any run exists (`unknown` state), not just observed ones.
      Files: new `api/rest/controller/dataset/`, new
      `api/rest/service/dataset/`, `api/rest/bind/bind.go`.
      Depends on: B1 (state/derivation models) + A2 (declarations) + B2 (state
      store).
- [ ] E2. Add the `caesium dataset` CLI group appended to the `cmds` slice in
      `cmd/execute.go`: `caesium dataset list [--status stale|violated|…] [--json]`,
      `caesium dataset status <namespace.name> [--json]` (state, SLO, last
      decision), and `caesium dataset advance <namespace.name> --watermark <v>`
      (the manual-arrival endpoint). Clean `cmd.OutOrStdout()` JSON, timed-out
      HTTP client, bearer API-key headers (mirror the shipped `caesium event
      push`/`caesium trigger events` CLI hygiene).
      Files: new `cmd/dataset/`, `cmd/execute.go`.
      Depends on: E1.

### Stream F — Console UI: dataset board, lineage overlay, derivations panel

The consumer-facing "is my table up to date" surface. Mirrors the
data-plane-memory-ui precedent (the backend REST/CLI ships first; UI consumes
it). New feature dir `ui/src/features/datasets/`. UI-gated by a `Features`
field so the nav hides when the backend has freshness off.

- [ ] F1. Add the dataset freshness board at `/datasets` (nav-level): status
      chip, staleness-vs-SLO bar, producing job, and the `stale-upstream` reason,
      readable without DAGs. Route in `ui/src/router.tsx`, an `api.ts` method per
      `GET /v1/datasets` / `:ns/:name`, a nav entry, and a
      `FreshnessEnabled` field on the `Features` struct
      (`api/rest/service/system/system.go:36`) so the page hides when
      `CAESIUM_FRESHNESS_ENABLED=false`.
      Files: new `ui/src/features/datasets/` (board), `ui/src/router.tsx`,
      `ui/src/lib/api.ts`, `ui/src/components/layout/Sidebar.tsx`,
      `api/rest/service/system/system.go`.
      Depends on: E1.
- [ ] F2. Add the lineage freshness overlay + "why did/didn't this run"
      derivations panel: the existing `LineageGraph` component
      (`ui/src/features/jobs/LineageGraph.tsx`) gains a freshness coloring overlay
      ("everything downstream of vendor-x is amber"; declared edges render before
      the first run), and the job/run detail gains a derivations panel rendering
      `DatasetDerivation` rows ("18:00 tick skipped — fresh (2h/6h)", "04:31
      derived by raw.vendor_x advance") with the run's consumed-watermark set
      linking back to the arrival event (`GET /v1/datasets/:ns/:name/derivations`).
      Files: `ui/src/features/jobs/LineageGraph.tsx`, `ui/src/features/datasets/`
      (derivations panel), `ui/src/lib/api.ts`.
      Depends on: F1 + E1.

### Stream G — Scheduling behavior: skip-when-fresh (P1) + freshness trigger (P2)

The behavior changes that make freshness *replace* cron rather than merely
observe. Sequenced last and gated behind `CAESIUM_FRESHNESS_ENABLED` so a green
substrate is proven before any tick is skipped or any job drops cron. Touches
the trigger/executor loop, distinct files from the evaluator.

- [ ] G1. **P1 — skip-when-fresh:** a cron tick consults dataset state and
      **skips** when every produced dataset is fresh and no consumed watermark
      advanced since the last run, recording `skipped_fresh` on
      `DatasetDerivation` (visible, opt-out per job via
      `metadata.datasets.skipWhenFresh: false` during trust-building). Cron
      remains the guaranteed upper-bound cadence; this only *removes* provably
      unnecessary runs. The cron path is `internal/trigger/cron/cron.go:82`
      `Listen`/`fireAt`, queued via the executor (`internal/executor/executor.go:36`).
      Files: `internal/trigger/cron/cron.go` (or the executor queue seam),
      `internal/freshness/` (skip decision), `pkg/jobdef/definition.go`
      (`skipWhenFresh` metadata flag).
      Depends on: C1 (state machine) + A1 (metadata surface).
- [ ] G2. **P2 — `trigger: {type: freshness}`:** a purely data-derived job may
      drop cron entirely and declare a freshness trigger; the evaluator owns its
      cadence (cron demoted to optional heartbeat). Add `TriggerTypeFreshness` +
      type-specific validation in `pkg/jobdef/definition.go` `ValidateTriggerSpec`
      and register freshness-triggered jobs with the evaluator instead of the cron
      loop.
      Files: `internal/models/trigger.go`, `pkg/jobdef/definition.go`
      (`ValidateTriggerSpec`), `internal/freshness/evaluator.go`.
      Depends on: C3 (derivation must exist for a freshness-only job to ever run)
      + G1.

#### Deferred — partition-level freshness

Per-partition watermarks (one watermark per *partition* rather than per
dataset) are the natural extension via
[`design-dynamic-fanout.md`](../../design-dynamic-fanout.md) and are **out of
v1 scope** (design Non-goal: "one watermark per dataset in v1"). Not part of
this plan's acceptance criteria; the models carry a nullable `namespace` column
from day one so the later extension does not require a migration rewrite.

## Harness Strengthening

- [ ] H-1. Ensure the integration server exercises the real freshness path: set
      `CAESIUM_FRESHNESS_ENABLED=true` on the `just integration-up` /
      `just integration-test` server (mirroring the lineage
      `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the `CLAUDE.md` gate calls out —
      an unset flag means the evaluator goroutine and `/v1/datasets` routes never
      start, so the scenarios would silently pass against a dead surface). Pass the
      same env through CI and the `test/` harness helpers so the Stream C/D/E
      scenarios drive the live surface, not an internal call. Land in the first
      wave so the substrate's end-to-end gate has a live surface from the start.
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [ ] N-1. Flip the roadmap Phase 4 "Freshness-driven scheduling" row to
      reference the shipped work (link this plan; mark P0/P1/P2 status as they
      land); update the
      [`design-freshness-scheduling.md`](../../design-freshness-scheduling.md)
      `> Status:` banner from "Brainstorm/Design — no implementation yet" to the
      shipped phases; document the `datasets` jobdef surface
      (`produces`/`consumes`/`freshness`/`maxStaleness`/`watermark`,
      `metadata.datasets.sources`, `skipWhenFresh`, `trigger: {type: freshness}`)
      in `docs/job-schema-reference.md`, `docs/job-definitions.md`, and
      `docs/caesium-job-llm-reference.md`; add a freshness/arrival example and a
      fan-in cascade example under `docs/examples/` (pinned images); and in
      `docs/README.md` **UPDATE the existing `design-freshness-scheduling.md`
      bullet** (line ~41) from "(proposed)" to reference this plan — keep it in
      backtick/inline-code form (the `TestDocsREADMEIndexesEveryTopLevelDoc`
      guardrail rejects clickable subdirectory links), do NOT add a duplicate
      entry.
      Files: `docs/roadmap.md`, `docs/design-freshness-scheduling.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–G (runs last, after the runtime ships).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — the declared registry (`dataset_declarations`)
  and the jobdef surface everything else reads. A merges first (largest schema
  blast radius). A1 → A2 → A3 is a strict chain (fields, then model+registry,
  then cross-job lint).
- **Stream B** (state substrate) depends on A2 — B2's state store and B3's
  watermark capture read declarations to know which output key is a watermark.
  B1 → B2 → B3.
- **Stream C** (evaluator) depends on B (reads `DatasetState`); C1 (observe-only)
  → C2 (reactive) → C3 (P2 derivation). C is where `CAESIUM_FRESHNESS_ENABLED`
  and the metrics land.
- **Stream D** (arrival bridge) depends on B2 (advance) + A2 (bindings) and the
  shipped event router; independent of C (advancing state is separate from
  evaluating it).
- **Stream E** (REST + CLI) depends on B1/B2 + A2 (reads state + declarations);
  independent of C — reading state doesn't need the evaluator running. E1 → E2.
- **Stream F** (UI) depends on E1 (endpoints) + F1 → F2.
- **Stream G** (scheduling behavior) depends on C: G1 (skip-when-fresh) on C1,
  G2 (freshness trigger) on C3 + G1.
- **H-1** is independent (justfile/CI/test harness) and supports the C/D/E
  integration scenarios; land it in the first wave so the evaluator's end-to-end
  gate has a live, enabled surface.
- **N-1** runs last, after A–G ship, so the roadmap/schema/design docs reflect
  reality.

**Suggested waves:**
- **W1 = A (A1→A2→A3) + H-1.** A is one strict chain; H-1 is independent.
- **W2 = B (B1→B2→B3).** Unblocked once A2's declarations exist.
- **W3 = C (C1→C2→C3) + D (D1) + E (E1→E2).** All unblocked once B is in; they
  touch different core files — C → evaluator/start/env/metrics/bus, D → event
  router bridge, E → controllers/bind/cmd — so they parallelize.
- **W4 = F (F1→F2) + G (G1→G2) + N-1.** F after E; G after C; N-1 last.

**Within-stream order:** A1 → A2 → A3 (strict). B1 → B2 → B3 (strict). C1 → C2 →
C3 (strict). E1 → E2. F1 → F2. G1 → G2. D1 standalone.

**Cross-stream file conflicts:**

- `pkg/jobdef/definition.go` — **A1** (the `datasets` Step/Metadata surface +
  `Validate`) and **G1**/**G2** (`skipWhenFresh` metadata flag,
  `ValidateTriggerSpec` freshness type) both edit the schema file with its dual
  `Step`/`rawStep` declaration. A1 lands in W1, G in W4 — no same-wave collision,
  but **sequence A → G** (A owns the initial `datasets` block; G extends it).
- `internal/models/models.go` — A2 (`DatasetDeclaration`), B1 (`DatasetState` +
  `DatasetDerivation`) append to the order-sensitive `All` slice. A2 (W1) before
  B1 (W2); additive, different lines, rebases mechanically. None is a hot
  per-run table, so no `pkg/db/db.go` `hotPathModels()` / `pkg/db/router.go`
  `hotTables` entry.
- `internal/freshness/` — A2 creates `registry.go`, B2/B3 create `state.go`, C
  creates `evaluator.go`, D adds arrival binding. Different files in the new
  package; within a wave they don't overlap (W3 has D + C editing different files
  here). The one shared file, `evaluator.go`, is C-only (C1→C2→C3 strict).
- `cmd/start/start.go` — **C1** adds the leader-gated evaluator + reactive
  subscribe wiring (`runAsync`). Single stream (A2's importer apply is in
  `internal/jobdef/`, not start.go), so no collision.
- `pkg/env/env.go` — only **C1** adds fields (`CAESIUM_FRESHNESS_ENABLED`,
  `CAESIUM_FRESHNESS_EVAL_INTERVAL`, `CAESIUM_FRESHNESS_MAX_DERIVATIONS_PER_TICK`).
  Single stream — no conflict. Arrival auth reuses the shipped
  `CAESIUM_EVENT_INGEST_API_KEY` (no new field).
- `internal/metrics/metrics.go` — only **C1** adds collectors (two edit sites:
  the `var (…)` block + `Register()`). Single stream — no conflict.
- `internal/event/bus.go` — **C1** adds the `freshness_violated` /
  `dataset_freshness_at_risk` event types (append to the `Type` const block near
  `:45`). Single stream.
- `api/rest/bind/bind.go` — only **E1** adds `/v1/datasets*` routes (the import
  block is the conflict-prone part; single stream avoids it). D1 routes arrivals
  through the *existing* event ingestion, adding no new REST route.
- `cmd/execute.go` — only **E2** appends the `caesium dataset` command group;
  **Stream E owns the entire `cmd/dataset/` group** (list, status, advance) so no
  other stream creates it.
- `internal/jobdef/` batch validator — **A3** (single-producer + cross-job
  dataset cycle) lives here, NOT in `pkg/jobdef/definition.go` (the
  single-`Definition` validator can't see cross-job cycles). A1's per-definition
  field validation *does* live in `definition.go` — different file from A3, so no
  A1↔A3 collision within Stream A.
- `api/rest/service/system/system.go` — only **F1** adds the `FreshnessEnabled`
  `Features` field. Single stream.
- **No `internal/cache/hash.go` change** — datasets are scheduling metadata, not
  execution inputs; the cache key is untouched (A1 asserts hash stability). No
  `go.mod`/`go.sum` change is anticipated (all substrates — event router, cache,
  lineage traversal, leader check — are shipped); if any stream adds a dependency,
  flag the `go.sum` conflict for `go mod tidy` resolution, not a hand-merge.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (D, E):** an integration scenario in `test/`
  that drives the **real surface** with `CAESIUM_FRESHNESS_ENABLED=true` — `GET
  /v1/datasets` and `POST …/advance` against the live server, or the CLI binary
  via `s.runCLI*` — and asserts observed output. **For `--json` CLI output,
  capture stdout SEPARATELY from stderr via `runCLIStdout`** (not the
  stream-merging `runCLIRaw`) and assert it is clean and parseable, per the
  `CLAUDE.md` gate. A unit test that hand-seeds a `DatasetState` proves the state
  machine, not the wiring — both are required.
- **New metric (C1):** assert via `internal/metrics/testutil` in a `*_test.go`;
  each collector must also appear in `Register()`.
- **Job-schema change (A1, A3, G2):** `caesium job lint --path docs/examples/`
  green on the new freshness + fan-in examples; a two-producer set and a
  cross-job dataset cycle rejected at lint with no partial persist.
- **UI (F1, F2):** `just ui-lint && just ui-test && just ui-e2e` — Playwright
  e2e drives the dataset board + lineage overlay against a live backend
  (data-plane-memory-ui precedent).
- **Leader-gating (C1):** a unit test with a fake `LeaderCheck` (the dequeuer's
  pattern) proves a non-leader node derives nothing; a `CAESIUM_FRESHNESS_ENABLED=false`
  test proves the goroutine and routes are inert.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the declared registry** is live: `datasets`
   (`produces`/`consumes`/`freshness`/`maxStaleness`/`watermark`,
   `metadata.datasets.sources`) parse and validate, apply upserts the declared
   graph into `dataset_declarations`, and cross-job lint rejects a two-producer
   set and a cross-job dataset cycle. Closed by a `test/` integration scenario
   (apply a two-job graph → declarations queryable) + a `caesium job lint`
   rejection scenario, green in CI.
2. **Stream B — the state substrate** is durable: `DatasetState` advances only on
   a watermark change (monotonic for RFC3339/numeric), refreshes `verified_at` on
   an unchanged-watermark success, never advances on a backfill run, and records
   the consumed-watermark set per run. Closed by state-store unit tests + a
   run-completion integration scenario showing `unknown` → run → `fresh`.
3. **Stream C — the evaluator** is a leader-gated runtime feature: with
   `CAESIUM_FRESHNESS_ENABLED=true`, the state machine emits the
   `fresh`/`stale`/`stale-upstream`/`violated` decisions with the
   `caesium_dataset_staleness_seconds` / `_derivations_total` /
   `caesium_freshness_violations_total` metrics registered, observe-only P0
   derives nothing, and P2 derivation starts a run through admission +
   `_trigger_depth` with fan-in dedupe. Closed by an observe-only scenario (SLO
   expiry → `stale-upstream`, zero runs) + a P2 fan-in scenario (three advances →
   one derived run) + a leader-gating unit test, green in CI.
4. **Stream D — arrival signals** advance state: an ingested/webhook event
   matching a source `arrival` binding advances the source watermark via the
   shipped event router, and an identical second event advances nothing. Closed
   by a `caesium event push` → advance integration scenario.
5. **Stream E — the operator surface** ships: `GET /v1/datasets*` +
   `POST …/advance` return truthful state/derivations, and `caesium dataset
   list/status/advance` drive the real endpoints with clean `runCLIStdout`
   stdout. Closed by integration scenarios for the REST reads and each CLI verb.
6. **Stream F — the Console UI** ships: the dataset freshness board, the lineage
   freshness overlay, and the derivations panel render against the live backend,
   gated by the `FreshnessEnabled` `Features` flag. Closed by `just ui-e2e`
   Playwright scenarios green in CI.
7. **Stream G — scheduling behavior** works: a cron tick with fresh outputs
   records `skipped_fresh` and starts no run (P1), and a `trigger: {type:
   freshness}` job runs purely on data derivation (P2). Closed by a
   skip-when-fresh scenario + a freshness-trigger scenario, green in CI.
8. **H-1 — the integration server** runs with `CAESIUM_FRESHNESS_ENABLED=true`
   so every C/D/E scenario drives the live evaluator + routes, not an internal
   call.
9. **N-1 — docs reflect reality:** the roadmap Phase 4 freshness row links this
   plan, the design-doc `> Status:` banner is updated per shipped phase, the
   `datasets` surface is documented across the schema references with working
   `docs/examples/` manifests, and this plan is indexed in `docs/README.md`.
10. **Cross-cutting:** `docs/roadmap.md`, `docs/design-freshness-scheduling.md`,
    and this plan's per-stream `## Progress` entries reflect every shipped stream
    and match the merged PRs. (Partition-level freshness remains explicitly
    deferred to `design-dynamic-fanout.md` — not a gate here.)

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line
   is satisfied (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every
   PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the
   active wave subsection in `## Progress` (or open a new wave
   subsection if none exists yet), and update any cross-linked design
   doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (freshness-scheduling <wave>-<stream>)` —
   e.g. `Add the dataset declaration registry (freshness-scheduling W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-freshness-scheduling.md`](../../design-freshness-scheduling.md) —
  the design of record. Source of truth for intent and scope.
- [`docs/roadmap.md`](../../roadmap.md) Phase 4 (Data-Plane Differentiators) —
  the design-wave entry this plan promotes from proposed to shipped; freshness is
  the strategic flagship.
- [`docs/design-event-triggers.md`](../../design-event-triggers.md) +
  [`exec-plans/completed/event-trigger-routing.md`](../completed/event-trigger-routing.md)
  — the shipped event ingestion, router, and `_trigger_depth` chain guard that
  arrival signals (Stream D) and derived-run cascades (Stream C) ride.
- [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md) +
  [Data-Plane Memory UI](../completed/data-plane-memory-ui.md) — the observed
  `lineage_datasets` graph, `QueryImpact` traversal, and cache identity this plan
  builds on; the CLI/REST-first-then-UI precedent Stream F follows.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) +
  `pkg/jobdef/schema.go` — the YAML contract Stream A extends with `datasets`;
  the schema file wins once merged.
- [`docs/job-schema-reference.md`](../../job-schema-reference.md),
  `docs/job-definitions.md`, `docs/caesium-job-llm-reference.md` — the schema docs
  N-1 extends with the `datasets` surface and the freshness trigger.
- Companion Phase 4 designs this plan interlocks with:
  [`design-window-scheduling.md`](../../design-window-scheduling.md) (IF vs WHEN),
  [`design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md)
  (quarantined datasets never fresh),
  [`design-contract-enforcement.md`](../../design-contract-enforcement.md)
  (contract violations block the advance),
  [`design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md)
  (`freshness_violated` incident class),
  [`design-dynamic-fanout.md`](../../design-dynamic-fanout.md) (deferred
  partition-level freshness).
