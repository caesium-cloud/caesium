# Data-Plane Memory — Substrate Build

Last updated: 2026-06-19

This plan implements the five-component substrate specified in
[`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md): the
work that turns Caesium's already-shipped content-addressed cache, event store,
and OpenLineage pipeline into a *queryable memory of what data flowed and why
each task ran*. Shipping it unlocks the EXPLAIN (`caesium why`), REPRODUCE
(digest-pinned receipt + `caesium verify`), and SKIP (value-verified
incremental execution) capabilities that separate Caesium from the other
zero-dependency schedulers.

Today the substrate is ~60% built and the differentiating 40% is unbuilt: the
decomposed task-identity hash is computed then discarded (only the SHA-256
digest persists), OpenLineage `Inputs`/`Outputs` are emitted empty, image tags
are never resolved to digests, DAG topology is hard-deleted on every apply, and
structured outputs are capped at 64 KB scalars. This plan closes those gaps in
**dependency / correctness-first order** so each query can run over a substrate
that actually exists.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work
backlog, `## Sequencing & Dependencies` captures cross-stream order,
and `## Acceptance Criteria` lists the gates that close out the entire
plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies
   are satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Project Posture

This substrate is the **demoted "second act" / retention layer** from
[`docs/differentiation-strategy.md`](../../differentiation-strategy.md) — it is
*not* the lead. Operational-sovereignty positioning leads; do **not** market
these features until the substrate ships. Build implications, enforced by the
Depends-on edges below:

- **Digest pinning (A1) is the correctness gate.** A reproducibility receipt or
  value-verified skip on an unpinned, mutable tag is a silent correctness
  failure. Nothing in REPRODUCE/SKIP ships ahead of A1.
- **Scope `caesium why` honestly** until the decomposed inputs are persisted
  (A2): it answers cache hit/miss, predecessor-hash change, and param change —
  *not* "field-level data causality" — until the blob exists.
- **Cross-job blast-radius requires the lineage dataset graph to exist first
  (C1).** Do not pitch cross-job impact as already-leveraged.

## Source-Of-Truth Note

When this plan and [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md)
disagree, the **design doc wins** — it carries the verified current-state code
citations and the correctness-first ordering. Strategic framing (lead axis,
do-not-market constraints) defers to
[`docs/differentiation-strategy.md`](../../differentiation-strategy.md). Any
change to the job-definition YAML contract (e.g. the `cache.pinDigests` field in
A1) additionally defers to `pkg/jobdef/definition.go`.

## Progress (as of 2026-06-19)

No implementation waves have shipped yet. The plan was published from the
PR #210 design merge (`docs/design-data-plane-memory.md`,
`docs/differentiation-strategy.md`); the first wave is the next eligible run of
the `exec-plan-wave` skill against this doc. Leaf items with no unmet
dependencies: **A1, B1, C1** (parallelizable in Wave 1).

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Content-key integrity: image digest resolution + persist decomposed HashInput (+ `caesium why`, reproducibility receipt) | **P0** | Not started |
| B | DAG topology versioning — append-only `dag_snapshot`, stop hard-deleting edges | P1 | Not started |
| C | Lineage dataset population + cross-job impact query | P1 | Not started |
| D | Large-object reference passing + value-verified skip | P2 | Not started |

## Streams

Design-component → stream map (the design doc labels components C1–C5; this plan
labels streams A–D and items `A1`, `B1`, …):

| Design component | Stream / items |
|---|---|
| C1 — image digest resolution | A1 |
| C2 — persist decomposed HashInput | A2 (+ A3 `caesium why`, A4 receipt) |
| C3 — version DAG topology | B1, B2 |
| C4 — populate OpenLineage datasets | C1, C2 |
| C5 — large-object reference passing | D1, D2 |

### Stream A — Content-key integrity (design C1 + C2)

Bundled because both rewrite `internal/cache/hash.go` and the hash-persistence
path (a true-conflict surface), and because correctness-first order makes this
the P0 gate that REPRODUCE and SKIP depend on. This stream turns the cache key
from a tamper-prone opaque digest into a tamper-evident, explainable record.

- [ ] A1. Resolve image tags to content digests and fold the **digest** (not the
      tag) into the task-identity hash; opt-in per job via `cache.pinDigests`,
      with a short-TTL tag→digest cache so steady-state runs pay no network
      cost. A moving `:latest` must produce a cache miss.
      Files: `internal/cache/hash.go` (hash the resolved digest), `internal/imagecheck/`,
      `internal/atom/{docker,kubernetes,podman}/engine.go` (digest resolution per engine),
      `pkg/jobdef/definition.go` + `pkg/jobdef/schema.go` (`cache.pinDigests`),
      `pkg/env/env.go` (`CAESIUM_CACHE_PIN_DIGESTS` default),
      `docs/caesium-job-llm-reference.md` + `docs/job-schema-reference.md`,
      new `test/dataplane_test.go` harness scenario asserting `cacheHit: false` on a re-pushed tag.
- [ ] A2. Persist the decomposed `HashInput` as a canonical, secret-redacted JSON
      blob on `TaskRun` and the cache `Entry`, written on the existing hash
      write-path, gated on cache being enabled and bounded/pruned with the cache
      TTL/LRU. Propagate to distributed workers via `TaskRun` fields the way
      `PredecessorCacheHashes`/`PredecessorCacheOutputs` already do.
      Files: `internal/cache/hash.go` (canonical serialization + env redaction),
      `internal/models/run.go` (new nullable JSON column on `TaskRun` and `TaskCache`),
      `internal/run/store.go` (write alongside `SetTaskHash`),
      `internal/dispatch/`, `internal/worker/` (distributed propagation).
      Depends on: A1.
- [ ] A3. Add `caesium why <run> --task <t>`: a read-side, field-by-field diff of
      two stored `HashInput` blobs ("CACHE MISS — predecessor `extract.row_count`
      changed 1.2M→1.4M; image, command, env identical"), joined to the
      `ExecutionEvent` store for trigger-side causation, emitting machine-readable
      JSON assertable in the harness. Scope to what the persisted inputs + event
      store answer; do not over-claim data causality.
      Files: new `cmd/why/` (+ `cmd/execute.go` `cmds` slice),
      `api/rest/controller/why/` + `api/rest/service/why/` + route in `api/rest/bind/bind.go`,
      `internal/run/` (read-side diff), harness assertion support.
      Depends on: A2.
- [ ] A4. Add the reproducibility receipt + `caesium verify`: a content-addressed,
      git-committable Merkle receipt = hash(sorted per-task identity hashes +
      resolved image digests + manifest content hash + git commit); `verify`
      re-derives and flags drift ("tag mutated: digest mismatch"). Does **not**
      resurrect deleted source data — re-executes identical code against the same
      typed inputs + pinned digests.
      Files: new `internal/receipt/`, new `cmd/verify/` (+ `cmd/execute.go`),
      `api/rest/controller/receipt/` + service + `api/rest/bind/bind.go`,
      `docs/design-data-plane-memory.md` (mark REPRODUCE shipped).
      Depends on: A1 + A2.

#### Deferred to a follow-on feature plan

Causal `caesium run diff`, the quarantined what-if `replay --set … --diff`, and
`caesium blame` over commit ranges are named in the design's feature table but
under-specified for this substrate plan (they consume A2/B1/C1/D1 but each needs
its own design pass). Draft a follow-on once A–D land; do not fabricate items
below the design level here.

### Stream B — DAG topology versioning (design C3)

Stop destroying pipeline history so "the DAG as of commit X" is reconstructable
from dqlite without `git checkout`. Independent of Stream A.

- [ ] B1. Stop hard-deleting `TaskEdge` rows on apply; add an append-only
      `dag_snapshot` model capturing full topology (tasks + edges) + per-edge
      `provenance_commit`, keyed by manifest content-hash + git SHA, dedup'd on
      unchanged topology. The live graph still drives execution; history is no
      longer overwritten.
      Files: `internal/jobdef/importer.go` (`reconcileEdgesTx` ~:602 `Unscoped().Delete`; in-place `provenance_commit` update),
      new `internal/models/dag_snapshot.go` + register in `internal/models/models.go` (`All` slice, additive),
      `internal/jobdef/` snapshot write.
- [ ] B2. Expose historical topology: an API (and optional CLI) to fetch the DAG
      as-of a snapshot/commit.
      Files: `api/rest/controller/` + `api/rest/service/` + `api/rest/bind/bind.go`,
      optional `cmd/`, `internal/jobdef/` query.
      Depends on: B1.

### Stream C — Lineage dataset population + impact (design C4)

The highest leverage-to-effort gap in the repo (~80% built — the emission
pipeline ships but emits empty datasets). Independent of Stream A.

- [ ] C1. Populate OpenLineage `Inputs`/`Outputs` from declared step I/O and from
      structured outputs that already carry file paths / table names; attach a
      dataset + schema facet; persist a **bounded** dataset graph (references +
      small facet summaries) in dqlite, emitting full facets via the existing
      http transport.
      Files: `internal/lineage/mapper.go` (all 7 mappers — replace `[]Dataset{}`),
      `internal/lineage/facets.go`, new `internal/models/lineage_dataset.go` + `internal/models/models.go` (additive),
      `docs/open_lineage.md`.
- [ ] C2. Add a cross-job impact query ("what breaks if this table changes") over
      the dataset graph, bound to the producing step + git commit/author.
      Files: `api/rest/controller/` + `api/rest/service/` + `api/rest/bind/bind.go`,
      optional `cmd/`, `internal/lineage/` query.
      Depends on: C1 (graph-change attribution is sharper with B1 but does not require it).

### Stream D — Large-object reference passing + value-verified skip (design C5)

Replace the 64 KB error cap with content-addressed reference passing, then use it
for a *proven* (not heuristic) skip. Touches `internal/cache/hash.go`, so it
sequences after Stream A.

- [ ] D1. Add a `##caesium::output` reference variant that offloads payloads over
      64 KB to a BYO volume/object store (reuse the volumes abstraction) and
      passes only a content-addressed reference (path + digest) between
      containers; fold the reference digest into `HashInput`. dqlite keeps
      bounded references, never blobs.
      Files: `pkg/task/output.go` (`MaxOutputBytes` path ~:28/:87, `BuildOutputEnv`),
      `internal/cache/hash.go` (reference digest into the key),
      volumes integration (`internal/jobdef/runtime/spec.go`, engines),
      `pkg/env/env.go`, `docs/caesium-job-llm-reference.md`.
      Depends on: A1.
- [ ] D2. Implement the value-verified short-circuit: replay a changed step; if its
      output reference digest matches the last successful run's, stop —
      downstream stays green (proven via content equality, preserving the
      cache-correctness invariant).
      Files: `internal/job/job.go` + `internal/worker/` (cache decision),
      new `test/` harness scenario asserting byte-identical short-circuit.
      Depends on: D1.

## Sequencing & Dependencies

**Cross-stream order.**
- Streams **A, B, C are independent** and can run in parallel in Wave 1 (leaf
  items A1, B1, C1).
- Stream **D depends on Stream A** (D1 folds a reference digest into
  `internal/cache/hash.go`, which Stream A rewrites) — sequence D into a wave
  after A1/A2 merge.

**Within-stream order.**
- A: `A1 → A2 → (A3, A4 in parallel)`.
- B: `B1 → B2`.
- C: `C1 → C2`.
- D: `D1 → D2`.

**Cross-stream file conflicts.**
- `internal/cache/hash.go` — Streams **A** (A1, A2) and **D** (D1). True
  conflict; sequence D after A (enforced by `D1 Depends on A1`).
- `internal/models/models.go` — Streams **B** (`dag_snapshot`) and **C**
  (`lineage_dataset`) both append to the `All` slice. Additive at different
  lines → OK in parallel; rebases mechanically.
- `pkg/env/env.go` — A1 (`CAESIUM_CACHE_PIN_DIGESTS`) and D1 (large-object
  config) both append fields and share `validate()`. Additive, but if A and D
  land in the same wave, sequence the env edit rather than hand-merge.
- `cmd/execute.go` `cmds` slice and `api/rest/bind/bind.go` routes — A3, A4, B2,
  C2 all append. Additive list/route appends; the `bind.go` import block is the
  conflict-prone part — expect a mechanical rebase, or sequence within a wave.
- `go.sum` — A1 may add a registry/digest-resolution dependency; D1 may add an
  object-store client. If both land in one wave, resolve with `go mod tidy`, not
  a hand-merge.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:

- **A1 / D1 (job-schema change):** `caesium job lint --path docs/examples/` and
  add/refresh a `docs/examples/*.job.yaml` exercising `cache.pinDigests` /
  reference output.
- **New metric** (e.g. `caesium_cache_digest_resolved_total`, cache-bust
  attribution counters): assert via `internal/metrics/testutil` in a `*_test.go`,
  and register the collector in both edit sites of `internal/metrics/metrics.go`.
- **Harness scenarios** (`cacheHit:false` for A1; `why` JSON for A3;
  byte-identical short-circuit for D2): land as `test/*.scenario.yaml` /
  `test/dataplane_test.go` behind `//go:build integration` — `just unit-test`
  does not compile `test/`, so the integration gate is the end-to-end signal.
- This plan's checkbox ticked, the active-wave `## Progress` bullet appended, and
  the cross-linked design doc / roadmap section refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — content-key integrity** is a runtime feature: a job with
   `cache.pinDigests` records `sha256:` digests on `TaskRun`/`Entry`, a re-pushed
   `:latest` yields `cacheHit:false` (harness scenario green in CI); `caesium why`
   prints the single discriminating `HashInput` field between two runs (assertable
   JSON); `caesium verify` re-derives a receipt and flags tag/digest drift.
2. **Stream B — DAG versioning**: applying a topology change creates a new
   `dag_snapshot` row and the prior topology remains queryable via the as-of API
   (integration scenario green).
3. **Stream C — lineage datasets**: emitted OpenLineage events carry non-empty
   `Inputs`/`Outputs` (asserted via the harness lineage assertions), and the
   impact query returns the downstream datasets for a changed step.
4. **Stream D — large-object + value-verified skip**: a step emitting a >64 KB
   payload via the reference protocol succeeds, and a byte-identical re-run
   short-circuits with downstream staying green (harness scenario green).
5. **Cross-cutting**: `docs/roadmap.md` and `docs/design-data-plane-memory.md`
   reflect every shipped stream (banner flipped to active, plan linked); this
   plan's per-stream `## Progress` entries match merged PRs; `just integration-test`
   is green for every stream that adds a runtime path.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is
   satisfied (consult `## Sequencing & Dependencies`). Leaf items today: A1, B1, C1.
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists
   yet), and update any cross-linked design doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (data-plane-memory <wave>-<stream>)` — e.g.
   `Resolve image tags to digests in the cache key (data-plane-memory W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md) — the
  substrate spec and source of truth (verified current-state code citations).
- [`docs/differentiation-strategy.md`](../../differentiation-strategy.md) — why
  this is the second act, not the lead; the do-not-market constraints.
- [`docs/design-incremental-execution.md`](../../design-incremental-execution.md)
  — the shipped content-addressed cache and its distributed-propagation pattern
  (`PredecessorCacheHashes`/`PredecessorCacheOutputs`) that A2 follows.
- [`docs/open_lineage.md`](../../open_lineage.md) — the shipped OpenLineage
  emission pipeline that Stream C populates.
- [`docs/roadmap.md`](../../roadmap.md) — `caesium why` / run-diff overlap with
  the "live DAG debugging" item (3.4), reimagined here as *causal*.
- `pkg/jobdef/definition.go` — the job-definition schema (the `cache.pinDigests`
  field in A1 defers to it).
