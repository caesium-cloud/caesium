# Data Circuit Breaker — Dataset Holds & Statistical Assertions

Last updated: 2026-09-05

**Plan 1 of [`closed-loop-arc.md`](closed-loop-arc.md) — the data loop.** The arc
closes a loop over the memory Caesium already keeps; this plan builds the data
half of it end to end: **observe** (a step self-reports what it produced via a
new `##caesium::metrics` marker) → **assert** (a post-task evaluator compares the
sample against a rolling baseline) → **hold** (a violating dataset breaks the
circuit; downstream consumers admit straight to `skipped` instead of eating
poison, and exactly one alert fires at the source) → **incident** (the hold opens
a `data_quality_hold` incident carrying the violation, the baseline snapshot, and
the impact cone) → **proposal** (an agent proposes `release_hold` or a producer
`apply_jobdef_patch`) → **approval** (a human decides through the tier-3 pipeline
Plan 0 wires) → **release** (the gate reopens, or the next clean producer run
reopens it), with every step of that chain **why-explainable** through the shipped
task-scoped explainer. Plan 1 is where the arc's *durable action* pipeline first
carries a non-failure signal, and where the Git-PR provenance route of
`apply_jobdef_patch` is built (Plans 2-E and 3-F reuse it and add nothing to it).

Caesium's data contracts today are **structural**: `outputSchema` is validated
post-task (`internal/run/schema_validation.go` `ValidateTaskOutputSchema` /
`ValidateTaskOutputSchemaInstance`, called from both executors — `internal/job/job.go`
and `internal/worker/runtime_executor.go`), violations persist on the `TaskRun`
(`SchemaViolations`, written by `internal/run/store.go` `SaveSchemaViolations`), and
`metadata.schemaValidation: warn|fail` decides whether the task goes red. That
catches *shape* problems, never the failures that poison consumers: a truncated
feed with a perfectly valid schema, a join key that goes 40% null, a watermark
stalled on yesterday's data. Bad *data* propagates one hop further with every
downstream trigger, and failing every consumer buries the root cause under N pages.

This plan ships [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md):
a circuit breaker on the **dataset**, not the run. A new `##caesium::metrics`
stdout marker (beside `##caesium::output` in `pkg/task/output.go` `parseMarkers`)
lets a step self-report what it observed; a post-task **assertion evaluator** (a
sibling to schema validation) records metrics, computes rolling baselines
server-side in Go, and on a `hold`-mode violation opens a `DatasetHold` and fires
**one** alert at the source; a **run-admission gate** in the run store
(`internal/run/store.go` `admit()`, in the same transaction that inserts the run)
skips downstream jobs that consume a held dataset with reason
`dataset_hold:<ns>/<name>` instead of feeding them poison; and holds release on
human ack (fail-closed under `CAESIUM_AUTH_MODE=none`) or the next clean producer
run. The whole feature is gated by `CAESIUM_DATA_ASSERTIONS_ENABLED` (default
`false`) — off means no evaluator, no gate, no routes.

Per the `CLAUDE.md` end-to-end gate, every new CLI verb (`caesium dataset
holds/release/metrics`) and REST endpoint (`GET /v1/datasets*`, `POST
/v1/datasets/holds/:id/release`) ships with a `test/` integration scenario that
drives the real surface against a live server with
`CAESIUM_DATA_ASSERTIONS_ENABLED=true`, capturing `--json` stdout separately via
`runCLIStdout` (`test/data_plane_e2e_test.go`). A green unit test that hand-feeds
the evaluator proves the evaluator, never the wiring.

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
this Source-Of-Truth Note.

**Recorded deviation — `CAESIUM_GIT_WRITE_CREDENTIALS` (F3).**
[`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md)
§ "Events, notifications, REST, env" enumerates exactly four env knobs —
`CAESIUM_DATA_ASSERTIONS_ENABLED`, `CAESIUM_BASELINE_WINDOW`,
`CAESIUM_BASELINE_MIN_SAMPLES`, `CAESIUM_DATASET_METRIC_RETENTION` (verified
2026-09-05) — and explicitly says "(No release-auth toggle…)". F3's
`CAESIUM_GIT_WRITE_CREDENTIALS` is therefore a **new config knob beyond the
design**, authorized in scope by [`closed-loop-arc.md`](closed-loop-arc.md)
§ Synergies (the row that assigns the Git-PR route of `apply_jobdef_patch` to this
plan's Stream F). Per the rule above, the design docs must be amended **before**
F3 lands: **N-2 below does that, and F3 depends on N-2.** No other item may add a
knob without the same treatment.

**On cross-plan ordering and on *why* something is in
scope, [`closed-loop-arc.md`](closed-loop-arc.md) wins** — it is the program-level
source of truth for the four-plan arc, and Stream F below exists because the arc's
Synergies table assigns the deferred Phase 3 to this plan. Strategic
priority/status is tracked in [`docs/roadmap.md`](../../roadmap.md) — the Phase-4
Data-Plane Differentiators table (§ "Data circuit breaker") — and the roadmap wins
on priority/status disagreements. The job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go); this plan extends
the shipped `steps[].datasets.produces` / `steps[].datasets.consumes` surface
(`StepDatasets`, `ProducedDataset`, `ConsumedDataset`) with `assertions`,
`onViolation`, and `release`, and adds `metadata.onUpstreamHold` — a schema change
(new fields + `Validate()`), gated so the fields are inert when
`CAESIUM_DATA_ASSERTIONS_ENABLED=false`.

**Arc conventions apply by reference, not by restatement.** This plan inherits
[`closed-loop-arc.md`](closed-loop-arc.md) `## Shared conventions` **1** (one
feature gate on `Environment` + `Features`, gating startup wiring, route mounting,
and jobdef validation), **2** (harness: every self-server lane gets the flag; auth
paths run on the auth lane), **3** (explainability: one persisted event + one item
+ one `why` scenario per plan), **4** (agent actions extend the one catalog and the
one approval pipeline — no second apply path, no second approval surface), **5**
(the shared stats substrate — not used by this plan), **6** (cite symbols, never
line numbers), **7** (N- item contents, including the loop tour), and **8** (wave
hygiene). Where an item below contradicts one of those, it says so explicitly.

### Resolved (2026-09-05) — the shared `DatasetDeclaration` registry race

[`freshness-scheduling.md`](../completed/freshness-scheduling.md) **shipped**
(Streams A–G + H-1 + N-1, PRs [#277](https://github.com/caesium-cloud/caesium/pull/277)–[#299](https://github.com/caesium-cloud/caesium/pull/299)),
and [`contract-enforcement.md`](../completed/contract-enforcement.md) has
**shipped** too and has since extended the same jobdef structs. Verified in the
current tree:

- `internal/models/dataset_declaration.go` exists (`DatasetDeclaration`, table
  `dataset_declarations`, keyed on `Name` with a nullable `Namespace`, carrying
  `Freshness`/`MaxStaleness`/`WatermarkKey`/`SchemaJSON`/`SchemaFrom`/`SchemaVersion`/
  `SkipWhenFresh`/`External`/`ArrivalBinding`).
- `pkg/jobdef/definition.go` has `StepDatasets{Consumes []ConsumedDataset,
  Produces []ProducedDataset}` on `Step` — i.e. the shipped YAML shape is
  `steps[].datasets.produces` / `steps[].datasets.consumes`, **not** a bare
  step-level `produces:`/`consumes:` — with `ProducedDataset{Name, Schema,
  SchemaFrom, Version, Freshness, MaxStaleness, Watermark}` and
  `ConsumedDataset{Name, Schema}` (scalar-or-mapping `UnmarshalYAML`/`UnmarshalJSON`).
  `Schema`/`SchemaFrom`/`Version` were added by contract-enforcement.
- `cmd/dataset/{dataset,list,status,advance,http}.go` — the Cobra group exists and
  registers `listCmd, statusCmd, advanceCmd`.
- `api/rest/controller/dataset/dataset.go` + `api/rest/service/dataset/dataset.go`
  exist; `api/rest/bind/bind.go` mounts `GET /datasets`, `GET /datasets/:ns/:name`,
  `GET /datasets/:ns/:name/derivations`, `POST /datasets/:ns/:name/advance`.

**Consequence: every "create-or-extend" item in this plan resolves to EXTEND.**
No item creates the registry model, the jobdef dataset block, the `cmd/dataset`
group, the dataset REST package, or the `/v1/datasets` route family. The original
coordination language is preserved below for its rationale, but the coin-flip is
decided.

> *Original coordination text (kept for rationale; superseded by the paragraph
> above).* **Shared `DatasetDeclaration` registry — coordinate with
> `freshness-scheduling.md`.** The design is explicit that the declared-dataset
> registry (`produces:` keyed `(namespace, name)`) is the **same YAML and model
> substrate** as [`design-freshness-scheduling.md`](../../design-freshness-scheduling.md);
> "whichever design lands first creates it, the other extends it." To keep the two
> plans from colliding on the base table, the canonical names are freshness's: the
> model is **`DatasetDeclaration`** in `internal/models/dataset_declaration.go`
> (table `dataset_declarations`), and the jobdef `produces`/`consumes` entries key
> the dataset identity on **`name`** (not `dataset`) — matching
> [`freshness-scheduling.md`](../completed/freshness-scheduling.md) Stream A and the
> [`contract-enforcement.md`](../completed/contract-enforcement.md) plan, both of which use
> `name`. The freshness plan's Stream A owns the `dataset_declarations` registry
> model + the `Metadata.Datasets` jobdef block; this plan's Stream A owns the
> assertion/`consumes` half of the same registry. **Neither ships a private copy of
> the base registry table, and neither introduces a second `Dataset` model.**
> Whichever plan's registry item merges first creates the `DatasetDeclaration`
> model + jobdef `produces` scaffold; the second plan's item extends it (adds
> columns / fields) rather than redefining it. If a Stream-A item here finds the
> base registry already merged by freshness, it **extends** — see the Sequencing
> section's cross-plan note. The [`contract-enforcement`](../completed/contract-enforcement.md)
> plan reads the same `consumes` edges for its compatibility graph but does not
> define registry structure.

## Progress (as of 2026-09-05)

No implementation waves have shipped yet. The plan was published from
[`design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md) alongside
its Phase-4 companions (`contract-enforcement`, `freshness-scheduling`,
`agent-in-the-loop-remediation`); it was **re-cut on 2026-09-05** as Plan 1 of
[`closed-loop-arc.md`](closed-loop-arc.md): the registry race is resolved (all
create-or-extend items are EXTEND), citations are refreshed to symbols, two
sibling-driven shape requirements are imposed on B1 and A5, and the deferred Phase
3 is pulled into scope as a new **Stream F**. **The first wave is the next
eligible run of the `exec-plan-wave` skill against this doc — W1 (`A` + `H-1`)
in the wave plan under `## Sequencing & Dependencies`.** The design's four phases (Phase 0
Observe → Phase 1 Assert → Phase 2 Break the circuit → Phase 3 Ergonomics) map onto
the streams below; of Phase 3, the `park` disposition alone remains deferred
(parked at the arc level), and backtest evaluation of assertions moved to Plan 3.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Observability substrate — `##caesium::metrics` marker, `DatasetMetric` model + `DatasetDeclaration` extension, jobdef `assertions`/`onViolation`/`release`/`onUpstreamHold` schema + lint, metrics persistence in both executors, `asOf`-cut baseline read, master env gate (Phase 0) | **P0** | Not started |
| B | Assertion evaluator — `run.EvaluateDataAssertions` with rolling baselines, cold-start warn-only, `warn`/`fail` dispatch, `DataViolation` persistence, factored pure `evaluate(...)` (Phase 1) | **P0** | Not started |
| C | Circuit breaker — `DatasetHold` model + partial-unique guard, hold-open path, downstream admission gate, release (clean-run + fail-closed ack), bus events + alert-once (Phase 2) | **P0** | Not started |
| D | Operator surface — `GET /v1/datasets/holds*` + `/metrics` reads + `caesium dataset holds/release/metrics` CLI | P1 | Not started |
| E | Console UI — hold badges on the lineage graph, ack/release panel + baseline sparkline, nav active-holds count | P1 | Not started |
| F | Agent & freshness integration (closes the loop) — `data_quality_hold` incident class, `release_hold` action, the Git-PR provenance route of `apply_jobdef_patch`, held ⇒ not-fresh, `why` provenance (former Phase 3) | **P0** | Not started |
| H-1 | Integration harness — `CAESIUM_DATA_ASSERTIONS_ENABLED=true` on the default lane **and every self-server lane**, auth-lane scenarios, metrics-emitting script image | — | Not started |
| N-1 | Docs — roadmap Phase-4 flip, design banner, schema references + examples, README index, `docs/tour-data-loop.md`, arc dashboard row | — | Not started |
| N-2 | Design amendment — add `CAESIUM_GIT_WRITE_CREDENTIALS` + the Git-PR provenance route to the design docs **before** F3 lands (Source-Of-Truth Note deviation) | — | Not started |
| (parked) | `park` run disposition + release-drain | — | **Parked** at the arc level (see `closed-loop-arc.md` § Parked, archived, filed) |

## Streams

### Stream A — Observability substrate: marker, models, schema, metrics persistence (Phase 0)

The container-native data plane every other stream builds on: the fourth stdout
marker, the metrics table plus the registry extension, the jobdef schema for
assertions, and metrics persistence wired into both executors' post-task
pipelines. Largest blast radius (`pkg/task/output.go`, `internal/models/`,
`pkg/jobdef/definition.go`, both executors), so it merges first. Phase 0 is
enforcement-free: it seeds baselines while the evaluator (B) and breaker (C) are
reviewed.

- [x] A1. Add the `##caesium::metrics` marker to `pkg/task/output.go`: a fourth
      marker beside `output`, `output-ref`, and `branch`, parsed in the same
      single-pass `parseMarkers` scan (marker stem check ordered so `metrics`
      doesn't collide with `output`). Payload is a flat JSON object; `dataset`
      selects the declared dataset (omitted ⇒ the step's sole declared dataset,
      error if ambiguous); values are JSON numbers or RFC3339 strings; multiple
      lines merge last-write-wins per `(dataset, metric)`. Metrics get their own
      `MaxMetricsBytes = 16 KiB` cap (separate from the 64 KiB `MaxOutputBytes` so
      a chatty emitter can't evict real outputs); malformed lines are skipped
      leniently like malformed output lines.
      *Refreshed 2026-09-05:* `parseMarkers` and `MaxOutputBytes = 65536` are
      verified present in `pkg/task/output.go`; the marker set now also includes
      the fan-out partition marker, so "fourth" means "a further marker in the same
      scan", not a literal ordinal — re-grep the marker table before editing.
      Files: `pkg/task/output.go` (+ `output_test.go`).
      **Done (W1-α):** `metricsMarker = "##caesium::metrics "` sits with the other
      marker constants in `pkg/task/output.go`; it shares no stem with
      `outputMarker`/`outputRefMarker`/`partitionMarker`, so its `strings.Cut` in
      `parseMarkers` is order-independent (the marker table was re-grepped first,
      as the refreshed note asks). `Markers` gains `Metrics []DatasetMetricSample`
      and `MetricsTruncated bool`; `metricsAccumulator` merges lines
      last-write-wins per `(dataset, metric)`, coerces JSON numbers and RFC3339
      strings (stored as epoch seconds, fraction preserved) and drops everything
      else, and skips malformed lines leniently. **Cap shape decided:** overflow
      past `MaxMetricsBytes = 16 KiB` DROPS the samples that do not fit and sets
      `MetricsTruncated` — it does not error, because `parseMarkers` returning an
      error discards the task's real OUTPUTS too, which is precisely what a
      separate cap exists to prevent. The cap is charged per entry
      (`len(dataset) + len(metric) + 32`) rather than by re-marshalling the whole
      set on every line, which would be quadratic; the goal is bounding memory for
      a runaway emitter.
- [x] A2. **Extend** the shipped `DatasetDeclaration` registry model with the
      assertion spec, and **add** the new `DatasetMetric` model. The registry race
      is decided: `internal/models/dataset_declaration.go` already exists
      (`DatasetDeclaration`, table `dataset_declarations`, keyed on `Name` with a
      nullable `Namespace`), so this item adds the assertion-spec column(s) — e.g.
      `AssertionsJSON`, `OnViolation`, `Release` — beside the existing
      `SchemaJSON`/`SchemaFrom`/`SchemaVersion` fields. **Do not create a second
      registry model, do not create a second `Dataset` model, and do not rename
      `Name`/`Namespace`/`Direction`.** `Namespace` is already the nullable column
      the tenancy open question (design open-question 4) asks for — no new tenant
      column is needed. The one genuinely new model is `DatasetMetric` — task-run
      ref (`CASCADE` like `LineageDataset`), namespace, name, metric, float value
      (watermarks stored as epoch seconds), created-at; pruned past
      `CAESIUM_DATASET_METRIC_RETENTION` (default `2160h`/90d) by an env-gated
      background pruner started in `cmd/start/start.go` (mirror an existing
      pruner). Register `DatasetMetric` in the order-sensitive `models.All` slice
      (`internal/models/models.go`). Both are catalog/observability tables, **not**
      hot per-run tables — do NOT add them to `hotPathModels()` / `hotTables`.
      > *Superseded body (kept for rationale; the coin-flip is decided — see the
      > "Resolved (2026-09-05)" subsection above).* "Add the `DatasetDeclaration`
      > declared-registry model (canonical name shared with `freshness-scheduling`
      > Stream A) … **Coordinate with `freshness-scheduling` Stream A on the base
      > registry table**: if freshness's `dataset_declarations` registry has already
      > merged, this item **extends** … rather than creating a second `Dataset`
      > registry; if this item merges first, it creates `DatasetDeclaration` under
      > the canonical name."
      Files: `internal/models/dataset_declaration.go` (**extend**),
      new `internal/models/dataset_metric.go`, `internal/models/models.go`,
      `pkg/env/env.go`, `cmd/start/start.go`.
      Depends on: A1 (the marker the metrics come from).
      **Done (W1-α):** `DatasetDeclaration` gains `AssertionsJSON`, `OnViolation`
      and `Release` beside the existing `SchemaJSON`/`SchemaFrom`/`SchemaVersion`;
      no second registry model, no rename. New `internal/models/dataset_metric.go`
      (`DatasetMetric`: `TaskRunID` with `constraint:OnDelete:CASCADE` like
      `LineageDataset`, namespace/name/metric/float value/created-at), registered
      in `models.All` after its FK parent and deliberately absent from
      `hotPathModels()`. `CAESIUM_DATASET_METRIC_RETENTION` (default `2160h`) plus
      `run.StartDatasetMetricRetentionPruner` (mirrors
      `event.StartWebhookEventRetentionPruner`) started from `cmd/start/start.go`
      under the master flag. **Citation correction:** the item's file list omits
      the writer — nothing would ever populate the three new columns — so
      `internal/freshness/registry.go` `BuildDeclarations` was extended to marshal
      `produces[].assertions` onto the row it already builds. Without that, A2 and
      A3 both ship inert.
- [x] A3. **Extend** the shipped jobdef dataset block with the assertion schema.
      The shipped YAML shape is `steps[].datasets.produces[]` /
      `steps[].datasets.consumes[]` — the `StepDatasets` struct on `Step` in
      `pkg/jobdef/definition.go` (`type StepDatasets struct { Consumes
      []ConsumedDataset; Produces []ProducedDataset }`, verified 2026-09-05) —
      **not** bare step-level `produces:`/`consumes:`. Add
      `assertions{rowCount, nullRate, freshness, custom[]}`,
      `onViolation ∈ warn|fail|hold`, and `release ∈ auto|manual` (default `auto`)
      as new fields on the existing `ProducedDataset` (which already carries
      `Name`, `Schema`, `SchemaFrom`, `Version`, `Freshness`, `MaxStaleness`,
      `Watermark`), and leave `ConsumedDataset`'s scalar-or-mapping
      `UnmarshalYAML`/`UnmarshalJSON` compatibility intact. Do not rename the
      `Name` identity key and do not fork the struct. Separately add
      `onUpstreamHold ∈ skip|run` (default `skip`) to `Metadata`
      (`type Metadata struct` in the same file) — that field is genuinely new.
      Wire the fields + `Validate()` + the dual
      `Step`/`rawStep` declaration + `UnmarshalYAML` in `pkg/jobdef/definition.go`,
      `pkg/jobdef/schema.go`, and `internal/jobdef/runtime/spec.go`. `caesium job
      lint` validates dataset names, `consumes` resolvability against declared
      producers, assertion syntax, and flags declared datasets never observed in
      lineage (`internal/lineage/`). **`consumes` resolvability lint already
      exists** for the freshness/contract graph — extend that check, do not add a
      second one. Per arc convention 1, the new fields must be inert (validation
      refused or ignored with a clear message) when
      `CAESIUM_DATA_ASSERTIONS_ENABLED=false`, mirroring the
      `freshnessFeatureEnabled()` check in `validateTrigger`. **Cache-hash note:** `produces`/`consumes`/
      `assertions` are post-task evaluation and admission concerns — they do NOT
      change what the container executes, so they are **not** added to
      `internal/cache/hash.go` `HashInput` (mirroring triggers). Document the one
      edge in the item: a cache-short-circuited task emits no fresh metrics, so the
      evaluator treats a cached task as "no new sample" (no assertion, no baseline
      write) — not a violation.
      > *Superseded body (2026-07-03; kept for rationale, corrected in the text
      > above on 2026-09-05).* "Add the jobdef schema for `produces`/`consumes`: a
      > step-level `produces []ProducedDataset{name, assertions{…}, onViolation,
      > release}` and `consumes [name...]`, plus `metadata.onUpstreamHold`. The base
      > `produces`/`consumes` block keyed on `name` (plus `freshness`/`watermark`)
      > is owned by `freshness-scheduling` Stream A; this item adds the
      > `assertions`/`onViolation`/`release` fields to `ProducedDataset` — it does
      > not rename the identity key or fork the struct." The bare step-level
      > `produces:`/`consumes:` YAML that sentence describes **does not exist**;
      > the shipped nesting is `steps[].datasets.*`.
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`,
      `internal/jobdef/runtime/spec.go`, `cmd/job/` (lint path),
      `internal/lineage/` (declared-vs-observed check).
      Depends on: A2 (registry identity the schema references).
      **Done (W1-α):** `ProducedDataset` gains `Assertions *DatasetAssertions`,
      `OnViolation` and `Release` (+ `EffectiveRelease()` for the `auto` default);
      `Metadata` gains `OnUpstreamHold` (+ `EffectiveOnUpstreamHold()` for the
      `skip` default). The assertion grammar follows the design's worked example:
      `rowCount`/`nullRate` shorthands (each an `AssertionSpec` with an optional
      `metric:` override plus `min`/`max`/`deltaFromBaseline`), `freshness`
      (`watermark` + `maxLag`) and `custom[]`. `deltaFromBaseline` is a PERCENTAGE
      STRING (`"50%"`, per the design's `deltaFromBaseline: 50%`), with one
      exported parser — `jobdef.ParseDeltaFromBaseline` — so the evaluator cannot
      interpret the grammar differently. Validation
      (`validateDatasetAssertionSurface` in `pkg/jobdef/schema.go`, called from
      `validateDatasets`) rejects an unknown disposition, an assertion with no
      bound, `min > max`, a non-percentage delta, a `custom` entry without a
      metric, an incomplete `freshness`, and two assertions on one metric; the
      whole surface is refused with a message naming
      `CAESIUM_DATA_ASSERTIONS_ENABLED=true` when the gate is off, mirroring
      `freshnessFeatureEnabled()` in `validateTrigger`.
      **Citation corrections:** (a) no `Step`/`rawStep` change was needed — the new
      fields hang off `StepDatasets`, which `rawStep` already declares, and no new
      step-level key was added; (b) `internal/jobdef/runtime/spec.go` needed NO
      change — it carries container specs and the `fanOut` decode, and assertions
      are post-task metadata that never reach a container spec; (c) the `consumes`
      resolvability lint was left alone as instructed
      (`internal/jobdef.ValidateDatasetGraph` → `freshness.ValidateGraph`, driven
      by `cmd/job/lint.go` and the server lint) — no second check was added. The
      declared-vs-observed check is new: `lineage.CheckDeclaredDatasetsObserved`
      (`internal/lineage/declared.go`) reports declared produced datasets no task
      run has ever emitted, surfaced as lint WARNINGS from
      `api/rest/controller/jobdef.Lint` beside `lint.CheckVolumeWriters`, gated on
      BOTH the master flag and `CAESIUM_OPEN_LINEAGE_ENABLED` (with capture off
      there are no observed rows at all, so every declaration would be reported —
      noise, not signal).
      **Cache-hash edge, as required:** nothing was added to
      `internal/cache/hash.go`; a cache-short-circuited task emits no fresh
      metrics, so the evaluator sees "no new sample" — no assertion, no baseline
      write, and explicitly not a violation.
- [x] A4. Persist emitted metrics in the post-task pipeline: add a
      `run.EvaluateDataAssertions(...)` call site beside
      `run.ValidateTaskOutputSchema` / `run.ValidateTaskOutputSchemaInstance` in
      **both** executors (`internal/job/job.go`,
      `internal/worker/runtime_executor.go` — cite the symbols, not line
      numbers; see the refreshed note below for the exact three call sites), gated on
      `CAESIUM_DATA_ASSERTIONS_ENABLED`. In Phase 0 the function's only job is to
      persist emitted metrics as `DatasetMetric` rows (including undeclared metrics
      — free baseline history for assertions added later); Stream B fills in the
      evaluation logic behind the same seam (new file in `internal/run/`, so B does
      NOT re-edit the executors). Replay-quarantined runs (`TaskRun.Quarantine`)
      are excluded completely — no metrics recorded.
      *Refreshed 2026-09-05 (arc convention 6 — symbols only):* there are
      **three** call sites and **two** functions.
      `internal/job/job.go` calls `run.ValidateTaskOutputSchemaInstance` (the
      fan-out-aware variant taking a `taskRunID`) in the fanned path and
      `run.ValidateTaskOutputSchema` in the unfanned path;
      `internal/worker/runtime_executor.go` calls
      `run.ValidateTaskOutputSchemaInstance`. Add the evaluator seam beside **all
      three**, and give it the instance-aware signature (it must attribute a
      `DatasetMetric` to a specific `TaskRun`, which a fanned step has N of).
      Decide and document the fan-out semantics in the item: per-partition samples
      are recorded individually; whether an assertion evaluates per-partition or on
      the group aggregate is an **open question** (see Open Questions below).
      Files: new `internal/run/data_assertions.go`, `internal/job/job.go`,
      `internal/worker/runtime_executor.go`.
      Depends on: A1 + A2 + A3.
      **Done (W1-α):** `run.EvaluateDataAssertions(store, runID, taskID,
      taskRunID, samples)` — the instance-aware signature — is called beside all
      three shipped seams: `run.ValidateTaskOutputSchemaInstance` in
      `internal/job/job.go`'s fanned path, `run.ValidateTaskOutputSchema` in its
      unfanned path (which passes `uuid.Nil` and resolves through
      `loadTaskRunByIDOrUnique`, exactly as `SaveSchemaViolations` does), and
      `runtimeExecutor.runSchemaValidation`'s twin `runDataAssertions` in
      `internal/worker/runtime_executor.go`. The samples reach those seams through
      `Markers.Metrics`, which meant widening the local executor's `executeAtom`
      closure by one return value. Phase 0 persists and nothing else — including
      undeclared metrics — and returns `nil` on any persistence problem (an
      observation must not turn a green run red); the `error` return exists for
      Stream B's `fail` disposition, which the executors already escalate.
      Quarantined runs record nothing. Dataset attribution: an explicit `dataset`
      selector wins outright (declared or not — the emitter named it, and the row
      is free history); an omitted selector resolves to the step's SOLE declared
      produced dataset, read off `dataset_declarations`; zero or several
      declarations make it ambiguous, and the sample is dropped with a log line
      naming the ambiguity rather than attributed arbitrarily.
      **Open Question 1 answered — fan-out is per-partition on both halves.**
      Samples are recorded per instance (each fanned `TaskRun` owns its rows), and
      evaluation in Stream B is per-instance too: the post-task pipeline has no
      group-completion seam, and C1's one-active-hold-per-dataset upsert already
      collapses N verdicts into one hold with an occurrence count — the exact
      collapse a group aggregate would need a new seam to achieve. Accepted
      limitation: a group-AGGREGATE assertion is not expressible in v1. Recorded
      in `docs/design-data-circuit-breaker.md` § Open questions as item 5.
- [x] A5. Add the master env gate `CAESIUM_DATA_ASSERTIONS_ENABLED` (default
      `false`) to the `Environment` struct in `pkg/env/env.go`, and surface it as a
      field on the `Features` struct in `api/rest/service/system/system.go` (so
      `GET /system/features` reports it and the UI can hide gated surfaces). Add the
      baseline read API — median + p10/p90 computed on read over the last
      `CAESIUM_BASELINE_WINDOW` (default `20`) clean (non-held, non-quarantined,
      succeeded) `DatasetMetric` rows per metric; no materialized baseline table.
      **Sibling-driven shape requirement (Plan 3 [`backtesting.md`](backtesting.md)
      Stream F item **F3** — "Add the assertion-threshold backtest — pure metric
      replay"; that plan's F2 is the unrelated "Link a backtest to an
      `apply_jobdef_patch` proposal" — depends on this):** the compute-on-read helper in `internal/run/baseline.go`
      must accept an **explicit `asOf` cut** — a run id or timestamp such that only
      samples *preceding* that run/time enter the window — so a backtest can compute
      the baseline "as it would have been at that run" with no future sample leaking
      in. Ship it as the primary signature (e.g.
      `Baseline(ctx, db, ns, name, metric string, window int, asOf time.Time)`),
      with the live path passing "now"; do **not** bolt `asOf` on later. The helper
      must be callable without an executor or a live run.
      *Refreshed 2026-09-05:* `Features` currently carries
      `DatabaseConsoleEnabled`, `LogConsoleEnabled`, `ExternalURL`,
      `AgentRemediationEnabled`, `FreshnessEnabled`, `ContractEnforcementEnabled` —
      append one field, additively.
      Files: `pkg/env/env.go`, `api/rest/service/system/system.go`, new
      `internal/run/baseline.go` (compute-on-read helper).
      Depends on: A2.
      **Done (W1-α):** `CAESIUM_DATA_ASSERTIONS_ENABLED` (default `false`) and
      `CAESIUM_BASELINE_WINDOW` (default `20`) on `Environment`;
      `Features.DataAssertionsEnabled` appended additively and reported by
      `GET /system/features` (asserted through the live endpoint on the
      integration lane, not read back from the env). `internal/run/baseline.go`
      ships `Baseline(ctx, db, ns, name, metric string, window int, asOf
      time.Time)` as the PRIMARY signature — the `asOf` cut is not bolted on —
      returning median + p10/p90 (linearly interpolated, which matters on a
      20-sample window) plus the oldest-first raw values for the console
      sparkline. It takes a `*gorm.DB` rather than a `*Store` and touches no run
      state, so Plan 3's backtest can call it with no executor and no live run.
      **One honest gap:** "clean" today means non-quarantined + succeeded (joined
      on `task_runs`); the design's third predicate, non-HELD, needs
      `DatasetHold`, which Stream C1 introduces — `cleanSampleQuery` is the single
      seam where that filter lands, named in the doc comment so C1 cannot miss it.

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
      `Store.SaveSchemaViolations` in `internal/run/store.go`). Dispatch: `warn` logs
      + persists (like `schemaValidation: warn`); `fail` returns an error the
      executors escalate exactly as schema `fail` mode (red run). `hold` is a no-op
      here — Stream C wires it. Add `CAESIUM_BASELINE_WINDOW`,
      `CAESIUM_BASELINE_MIN_SAMPLES` to `pkg/env/env.go` and
      `caesium_data_assertions_total{result}` to `internal/metrics/metrics.go`
      (both edit sites: the `var (...)` block + `Register()`).
      *Refreshed 2026-09-05 (arc convention 6):* `SaveSchemaViolations` is a method
      on `*Store` in `internal/run/store.go` with signature
      `SaveSchemaViolations(runID, taskRef uuid.UUID, violations []pkgtask.SchemaViolation) error`
      — mirror that shape (`taskRef` is the fan-out-aware task-run reference). `ValidateTaskOutputSchema` is the escalation precedent
      for `fail` mode: it returns a `fmt.Errorf` the executors escalate, and in
      `warn` mode it publishes a dedicated `schema_violation_recorded` event via
      `publishSchemaViolationEvent` so the leader-gated incident subscriber can see
      a non-failing violation. Follow that pattern for warn-mode data violations.
      **Sibling-driven shape requirement (Plan 3 [`backtesting.md`](backtesting.md)
      Stream F item **F3**, the assertion-threshold backtest — not that plan's F2 —
      depends on this):** factor the evaluation core so a **side-effect-free**
      `evaluate(spec, sample, baseline) []Violation` is exported and callable
      **without executors, without opening or releasing holds, and without writing
      `DataViolation` rows**. `EvaluateDataAssertions` becomes the I/O shell (load
      baseline → call `evaluate` → persist → dispatch); `evaluate` is the pure core
      Plan 3 F3 replays over recorded `DatasetMetric` history. Ship it that way in
      B1; do not refactor it into that shape afterwards.
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
      *Refreshed 2026-09-05:* `QueryImpact(ctx, db, namespace, name string, maxDepth int) (*ImpactResult, error)`
      is verified present in `internal/lineage/impact.go`. The conditional-INSERT
      precedent is `insertRunIfSlotTx` in `internal/run/store.go` (used by
      `admit()`), and `internal/incident/store.go` `OpenOrAppend` is the
      idempotent-upsert-with-occurrence-count precedent (atomic conditional insert
      on a unique `active_dedupe_key`, `ON CONFLICT DO NOTHING`) — copy its shape,
      it is exactly the "one active row per key, append occurrences" problem. The
      hold's violation JSON must carry the **baseline snapshot** as well as
      observed/bound, because Stream F's incident bundle serves it to the agent.
      Files: new `internal/models/dataset_hold.go`, `internal/models/models.go`,
      `internal/run/data_assertions.go`, `internal/metrics/metrics.go`.
      Depends on: B1.
- [ ] C2. Add the downstream **run-admission gate** in the run store: inside
      `Store.admit(tx, model, req)` (`internal/run/store.go`), resolve the consuming
      job's declared `consumes` list and query active `DatasetHold` rows **in the
      same transaction** that inserts the run. On a hit the default disposition is
      **skip-with-reason** — the run is created directly in terminal `skipped`
      status with `SkipReason: dataset_hold:<ns>/<name>`, reusing the
      concurrency-skip machinery (the `admissionSkipped` decision + the
      `skipReason` field on `admissionResult`) so run history shows *why* nothing
      ran. Honor the per-job `metadata.onUpstreamHold: skip|run` override (default
      `skip`). **Name the lookup path:** `admit` runs inside a store transaction
      with no jobdef in hand, so `onUpstreamHold` must be a **persisted scalar
      column on `models.Job`**, written when the jobdef is applied, exactly like
      the shipped `Job.SchemaValidation` column (`internal/models/job.go`,
      `SchemaValidation string \`gorm:"type:text;not null;default:''"\`` — the
      precedent for persisting a `metadata.*` scalar and reading it back off the
      job row at runtime). Add `Job.OnUpstreamHold` the same way and read it in
      `admit`; do **not** re-parse the stored jobdef on the admission hot path.
      Emit `run_held_upstream` (non-paging). Every path into a run (cron,
      event/chained triggers, HTTP, manual, queue dequeue) funnels through this
      store, so one decision covers all. Mid-run task starts do NOT re-check holds
      in v1 (documented limitation). Add `caesium_runs_held_upstream_total`.
      *Refreshed 2026-09-05 — IMPORTANT correction (arc convention 6).* The
      machinery is real but does **less** than this item assumes. Verified in
      `internal/run/store.go`: `admit(tx, model, req)` exists; `admissionResult`
      has an `admissionSkipped` decision and a `skipReason` string field; the only
      producer today sets `skipReason = "max_concurrency"`. But a
      concurrency-skipped run **creates no `JobRun` row at all** — `admit` reaches
      `admissionSkipped` precisely *because* `insertRunIfSlotTx` did not insert, the
      caller logs, increments `metrics.RunSkippedTotal`, and returns
      `ErrRunSkipped`. And `models.JobRun` (`internal/models/run.go`) has **no
      `SkipReason` column** (the only `SkipReason` in `internal/models/` is on
      `EventTriggerMatch`). So this item must, additively:
      (a) add a `SkipReason` column to `models.JobRun`;
      (b) add a terminal-`skipped` **insert** path (a hold-skipped run is a row, not
      an absence — the design's "an invisible non-run would be silent-poison's evil
      twin"), leaving the concurrency path's no-row behavior unchanged;
      (c) reuse the existing `admissionSkipped`/`skipReason` plumbing to carry the
      reason to that insert. Confirm `JobRunStatus*` gains/uses a `skipped` value.
      (d) **A hold-skipped run has task rows.** Task rows do not exist at
      admission — for a normal run the local executor registers them after
      start (`internal/job/job.go` calls `store.RegisterTasks(runID,
      []RegisterTaskInput{…})`, one input per catalog task at
      `PartitionIndex: 0`), so the insert path must **create** them itself:
      in the same store transaction as the terminal-`skipped` `JobRun`
      insert, register one row per catalog `Task` of the job (a tx-scoped
      variant of `RegisterTasks` — factor `registerTasksTx` out of it if one
      does not exist — with `OutstandingPredecessors: 0`, `PartitionIndex: 0`,
      the catalog `Task` + `Atom` loaded the way `RegisterTasks` loads them),
      then flip each from `pending` to `skipped` via `markInstanceSkippedWhereTx`
      with the reason `dataset_hold:<ns>/<name> hold=<id>` in the row's `error`
      column (the exact shape trigger-rule skips already have) and emit
      `task_skipped` per row. **Fan-out representation:** a fanned step is
      registered exactly as a fresh run registers it before its producer has
      emitted — one **unexpanded template row** (`PartitionIndex 0`,
      `PartitionCount 0`; see `internal/models/run.go` `TaskRun` and
      `internal/run/fanout.go` `HasFanOutSuccessor`'s "unexpanded, expandable
      template row") — and that template row is what gets skipped: no
      per-partition instances are synthesised, because the producer never ran
      and the partition list never existed. This is the same shape a fanned
      group has today whenever its producer never runs (the group "collapses to
      its single template row"), so the DAG view and `why --task <fanned step>`
      already know how to render it. Run history, the DAG view and the
      task-scoped `why` (F5) therefore all have rows to read; "a run with no
      tasks" is not a shape any reader has to tolerate. (Revised 2026-09-06
      after review, twice: the first wording left the run task-less; the second
      cited an update-only helper for rows that did not yet exist and left the
      fan-out shape undefined.)
      **Depends inline on [`trust-the-substrate.md`](../completed/trust-the-substrate.md) Stream A**
      (the tolerant-rule stranding fix): until a failed plain task's successors
      advance correctly in the SQL lane, "downstream skipped because held" is not
      distinguishable from "downstream never dispatched", and the gate's integration
      assertions are ambiguous.
      Files: `internal/run/store.go`, `internal/models/run.go`,
      `internal/models/job.go` (the `OnUpstreamHold` column),
      `internal/metrics/metrics.go`.
      Depends on: C1 + A3.
- [ ] C3. Add release semantics. **Clean run** — when a producing task finishes
      with all assertions passing, the evaluator releases any active hold on that
      dataset (`release_reason: clean_run`, recording the clearing run) in the same
      transaction that records the metrics; a clean run only releases holds on
      datasets it re-produced. **Honor the per-dataset `release: auto|manual`
      (default `auto`) that A3 declares on `ProducedDataset`** — the design is
      explicit that clean-run release is "configurable per dataset (`release:
      auto|manual`, default `auto`)", so a dataset declared `release: manual`
      **does not** auto-release on a clean run and stays held until a human ack.
      Without this, A3's `release` field ships inert. **Human ack** — `POST /v1/datasets/holds/:id/release`
      with reason + optional per-assertion tolerance windows
      (`--tolerate <assertion>=<dur>`), audited via `AuditLog`
      (`internal/models/audit_log.go`). **Fail-closed:** Caesium defaults to
      `CAESIUM_AUTH_MODE=none` (the `Environment.AuthMode` field in
      `pkg/env/env.go`) which attaches no auth
      middleware, so with no auth mode active the release endpoint is **disabled
      (403 naming the precondition)** and holds release only through the `clean_run`
      path; `ReleasedBy` then always records an authenticated principal. Emit
      `dataset_released`.
      The `/v1/datasets/holds/:id/release` route extends the base dataset REST
      package owned by [`freshness-scheduling.md`](../completed/freshness-scheduling.md) Stream E
      — see Stream D's cross-plan ownership note; this item adds a handler file and
      appends one route line, it does not create the package.
      **Mount it conditionally (arc convention 1 — "off means no routes").** The
      `/datasets/holds/:id/release` route must be registered **inside an
      `if assertionsEnabled()` guard** in `Protected()`, copying the shape of the
      shipped `if contractsvc.Enabled() { g.GET("/contracts/graph", …) }` block in
      `api/rest/bind/bind.go`. Do **not** copy the adjacent unconditional bare
      `{ … }` dataset block (`g.GET("/datasets", dc.List)` and friends) — the arc
      calls that out explicitly as an imperfect precedent not to copy. Assert the
      flag-off absence in `api/rest/bind/bind_test.go`, mirroring the shipped
      `TestProtectedGatesContractGraphRoute` (H-1 enables the flag on every
      self-server lane, so no integration lane can observe the off state).
      *Refreshed 2026-09-05:* the dataset REST package **exists** (verified:
      `api/rest/controller/dataset/dataset.go`, `api/rest/service/dataset/dataset.go`,
      routes `GET /datasets`, `GET /datasets/:ns/:name`,
      `GET /datasets/:ns/:name/derivations`, `POST /datasets/:ns/:name/advance` in
      `api/rest/bind/bind.go` `Protected()`), so this is unconditionally an
      **extend**. `internal/models/audit_log.go` exists. The fail-closed 403 scenario runs on the auth lane (see H-1): assert
      403 under `AUTH_MODE=none` on the default lane **and** success under
      `api-key` on `just integration-test-agent`.
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
      *Refreshed 2026-09-05:* these three event types are what Stream F4/F5 render
      and what F1's non-failure incident entry point keys off — keep their payloads
      rich enough to carry `(namespace, name)`, the hold id, the violated assertion,
      and the observed/bound/baseline triple, so F1 does not need a second read.
      Files: `internal/event/bus.go`, `internal/notification/subscriber.go`.
      Depends on: C1 + C2 + C3 (the three emit sites).

#### History — the former "Deferred: Phase 3 ergonomics & reach" note

> *Original text (2026-07-03), superseded on 2026-09-05 by Stream F and by the
> arc's parking decisions. Kept verbatim for its rationale.*
>
> Recorded as a follow-on, **not** part of this plan's acceptance criteria: the
> `park` run disposition with release-drain (the dequeuer is concurrency-driven
> today and needs its own leader-gated draining wiring); the agent-in-the-loop
> `data_quality_hold` incident class + `release_hold` action
> ([`design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md)); freshness
> integration (a held dataset is not fresh —
> [`design-freshness-scheduling.md`](../../design-freshness-scheduling.md)); and
> backtest evaluation of assertions over historical metrics
> ([`design-backtesting.md`](../../design-backtesting.md)). Draft these against the
> Stream C/D endpoints once this plan completes.

**Disposition as of 2026-09-05:**

- The `data_quality_hold` incident class, the `release_hold` action, the Git-PR
  provenance route, freshness integration, and `why` provenance are **in scope** as
  **Stream F** below.
- The **`park` run disposition with release-drain stays deferred**, parked at the
  arc level (`closed-loop-arc.md` § "Parked, archived, filed": *"`park` run
  disposition with release-drain (circuit-breaker Phase 3 leftover)"*). Its
  rationale is unchanged: the dequeuer is concurrency-driven today and hold-release
  draining needs its own leader-gated wiring. No checkbox here.
- **Backtest evaluation of assertions moved to Plan 3**,
  [`backtesting.md`](backtesting.md) **Stream F item F3** — "Add the
  assertion-threshold backtest — pure metric replay, no re-execution"
  (`caesium backtest assertions` over `DatasetMetric` history with an `asOf`-cut
  baseline). **It is F3, not F2** (verified 2026-09-05: that plan's F1 is
  `backtest_patch`, F2 is "Link a backtest to an `apply_jobdef_patch` proposal",
  F3 is the assertion backtest). This plan does **not** carry a checkbox for it; it
  ships the two shapes F3 calls (B1's pure `evaluate(...)`, A5's `asOf` baseline).

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
subcommands — rather than creating a second package or Cobra group.

> *Superseded coordination text (kept for rationale; the race is decided by the
> paragraph below).* "If freshness Stream E has not merged yet, whichever
> dataset-surface item lands first creates the package skeleton + the
> `cmds`-slice / `Protected()` registration under these canonical paths; the
> other extends it. The two plans' `bind.go` / `cmd/execute.go` dataset edits must
> not land in the same wave (see Sequencing)."

*Resolved 2026-09-05:* freshness Stream E shipped (#290). `cmd/dataset/dataset.go`
registers `listCmd, statusCmd, advanceCmd` on an existing `Cmd` group already in
`cmd/execute.go`'s `cmds` slice; the REST package and routes exist. **Both D items
are unconditionally EXTEND**, and no `cmd/execute.go` edit is needed for the group
itself. `GET /v1/datasets` already exists (`api/rest/service/dataset/dataset.go`
`List`, returning `ListResult` with SLO/producing-job detail) — D1 **adds hold
status to that existing response** rather than creating the route.

- [ ] D1. Add the dataset read endpoints. `GET /v1/datasets` **already exists**
      (`api/rest/service/dataset/dataset.go` `List`, returning `ListResult` with
      SLO/producing-job detail) — this item **extends** `List`/`Detail` with hold
      status (and, for F4, with the held⇒not-fresh status) rather than creating the
      route. The two genuinely new routes are
      `GET /v1/datasets/holds?status=active` and
      `GET /v1/datasets/:ns/:name/metrics` (baseline series), appended in
      `Protected()` (`api/rest/bind/bind.go`).
      **Mount both new routes conditionally (arc convention 1 — "off means no
      routes"): inside an `if assertionsEnabled()` guard**, copying the shipped
      `if contractsvc.Enabled() { … }` block in `api/rest/bind/bind.go` — **not**
      the adjacent unconditional bare `{ … }` dataset block, which the arc names as
      an imperfect precedent not to copy. Assert the flag-off absence of
      `/v1/datasets/holds` and `/v1/datasets/:ns/:name/metrics` in
      `api/rest/bind/bind_test.go` (mirroring the shipped
      `TestProtectedGatesContractGraphRoute`), and assert that `GET /v1/datasets`
      — the pre-existing, unconditionally-mounted route — keeps working. The
      hold-status *field* added to
      `List`/`Detail` is likewise empty/absent when the flag is off.
      Watch the route-shape collision:
      `/datasets/:ns/:name` is already registered, so `/datasets/holds` must be
      declared such that the router does not treat `holds` as an `:ns` — verify
      with a live-server scenario, not by reading the mux.
      Files: `api/rest/controller/dataset/`, `api/rest/service/dataset/`,
      `api/rest/bind/bind.go`.
      Depends on: A2 (models) + A5 (baseline read) + C1 (`DatasetHold`).
- [ ] D2. Add the circuit-breaker subcommands to the `caesium dataset` CLI group —
      `holds [--status active] [--json]`,
      `release <ns>/<name> [--reason …] [--tolerate <assertion>=<dur>]`,
      `metrics <ns>/<name> --metric rowCount [--json]` — **extending** the existing
      `cmd/dataset/` group owned by freshness Stream E (which contributes
      `list`/`status`/`advance`). The group exists and is already in
      `cmd/execute.go`'s `cmds` slice, so this item adds three subcommand files and
      three `Cmd.AddCommand(...)` arguments — **no `cmd/execute.go` edit**.
      **`release` takes a dataset, the endpoint takes a hold id:** C3's route is
      `POST /v1/datasets/holds/:id/release`, so `release <ns>/<name>` must first
      resolve the dataset's **active** hold via
      `GET /v1/datasets/holds?status=active` (D1) and then call the release route
      with that id; when no active hold exists it must fail with a clear,
      non-zero-exit message naming the dataset (never a bare 404 body).
      `--json` goes to **stdout, clean and parseable** via `cmd.OutOrStdout()`
      (asserted in tests via `runCLIStdout`, never the stream-merging capture); a
      timed-out HTTP client + bearer API-key header like the shipped
      `cmd/event`/`cmd/trigger` groups.
      *Refreshed 2026-09-05:* reuse the shipped `cmd/dataset/http.go`
      client rather than adding a fourth HTTP helper. `runCLIStdout` is
      `(*IntegrationTestSuite).runCLIStdout` in `test/data_plane_e2e_test.go`.
      Files: `cmd/dataset/` (**extend**).
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

### Stream F — Agent & freshness integration (closes the loop)

The former Phase 3, pulled into scope by [`closed-loop-arc.md`](closed-loop-arc.md)
(§ Synergies, row "Circuit-breaker Phase 3 was deferred"). Without F, the data loop
observes and holds but never *acts durably*: a hold sits waiting for a human to
notice it in a UI. With F, a hold is an incident, an incident yields a proposal, a
proposal is approved, and the approval either releases the hold or opens a PR
against the producer's job definition — and every one of those steps is
`why`-explainable.

**Cross-plan dependency, stated once and repeated per item.** F depends on
[`trust-the-substrate.md`](../completed/trust-the-substrate.md) **C4** (proposal →
`ApprovalRequest` + `awaiting_approval`) and **C7** (approve → execute, plus the
*direct* `apply_jobdef_patch` route) and on its **H-1** (the de-hollowed, widened
auth lane). Verified in the current tree (2026-09-05), that pipeline is **not
wired**: `internal/incident/executor.go` `Execute` records a tier-3 action as
`proposed` with the comment *"the tier-3 approval flow (Stream D / B4) creates the
ApprovalRequest and resolves it. No execution here"* and nothing creates the
`ApprovalRequest`; `api/rest/service/incident/approvals.go` `Service.decide` flips
the `AgentAction` to `approved` and advances the incident, but nothing executes an
approved action; `internal/incident/actions.go` `dispatch` has **no case** for
`ActionTypeApplyJobdefPatch` / `ActionTypeSkipTask` / `ActionTypeOverrideSchemaGate`
(they fall to the defensive `default` that returns `ErrUnknownAction`); and
`internal/incident/provenance.go` does not exist. F builds the **Git-PR route
only** — Plan 0 builds the pipeline and the direct route. Per arc convention 4,
**no second apply path and no second approval surface.** Plans 2-E and 3-F reuse
this route and add nothing to it.

Also verified for grounding: `internal/incident/subscriber.go`
`classifierFailureTypes` is `{task_failed, run_failed, run_timed_out, sla_missed,
schema_violation_recorded}` and `handleFailure` is the **only** path to
`Store.OpenOrAppend` — every incident today originates in a *failure* event. F1 is
therefore a genuinely **new, non-failure entry point**, and the design's own
precedent for it is warn-mode schema validation, which publishes
`schema_violation_recorded` from `internal/run/schema_validation.go`
(`publishSchemaViolationEvent`) precisely because the task did not fail.

- [ ] F1. Open a `data_quality_hold` incident when a hold opens. (a) Add
      `ClassDataQualityHold FailureClass = "data_quality_hold"` to
      `internal/incident/classifier.go` (the const block beside `ClassOOM` /
      `ClassSchemaViolation`) and a `Classify` branch: the `dataset_held` bus event
      type maps to it, in the event-type switch at the top of `Classify` (the same
      precedence tier as `TypeSchemaViolationRecorded`), so the mapping is
      deterministic and needs no log-tail heuristic. (b) Add `event.TypeDatasetHeld`
      (C4's `dataset_held`) to `classifierFailureTypes` in
      `internal/incident/subscriber.go`, and give `handleFailure` a hold branch (or
      a sibling `handleHold`) that resolves the **producing** job/run/task from the
      event rather than a failed `TaskRun` — the task **succeeded**, so
      `resolveContext`'s "first FAILED instance" attribution does not apply and must
      not be reused blindly. Route through `Store.OpenOrAppend` so dedupe,
      cooldown, occurrence-append, and the frozen `AllowedJobs` allowlist all
      behave as they do for failures (one incident per hold, repeat occurrences
      appended). (c) Carry the evidence: the `Evidence` JSON must include the
      violated assertion, observed vs bound, the **baseline snapshot** C1 records on
      the hold, the hold id, `(namespace, name)`, and the **impact cone** from
      `internal/lineage/impact.go` `QueryImpact` (C1 already attaches it to the
      hold — reuse, do not re-query). Do **not** widen the incident's frozen
      allowlist beyond what `FreezeAllowlist` computes; the cone is evidence, not
      authorization. (d) Only when the master gate and
      `CAESIUM_AGENT_REMEDIATION_ENABLED` are both on.
      Files: `internal/incident/classifier.go`, `internal/incident/subscriber.go`,
      `internal/incident/store.go` (if `OpenParams` needs a non-failure shape),
      `test/` (auth-lane scenario). **Cross-plan blocker (documented inline, not in
      `Depends on:` per the draft-exec-plan PLAYBOOK):** the auth-lane scenario is
      not runnable until [`trust-the-substrate.md`](../completed/trust-the-substrate.md) **H-1**
      makes that lane real — see `## Sequencing & Dependencies` § Cross-PLAN order.
      Depends on: C1 + C4.
- [ ] F2. Add `release_hold` to the action catalog and make it executable through
      the approval pipeline. (a) **Catalog:** `ActionTypeReleaseHold =
      "release_hold"` in `internal/incident/actions.go`, registered in
      `actionCatalog`. Tier per the design (*"`release_hold` joins the action
      catalog at tier 2 (tier 3 with tolerance windows)"*): register it
      `TierGated`, and have the dispatch path **escalate to the approval gate when
      the params carry tolerance windows** — a tolerance window snoozes an assertion
      and is a durable policy change, so it must terminate at a human. If the
      executor's tier is a static map lookup and cannot express "tier depends on
      params", say so in the PR and register it `TierApproval` outright rather than
      inventing a second gating mechanism (arc convention 4). (b) **Params:**
      extend `ActionParams` with the hold identity (`HoldID *uuid.UUID`, or
      `Namespace`/`Name`), `Reason`, and `Tolerate map[string]string` (assertion →
      duration). (c) **Boundary:** `verifyActionBoundary` currently checks
      `params.JobID`/`params.RunID` against the incident's job — extend it so a
      named hold must belong to a dataset **produced by the incident's job**, or
      the action is refused with `ErrCrossBoundaryTarget`. This is the security
      boundary; an agent must not release an unrelated team's hold. (d)
      **Dispatch:** add a `case ActionTypeReleaseHold` in `Executor.dispatch` and a
      `ReleaseHold(ctx, holdID uuid.UUID, reason string, tolerate map[string]time.Duration) error`
      method on the `ActionOps` interface, implemented by the concrete adapter
      wired in `cmd/start/start.go` onto C3's release service (**one** release
      path — the adapter calls the same service the REST endpoint calls, so the
      `AuditLog` entry and the fail-closed posture are identical). (e) **Agent
      reach:** `release_hold` is proposable through the MCP `propose_action` tool
      (`internal/mcp/tools.go` `AgentTools`) with no tool-schema change — the tool
      takes a free-form `type` + `params` object — so verify end-to-end rather than
      editing the schema. (f) **Bundle/context enrichment:** extend
      `internal/incident/bundle.go` `Bundle` with a `DataQuality` section (violated
      assertion, observed, bound, baseline snapshot + the last-N metric series,
      hold id + occurrence count, the impact cone) populated in `BuildBundle` when
      the incident class is `data_quality_hold`; and extend the `get_context` tool's
      `kind` enum handling so the agent can fetch the metric series. Keep the
      existing `BundleFailure` semantics intact — a hold incident has no failed
      `TaskRun`, so `BuildBundle`'s `bundleAttributionRow` path must degrade
      gracefully rather than mis-describe a succeeded instance as "the failure".
      Files: `internal/incident/actions.go`, `internal/incident/executor.go`,
      `internal/incident/bundle.go`, `internal/mcp/tools.go` (verify; edit only if
      `get_context` needs a new `kind`), `api/rest/controller/agent/` (context
      handler), `cmd/start/start.go` (ops adapter), `test/` (auth-lane scenario).
      **Cross-plan blockers (documented inline, not in `Depends on:` per the
      draft-exec-plan PLAYBOOK):** [`trust-the-substrate.md`](../completed/trust-the-substrate.md)
      **C4 + C7** (the proposal → `ApprovalRequest` → approve → execute pipeline,
      unwired today — see the stream preamble) and its **H-1** (the real auth lane
      the scenario runs on). See `## Sequencing & Dependencies` § Cross-PLAN order.
      Depends on: F1 + C3.
- [ ] F3. Build the **Git-PR provenance route** of `apply_jobdef_patch` — B4's
      unbuilt half — so a producer patch proposed from a hold lands as a pull
      request instead of a silent server-side edit. (a) **Credentials:** add a new
      `CAESIUM_GIT_WRITE_CREDENTIALS` field to the `Environment` struct in
      `pkg/env/env.go`. Shape it after the shipped `CAESIUM_JOBDEF_GIT_SOURCES`
      precedent (`pkg/env/jobdef.go` `GitSources` / `GitSourceConfig` /
      `GitBasicAuth`, a JSON-decoded `envconfig.Decoder`): a JSON array keyed by
      repo URL or `source_id`, each entry carrying a forge kind, an API base URL,
      and a token (direct or `secret://` ref, matching `GitBasicAuth`'s
      `PasswordRef` pattern). Read-only sync credentials are deliberately **not**
      reused — write access is a separate grant. (b) **Router:** create
      `internal/incident/provenance.go` (the file `agent-in-the-loop-remediation.md`
      B4 cites and which does not exist). Given a job and a rendered jobdef patch,
      it decides the route from `models.Job`'s git provenance fields —
      `ProvenanceSourceID`, `ProvenanceRepo`, `ProvenanceRef`, `ProvenanceCommit`,
      `ProvenancePath` (verified present on `internal/models/job.go`): non-empty
      `ProvenanceRepo` ⇒ **Git-PR route**; empty ⇒ the *direct* `jobdefs diff/apply`
      route Plan 0 C7 builds. (c) **PR:** a small forge client (**GitHub first**;
      the interface admits others) that branches from `ProvenanceRef`, writes the
      rendered manifest at `ProvenancePath`, and opens a PR whose body carries the
      incident id, the violated assertion, the observed/bound/baseline triple, and
      the rendered diff. Use `internal/jobdef/git` only for what it already does
      (clone/fetch/auth via go-git `httpauth.BasicAuth`/`sshauth`); PR creation is a
      forge **API** call, not a git operation — do not extend `git_sync.go`. (d)
      **Degrade honestly:** with no matching write credential, the action does not
      half-succeed — it degrades to `escalate` carrying the rendered diff, exactly
      as arc convention 4 specifies, and the `AgentAction` result records why. (e)
      Refused under `CAESIUM_AUTH_MODE=none` like every other tier-3 action.
      **Plans 2-E and 3-F reuse this route and add nothing to it.** Verified
      2026-09-05: [`resource-right-sizing.md`](resource-right-sizing.md) Stream E
      **has already been recast** — its *Design history* subsection records "**Why
      the design's `POST /v1/jobs/:id/resources/apply` provenance router is NOT
      built**… **So the endpoint is dropped**, not front-ended", it lists the
      deviation under its own Convention 4 note, and its acceptance criteria assert
      that `POST /v1/jobs/:id/resources/apply` **does not exist**. Plan 2 consumes
      this route and adds nothing; no re-pointing is needed.
      **(f) Design amendment first:** `CAESIUM_GIT_WRITE_CREDENTIALS` is a config
      knob the design of record does not enumerate — **N-2 must land before this
      item** (see the Source-Of-Truth Note's recorded deviation).
      Files: `pkg/env/env.go` (+ a `pkg/env/` decoder file beside `jobdef.go`),
      new `internal/incident/provenance.go`, new forge client package (e.g.
      `internal/incident/forge/`), `internal/incident/actions.go` (the
      `apply_jobdef_patch` dispatch case routes through the provenance router),
      `cmd/start/start.go` (wiring), `test/` (auth-lane scenario asserting the
      no-credential degrade-to-escalate path; the live-PR path is unit-tested
      against a fake forge — see Open Questions).
      **Cross-plan blocker (documented inline, not in `Depends on:` per the
      draft-exec-plan PLAYBOOK):** [`trust-the-substrate.md`](../completed/trust-the-substrate.md)
      **C4 + C7** — the pipeline and the *direct* `apply_jobdef_patch` route this
      one branches beside. See `## Sequencing & Dependencies` § Cross-PLAN order.
      Depends on: F2 + N-2.
- [ ] F4. Make **held ⇒ not fresh**. A poisoned-but-recent partition must never
      satisfy a freshness trigger or advance downstream scheduling. In
      `internal/freshness/evaluator.go`, `statusFor(...)` computes the dataset's
      status from state + SLO + upstream readiness; add an active-`DatasetHold`
      check that overrides the computed status (and its reason string) with a held
      disposition. Prefer reusing an existing terminal-ish value from
      `internal/models/dataset_state.go` (`DatasetStatusUnknown`, `Fresh`, `Stale`,
      `StaleUpstream`, `Violated`, `Quarantined`) over adding a new one — the design
      forbids overloading `Quarantined` (which means "non-authoritative replay
      run"), so if none of the existing values fits, add `DatasetStatusHeld` and say
      so explicitly in the PR. The reason string must name the hold
      (`dataset_hold:<ns>/<name>`). Reflect it in `GET /v1/datasets` (D1 already
      adds hold status to `api/rest/service/dataset/dataset.go` `List`/`Detail` —
      this item makes the *freshness status field itself* honest, not just an
      adjacent badge) and make sure `Evaluator.evaluateProducedDataset`'s
      derivation path does not fire a freshness-triggered run off a held dataset.
      Files: `internal/freshness/evaluator.go`, `internal/models/dataset_state.go`
      (only if a new status value is unavoidable), `api/rest/service/dataset/`,
      `test/` (a scenario asserting a held dataset reports not-fresh and does not
      derive).
      Depends on: C1 + D1.
- [ ] F5. **Why-provenance** (arc convention 3 — this plan's single explainability
      item). Make the hold decisions answerable by the shipped **task-scoped**
      explainer: `caesium why <run-id> --task <task> --job-id <job-id>` and
      `GET /v1/jobs/:id/runs/:run_id/why`. (a) Persist enough provenance on the
      `run_held_upstream` skip (C2) and the `dataset_held`/`dataset_released`
      events (C4) that the explainer can name the dataset, the violated assertion,
      the observed-vs-bound values, and the hold id. (b) Surface it: extend
      `internal/run/why.go` — `WhyTrigger` (populated by `loadTrigger`, which reads
      the run row and the `run_started` `ExecutionEvent`) is the natural carrier for
      "this run was admitted/skipped because dataset X is held", and the
      `WhyExplanation.Summary` line must say so. Render it in `cmd/why/why.go`
      `renderTable` (and keep it in the `--json` shape). **Do not add a run-level
      `why` verb** — the arc is explicit that today's explainer is task-scoped and
      no plan adds one. **The explanation surface is (decided 2026-09-06):** C2(d)
      gives a hold-skipped run a `skipped` `TaskRun` per catalog task, so
      `caesium why <run-id> --task <any step> --job-id <job-id>` resolves a task
      exactly as it does today; this item adds the missing verdict —
      `VerdictSkipped` (`SKIPPED`) beside `CACHE_HIT`/`CACHE_MISS`/`CACHE_DISABLED`/
      `UNKNOWN` in `internal/run/why.go` — populated when the instance row's
      status is `skipped`, carrying the row's `error` reason (which today's
      trigger-rule skips also benefit from: "SKIPPED — trigger rule
      `all_success` not satisfied"), and `WhyTrigger` carries the run-level
      hold provenance so the `Summary` line reads
      `SKIPPED — dataset <ns>/<name> held since run <id> (assertion <name>:
      observed <v> vs bound <b>; hold <hold-id>)`. Render both in `renderTable`
      and in the API's JSON. (c) One integration scenario asserts that exact
      `why` output for a hold-skipped run, capturing **stdout separately** via
      `runCLIStdout` (never the stream-merging `runCLIRaw`).
      Files: `internal/run/why.go`, `cmd/why/why.go`,
      `api/rest/controller/why/why.go`, `test/`.
      Depends on: C2 + C4.

## Harness Strengthening

- [x] H-1. Ensure the integration server exercises the real assertion path: set
      `CAESIUM_DATA_ASSERTIONS_ENABLED=true` on the `just integration-up` /
      `just integration-test` server (mirror the lineage
      `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the `CLAUDE.md` gate calls out), pass
      the same env through CI, and add a small **metrics-emitting script image**
      (echoes `##caesium::metrics`) the Stream A/B/C scenarios drive so they hit the
      live surface rather than an internal call. Add a low
      `CAESIUM_BASELINE_MIN_SAMPLES` / short `CAESIUM_DATASET_METRIC_RETENTION` if a
      scenario needs a tight window.
      *Refreshed 2026-09-05 — per arc convention 2, the flag goes on the default
      lane AND every lane that runs its own server*, because a lane that starts its
      own server silently goes red when a flag lands only on `integration-up`.
      Verified recipes in the `justfile` (each has its own `-e CAESIUM_...` block;
      the `CAESIUM_FRESHNESS_ENABLED=true` line is the exact precedent to copy in
      each): `integration-up` (default, used by `integration-test`),
      `integration-up-distributed` (`integration-test-distributed`),
      `integration-up-owner-memory` (`integration-test-owner-memory`),
      `integration-up-agent` (`integration-test-agent`), `integration-up-infra`
      (`integration-test-infra`), `integration-test-podman` (inline server), and —
      **critically for Stream E** — the two Playwright lanes `ui-e2e` and
      `ui-e2e-auth`, which each `container run -d --name caesium-server …` with
      their own `-e CAESIUM_...` block already carrying `CAESIUM_FRESHNESS_ENABLED=true`.
      Without the flag on those two, the hold routes and holds do not exist and
      **acceptance criterion 5 (the hold badge / release panel / sparkline e2e)
      can never pass**. Their CI counterparts (`.github/workflows/ci.yml` jobs
      `ui-e2e` and `ui-e2e-auth`) start their own inline server blocks too — set
      the flag in both places, not just the `justfile`.
      The helm/kind lane's env lives in
      `helm/caesium/ci/test-values-k8s.yaml` (which already carries
      `CAESIUM_FRESHNESS_ENABLED`), consumed by the `helm-integration-test` job.
      The CI jobs that must stay green are `build-and-integration-test`,
      `build-and-integration-test-arm64`, `-distributed`, `-owner-memory`,
      `-agent-auth`, `-infra`, `-infra-arm64`, `podman-integration-test`,
      `helm-integration-test`, `ui-e2e`, and `ui-e2e-auth` in
      `.github/workflows/ci.yml`.
      **Auth-gated scenarios run on the auth lane** (`just integration-test-agent`,
      whose `integration-up-agent` recipe already sets `CAESIUM_AUTH_MODE=api-key`
      and `CAESIUM_AGENT_REMEDIATION_ENABLED=true`): C3's hold-release contrast
      (403 under `AUTH_MODE=none` on the default lane vs success under `api-key`
      here) and every Stream F approval scenario (`data_quality_hold` opens,
      `release_hold` proposes → `awaiting_approval` → `caesium incident approve` →
      the hold actually releases). **That lane runs in LOCAL execution mode**, so
      design these scenarios around cache-served / dry-run paths and never around a
      re-executing quarantined replay. **Depends inline on
      [`trust-the-substrate.md`](../completed/trust-the-substrate.md) H-1**, which first makes
      that lane real (today its test runner never receives `CAESIUM_AUTH_MODE`, so
      both scenarios skip and the recipe passes in 0.068s) and widens its `-run`
      pattern to include `TestHold*`/`TestIncident*` with a minimum `--- PASS`
      count; and on **Plan 0 C4/C7** for the approval pipeline those scenarios
      drive.
      Files: `justfile` (**eight** recipes — the six above plus `ui-e2e` and
      `ui-e2e-auth`), `helm/caesium/ci/test-values-k8s.yaml`,
      `.github/workflows/ci.yml`, `test/` harness helpers, `build/` (or `test/`)
      metrics-emitting fixture image.
      **Done (W1-β):** `-e CAESIUM_DATA_ASSERTIONS_ENABLED=true` added next to
      `CAESIUM_FRESHNESS_ENABLED=true` in all eight `justfile` server blocks
      (`integration-up`, `integration-up-distributed`,
      `integration-up-owner-memory`, `integration-up-agent`,
      `integration-up-infra`, `integration-test-podman`, `ui-e2e`,
      `ui-e2e-auth`), the three matching inline server blocks in
      `.github/workflows/ci.yml` (`ui-e2e`, `ui-e2e-auth`,
      `podman-integration-test`), and `helm/caesium/ci/test-values-k8s.yaml`.
      Re-grepped `CAESIUM_FRESHNESS_ENABLED` after the edit — zero misses. The
      justfile's local-dev-only `k8s-distributed` recipe (positional
      `--set config.extraEnv[N]`, not part of the CI job matrix or any
      `test/` scenario) was deliberately left untouched — out of this item's
      scope and out of `docs/ci.md` §5's lane list. New
      `test/data_assertions_lane_test.go` adds `requireDataAssertionsLane()`
      (gates on live `GET /v1/system/features` `data_assertions_enabled`,
      mirroring `requireAuthLane`'s pattern but keying off the server's own
      report rather than a runner-side marker env, since H-1 enables the flag
      on every lane rather than one dedicated lane) plus a self-test. New
      `test/metrics_fixture_test.go` adds the metrics-emitting fixture:
      `metricsProducerStep`/`metricsProducerStepForDataset` return a `steps:`
      YAML entry on the canonical `alpine:3.23` image whose command echoes
      `##caesium::metrics` marker lines, for Stream A/B/C scenarios to embed.
      Pre-existing env drift on `integration-up-agent`/`integration-up-distributed`/
      `integration-up-owner-memory`/`podman-integration-test` is tracked by
      [issue #425](https://github.com/caesium-cloud/caesium/issues/425) and
      intentionally not touched here. Did not add a low
      `CAESIUM_BASELINE_MIN_SAMPLES` / short `CAESIUM_DATASET_METRIC_RETENTION`
      — no scenario needs a tight window yet; Stream A/B/C add one if/when a
      scenario requires it.

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
      *Refreshed 2026-09-05, per arc convention 7 — three additions and one
      correction:*
      (a) **`docs/job-schema-reference.md` is GENERATED** from
      `internal/jobdef/report` and pinned by `TestGeneratedSchemaReferenceIsCurrent`
      — **update the generator (`internal/jobdef/report/report.go`) and regenerate**;
      never hand-edit the doc.
      (b) Add **`docs/tour-data-loop.md`** — the 10-minute end-to-end walkthrough a
      newcomer follows on their own machine to see the loop close: declare a dataset
      with an assertion → emit a bad metric → watch the hold open and the downstream
      run skip with reason → see the `data_quality_hold` incident and its bundle →
      approve a `release_hold` → watch the gate reopen, with `caesium why` naming
      the hold at each step. Index it in `docs/README.md` (backtick form for the
      exec-plan path; a top-level `docs/*.md` may be a link).
      (c) **Tick the arc dashboard row** for "1 data-circuit-breaker" in
      [`closed-loop-arc.md`](closed-loop-arc.md) (waves shipped, last PR) in this
      same PR, and move this plan to `docs/exec-plans/completed/` repointing the
      arc's links — the arc dashboard is the single place status lives.
      (d) Any example manifest must use a **canonically pinned image**
      (`alpine:3.23`, `busybox:1.36.1`, or a `caesiumcloud/...` image) — a guardrail
      test scans `.md` files under `docs/` for unpinned base-image refs (the canonical pins are `alpine:3.23` and `busybox:1.36.1`).
      Files: `docs/roadmap.md`, `docs/design-data-circuit-breaker.md`,
      `internal/jobdef/report/` (generator) + the regenerated
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, new
      `docs/tour-data-loop.md`, `docs/README.md`,
      `docs/exec-plans/active/closed-loop-arc.md` (dashboard row only).
      Depends on: A5 + B1 + C4 + D2 + E2 + F5 (every stream's last item — this
      item runs last, in its own trailing wave).
- [x] N-2. **Amend the design docs for `CAESIUM_GIT_WRITE_CREDENTIALS` and the
      Git-PR provenance route, before F3 lands.** The Source-Of-Truth Note forbids
      adding a config knob the design does not enumerate without amending the
      design first, and
      [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md)
      § "Events, notifications, REST, env" today enumerates exactly
      `CAESIUM_DATA_ASSERTIONS_ENABLED`, `CAESIUM_BASELINE_WINDOW`,
      `CAESIUM_BASELINE_MIN_SAMPLES`, `CAESIUM_DATASET_METRIC_RETENTION` (verified
      2026-09-05). Add `CAESIUM_GIT_WRITE_CREDENTIALS` to that env list with its
      JSON shape (modelled on `CAESIUM_JOBDEF_GIT_SOURCES`), state that write
      credentials are a **separate grant** from read-only sync credentials, and
      state the degrade-to-`escalate` behavior when no entry matches. Mirror the
      same amendment into
      [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md)
      where the `apply_jobdef_patch` action and its provenance routes are
      described, so the Git-PR route stops being an undocumented half of B4. This
      is the pattern [`backtesting.md`](backtesting.md) uses for its own divergence
      ("this divergence is only legal once the design says so: **N-2 amends
      `docs/design-backtesting.md` first**"). Docs-only, no code.
      Files: `docs/design-data-circuit-breaker.md`,
      `docs/design-agent-in-the-loop.md`.
      Depends on: (none — may land any time before F3).
      **Done:** amended `docs/design-data-circuit-breaker.md` § "Events,
      notifications, REST, env" (fifth env knob `CAESIUM_GIT_WRITE_CREDENTIALS`,
      JSON shape modelled on `CAESIUM_JOBDEF_GIT_SOURCES`, separate-grant and
      degrade-to-`escalate` semantics) and `docs/design-agent-in-the-loop.md`
      where `apply_jobdef_patch`'s provenance routing is described (documented
      both the shipped direct route — trust-the-substrate C7,
      `ErrPatchAltersRemediation` — and the unshipped Git-PR route — Plan 1 F3,
      `internal/incident/provenance.go`), each under a dated "Amended
      2026-09-07 (Plan 1 N-2)" marker. F3 may now land.

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — the marker, models, jobdef schema, metrics
  persistence, and master gate. B (evaluator), C (breaker), D (surface), E
  (UI), and F (agent/freshness) all consume it. A merges first (largest blast
  radius).
- **Stream B** depends on A4 (the evaluator seam) + A5 (baseline read); it fills in
  the `internal/run/data_assertions.go` logic without re-touching the executors.
- **Stream C** depends on B1 (adds the `hold` disposition to the evaluator);
  C2 additionally depends on A3 (`consumes`), so C sequences C1 → (C2, C3) → C4.
  C4 depends on C1+C2+C3 (its three emit sites).
- **Stream D** depends on A2/A5/C1 (reads) and C3 (the release endpoint the CLI
  drives) — runs after C.
- **Stream E** depends on D1 (the reads it renders) + C3; runs after D.
- **Stream F runs after C and D** — F1/F2 need C1+C4 (the hold and its events) and
  C3's release service; F4 needs D1's dataset read shape; F5 needs C2+C4. F is the
  last runtime stream. **F1/F2/F3 all edit
  `internal/incident/{classifier,actions,executor,bundle}.go` and/or
  `cmd/start/start.go`, which the file-conflict list below and the arc's table both
  mark a true conflict — so they get one wave each.** F4 (`internal/freshness/`)
  and F5 (`internal/run/why.go`, `cmd/why/why.go`) are file-disjoint from all three
  and from each other, so they ride along.
- **H-1** is independent (justfile/CI/harness) and supports the A/B/C integration
  scenarios; land it in the first wave so the substrate's end-to-end gate has a
  live, enabled surface to drive. Its auth-lane half is only *exercisable* once
  Plan 0 H-1 has landed.
- **N-1** runs last, **in its own trailing wave after A–F have all shipped**, so
  the roadmap/schema/design docs and the arc dashboard reflect reality — the arc
  dashboard's "waves shipped / last PR" row cannot be filled while Stream F is
  still in flight. It never shares a wave with a Stream-F item.
- **N-2** is docs-only and unblocked from day one, but **must merge before F3**
  (the Source-Of-Truth Note's recorded deviation). Land it in any wave up to and
  including the one before F3's.

**Cross-PLAN order.** [`closed-loop-arc.md`](closed-loop-arc.md) is authoritative
here; the ordering it fixes and this plan honors:

- **Plan 0 → Plan 1.** C3's 403-under-`AUTH_MODE=none` scenario and all of Stream F
  need [`trust-the-substrate.md`](../completed/trust-the-substrate.md) **H-1** (real, widened
  auth lane) and **C4/C7** (proposal → `ApprovalRequest` → approve → execute, plus
  the direct `apply_jobdef_patch` route). F3 builds the **Git-PR** route on top of
  C7's direct route; it does not rebuild the pipeline.
- **Plan 0 Stream A → Plan 1 C2.** The default-mode tolerant-rule stranding fix
  must land first so "downstream skipped because held" is distinguishable from
  "downstream never dispatched".
- **Plan 1 → Plan 2 → Plan 3 on `internal/incident/`.** Plan 2's Stream E
  (`propose_resources`) and Plan 3's Stream F (`backtest_patch`, assertion
  backtests) both extend `internal/incident/{classifier,actions,executor}.go` and
  `cmd/start/start.go`. Sequential by plan — **never in the same wave**.
- **Plan 1 → Plan 3 F3 (shape contract).** A5 ships the `asOf`-cut baseline helper
  and B1 ships the pure `evaluate(spec, sample, baseline) []Violation`;
  [`backtesting.md`](backtesting.md) Stream F item **F3** ("Add the
  assertion-threshold backtest — pure metric replay") calls both, and that plan's
  own preamble records "**F3** needs Plan 1 A2 … A5". Ship them in that
  shape in the first place.
- **Plan 2 Stream A may overlap Plan 1's D/E waves** (disjoint files) but must
  **never share a wave with Plan 1 Stream A** — both edit `internal/job/job.go` and
  `internal/worker/runtime_executor.go`. **Plan 1's Stream F is explicitly NOT in
  that allowance:** F3 adds a field to `pkg/env/env.go` and wires
  `cmd/start/start.go`, both of which the arc's conflict table marks "additive; one
  plan's item per wave", and F1/F2/F3 sit on the `internal/incident/` +
  `cmd/start/start.go` row the arc marks "sequential by plan". No Plan 2 item shares
  a wave with a Plan 1 Stream-F item.

> *Superseded (2026-09-05) — the two cross-plan coordination blocks the original
> plan carried, kept for rationale.* **Shared `DatasetDeclaration` registry:** the
> base declared-registry table + jobdef `produces` scaffold is shared with
> [`freshness-scheduling`](../completed/freshness-scheduling.md) Stream A; whichever
> plan's Stream-A registry item merged first **creates** the registry model, the
> second **extends** it, and the two must not be in flight in the same wave.
> **Shared `cmd/dataset/` group + `/v1/datasets` package:** this plan's C3/D1/D2 add
> handlers/subcommands/routes to the same dataset REST package + Cobra group that
> freshness **Stream E** owns; whichever dataset-surface item merged first
> **creates** the skeleton + registration under the canonical paths, the other
> **extends** it, and this plan's `bind.go` / `cmd/execute.go` dataset edits must
> never land in the same wave as a freshness Stream-E edit. **Both races are
> decided:** freshness shipped (#277–#299), so every such item here is an EXTEND
> and no coordination remains.

**Suggested waves:**
- **W1 = A (A1 → A2 → A3 → A4 → A5) + H-1.** A is one near-strict chain (marker,
  then models, then schema, then persistence seam, then gate/baseline).
- **W2 = B.** Unblocked once A's seam + baseline read are in.
- **W3 = C (C1 → (C2, C3) → C4).** The breaker on top of B's evaluator.
- **W4 = D (D1 → D2) + E (E1 → E2) + N-2.** N-2 is docs-only and disjoint; it must
  precede F3.
- **W5 = F1 + F4.** File-disjoint (`internal/incident/{classifier,subscriber,store}.go`
  vs `internal/freshness/evaluator.go` + `internal/models/dataset_state.go` +
  `api/rest/service/dataset/`).
- **W6 = F2.** Alone — it owns `internal/incident/actions.go`,
  `internal/incident/executor.go`, `internal/incident/bundle.go` and
  `cmd/start/start.go`, the plan's true-conflict set.
- **W7 = F3 + F5.** F3 keeps the incident/`start.go`/`env.go` files; F5 is
  file-disjoint (`internal/run/why.go`, `cmd/why/why.go`,
  `api/rest/controller/why/why.go`). The loop closes here.
- **W8 = N-1.** Its own trailing wave, after every runtime item has merged, so the
  roadmap flip and the arc dashboard's "waves shipped / last PR" row are true.

**Why F is not one wave:** a wave is a set of *parallel* agents in separate
worktrees, so "sequential within the wave" is not a shape `exec-plan-wave` can
dispatch. F1, F2 and F3 collide on
`internal/incident/{classifier,actions,executor,bundle}.go` + `cmd/start/start.go`,
which this plan's own conflict list and the arc's table both call a true conflict —
hence one per wave.

**Within-stream order:** A1 → A2 → A3 → A4, with A5 parallel to A3/A4 after A2.
C1 → C2 and C1 → C3 (parallel), then C4. D1 → D2. E1 → E2. F1 → F2 → F3 (each in
its own wave; F3 reuses F2's ops-adapter wiring and needs N-2 merged); F4 rides
with F1 and F5 rides with F3, both file-disjoint from the incident chain.

**Cross-stream file conflicts:**

- `internal/run/data_assertions.go` — A4 *creates* it (persist-only), B1 fills in
  evaluation, C1 adds the `hold` disposition, C3 adds clean-run release. All
  **sequential across waves** (A → B → C), never same-wave; no parallel edit.
- `internal/run/store.go` — B1 (`SaveDataViolations` + `DataViolations` column) and
  C2 (admission gate) both edit it; B (W2) before C (W3), so no same-wave collision.
  **Cross-plan:** the arc's conflict table sequences `internal/run/store.go` +
  `internal/run/fanout.go` as 0-A1 → 1-B1/C2 → 4-A1, by plan order.
- `internal/models/models.go` — A2 (`DatasetMetric`), C1 (`DatasetHold`) append to
  the order-sensitive `All` slice; A2 (W1) before C1 (W3).
- `internal/models/run.go` — **C2 only** in this plan (the `JobRun.SkipReason`
  column). Additive, but it is a hot per-run model — no other item touches it.
- `pkg/jobdef/definition.go` — **A3 only in this plan** (fields on the existing
  `ProducedDataset` + `Metadata.onUpstreamHold` + `Validate()`). **Cross-plan
  true-conflict** row in the arc table (1-A3, 2-B1, 3-A3, 4-A2): one plan per wave.
- `pkg/env/env.go` — A2 (`CAESIUM_DATASET_METRIC_RETENTION`), A5
  (`CAESIUM_DATA_ASSERTIONS_ENABLED`), B1 (`CAESIUM_BASELINE_WINDOW`,
  `CAESIUM_BASELINE_MIN_SAMPLES`), F3 (`CAESIUM_GIT_WRITE_CREDENTIALS`, gated on
  N-2's design amendment) all add
  fields; additive across waves, shared `validate()` — flag for a clean rebase, not
  a hand-merge. Every plan in the arc appends here: one plan's item per wave.
- `internal/metrics/metrics.go` — B1 (`data_assertions_total`), C1 (`holds_total`
  + `holds_active`), C2 (`runs_held_upstream_total`) each add a collector (two edit
  sites: the `var (...)` block + `Register()`). B is W2, C is W3 — no same-wave
  overlap; C1 + C2 are the one same-wave additive overlap in W3, call it out.
- `api/rest/bind/bind.go` — C3 (release route) and D1 (read routes) append to
  `Protected()`; C3 (W3) before D1 (W4). Additive.
- `cmd/execute.go` — **no edit** (the `dataset` group is already registered).
- `internal/event/bus.go` + `internal/notification/subscriber.go` — C4 only in this
  plan; **cross-plan**, one plan's explainability item per wave (arc table).
- `internal/run/why.go` + `cmd/why/why.go` — F5 only in this plan; **cross-plan**,
  one plan's explainability item per wave.
- `internal/incident/{classifier,actions,executor,bundle}.go` + `cmd/start/start.go`
  incident wiring — **F1 (W5), F2 (W6), F3 (W7): one item per wave, never two in
  the same wave.** F1 touches `classifier.go`/`subscriber.go`/`store.go`; F2 touches
  `actions.go`/`executor.go`/`bundle.go`/`start.go`; F3 touches
  `actions.go`/`start.go`/`env.go` + the new `provenance.go`. **Cross-plan
  true-conflict** with Plan 0 C4/C7, Plan 2 E, Plan 3 F: sequential by plan, never
  the same wave.
- `cmd/start/start.go` — A2 (metric-retention pruner) in W1; F2 (ops adapter) in
  W6; F3 (provenance wiring) in W7. Additive, three different waves.
- `internal/models/job.go` — **C2 only** (the `Job.OnUpstreamHold` column, mirroring
  the shipped `Job.SchemaValidation`). Additive.
- `internal/freshness/evaluator.go` — F4 only.
- `ui/src/lib/api.ts`, `ui/src/router.tsx` — E1 + E2 append; import blocks conflict
  if truly concurrent, so sequence E1 → E2 (already a dependency). Cross-plan: one
  plan's UI stream per wave.
- `justfile`, `.github/workflows/ci.yml`, `helm/caesium/ci/test-values-k8s.yaml` —
  H-1 only; cross-plan, one plan's H-1 per wave.
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
- **Stream F (agent, approvals, provenance, freshness, `why`):** every F scenario
  that touches an approval or a release runs on the **auth lane**
  (`just integration-test-agent`, `CAESIUM_AUTH_MODE=api-key`) and must appear as a
  visible `--- PASS` line, not a skip. F1: one hold ⇒ exactly one
  `data_quality_hold` incident whose bundle (`GET /v1/agent/incidents/:id/bundle`)
  carries the violation, baseline snapshot and impact cone. F2: `propose_action
  release_hold` reaches `awaiting_approval`, `caesium incident approve` **executes**
  it, and the hold is released with an `AuditLog` entry. F3: with no
  `CAESIUM_GIT_WRITE_CREDENTIALS` entry, an `apply_jobdef_patch` on a git-synced job
  degrades to `escalate` carrying the rendered diff (asserted end-to-end); the PR
  path is unit-tested against a fake forge client. F4: a held dataset reports
  not-fresh via `GET /v1/datasets` and does not derive a freshness-triggered run.
  F5: `caesium why` on a hold-skipped run names the dataset and the hold, asserted
  from **stdout captured separately** via `runCLIStdout`.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the observability substrate** is live: a step emits
   `##caesium::metrics` (capped at 16 KiB, last-write-wins), the metrics persist as
   `DatasetMetric` rows against the declared `DatasetDeclaration` registry, `produces`/
   `consumes`/`assertions` lint clean, and the master `CAESIUM_DATA_ASSERTIONS_ENABLED`
   gate is reported by `GET /system/features`. **With the flag off, the three new
   routes (`/v1/datasets/holds`, `/v1/datasets/:ns/:name/metrics`,
   `/v1/datasets/holds/:id/release`) return 404** — they are mounted inside an
   `if assertionsEnabled()` guard, not the unconditional dataset block. Closed by a
   `test/` integration scenario: a metrics-emitting run persists metrics readable via
   `GET /v1/datasets/:ns/:name/metrics` on the enabled lane, **plus a flag-off
   route-table test** — H-1 turns the flag on for *every* self-server lane, so
   there is no integration lane left with it off; the flag-off assertion is
   therefore a Go test in `api/rest/bind/bind_test.go` that builds `Protected()`
   with the gate off and asserts the three routes are absent, mirroring the shipped
   `TestProtectedGatesContractGraphRoute`. That is the one deliberate exception to
   the `CLAUDE.md` end-to-end gate in this plan, and it is recorded here rather
   than left implicit. The baseline helper accepts an explicit `asOf` cut.
2. **Stream B — the assertion evaluator** works: a declared assertion in `warn`
   mode records a `DataViolation` without failing the task; `fail` mode goes red
   exactly like `schemaValidation: fail`; a missing declared metric is a violation;
   cold-start `deltaFromBaseline` is warn-only below `CAESIUM_BASELINE_MIN_SAMPLES`;
   `caesium_data_assertions_total{result}` registered; and a side-effect-free
   `evaluate(spec, sample, baseline) []Violation` is callable with no executor, no
   hold writes, and no `DataViolation` writes. Closed by integration scenarios for
   warn / fail / cold-start, plus a unit test proving
   `evaluate(spec, sample, baseline)` runs with no DB.
3. **Stream C — the circuit breaker** breaks the circuit: a `hold`-mode violation
   opens exactly one `DatasetHold` + one `dataset_held` event (repeat violations
   append occurrences, no re-alert), a downstream job that `consumes` the held
   dataset is admitted directly to `skipped` with reason
   `dataset_hold:<ns>/<name>` + a `run_held_upstream` event, and the hold releases
   on a clean producer rerun **and** (when an auth mode is active) on
   `POST /v1/datasets/holds/:id/release` — with that endpoint returning 403 under
   `CAESIUM_AUTH_MODE=none`. A dataset declared **`release: manual`** stays held
   after a clean producer rerun (only `release: auto`, the default, auto-releases).
   Closed by hold-open, downstream-skip, clean-run-release,
   `release: manual`-stays-held, and fail-closed-403 integration scenarios;
   holds/active metrics registered.
4. **Stream D — the operator surface** ships: `GET /v1/datasets`,
   `/v1/datasets/holds`, `/v1/datasets/:ns/:name/metrics` read the live registry/holds,
   and `caesium dataset list/holds/release/metrics` drive the real endpoints,
   `--json` asserted via `runCLIStdout` (clean stdout captured separately from stderr).
5. **Stream E — the Console UI** surfaces holds: held dataset nodes are badged on
   the lineage graph with the downstream cone shaded, the active-holds count joins
   the nav badges, the ack/release panel drives the release endpoint, and
   `deltaFromBaseline` assertions render a baseline sparkline — each gated by a
   Playwright e2e against a live backend.
6. **Stream F — the loop closes:** a hold opens exactly one `data_quality_hold`
   incident whose agent bundle carries the violation, the baseline snapshot and the
   impact cone; `release_hold` is proposable via MCP `propose_action`, reaches
   `awaiting_approval`, and **executes** on `caesium incident approve` on the auth
   lane, releasing the hold through the same service the REST endpoint uses; an
   `apply_jobdef_patch` against a git-synced job routes through
   `internal/incident/provenance.go` to a **Git PR** carrying the rendered jobdef
   diff (and degrades to `escalate` with the diff when
   `CAESIUM_GIT_WRITE_CREDENTIALS` has no matching entry); a held dataset reports
   **not fresh** and does not derive a freshness-triggered run; and
   `caesium why <downstream-run> --task <task> --job-id <job-id>` returns verdict
   `SKIPPED` and a summary naming the held dataset, the violated assertion and
   the hold id (the run has one `skipped` task row per catalog task, C2(d)).
7. **H-1 — every lane runs the real path:** `CAESIUM_DATA_ASSERTIONS_ENABLED=true`
   is set on `integration-up` **and** on `integration-up-distributed`,
   `-owner-memory`, `-agent`, `-infra`, `integration-test-podman`, **`ui-e2e`,
   `ui-e2e-auth`** (both the `justfile` recipes and their inline server blocks in
   `.github/workflows/ci.yml`), and the
   helm/kind values file, with a metrics-emitting fixture image, so the Stream
   A/B/C/E/F scenarios drive the live binary in CI and no self-server lane silently
   goes red.
8. **N-1 — docs reflect reality:** the `docs/roadmap.md` Phase-4 "Data circuit
   breaker" entry flipped to Shipped, the design-doc `> Status:` banner updated, the
   `metrics` marker + `produces`/`assertions`/`consumes` fields documented by
   **regenerating** `docs/job-schema-reference.md` from `internal/jobdef/report`
   (never hand-edited) plus `docs/job-definitions.md` and
   `docs/caesium-job-llm-reference.md`, a working `docs/examples/` manifest with a
   canonically pinned image, **`docs/tour-data-loop.md`** published, this plan
   indexed in `docs/README.md` (backtick form), and the
   [`closed-loop-arc.md`](closed-loop-arc.md) dashboard row for plan 1 ticked.
   **N-2** has amended `docs/design-data-circuit-breaker.md` (and
   `docs/design-agent-in-the-loop.md`) with `CAESIUM_GIT_WRITE_CREDENTIALS` and the
   Git-PR provenance route **before F3 merged**, so no shipped config knob is
   absent from the design of record.
9. **Cross-cutting (arc acceptance criterion 7):** **every automated decision this
   plan introduces is `why`-explainable** and asserted so by an integration
   scenario — the hold, the downstream skip, the release (clean-run and acked), and
   the approved agent action each leave a persisted event with enough provenance
   that the task-scoped explainer names it. `docs/roadmap.md`,
   `docs/design-data-circuit-breaker.md`, and this plan's per-stream `## Progress`
   entries reflect every shipped stream and match the merged PRs; the shared
   `DatasetDeclaration` registry stays a single table (no duplicate registry, no
   second `Dataset` model); and no second jobdef-apply path or second approval
   surface exists anywhere in the tree. (The `park` disposition remains parked at
   the arc level — not a gate here. Assertion backtesting is Plan 3 **F3** — not a gate
   here.)

## How To Pick Up Work

1. Read [`closed-loop-arc.md`](closed-loop-arc.md) first (it is the program-level
   source of truth and defines the shared conventions this plan links to), then
   this file end-to-end, so you understand the streams, their interdependencies,
   and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`), including its **cross-plan**
   dependencies — Stream F and the auth-lane half of H-1 are not eligible until
   [`trust-the-substrate.md`](../completed/trust-the-substrate.md) H-1 and C4/C7 have merged.
3. Re-grep before you edit. Every citation here is a **symbol**, not a line
   number, and the tree moves; if a symbol has moved or changed shape, fix the
   plan's citation in the same PR.
4. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
5. Run the verification block under `## Verification (Run For Every PR)`.
6. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
7. Open the PR with title format
   `<Imperative subject> (data-circuit-breaker <wave>-<stream>)` — e.g.
   `Add the ##caesium::metrics marker (data-circuit-breaker W1-α)`. GitHub appends
   `(#NNN)` on squash-merge.

## Cross-References

- [`closed-loop-arc.md`](closed-loop-arc.md) — **the umbrella arc; this is Plan 1.**
  Shared conventions 1–8, the cross-plan file-conflict table, the Synergies row that
  puts Stream F in scope, and the parking decisions (`park` disposition) all live
  there. It wins on cross-plan ordering and on why something is in scope.
- [`trust-the-substrate.md`](../completed/trust-the-substrate.md) — **Plan 0.** Stream A (the
  stranding fix C2's skip semantics depend on), C4/C7 (the approval pipeline and
  the direct `apply_jobdef_patch` route Stream F extends), H-1 (the real, widened
  auth lane every F scenario runs on).
- [`backtesting.md`](backtesting.md) **Stream F, item F3** — "Add the
  assertion-threshold backtest — pure metric replay, no re-execution", where
  assertion backtesting lives (moved out of this plan's Phase 3). It calls B1's
  pure `evaluate(spec, sample, baseline)` and A5's `asOf`-cut
  `internal/run/baseline.go` helper; ship them in that shape. **It is F3, not
  F2** — verified 2026-09-05, that plan's Stream F is F1 `backtest_patch`,
  F2 "Link a backtest to an `apply_jobdef_patch` proposal", F3 the assertion
  backtest, F4 (stretch) the resource-override backtest, F5 `why`, F6 the
  timeline; its own preamble reads "**F3** needs Plan 1 A2 …, A5". (An earlier
  review claimed backtesting.md carries two items numbered F3 — it does not;
  `grep -n '^- \[ \] F[0-9]\.'` returns six distinct ids. No renumber is needed
  there.)
- [`resource-right-sizing.md`](resource-right-sizing.md) **Stream E** — the compute
  loop's durable action (`propose_resources` → an `apply_jobdef_patch` proposal).
  Per the arc's Synergies table it **reuses** the provenance route F3 builds and
  adds nothing to it. **Verified 2026-09-05: Plan 2's Stream E has already been
  recast** — its *Design history* subsection records why the design's
  `POST /v1/jobs/:id/resources/apply` provenance router is **not** built ("**So the
  endpoint is dropped**, not front-ended"), and its acceptance criteria assert that
  endpoint **does not exist**. Nothing to re-point. (Stale in the other direction:
  that plan still says this plan's "Stream F is not yet drafted" in two places — a
  sibling change to raise, not fixable here.)
- [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md) —
  the design of record. Source of truth for intent and scope.
- [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) and
  [`agent-in-the-loop-remediation.md`](../completed/agent-in-the-loop-remediation.md) —
  the shipped incident runtime Stream F extends: incident classes, the typed action
  catalog + tiers, the approval gate, and the MCP tool surface. `dataset_held`
  becomes the `data_quality_hold` class; `release_hold` joins the catalog.
- [`docs/roadmap.md`](../../roadmap.md) Phase 4 Data-Plane Differentiators — the
  "Data circuit breaker" entry this plan closes.
- [`freshness-scheduling.md`](../completed/freshness-scheduling.md) — **shipped**
  (#277–#299); owns the `DatasetDeclaration` registry, the jobdef
  `steps[].datasets` block, the `cmd/dataset` group, and the `/v1/datasets` REST
  package this plan **extends**. Stream F4 makes its freshness evaluator
  hold-aware.
- [`contract-enforcement.md`](../completed/contract-enforcement.md) — **shipped**;
  the static/PR-time half of the contract story; reads the same `consumes` edges and already extended
  `ProducedDataset`/`ConsumedDataset` with `Schema`/`SchemaFrom`/`Version`. This
  plan is the runtime breaker for what static analysis cannot see (the *values*).
- [`docs/job-schema-reference.md`](../../job-schema-reference.md) (**generated** from
  `internal/jobdef/report` — update the generator), `docs/job-definitions.md`,
  `docs/caesium-job-llm-reference.md` — the schema docs N-1 extends; plus
  `docs/tour-data-loop.md`, the loop tour N-1 publishes.
- `pkg/task/output.go` (`parseMarkers`), `internal/run/schema_validation.go`
  (`ValidateTaskOutputSchema` / `ValidateTaskOutputSchemaInstance`),
  `internal/run/store.go` (`admit()`, `SaveSchemaViolations`, `insertRunIfSlotTx`),
  `internal/run/why.go` (`loadTrigger`, `WhyTrigger`), `cmd/why/why.go`
  (`renderTable`), `internal/lineage/impact.go` (`QueryImpact`),
  `internal/incident/store.go` (`OpenOrAppend`), `internal/incident/actions.go`
  (`actionCatalog`, `dispatch`, `ActionOps`), `internal/incident/bundle.go`
  (`BuildBundle`), `internal/mcp/tools.go` (`AgentTools`),
  `internal/freshness/evaluator.go` (`statusFor`), `internal/jobdef/git/git_sync.go`,
  `pkg/env/jobdef.go` (`GitSources`), `internal/models/job.go` (`Provenance*`),
  `internal/event/bus.go` — the shipped substrates this plan builds on.

## Open Questions

Recorded rather than asserted (arc convention 6 — an unverified claim is a
question, not a fact). Each must be answered *in the PR that first touches it*.

1. **Fan-out assertion semantics (A4).** A fanned step produces N `TaskRun`
   instances and therefore N metric samples per trigger. Does an assertion evaluate
   per-partition (N verdicts, N possible holds on one dataset — which the
   one-active-hold-per-dataset guard forbids) or once over the group aggregate
   (which needs a group-completion seam the post-task pipeline does not have)? The
   design predates fan-out and does not say. Answer in A4 and record it in the
   design doc's Open Questions before C1 builds on it.
   **Answered 2026-09-08 (A4, W1-α): per-partition on both halves.** Samples are
   recorded per instance; evaluation is per-instance; C1's
   one-active-hold-per-dataset upsert collapses N verdicts into one hold with an
   occurrence count, so the "N possible holds" objection does not arise and no
   group-completion seam is needed. A group-AGGREGATE assertion is out of scope
   for v1. Recorded in `docs/design-data-circuit-breaker.md` § Open questions as
   item 5.
2. **`release_hold` tier (F2).** The design says "tier 2, tier 3 with tolerance
   windows", but `actionCatalog` is a static `map[string]int` consulted by
   `ActionTier(actionType)` — the tier cannot currently depend on params. Either
   the executor grows a param-aware tier hook (a change to shared machinery Plans 2
   and 3 also use) or `release_hold` registers `TierApproval` outright. Decide in
   F2; do not invent a second gating mechanism.
3. **Held dataset status value (F4).** Is a held dataset `stale`, `violated`, or a
   new `held`? `models.DatasetStatus*` has no held value, and the design forbids
   overloading `Quarantined`. `Violated` is the closest existing fit but already
   means "freshness SLO breached", which would make the reason string carry all the
   distinction. Decide in F4.
4. **`why` for a task-less run (F5) — resolved 2026-09-06.** A hold-skipped run
   is not task-less: C2(d) materialises a `skipped` `TaskRun` per catalog task, and
   F5 adds the `SKIPPED` verdict to the task-scoped explainer, so
   `caesium why <run-id> --task <any step>` answers "why did nothing run?"
   without a run-level verb. Remaining sub-question: whether the run *list*
   should also surface `SkipReason` inline (Stream E may add it; not a gate).
5. **Forge coverage and its test bar (F3).** GitHub first is settled; GitLab and
   Gitea are not scoped. Neither is the CI verification bar for the live-PR path —
   an integration test cannot open a real PR, so the honest bar is a unit test
   against a fake forge client plus an integration test of the degrade-to-escalate
   path. Confirm that bar in F3 rather than claiming end-to-end PR coverage.
6. **Multi-producer release (design open question 2).** Does a clean run of
   producer B release a hold opened by producer A? The design proposes "no — only
   the holder's clean run". C3 must implement one answer explicitly; record it.

