# Design: Cross-Job Contract Enforcement at Apply Time

> Status: Brainstorm/Design — proposal for static, apply-time enforcement of
> cross-job data contracts. No implementation yet. The static complement to
> [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) scenario 2
> (schema drift) and the apply-time counterpart of
> [`design-data-circuit-breaker.md`](design-data-circuit-breaker.md).
> Companion roadmap item: §2.1 PR Preview Runs (the PR surface this plugs
> into).

## Problem

Steps already declare per-step contracts — `OutputSchema` (JSON Schema for the
step's `##caesium::output` keys) and `InputSchema` (keyed by predecessor step
name) in `pkg/jobdef/definition.go`, validated **at runtime, within one run**
by `pkg/task/schema.go`. But data also flows *between jobs*:

- **Trigger chaining.** Job B declares an `event` trigger matching
  `run_completed` filtered on `job_alias: vendor-x-daily`
  ([`design-event-triggers.md`](design-event-triggers.md) WS3). The payload
  is the full marshaled `JobRun` — including `Tasks[].Output`
  (`internal/run/store.go`) — and B's `paramMapping` JSONPath expressions
  pull values from it into run params, which become env vars for B's steps.
- **Datasets.** Steps emit output-refs recorded as `LineageDataset` rows
  (`internal/models/lineage_dataset.go`); `internal/lineage/impact.go`
  `QueryImpact` walks producer→consumer edges across jobs.

Nothing checks, at `lint`/`diff`/`apply` time, that a change to the
*producer's* `outputSchema` is compatible with what *consumers* extract or
require. The failure mode: team A trims `customer_id` from `vendor-x-daily`
on Tuesday; `reporting-daily` (team B) fails its `inputSchema` gate — or
silently loads nulls — at 3 a.m. Wednesday. The runtime gate works exactly as
designed and is still the wrong place to discover the problem. The
agent-in-the-loop design remediates that page; this design prevents it:
**the 3 a.m. failure becomes a lint error in the producer's PR**, with the
consumer and its owning team named:

```
BREAKING: vendor-x-daily step "export" outputSchema removes required field
"customer_id" consumed by reporting-daily step "load" (owner: team=b,
via event-trigger chain vendor-x-daily → reporting-daily).
```

## Fit with Design Principles

1. **Container-native execution.** Pure control-plane analysis over declared
   schemas; nothing changes about what runs in containers.
2. **Declarative and GitOps-first.** Contracts are YAML, enforced where
   GitOps changes land — lint, diff, apply, the PR flow (roadmap §2.1) — and
   a breaking change is reviewable as a diff annotation.
3. **Zero-dependency simplicity.** The graph derives from data the server
   already holds (persisted jobdefs, triggers, lineage rows) plus one small
   table for break acknowledgments. No new services.
4. **Smart by default.** Edges are *inferred* from trigger chaining and
   lineage evidence before anyone declares a dataset block; declarations
   upgrade warnings to hard guarantees.
5. **Data engineering first.** Breaking-change semantics on data contracts
   is the schema-registry discipline data teams already expect.

## Overview

```
  producer PR: vendor-x-daily.job.yaml (outputSchema edited)
        │
  caesium job lint/diff/apply → POST /v1/jobdefs/{lint,diff,apply}
        │
  ┌─────▼────────────────────────────────────────────┐
  │ Contract graph derivation — edges =              │
  │  1. declared  produces/consumes dataset blocks   │
  │  2. inferred  event-trigger chains + paramMapping│
  │  3. evidence  lineage_datasets (observed runs)   │
  └─────┬────────────────────────────────────────────┘
  ┌─────▼────────────────────────────────────────────┐
  │ Schema compat checker: new producer schema vs    │
  │ old schema + each consumer's declared requirement│
  │ breaking → error   unknown → warn   ok → pass    │
  └─────┬────────────────────────────────────────────┘
        ▼
  lint: findings    diff: per-edge     apply: refused unless
  in response       annotations        compatible or acknowledged
```

Edge classes are ranked by confidence and enforced accordingly:

| Edge source | How derived | Default action on break |
|---|---|---|
| Declared `produces`/`consumes` | new YAML block (shared with [`design-freshness-scheduling.md`](design-freshness-scheduling.md)) | **fail** |
| Trigger chain + `paramMapping` paths into `$.tasks[*].output.*` | static analysis of persisted triggers (same merge logic as `ValidateTriggerChains`, `internal/jobdef/trigger_cycle.go`) | **fail** when the extracted key is provably removed/retyped; warn otherwise |
| Lineage evidence only (`lineage_datasets` rows, no declaration) | runtime observation | **warn** (evidence, not a promise) |

## What contract exists cross-job today (precision matters)

Within a run, predecessor outputs reach a step as
`CAESIUM_OUTPUT_<STEP>_<KEY>` env vars (`pkg/task/output.go`
`BuildOutputEnv`), and `InputSchema` is keyed by *within-job predecessor
step names* — **neither crosses a job boundary**. Across jobs, the only
Caesium-mediated channel is the lifecycle event payload described above:
today's cross-job "contract" is an undeclared, stringly-typed JSONPath into
another team's run payload. This design statically checks those paths
against the producer's `outputSchema` and adds explicit declarations.
Dataset I/O that never transits the event payload (a step writes S3, another
job reads S3) is invisible to Caesium except as lineage evidence — which is
why declarations are the only path to *fail*-grade enforcement for dataset
edges.

## YAML: declared produces/consumes

The step-level `datasets` block is **the same block** introduced by
[`design-freshness-scheduling.md`](design-freshness-scheduling.md) for
freshness SLOs — one declaration, two consumers of it. Freshness reads
`freshness`/`watermark`; this design adds `schema`/`schemaFrom` to `produces`
entries and lets a `consumes` entry be an object carrying the consumer's
required schema. Dataset `name` maps to the OpenLineage `(namespace, name)`
identity recorded in `lineage_datasets`.

```yaml
# vendor-x-daily.job.yaml (producer, team A)
metadata:
  alias: vendor-x-daily
  labels: {team: a}                # ownership convention (see Ownership)
steps:
  - name: export
    image: vendor-x-export:latest
    outputSchema:
      type: object
      required: [customer_id, row_count]
      properties:
        customer_id: {type: string}
        row_count: {type: integer}
    datasets:
      produces:
        - name: lake.vendor_x_customers
          schemaFrom: output       # reuse this step's outputSchema…
          # schema: {...}          # …or declare an inline dataset schema
          version: 2               # bumped on intentional breaks
---
# reporting-daily.job.yaml (consumer, team B)
metadata:
  alias: reporting-daily
  labels: {team: b}
trigger:
  type: event
  configuration:
    events:
      - type: run_completed
        filter: {job_alias: vendor-x-daily}
    paramMapping:
      upstream_rows: "$.tasks[0].output.row_count"
steps:
  - name: load
    image: etl:1.4
    datasets:
      consumes:
        - name: lake.vendor_x_customers
          schema:                  # what THIS consumer requires — a subset,
            type: object           # not a copy of the producer's schema
            required: [customer_id]
            properties:
              customer_id: {type: string}
```

Lint validates that `schemaFrom: output` names a step with an
`outputSchema`, that schemas compile under `santhosh-tekuri/jsonschema/v6`,
and that dataset names are well-formed.

## Breaking-change semantics

Full JSON Schema compatibility is undecidable in general (schemas embed
arbitrary boolean combinators), so the checker implements a **pragmatic
subset** with an honest fourth verdict:

- **Breaking** (error): a `required` field removed from the schema or from
  `properties` entirely; a `type` narrowed or changed (`string→integer`;
  `integer→number` is widening, allowed); `enum` values removed;
  `additionalProperties` tightened to `false` when a consumer requires a key
  outside `properties`; any consumed key (declared in
  `consumes.schema.required` or referenced by a `paramMapping` path) no
  longer satisfiable.
- **Compatible** (pass): additive optional properties, new enum values,
  widened types, relaxed constraints, doc-only edits.
- **Compatible-per-consumer**: a narrowing change that still satisfies every
  *declared* consumer requirement (e.g. drops a field nobody consumes)
  passes with an informational note.
- **Unknown** (warn, never silently pass): any construct outside the subset —
  `$ref`, `allOf`/`anyOf`/`oneOf`/`not`, `if/then/else`,
  `patternProperties`, `dependentSchemas` — reported as "cannot prove
  compatibility; verify manually or simplify."

New machinery: `santhosh-tekuri/jsonschema/v6` gives us **compilation and
instance validation only** (`pkg/task/schema.go` `ValidateOutput` checks a
value against one schema). Compatibility is schema-vs-schema and is written
fresh — a new `pkg/jobdef/schemacompat` package walking two raw
`map[string]any` trees over exactly the subset above, returning typed
findings `{Kind, Path, Detail, Verdict}`. Deterministic, unit-testable, kept
in `pkg/` because the CLI needs it offline.

## Scenarios

### 1. Producer PR blocked, consumers named

Team A's PR removes `customer_id` from `export`'s `outputSchema`. The §2.1 PR
action runs `caesium job lint --server` → `POST /v1/jobdefs/lint`. The server
merges the incoming def with persisted jobs (the `ValidateTriggerChains`
pattern), finds the edge to `reporting-daily`, and the checker flags the
removed required field. The PR comment reads: *"BREAKING: removes
`customer_id` required by `reporting-daily` (team: b) and `billing-monthly`
(team: c)."* CI fails; `caesium job apply` refuses with the same message.
Nobody gets paged.

### 2. Additive change passes

Team A adds optional `customer_segment`. Verdict: compatible. Lint prints an
informational line, diff annotates the edge "compatible (additive)", apply
proceeds. Consumers change nothing.

### 3. Intentional break: version bump + deprecation window + notification

`customer_id` genuinely must go. The escape hatch is explicit and two-sided:

1. Producer bumps `datasets.produces[].version: 2` and applies with
   `caesium job apply --allow-breaking dataset=lake.vendor_x_customers`. The
   server records a `ContractAck` row (actor, edge-set digest,
   `deprecationUntil` — default `CAESIUM_CONTRACT_DEPRECATION_WINDOW`, 14d).
2. Every consumer's owning team is notified via the existing
   `internal/notification` pipeline (new `contract_break_declared` event
   routed by `NotificationPolicy` job selectors, the same matching that
   routes failures to Slack), naming the field, window, and producer.
3. During the window the check downgrades to warn for the acknowledged
   digest only; consumers' own applies warn "consuming a deprecated
   contract". When the window lapses, unmigrated consumers' next apply
   fails, and the ack is spent — a *new* breaking change needs a new ack.
4. With auth enabled, `--allow-breaking` can be policy-restricted (producing
   team or operator role). See Ownership.

## Backend

### Contract graph derivation

New package `internal/contract`. Given the incoming definitions plus the
persisted world (same merge shape as `triggerChainNodes` in
`internal/jobdef/trigger_cycle.go`, which already joins `jobs` × `triggers`
and substitutes incoming defs for their persisted versions):

1. **Declared edges** — match `produces`/`consumes` on dataset name across
   all jobs.
2. **Trigger-chain inference** — reuse the lifecycle-pattern matching from
   `trigger_cycle.go` (`patternCanMatchCaesiumLifecycle`,
   `triggerChainPatternSourceAlias`) for A→B edges; then statically parse
   B's `paramMapping` values and flag paths of shape
   `$.tasks[<i>].output.<key>` — those name concrete producer output keys.
   Positional indexing is brittle: when the index can't be resolved to a
   step, the key is checked against the union of A's step `outputSchema`s
   and findings degrade to warn.
3. **Lineage evidence** — distinct cross-job `(namespace, name)` pairs where
   one job's task runs wrote `direction='output'` and another's read
   `direction='input'` (the `QueryImpact` join, aggregated to job level),
   marked `evidence` with a `lastSeen` timestamp.

The graph is **derived, not stored**: edges are recomputed per request from
authoritative sources, so they can never go stale against jobdef edits. The
only new persisted model is `ContractAck` (id, dataset identity or edge-set
digest, actor, reason, created/expires). No `ContractEdge` table in v1;
`GET /v1/contracts/graph` computes on demand (job counts are small; the
trigger-cycle validator already does an equivalent full scan on every apply).

### Integration points (server-side, because only the server sees all jobs)

- **`POST /v1/jobdefs/lint`** (`api/rest/controller/jobdef/lint.go`): after
  the existing `Validate()` + `ValidateTriggerChains` steps, run the contract
  check; response gains `contracts: {breaking: [...], warnings: [...],
  edges: n}`. The existing within-job `contractSummary` folds into the same
  section.
- **`POST /v1/jobdefs/diff`**: per-job diff entries gain `contractFindings`
  so the UI/PR comment can badge edges.
- **`POST /v1/jobdefs/apply`**: the check runs **inside the importer's apply
  transaction** (`internal/jobdef/importer.go` `ApplyWithOptions` already
  wraps reconcile in `i.db.Transaction`), reading persisted consumers under
  the same transaction that persists the producer — racing applies serialize
  on the store, so there is no TOCTOU window between check and write.
  Breaking findings without a valid ack: HTTP 409 with the findings.
- **Batch semantics**: producer and consumers updated in one apply batch are
  checked as a set — a coordinated migration in one PR passes without an
  ack, since the new consumer schemas are the comparison target.

### Offline vs server lint — two honest modes

CLI `caesium job lint` is offline today: it passes a `nil` DB to
`ValidateTriggerChains` and prints *"trigger-cycle lint is file-scoped;
cross-job cycles against persisted triggers are validated at apply"*
(`cmd/job/lint.go`). Contract lint keeps that honesty: **offline** (default)
checks only edges derivable inside the linted file set and appends the same
style of scope note; **`--server`** (new flag) POSTs to `/v1/jobdefs/lint`
and reports findings against the persisted world. The §2.1 PR flow always
uses server mode.

### Config

- `CAESIUM_CONTRACT_ENFORCEMENT` — `""` (off, default), `warn`, `fail`.
  Mirrors `metadata.schemaValidation`'s tri-state. Off ⇒ no graph
  computation, no routes registered (reported by `GET /system/features`).
- `CAESIUM_CONTRACT_DEPRECATION_WINDOW` (default `336h`).
- Evidence-only edges never exceed warn regardless of mode.

### Ownership and auth (advisory until auth is on)

"Owned by team B" resolves from `metadata.labels.team` — already a live
convention (labels ride on every run/event as `JobLabels`). With
`CAESIUM_AUTH_MODE=none` (the default) ownership is **advisory**: messages
name the team, notifications route by label, but anyone can pass
`--allow-breaking`. With auth enabled, the ack path can require an operator
role or a key scoped to the producing job — the same honesty the
agent-in-the-loop design applies to its approval gates.

## CLI

```
caesium contract graph [--dataset ns/name] [--json]   # GET /v1/contracts/graph
caesium contract check --path jobs/ [--json]          # server-mode contract lint only
caesium job lint --server                             # existing lint + contract findings
caesium job apply --allow-breaking dataset=<ns/name> [--reason ...]
```

Per the repo testing gate: `--json` output goes to stdout, clean and
parseable, stderr for logs.

## CI / PR flow (roadmap §2.1)

The planned GitHub Action (`lint → diff → dev --once → comment`) gains a
contract section: breaking findings render as a table (field, producer step,
consumers, owning teams, edge source) in the PR comment, and the Action
exits nonzero. `caesium job diff --format=markdown` includes the per-edge
badges. This is where the feature earns its keep — at review time, in the
producer's repo, before merge.

## Frontend (Caesium Console)

1. **Contract graph view** — renders `GET /v1/contracts/graph`, reusing the
   existing `ui/src/features/jobs/LineageGraph.tsx` renderer (nodes:
   jobs/datasets; edge styling by class: declared / inferred / evidence).
2. **JobDefs diff badges** — `ui/src/features/jobdefs/JobDefsPage.tsx`
   already lints then diffs pasted YAML; its diff tab gains
   breaking/compatible/unknown badges per edge from `contractFindings`, with
   named consumers and teams inline. Apply is disabled on breaking findings
   unless an ack reason is entered.
3. **Dataset detail** — consumers, schema version, open deprecation windows.

## Interplay

- **[`design-freshness-scheduling.md`](design-freshness-scheduling.md)** —
  the step-level `datasets` block is *shared substrate*: freshness uses it
  for time (is the data recent enough), this design for shape (is it the
  right shape). One declaration, two enforcers.
- **[`design-data-circuit-breaker.md`](design-data-circuit-breaker.md)** —
  enforces at runtime what this enforces at apply: the breaker trips on
  observed bad data crossing an edge; this design keeps the *declared*
  version of the same break from ever deploying. Same edge model.
- **[`design-agent-in-the-loop.md`](design-agent-in-the-loop.md)** — scenario
  2's agent-proposed schema patches flow through `jobdefs/diff` + `apply`, so
  they are contract-checked before a human sees the approval card; a patch
  that would break a downstream team is rejected or annotated automatically.
- **[`design-backtesting.md`](design-backtesting.md)** — backtests replay
  historical definitions; the graph endpoint gives them the edge set to
  validate a proposed schema against historical consumer versions.

## Testing

Per the end-to-end gate in `CLAUDE.md`:

- `pkg/jobdef/schemacompat` unit tests: table-driven verdict matrix (removed
  required, type narrow/widen, enum shrink/grow, additive optional, each
  unknown construct → unknown), plus fuzzing so no schema crashes the walker.
- Integration tests in `test/` driving the real surface — apply producer +
  consumer, then: (a) breaking change → apply 409 naming consumer and team,
  via CLI with `runCLIStdout` asserting clean parseable `--json`; (b)
  additive change applies; (c) `--allow-breaking` incl. notification emission
  and window expiry; (d) coordinated one-batch migration passes; (e)
  paramMapping-inferred edge catches a removed output key with no
  declarations; (f) `CAESIUM_CONTRACT_ENFORCEMENT` unset ⇒ fully inert.
  Enable the gate in `just integration-up` so the path executes in CI. UI
  badges and graph view get Playwright e2e against the live backend.

## Phasing

- **Phase 0 — Visibility.** Graph derivation (all three edge classes),
  `GET /v1/contracts/graph`, `caesium contract graph`, warn-only findings in
  server lint. No enforcement, no new YAML.
- **Phase 1 — Declarations + enforcement.** `datasets` schema fields
  (coordinated with freshness-scheduling), compat checker, apply-transaction
  enforcement, `--allow-breaking` + `ContractAck` + deprecation
  notifications, CLI/CI surfaces.
- **Phase 2 — UI + PR polish.** Graph view, diff badges, §2.1 Action
  section, auth-gated ack policy.

## Non-Goals (v1)

- No general schema registry (no Avro/Protobuf, no external subjects); JSON
  Schema on Caesium outputs/datasets only.
- No full JSON Schema compatibility decision procedure — the subset plus the
  `unknown` verdict is the contract.
- No runtime behavior change: the `schemaValidation` gate is untouched; this
  never blocks a *run*, only an *apply*.
- No cross-cluster contracts; one server's persisted world is the graph.
- No automatic consumer migration (the agent design's patch proposals are
  the assisted path).

## Open Questions

1. **Payload-shape contracts for event params.** Should consumers declare a
   schema for the *trigger event payload* itself (not just datasets),
   formalizing today's `paramMapping` extraction? Leaning yes, later phase —
   it turns the inferred-edge check into a declared one.
2. **Evidence-edge decay.** How long does a lineage-observed edge keep
   warning after the consumer stops reading the dataset? Tie to
   `lineage_datasets` pruning, or a dedicated `lastSeen` horizon?
3. **Namespace interplay (roadmap §3.1).** Cross-namespace contract edges
   should probably require declarations (no inference across tenant
   boundaries); decide before multi-tenancy lands.
4. **Ack UX under GitOps.** For git-synced jobs, should the ack live in the
   YAML (`datasets.produces[].breakingChangeAck`) so the escape hatch itself
   is PR-reviewed, rather than a CLI flag? Leaning YAML-first there,
   mirroring the agent design's provenance routing.
