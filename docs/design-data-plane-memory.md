# Design: Data-Plane Memory (the second-act substrate)

> Status: Shipped (2026-06-20) — all five components (10 items across streams A–D) merged to master; execution record archived at [`exec-plans/completed/data-plane-memory.md`](exec-plans/completed/data-plane-memory.md). This is the **retention / differentiation layer**, not the lead positioning — see [`differentiation-strategy.md`](differentiation-strategy.md). It turns Caesium's already-shipped content-addressed cache, event store, and OpenLineage pipeline into a *queryable memory of what data flowed and why each task ran*: opt-in image digest pinning, a persisted decomposed `HashInput`, `caesium why` (field-level causal explainer), a git-committable reproducibility receipt + `caesium verify`, append-only DAG-topology history, populated OpenLineage datasets + cross-job impact query, large-object reference passing, and a value-verified short-circuit. Sequenced **after** sovereignty positioning and built in a **correctness-first order** (digest pinning gates REPRODUCE/SKIP).

## Thesis (scoped)

Caesium uniquely computes a transitive content hash that folds in the **typed data that flowed between steps** (`PredecessorOutputs`), and records an event-sourced history of every run on embedded dqlite. That substrate makes three queries possible that a pure container scheduler structurally cannot answer and that the open-core incumbents will not commoditize:

- **EXPLAIN** — `caesium why`: why did this task run / skip / re-run?
- **REPRODUCE** — a digest-pinned, git-committable run receipt + a cache-backed quarantined what-if replay.
- **SKIP** — value-verified content-addressed incremental execution ("Bazel for data pipelines").

**The honest gap:** today the substrate is ~60% built, and each query above depends on persistence that does not exist yet. This spec closes that gap in dependency order. Until it lands, scope claims to what the event store can answer today (cache hit/miss, predecessor-hash change, param change).

## Goals

1. Make the data-plane memory **queryable**, not just computed-and-discarded.
2. Preserve every architectural constraint: zero external dependencies, single binary, container-only (no SDK), GitOps determinism, dqlite write characteristics, and the cache-correctness invariant (*a miss is always safe; a false hit is a bug*).
3. Each component is independently shippable and independently valuable; later components are strictly additive.

## Non-goals

- A managed lineage/observability backend, a hosted control plane, or any phone-home. Everything stays self-hosted.
- Competing with Datafold/dbt audit_helper on full row/column value diffing. We attribute *which step/output changed and why a task re-ran*; we hand off to those tools for value-level dataset diffs.
- Reconstructing source data that has been overwritten. We store task identity and structured contract values, **not** the datasets a task reads. Replay re-executes identical code against the same typed inputs and pinned digests — it does not resurrect deleted data.

---

## Components, in correctness-first build order

> Verified current state is cited with file:line. The house migration pattern is GORM `AutoMigrate(models.All...)` (`internal/models/models.go`); new columns are added as struct fields with GORM tags and created at startup — additive, nullable, backward-compatible. The house pattern for getting per-task context to distributed workers is scheduler-propagated fields on `TaskRun` (e.g. `PredecessorCacheHashes`/`PredecessorCacheOutputs`, per [`design-incremental-execution.md`](design-incremental-execution.md)); any new per-task field must follow that pattern so local and distributed modes behave identically.

### Component 1 — Image digest resolution (FIRST: correctness gate)

**Current state.** Image digest pinning does **not exist**. `HashInput.Image` (`internal/cache/hash.go:19`) hashes the literal tag (`alpine:3.23`, `foo:latest`); `Compute()` (`hash.go:33`) writes `image:%s` as-is. `internal/imagecheck/check.go` only checks local availability via `ImageInspectWithRaw`; it never resolves or stores a digest. No `RepoDigest`/`@sha256` handling anywhere in `internal/` or `pkg/`.

**Why first.** A reproducibility receipt or a content-addressed skip built on a mutable tag is unsound: a `:latest` that changes underneath leaves the hash unchanged, so the cache serves a stale result and the "receipt" attests to the wrong image. **Shipping REPRODUCE before this is a silent correctness failure** (see the kill-conditions in the strategy doc).

**Proposed change.** Resolve each step's image tag to its content digest (`sha256:…`) at task-spec construction time and (a) record the resolved digest on `TaskRun` and the cache `Entry`, and (b) **fold the digest, not the tag, into `HashInput`** so the cache key itself becomes tamper-evident. Resolution reuses the engine abstraction (`internal/atom` Docker/Podman/k8s); for k8s, prefer the digest already present in the pull status. Respect the constraint that tags were *deliberately* hashed literally to avoid per-check network latency: make digest resolution **opt-in per job** (e.g. `cache.pinDigests: true`) and cache the tag→digest mapping with a short TTL so steady-state runs pay no network cost.

**Effort:** M. **Risk:** added pull-time latency on first resolution; registry auth for private images (reuse the existing secret providers). **Unlocks:** the integrity of REPRODUCE and the correctness of SKIP across tag drift.

### Component 2 — Persist the decomposed `HashInput`

**Current state.** `HashInput` (`hash.go:16-30`) is rich (image, command, env, mounts, k8s spec, predecessor hashes, **predecessor outputs**, run params, cache version), but `Compute()` returns only the final hex SHA-256. Persistence keeps **only that opaque digest**: `TaskRun.Hash` (`internal/models/run.go:49`), written by `SetTaskHash` (`internal/run/store.go:276`). The cache `TaskCache` entry (`run.go:92-103`) persists `{Hash, Result, Output, BranchSelections, RunID, TaskRunID, ExpiresAt}` — **no decomposed inputs.** So today the system can only conclude *"the hashes differ,"* which is no better than Argo's "the string differed."

**Proposed change.** At hash-compute time, serialize the decomposed `HashInput` to a canonical JSON blob and store it as one new **nullable** JSON column on `TaskRun` (and on the cache `Entry`, so a cache hit can also be explained). This is additive (`AutoMigrate`), written on the existing write path, and gated behind cache being enabled. **Guardrails:** redact/whitelist env values before persisting (secrets resolve through `internal/jobdef/secret` and are already excluded from the hash — keep them out of the blob too); bound blob size and prune with the existing cache TTL/LRU.

**Effort:** M. **Risk:** dqlite write volume (mitigated: one bounded blob per task, not per log line); secret leakage (mitigated by redaction). **Unlocks:** `caesium why` at **field granularity** — a pure read-side, field-by-field diff of two stored blobs ("CACHE MISS — predecessor `extract.row_count` changed 1.2M→1.4M; image, command, env identical; had it been unchanged this would have skipped"), joined to the `ExecutionEvent` store for trigger-side causation. Also makes `caesium run diff` causal (data-delta and re-run-cause become the *same* computation, because the hash already contains predecessor outputs).

### Component 3 — Version DAG topology (stop destroying history)

**Current state.** On every apply, `reconcileEdgesTx` hard-deletes all edges: `tx.Unscoped().Where("job_id = ?", …).Delete(&models.TaskEdge{})` (`internal/jobdef/importer.go:602`), then recreates them. `provenance_commit` is overwritten in place on update (`importer.go:~390-401`). There is **no** `*_version` / `*_snapshot` / `*_history` table in `internal/models`. `RunCheckpoint` snapshots *run* state, not *job-definition* topology. So "the pipeline as of commit X" is reconstructable only by `git checkout` + re-apply — which means any "bisect/time-travel over pipeline behavior" claim is really a git feature, not a Caesium one.

**Proposed change.** Add an **append-only** `dag_snapshot` (or `job_version`) table that records, per apply, the full topology (tasks + edges) and per-edge `provenance_commit`, keyed by content hash of the manifest + git SHA. Reconciliation continues to maintain the *live* graph for execution, but no longer destroys history. This is the prerequisite for any "why did the graph change between these two runs" or commit-range analysis, reconstructable from dqlite alone.

**Effort:** M–L. **Risk:** snapshot growth (bound with retention/dedup on unchanged topology — most applies don't change the DAG). **Unlocks:** version-aware EXPLAIN ("this edge was added in commit `3f2a1c` by alice@"), and a future `caesium blame` over commit ranges without `git checkout`.

### Component 4 — Populate OpenLineage datasets

**Current state.** The emission pipeline is production-grade and **shipped** — facets (Execution, DAG, Provenance, SourceCodeLocation, JobType, ParentRun, ErrorMessage), namespace handling, composite/console/file/http/retry transports — but **every mapper emits empty datasets**: `Inputs: []Dataset{}` / `Outputs: []Dataset{}` across all seven mappers in `internal/lineage/mapper.go` (e.g. lines 128-130, 255-257, 324-326). So OpenLineage consumers receive runs with no data graph. *This is the highest leverage-to-effort gap in the repo: ~80% built.*

**Proposed change.** Derive datasets from (a) declared step I/O and (b) structured outputs that already carry file paths / table names, and attach a dataset + schema facet in the task mappers (after metadata load, before the `RunEvent` return). Keep only references/digests + small facet summaries in dqlite; emit full facets out via the existing http transport to a BYO consumer (respects the bounded-storage constraint). This is the foundation for cross-job impact analysis ("what breaks if this table changes") — but note that **cross-job blast-radius requires this component to exist first**; do not pitch it as already-leveraged. (This closes the gap previously tracked as the lineage "Component 0" spec-drop.)

**Effort:** M. **Risk:** dataset identity/naming consistency across heterogeneous containers. **Unlocks:** a real, self-hosted, SDK-free lineage + change-impact catalog — the un-paywalled answer to Dagster's catalog.

### Component 5 — Raise the 64 KB cap / large-object reference passing

**Current state.** `MaxOutputBytes = 65536` (`pkg/task/output.go:28`); exceeding it **errors the task** (`output.go:87-94`). Outputs flow downstream as `CAESIUM_OUTPUT_<STEP>_<KEY>` env via `BuildOutputEnv` (`output.go:297-315`). There is **no** large-object / reference-passing path — values are strings in memory and in dqlite. So "value-verified skip" and any data-level diff cannot see a dataframe or a file the way Flyte's auto-offload or Datafold can.

**Proposed change.** Add a `##caesium::output` variant that offloads a large payload to a **BYO** volume/object store (reuse the volumes abstraction) and passes only a **content-addressed reference** (path + digest) between containers — keeping dqlite to bounded references, never blobs. Hash the *reference digest* into `HashInput` so a step that emits byte-identical output produces a cache hit, enabling a **value-verified short-circuit**: replay a changed step, and if its output digest matches the last successful run's, **stop** — downstream stays green, proven not heuristic.

**Effort:** L. **Risk:** BYO storage credentials/availability (opt-in, via existing secret providers); must never become a *mandatory* dependency. **Unlocks:** value-verified SKIP across heterogeneous containers (Spark/bash/dbt), and large-object passing that closes the most-cited expressiveness gap vs. Flyte/Mage.

---

## What each feature needs (don't ship ahead of its substrate)

| Feature | Requires | Honest scope until then |
|---|---|---|
| `caesium why` (field-level) | C2 (+ C3 for graph-change causation) | Scope to cache hit/miss, predecessor-hash change, param change — call it that, not "data causality" |
| Reproducibility receipt / `verify` | **C1 first**, then C2 | **Shipped** (C1+C2 landed): `caesium receipt get` / `caesium verify` re-derive a content-addressed receipt and flag drift. An unpinned-tag task is marked **degraded/unverifiable** and never attested as reproducible — the silent-failure mode is surfaced, not suppressed. |
| Quarantined what-if replay | C1, C2 (+ C5 for data-diff) | **Shipped**: replay is quarantined and baseline-scoped; local-mode no-override replays complete via cache hits, side-effect/observability surfaces are suppressed, and re-executing replay remains fail-closed locally until the distributed tier runs it. |
| `caesium run diff` (causal) | C2 | **Shipped**: cache-bust attribution only; hand off value diffs to dbt/Datafold |
| `caesium blame` (topology attribution) | C3 | **Shipped**: commit/snapshot blame over `dag_snapshot` for topology + image + command only; `env`/`spec`/`retries`/`cache`/schema/`sla`/`triggerRules` edits are intentionally untracked until the snapshot descriptor expands. |
| Value-verified skip ("Bazel for data") | C5 (+ C1) | Metadata-only skip exists today (shipped cache); value-verification needs C5 |
| Cross-job impact analysis | C4 (+ C3) | Intra-DAG only until lineage datasets are populated |

## Constraints this spec must honor

- **Zero mandatory dependencies.** Everything stateful fits in dqlite, or is strictly opt-in/BYO (digest cache, artifact CAS). No Postgres/Redis/Kafka/object-store-by-default.
- **dqlite write characteristics.** Writes serialize through Raft (`busy_timeout` is a no-op). Persist bounded, batched, pruned state — one blob per task, not per log line; reuse the cache TTL/LRU and checkpoint cadence.
- **Cache-correctness invariant.** Any change to `HashInput` (digests in C1, reference digests in C5) must keep hashing conservative and all-inputs-inclusive; a miss must remain always safe.
- **Distributed parity.** New per-task context must propagate via `TaskRun` fields the way `PredecessorCacheHashes`/`PredecessorCacheOutputs` do — never reconstructed independently per worker.
- **GitOps determinism.** All behavior expressible in YAML and reproducible from the manifest + params.
- **No SDK.** The only contract with user code stays: image + command + env + stdout markers + exit code + mounted volumes.

## Sequencing & acceptance

1. **C1 digest resolution** → acceptance: a job with `cache.pinDigests: true` records `sha256:…` on `TaskRun`/`Entry`; a moving `:latest` produces a cache **miss** (verified by a harness scenario asserting `cacheHit: false`).
2. **C2 decomposed-input persistence** → acceptance: `caesium why <run> --task <t>` prints the single discriminating field between two runs; machine-readable JSON assertable in the harness.
3. **C3 DAG versioning** → acceptance: applying a topology change creates a new `dag_snapshot`; the prior topology is still queryable.
4. **C4 lineage datasets** → acceptance: emitted OpenLineage events carry non-empty `Inputs`/`Outputs`; impact query returns downstream datasets for a changed step.
5. **C5 large-object refs** → acceptance: a step emitting a >64 KB payload via the reference protocol succeeds; a byte-identical re-run short-circuits with downstream staying green.

Each ships behind the existing opt-in cache/lineage config and is independently revertible.

## Open questions

- Digest resolution for air-gapped registries (the sovereignty buyer): resolve at apply-time against the local mirror, or at first run? Likely apply-time with a recorded digest so runs are reproducible offline.
- Retention policy for `dag_snapshot` and decomposed-input blobs under high apply/run frequency — dedup-on-unchanged + TTL, surfaced via the existing cache-prune machinery.
- Whether `caesium why`'s trigger-side causation should read the `ExecutionEvent` payload directly or a derived index (write-amplification vs. query latency).

## Related documents

- [`differentiation-strategy.md`](differentiation-strategy.md) — why this is the second act, not the headline.
- [`design-incremental-execution.md`](design-incremental-execution.md) — the shipped content-addressed cache and its distributed propagation pattern.
- [`open_lineage.md`](open_lineage.md) — the shipped OpenLineage emission pipeline this populates.
- [`roadmap.md`](roadmap.md) — `caesium why` / run-diff overlap with the "live DAG debugging" item (3.4), reimagined here as *causal* rather than a visual state-viewer.
- [`exec-plans/completed/data-plane-memory.md`](exec-plans/completed/data-plane-memory.md) — the shipped substrate build plan (streams A–D, #213–#222).
- [`exec-plans/completed/data-plane-memory-ii.md`](exec-plans/completed/data-plane-memory-ii.md) — the follow-on plan building the causal query verbs (`run diff`, quarantined `replay`, `blame`) on top of this substrate; honors the "What each feature needs" table above.
