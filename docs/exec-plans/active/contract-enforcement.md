# Data-Contract Enforcement — Cross-Job Schema Compatibility at Apply Time

Last updated: 2026-07-03

Caesium validates per-step data contracts (`OutputSchema` / `InputSchema` in
`pkg/jobdef/definition.go`) **at runtime, within one run** via
`pkg/task/schema.go`. But data also flows *between* jobs — through event-trigger
chaining (`paramMapping` JSONPath into the upstream `JobRun` payload) and through
datasets (`internal/models/lineage_dataset.go` producer→consumer edges walked by
`internal/lineage/impact.go` `QueryImpact`). Nothing checks, at
`lint`/`diff`/`apply` time, that a change to a *producer's* `outputSchema` stays
compatible with what *consumers* extract or require. The failure mode is the 3 a.m.
page: team A trims `customer_id` from `vendor-x-daily`; `reporting-daily` (team B)
fails its `inputSchema` gate — or silently loads nulls — at 3 a.m. the next morning.

This plan ships [`docs/design-contract-enforcement.md`](../../design-contract-enforcement.md):
a control-plane graph derivation over data the server already holds (persisted
jobdefs, triggers, lineage rows), a pragmatic-subset JSON-Schema **compatibility
checker** (breaking / compatible / unknown, never a silent pass), apply-time
**enforcement** inside the importer transaction (breaking change → HTTP 409 unless
acknowledged), an intentional-break escape hatch (`--allow-breaking` +
`ContractAck` + deprecation-window notifications), and the operator surfaces (a
`GET /v1/contracts/graph` REST read, a `caesium contract` CLI group, contract
findings folded into `caesium job lint --server`, and a Console graph view + diff
badges). The 3 a.m. failure becomes a lint error in the producer's PR, named by
consumer and owning team.

The design phases (Phase 0 Visibility → Phase 1 Declarations+Enforcement → Phase 2
UI+PR polish) map onto the streams below; enforcement is off by default
(`CAESIUM_CONTRACT_ENFORCEMENT=""`) and never blocks a *run*, only an *apply*.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work backlog,
`## Sequencing & Dependencies` captures cross-stream order, and
`## Acceptance Criteria` lists the gates that close out the entire plan. Any agent
can:

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

When this plan and [`docs/design-contract-enforcement.md`](../../design-contract-enforcement.md)
disagree, **the design doc wins on INTENT and SCOPE** (what the graph derivation,
the compatibility subset, the enforcement points, and the escape hatch must do). No
item may add a NEW edge class, verdict grade, config knob, endpoint, or CLI verb
beyond what the design enumerates without first amending the design doc and this
plan's Source-Of-Truth Note. Strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) §2.1 (PR Preview Runs — the PR surface this
plugs into) and the Phase-4 design table; the roadmap wins on priority/status
disagreements.

Two cross-plan contracts bind this plan:

- **The step-level `datasets` block is shared substrate owned by
  [`freshness-scheduling.md`](../completed/freshness-scheduling.md) Stream A** (which introduces
  `Step.Datasets` with `produces`/`consumes`/`freshness`/`watermark`). This design
  ADDS `schema`/`schemaFrom` to `produces` entries and a `schema` to `consumes`
  entries — it does **not** own the base block. Stream E coordinates: whichever plan
  lands `Step.Datasets` in `pkg/jobdef/definition.go` first introduces the struct;
  the other extends it. On any disagreement about the base block's shape, the
  freshness plan wins; on the schema-compat fields, this plan wins.
- **The compatibility checker uses `github.com/santhosh-tekuri/jsonschema/v6`
  (already in `go.mod` at v6.0.2) for compilation/instance validation ONLY.**
  Schema-vs-schema compatibility is written fresh in `pkg/jobdef/schemacompat` over
  the design's explicit subset — if an item finds it needs a construct outside that
  subset, it returns the `unknown` verdict, it does not extend the library.

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from
[`docs/design-contract-enforcement.md`](../../design-contract-enforcement.md); the
first wave is the next eligible run of the `exec-plan-wave` skill against this doc.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Schema-compatibility checker — new `pkg/jobdef/schemacompat` walker, verdict types, table-driven matrix + fuzz | **P0** | Not started |
| B | Contract graph derivation — new `internal/contract` (inferred trigger-chain + evidence lineage edges, then declared) | **P0** | Not started |
| C | Apply-time enforcement, `ContractAck`, `--allow-breaking`, deprecation-window notifications | **P0** | Not started |
| D | REST + CLI operator surface — `GET /v1/contracts/graph`, `caesium contract`, `job lint --server`, findings in lint/diff | P1 | Not started |
| E | Datasets `schema`/`schemaFrom` declarations (coordinated with freshness Stream A) | P1 | Not started |
| F | Console UI — contract graph view, JobDefs diff badges, dataset detail | P2 | Not started |
| H-1 | Integration harness — enable `CAESIUM_CONTRACT_ENFORCEMENT` on the live integration server | — | Not started |
| N-1 | Docs — roadmap §2.1, design-doc banner, schema references, examples, README | — | Not started |

## Streams

### Stream A — Schema-compatibility checker (`pkg/jobdef/schemacompat`)

The deterministic, offline core every enforcement decision rests on: given two raw
JSON-Schema trees (`map[string]any`), decide whether the new one is a **breaking**,
**compatible**, or **unknown** change relative to the old one, plus the *per-consumer*
case (a narrowing change that still satisfies a declared consumer requirement).
Lives in `pkg/` (not `internal/`) because the CLI needs it offline. New package, no
shared files — merges cleanly, so it is the foundation the other streams import.

- [x] A1. Add the `pkg/jobdef/schemacompat` package: a `Compare(oldSchema,
      newSchema map[string]any) []Finding` walker returning typed findings
      `{Kind, Path, Detail, Verdict}` over exactly the design's subset —
      **breaking**: a `required` field removed (from `required` or `properties`
      entirely), a `type` narrowed/changed (`string→integer`; `integer→number` is
      widening, allowed), `enum` values removed, `additionalProperties` tightened to
      `false` when a required key falls outside `properties`; **compatible**:
      additive optional properties, new enum values, widened types, relaxed
      constraints, doc-only edits; **unknown** (never a silent pass): `$ref`,
      `allOf`/`anyOf`/`oneOf`/`not`, `if/then/else`, `patternProperties`,
      `dependentSchemas` — reported as "cannot prove compatibility." Add a
      `Satisfies(schema, requirement map[string]any) []Finding` helper for the
      per-consumer subset check (does the new producer schema still satisfy a
      consumer's declared `required`/`type` requirement?). Pure logic; use
      `santhosh-tekuri/jsonschema/v6` for schema *compilation* validation only, not
      for compatibility.
      Files: new `pkg/jobdef/schemacompat/compat.go`, new
      `pkg/jobdef/schemacompat/compat_test.go`, new
      `pkg/jobdef/schemacompat/fuzz_test.go`.
      Note: Landed the exported `schemacompat` API (`Verdict`, verdict constants,
      `FindingKind`, `Finding`, `Compare`, and `Satisfies`) with deterministic
      path-based findings over the design subset. Unknown/out-of-subset constructs
      fail closed with `VerdictUnknown`, while required removals, type narrowing,
      enum shrinkage, and unsatisfiable `additionalProperties: false` requirements
      report breaking findings.
- [x] A2. Add the table-driven verdict matrix + fuzz corpus: every subset row
      (removed required, type narrow/widen, enum shrink/grow, additive optional,
      `additionalProperties` tighten, each unknown construct → `unknown`) as a
      deterministic case, plus a `FuzzCompare` that asserts the walker never panics
      on arbitrary nested `map[string]any` trees.
      Files: `pkg/jobdef/schemacompat/compat_test.go`,
      `pkg/jobdef/schemacompat/fuzz_test.go`.
      Depends on: A1.
      Note: Added table-driven coverage for the breaking/compatible/unknown matrix,
      nested required-field paths, relaxed constraints, doc-only edits, and
      `Satisfies` producer-vs-consumer required/type behavior. Added `FuzzCompare`
      with meaningful JSON object seeds so normal unit-test execution covers the
      seed corpus and fuzzing checks arbitrary nested schema maps for panics.

### Stream B — Contract graph derivation (`internal/contract`)

The derived (never stored) cross-job edge set the checker runs over. Given the
incoming definitions merged with the persisted world — reusing the
`triggerChainNodes` join shape in `internal/jobdef/trigger_cycle.go:55` (which
already joins `jobs` × `triggers` and substitutes incoming defs for persisted
versions) — derive the three edge classes. Edges are recomputed per request from
authoritative sources so they never go stale against jobdef edits. New package,
read-only against existing tables.

- [x] B1. Add the `internal/contract` graph deriver for the two evidence-driven edge
      classes: **inferred** — reuse `patternCanMatchCaesiumLifecycle` +
      `triggerChainPatternSourceAlias` (`internal/jobdef/trigger_cycle.go:293,236`)
      to find A→B trigger-chain edges, then statically parse B's `paramMapping`
      values for paths of shape `$.tasks[<i>].output.<key>` that name concrete
      producer output keys (degrade the finding to `warn` when a positional index
      can't be resolved to a step — check the key against the union of A's step
      `outputSchema`s); **evidence** — distinct cross-job `(namespace, name)` pairs
      where one job wrote `direction='output'` and another read `direction='input'`
      (the `QueryImpact` join in `internal/lineage/impact.go`, aggregated to job
      level, marked `evidence` with a `lastSeen` timestamp). Return a typed
      `Graph{Nodes, Edges}` with each edge carrying its class
      (declared/inferred/evidence). Evidence-only edges never exceed `warn`.
      Files: new `internal/contract/graph.go`, new `internal/contract/derive.go`,
      new `internal/contract/graph_test.go`.
      Depends on: A1 (edges reference the checker's `Finding` type for path-vs-schema
      verdicts).
      Note: Added JSON-ready `Graph{Nodes, Edges}` types, `declared`/`inferred`/
      `evidence` edge-class constants, a pure `DeriveGraph(DeriveInput)` API, and
      a `Deriver` with GORM-backed readers that substitute incoming definitions
      for persisted jobs. B1 covers inferred lifecycle `paramMapping` output-key
      checks with `schemacompat.Finding` verdicts and lineage evidence edges with
      warn-only `VerdictUnknown` plus `lastSeen`; tests cover missing keys,
      unresolved task indexes, unknown scoped producers, ignored non-output paths,
      job-id source filters, and evidence last-seen aggregation.
- [ ] B2. Fold the **declared** edge class into the graph: match
      `datasets.produces`/`datasets.consumes` on dataset `name` across all merged
      jobs, attaching the producer's `schemaFrom: output` (resolved to that step's
      `outputSchema`) or inline `schema`, and each consumer's required `schema`.
      Run the Stream A checker on each declared edge (new producer schema vs old +
      each consumer requirement) so the graph carries breaking/compatible/unknown
      verdicts per declared edge. Batch semantics: a producer and its consumers
      updated in one apply batch are checked as a set (the new consumer schemas are
      the comparison target), so a coordinated migration passes without an ack.
      Files: `internal/contract/derive.go`, `internal/contract/graph_test.go`.
      Depends on: B1 + E1 (the `datasets` schema fields the declared edges read).

### Stream C — Apply-time enforcement, acks & break notifications

Where the graph earns its keep: the check runs **inside the importer's apply
transaction** (`internal/jobdef/importer.go:111` `ApplyWithOptions`, which already
wraps reconcile in `i.db.Transaction`) reading persisted consumers under the same
transaction that persists the producer — so racing applies serialize on the store
and there is no TOCTOU window. Breaking findings without a valid ack → HTTP 409.
Plus the two-sided intentional-break escape hatch.

- [ ] C1. Add the `ContractAck` GORM model (dataset/edge-set digest, actor, reason,
      created/expires) + register it in the `All` slice (`internal/models/models.go`,
      appended after `LineageDataset`); add `CAESIUM_CONTRACT_ENFORCEMENT` (`""` off /
      `warn` / `fail`, mirroring `metadata.schemaValidation`'s tri-state) and
      `CAESIUM_CONTRACT_DEPRECATION_WINDOW` (default `336h`) to `pkg/env/env.go`; run
      the Stream B graph + Stream A checker inside `ApplyWithOptions`'s transaction
      and return **HTTP 409 naming the consumer(s) and owning team(s)** on a breaking
      finding with no valid ack (respected by `api/rest/controller/jobdef/apply.go`).
      `ContractAck` is a catalog table (NOT hot-path — do not add to
      `hotPathModels()`/`hotTables`). When enforcement is off, the graph is not
      computed and the path is fully inert. Add
      `caesium_contract_breaks_blocked_total{dataset}`.
      Files: new `internal/models/contract_ack.go`, `internal/models/models.go`,
      `pkg/env/env.go`, `internal/jobdef/importer.go`,
      `api/rest/controller/jobdef/apply.go`, `internal/contract/enforce.go`,
      `internal/metrics/metrics.go`.
      Depends on: A1 + B1.
- [ ] C2. Add the intentional-break escape hatch: `caesium job apply
      --allow-breaking dataset=<name> [--reason ...]` records a `ContractAck` row
      (actor, edge-set digest, `deprecationUntil`); during the window the enforcement
      check downgrades to `warn` for the acknowledged digest only, and consumers'
      own applies warn "consuming a deprecated contract"; when the window lapses the
      ack is spent and an unmigrated consumer's next apply fails. Emit a
      `contract_break_declared` event routed through the existing
      `internal/notification` pipeline (`NotificationPolicy` job-selector matching,
      `internal/notification/subscriber.go:187` `matchPolicies`), naming the field,
      window, and producer. Under `CAESIUM_AUTH_MODE=none` ownership is advisory
      (anyone can pass `--allow-breaking`); with auth on the ack path can be
      policy-restricted.
      Files: `cmd/job/apply.go`, `internal/jobdef/importer.go`,
      `internal/contract/enforce.go`, `internal/notification/subscriber.go`,
      `api/rest/controller/jobdef/apply.go`.
      Depends on: C1.

### Stream D — REST + CLI operator surface

The read/inspect surfaces over the graph and checker: the graph endpoint, the
contract CLI, and contract findings folded into the existing lint/diff responses.
Reuses the existing `api/rest/controller/jobdef/` controllers — do NOT fork them.

- [x] D1. Add `GET /v1/contracts/graph` (`[--dataset ns/name]` filter) computing the
      Stream B graph on demand, bound in `Protected()` of `api/rest/bind/bind.go`
      (alongside the existing `/jobdefs/*` routes at `bind.go:126-128`); fold contract
      findings into the existing lint/diff responses — `POST /v1/jobdefs/lint`
      (`api/rest/controller/jobdef/lint.go`) gains `contracts: {breaking, warnings,
      edges}`, `POST /v1/jobdefs/diff` (`diff.go`) gains per-job `contractFindings`
      so the UI/PR comment can badge edges; add a `ContractEnforcementEnabled` field
      to the `Features` struct (`api/rest/service/system/system.go:36`) so
      `GET /system/features` reports the gate. Add
      `caesium_contract_findings_total{verdict}`.
      Files: new `api/rest/controller/contract/graph.go`, new
      `api/rest/service/contract/`, `api/rest/bind/bind.go`,
      `api/rest/controller/jobdef/lint.go`, `api/rest/controller/jobdef/diff.go`,
      `api/rest/service/system/system.go`, `internal/metrics/metrics.go`.
      Depends on: A1 + B1.
      Note: W2-delta added the env-gated `GET /v1/contracts/graph` REST read through
      a new `api/rest/service/contract` wrapper over `internal/contract.NewGORMDeriver`
      with optional `dataset=ns/name` filtering, no route registration when
      `CAESIUM_CONTRACT_ENFORCEMENT` is unset, and `GET /v1/system/features`
      reporting `contract_enforcement_enabled` from the same raw env gate. Server
      lint now adds optional `contracts: {breaking, warnings, edges}` and diff wraps
      added/removed/modified entries with optional `contractFindings`, both omitted
      when the gate is off. Added the read-only RBAC policy and scoped-key global-read
      denial for `/v1/contracts/graph`, `caesium_contract_findings_total{verdict}`,
      and an integration scenario that applies a producer/consumer pair, reads the
      real graph endpoint, and verifies the feature flag.
- [ ] D2. Add the `caesium contract` Cobra group — `caesium contract graph
      [--dataset ns/name] [--json]` (GET `/v1/contracts/graph`) and `caesium contract
      check --path jobs/ [--json]` (server-mode contract lint) — appended to the
      `cmds` slice in `cmd/execute.go` (after `cache.Cmd`); add a `--server` flag to
      `caesium job lint` (`cmd/job/lint.go`) that POSTs to `/v1/jobdefs/lint` and
      reports contract findings against the persisted world, keeping the existing
      offline `nil`-DB path (`cmd/job/lint.go:51`) and its file-scoped scope note
      (`:113`) as the default. Per the `CLAUDE.md` gate, `--json` writes clean,
      parseable output to **stdout** via `cmd.OutOrStdout()`.
      Files: new `cmd/contract/`, `cmd/execute.go`, `cmd/job/lint.go`.
      Depends on: D1.

### Stream E — Datasets `schema`/`schemaFrom` declarations

The YAML surface that upgrades inferred/evidence edges to declared, `fail`-grade
contracts. This design ADDS schema fields to the step-level `datasets` block that
[`freshness-scheduling.md`](../completed/freshness-scheduling.md) Stream A introduces — it does
not own the base block. **Coordinate on `pkg/jobdef/definition.go`: whichever plan
lands `Step.Datasets` first introduces the struct; this item extends it.**

- [x] E1. Add `schema`/`schemaFrom` to `datasets.produces` entries (`schemaFrom:
      output` reuses the step's `outputSchema`; `schema: {...}` declares an inline
      dataset schema; `version` bumped on intentional breaks) and a required `schema`
      to `datasets.consumes` entries (what THIS consumer requires — a subset, not a
      copy of the producer's schema) on `pkg/jobdef/definition.go`. Add lint
      validation in `Validate()`: `schemaFrom: output` names a step that has an
      `outputSchema`, schemas compile under `santhosh-tekuri/jsonschema/v6`, and
      dataset names are well-formed. Reflect the fields in `pkg/jobdef/schema.go`.
      **No engine/cache change** — dataset schemas are apply-time metadata, never
      part of the step-execution hash (assert with a hash-stability unit test that
      adding `datasets.produces[].schema` leaves `internal/cache/hash.go`'s key
      unchanged).
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`, new
      `internal/cache/hash_test.go` assertion (hash stability).
      Note: coordinates with freshness Stream A on the base `datasets` block — see the
      Source-Of-Truth Note. Not an in-plan dependency; documented cross-plan.
      W1-epsilon note: final YAML shape is `datasets.produces[].{name,schema|schemaFrom,version,freshness,maxStaleness,watermark}` with `schemaFrom: output` resolving to the producing step's `outputSchema`, and `datasets.consumes[]` accepts either legacy scalar names or object entries `{name, schema}`. Scalar consumes remain valid without schema for shipped freshness manifests; object consumes require `schema`. Validation now trims/dedupes dataset names, rejects `schema` plus `schemaFrom`, restricts `schemaFrom` to `output` with a non-empty step `outputSchema`, and compiles inline produce/consume schemas with `santhosh-tekuri/jsonschema/v6`.

### Stream F — Console UI (contract graph + diff badges)

The Console surface, shipped after the REST reads (mirrors the data-plane-memory-ui
precedent: backend REST/CLI first, UI consumes it). New feature dir gated by the
`ContractEnforcementEnabled` feature flag.

- [ ] F1. Add the contract graph view rendering `GET /v1/contracts/graph`, reusing
      the existing `ui/src/features/jobs/LineageGraph.tsx` renderer (nodes:
      jobs/datasets; edge styling by class: declared / inferred / evidence); add the
      route in `ui/src/router.tsx`, the API method in `ui/src/lib/api.ts`, a nav
      entry in `ui/src/components/layout/Sidebar.tsx`, all gated on the
      `ContractEnforcementEnabled` feature flag from `GET /system/features`.
      Files: new `ui/src/features/contracts/`, `ui/src/router.tsx`,
      `ui/src/lib/api.ts`, `ui/src/components/layout/Sidebar.tsx`.
      Depends on: D1.
- [ ] F2. Add contract badges to the JobDefs diff tab: extend
      `ui/src/features/jobdefs/JobDefsPage.tsx` (which already lints then diffs pasted
      YAML) so its diff tab renders breaking/compatible/unknown badges per edge from
      the `contractFindings` in the diff response, with named consumers and teams
      inline, and disables Apply on breaking findings unless an ack reason is entered.
      Files: `ui/src/features/jobdefs/JobDefsPage.tsx`, `ui/src/lib/api.ts`.
      Depends on: D1 + C2 (the ack-reason path).

## Harness Strengthening

- [x] H-1. Enable the contract path on the live integration server so the Stream C/D
      scenarios drive the real surface, not an internal call: set
      `CAESIUM_CONTRACT_ENFORCEMENT=fail` (and a short
      `CAESIUM_CONTRACT_DEPRECATION_WINDOW` if the ack-expiry test needs a tight
      bound) on the `just integration-up` / `just integration-test` server, and pass
      the same env into the CI integration job (mirror the
      `CAESIUM_EVENT_INGEST_API_KEY` precedent at `justfile:32,260` and
      `.github/workflows/ci.yml`). Add the shared test helpers the contract scenarios
      need (apply producer+consumer fixtures, `runCLIStdout` split-stream capture for
      `--json`).
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.
      Note: W1-η set `CAESIUM_CONTRACT_ENFORCEMENT=fail` on every server-boot site,
      mirroring the `CAESIUM_FRESHNESS_ENABLED` precedent — all justfile lanes
      (docker, distributed, agent, podman), the helm chart `config.extraEnv[2]`, and
      the three ci.yml server blocks — so no lane drifts red when enforcement lands.
      The env var is inert until C1 adds it to `pkg/env/env.go`. Deliberately
      deferred to W2: the `CAESIUM_CONTRACT_DEPRECATION_WINDOW` bound and the `test/`
      fixture helpers land alongside the C/D scenarios that use them (helpers now
      would be unused code; the window depends on C2's actual expiry-test shape).

## Navigational / Organizational Improvements

- [ ] N-1. Flip the [`docs/design-contract-enforcement.md`](../../design-contract-enforcement.md)
      `> Status:` banner from "Brainstorm/Design" to shipped (naming this plan);
      update `docs/roadmap.md` §2.1 (PR Preview Runs) to note the contract section of
      the PR flow ships, and the Phase-4 design table row (`docs/roadmap.md:226`);
      document the `datasets` `schema`/`schemaFrom`/`consumes.schema` fields and the
      `CAESIUM_CONTRACT_*` env vars in `docs/job-schema-reference.md`,
      `docs/job-definitions.md`, and `docs/caesium-job-llm-reference.md`; add a
      producer+consumer contract example under `docs/examples/`; index this plan in
      `docs/README.md` in backtick/inline-code form (the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects clickable
      subdirectory links). Runs last, after the runtime ships.
      Files: `docs/design-contract-enforcement.md`, `docs/roadmap.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–F (runs last).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B, C, and D all import the `schemacompat` checker.
  A is a clean new package with no shared-file edits, so it merges first.
- **Stream B** (graph derivation) depends on A1 for the `Finding` type; B1
  (inferred+evidence edges) has no YAML dependency (Phase 0), so it lands early. B2
  (declared edges) depends on E1 (the `datasets` schema fields).
- **Stream C** (enforcement) depends on A1 + B1 — it runs the checker over the graph
  inside the apply transaction. C1 → C2 (the ack path builds on the enforcement
  point).
- **Stream D** (REST/CLI) depends on A1 + B1 (graph endpoint + lint/diff findings);
  D1 → D2 (the CLI calls the endpoint D1 adds).
- **Stream E** (datasets schema fields) is gated by a **cross-plan** coordination on
  `pkg/jobdef/definition.go` with freshness-scheduling Stream A — not an in-plan
  Depends-on. Land it once the base `datasets` block exists (either plan may
  introduce it).
- **Stream F** (UI) depends on D1 (graph endpoint + findings); F2 also on C2 (ack
  reason).
- **H-1** is independent (justfile/CI/harness) and supports the C/D integration
  scenarios; land it in the first wave so enforcement has a live, gated surface to
  drive.
- **N-1** runs last, after A–F ship, so roadmap/schema/design docs reflect reality.

**Within-stream order:** A1 → A2. B1 → B2. C1 → C2. D1 → D2. E1 standalone. F1, F2
both after D1 (F2 also after C2).

**Suggested waves:**
- **W1 = A1 + B1 + E1 + H-1.** A1 (pure package, no deps), B1 (inferred+evidence
  graph, only needs A1 — sequence A1→B1 within the wave or split A1 to a pre-wave),
  E1 (datasets schema, cross-plan coordinated), H-1 (harness).
- **W2 = A2 + B2 + C1 + D1.** B2 needs E1; C1/D1 need A1+B1.
- **W3 = C2 + D2 + F1.** C2 needs C1; D2 needs D1; F1 needs D1.
- **W4 = F2 + N-1.** F2 needs D1+C2; N-1 last.

**Cross-stream file conflicts:**

- `pkg/jobdef/definition.go` — **E1** (datasets `schema`/`schemaFrom` fields) is the
  only item in THIS plan that edits it, but it is a **true-conflict file shared with
  freshness-scheduling Stream A** (the base `datasets` block). Sequence across plans:
  whichever lands `Step.Datasets` first; the other extends. Never edit it in the same
  wave as a freshness definition.go item.
- `internal/models/models.go` — **C1** (`ContractAck`) appends to the `All` slice
  (order-sensitive: catalog table, append after `LineageDataset`). Additive; single
  in-plan editor.
- `pkg/env/env.go` — **C1** adds `CAESIUM_CONTRACT_ENFORCEMENT` +
  `CAESIUM_CONTRACT_DEPRECATION_WINDOW`. Single in-plan editor; additive.
- `internal/metrics/metrics.go` — **C1** (`caesium_contract_breaks_blocked_total`)
  and **D1** (`caesium_contract_findings_total`) each add a collector (two edit
  sites: the `var (...)` block + `Register()`). C1 and D1 land in the same wave (W2)
  — the one genuine same-wave additive overlap; call it out to the W2 agents (rebases
  mechanically on different lines).
- `api/rest/controller/jobdef/` — **C1** edits `apply.go` (enforcement 409), **D1**
  edits `lint.go` + `diff.go` (findings in response). Different files, no collision.
- `internal/jobdef/importer.go` — **C1** (enforcement in `ApplyWithOptions` txn) and
  **C2** (ack recording) both edit it; same stream, sequence C1 → C2.
- `internal/contract/enforce.go` — C1 creates it, C2 extends it; same stream,
  sequenced.
- `api/rest/bind/bind.go` — **D1** adds the `/v1/contracts/graph` route (additive
  import + route line). `cmd/execute.go` — **D2** appends the `contract` command
  group (additive). `ui/src/router.tsx` / `ui/src/lib/api.ts` /
  `ui/src/components/layout/Sidebar.tsx` — **F1/F2** append (list appends; import
  blocks rebase mechanically).
- `go.mod`/`go.sum` — **no new dependency**: `santhosh-tekuri/jsonschema/v6` is
  already present (v6.0.2), so no `go mod tidy` conflict.
- `internal/cache/hash.go` — **unchanged**: dataset schemas are apply-time metadata,
  not part of the step-execution hash (E1 asserts this with a hash-stability test).

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Everything is containerized (the builder images carry the dqlite CGO deps); host
`go build`/`go test` is discouraged per `CLAUDE.md`. `just unit-test` does NOT
compile `test/` (it is behind `//go:build integration`), so a passing unit-test is
necessary but not sufficient — the integration gate is the end-to-end signal.

Per-stream additions:

- **Compat checker (A):** the table-driven verdict matrix + `FuzzCompare` green under
  `just unit-test` (removed required → breaking, `integer→number` → compatible, each
  unknown construct → unknown, no panic on arbitrary trees).
- **New REST endpoint / CLI verb (C, D):** an integration scenario in `test/` that
  drives the **real surface** — apply producer + consumer, then a breaking change →
  `apply` returns 409 naming the consumer and team; `caesium contract graph --json`
  and `caesium job lint --server` via the CLI binary with **stdout captured
  separately from stderr** (`runCLIStdout`), asserting clean parseable JSON. A unit
  test that hand-calls `Compare` proves the checker, not the wiring — both required.
- **Enforcement + ack (C):** integration scenarios for `--allow-breaking` (ack
  recorded, notification emitted, window expiry re-blocks), a coordinated one-batch
  producer+consumer migration passing without an ack, and a paramMapping-inferred
  edge catching a removed output key with no declarations.
- **`CAESIUM_CONTRACT_ENFORCEMENT` unset ⇒ fully inert** — an integration assertion
  that no graph computation happens and no `/v1/contracts/graph` route is registered
  (feature-flag off).
- **New metric (C1, D1):** assert via `internal/metrics/testutil` in a `*_test.go`;
  the collector must also appear in `Register()`.
- **Job-schema change (E1):** `caesium job lint --path docs/examples/` green on the
  new contract example; the hash-stability unit test proves `internal/cache/hash.go`
  is untouched.
- **UI (F):** `just ui-lint && just ui-test && just ui-e2e` — Playwright e2e for the
  graph view and diff badges against a live backend.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended, and
  any cross-linked doc (roadmap/schema/design) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the schema-compatibility checker** is a deterministic, offline
   `pkg/jobdef/schemacompat` package: the table-driven verdict matrix and
   `FuzzCompare` are green in CI, covering breaking/compatible/unknown across the
   design's subset with an honest "can't tell" verdict for out-of-subset constructs.
2. **Stream B — the contract graph** derives all three edge classes (declared,
   inferred trigger-chain, evidence lineage) per request from authoritative sources,
   reusing the `trigger_cycle.go` join and `QueryImpact`, carrying per-edge verdicts.
   Closed by a `test/` integration scenario asserting a paramMapping-inferred edge is
   found and its removed output key flagged.
3. **Stream C — apply-time enforcement** blocks a breaking change: a producer PR that
   removes a consumed required field → `caesium job apply` returns HTTP 409 naming
   the consumer and owning team, inside the importer transaction; `--allow-breaking`
   records a `ContractAck`, emits a `contract_break_declared` notification, downgrades
   to warn during the deprecation window, and re-blocks on expiry. Closed by
   integration scenarios for the 409, the coordinated one-batch migration passing,
   and the ack window lifecycle, green in CI.
4. **Stream D — the operator surface** is live: `GET /v1/contracts/graph` returns the
   graph, `caesium contract graph/check --json` and `caesium job lint --server` drive
   it with clean stdout asserted via `runCLIStdout`, and contract findings fold into
   the `jobdefs/lint` and `jobdefs/diff` responses; `GET /system/features` reports
   `ContractEnforcementEnabled`.
5. **Stream E — declared contracts** work: `datasets.produces[].schema`/`schemaFrom`
   and `datasets.consumes[].schema` parse and lint (`schemaFrom: output` resolves,
   schemas compile), a working example lints green under `docs/examples/`, and the
   hash-stability test proves the cache key is untouched.
6. **Stream F — the Console** renders the contract graph (edge styling by class) and
   badges JobDefs diff edges breaking/compatible/unknown with named consumers/teams,
   disabling Apply on breaking findings without an ack reason. Closed by Playwright
   e2e against a live backend.
7. **H-1 — the integration server** runs with `CAESIUM_CONTRACT_ENFORCEMENT` set so
   the Stream C/D scenarios exercise the live gated path in CI, not an internal call.
8. **N-1 — docs reflect reality:** the `design-contract-enforcement.md` `> Status:`
   banner flipped, `docs/roadmap.md` §2.1 + the Phase-4 table updated, the `datasets`
   schema fields and `CAESIUM_CONTRACT_*` env vars documented in the schema
   references with a working `docs/examples/` manifest, and this plan indexed in
   `docs/README.md`.
9. **Cross-cutting:** `docs/roadmap.md`, `docs/design-contract-enforcement.md`, the
   freshness-scheduling plan (on the shared `datasets` block), and this plan's
   per-stream `## Progress` entries reflect every shipped stream and match the merged
   PRs.

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
   `<Imperative subject> (contract-enforcement <wave>-<stream>)` — e.g.
   `Add the schema-compatibility checker (contract-enforcement W1-α)`. GitHub appends
   `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-contract-enforcement.md`](../../design-contract-enforcement.md) — the
  design of record. Source of truth for intent and scope.
- [`docs/roadmap.md`](../../roadmap.md) §2.1 PR Preview Runs & Visual DAG Diff — the
  PR surface this feature plugs its contract section into; and the Phase-4 design
  table (`docs/roadmap.md:226`).
- [`freshness-scheduling.md`](../completed/freshness-scheduling.md) — sibling active plan that
  **owns the base step-level `datasets` block**; Stream E here extends it with schema
  fields. Coordinate on `pkg/jobdef/definition.go`.
- [`docs/design-event-triggers.md`](../../design-event-triggers.md) and the shipped
  [`event-trigger-routing.md`](../completed/event-trigger-routing.md) — the WS3
  trigger-chaining substrate (`internal/jobdef/trigger_cycle.go`,
  `paramMapping`) the inferred edge class analyzes.
- [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md) and
  [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) — the
  runtime and remediation complements to this apply-time enforcer (same edge model).
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the job-definition
  schema Stream E extends; `internal/jobdef/trigger_cycle.go`,
  `internal/lineage/impact.go` — the join/graph substrate Stream B reuses.
