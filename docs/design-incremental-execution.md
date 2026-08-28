# Design: Smart Incremental Execution

> Status: Implemented (Phases 1–5) — task identity hashing, local **and** distributed cache hits, cache-aware restart-from-failure, invalidation API/CLI, metrics, and a background pruner are all shipped. Operator usage is the `cache` field in [job-schema-reference.md](job-schema-reference.md). This document is the design of record for that shipped system; remaining work is Phase 6 polish and the [Future Extensions](#future-extensions).

## What shipped

Caesium has a build-cache for pipelines: a re-run skips tasks whose inputs haven't changed and replays their cached outputs, and a failed run can restart from the failure point preserving succeeded/cached tasks. No other mainstream scheduler (Airflow, Dagster, Prefect, Argo) offers this as a first-class declarative feature.

- **Task identity hashing** (`internal/cache/hash.go`) — SHA-256 over deterministic inputs (see [below](#task-identity-hash)).
- **Cache store** (`internal/cache/store.go`) — `Get`/`Put`/`Invalidate`/`InvalidateJob`/`Prune` backed by a `task_cache` table.
- **Local + distributed cache hits** — local check in `internal/job/job.go`; distributed via scheduler-propagated `PredecessorCacheHashes`/`PredecessorCacheOutputs` on `TaskRun` and `CacheHitTaskClaimed` (`internal/run/store.go`, `internal/dispatch/dispatch.go`, `internal/worker/completion_sink.go`).
- **`cached` task status** + `IsTerminalSuccess` (treats `cached` == `succeeded` in trigger-rule/indegree paths); `task_cached` SSE event.
- **Cache-aware restart-from-failure** — `Store.RetryFromFailure` (`internal/run/store.go`) resets a failed run so previously-succeeded and cached tasks are preserved and only failed/skipped tasks re-run; exposed via `POST /v1/jobs/{id}/runs/{run_id}/retry`.
- **Invalidation API + CLI** — `GET/DELETE /v1/jobs/{id}/cache`, `DELETE …/cache/{task_name}`, `POST /v1/cache/prune`; `caesium cache list/invalidate/prune` (`cmd/cache/`).
- **Metrics + pruner** — cache hit/miss/entries metrics; `cache.StartPruner` background goroutine (`CACHE_PRUNE_INTERVAL`, default 1h).
- **Config** — `cache` on both step and `metadata` (job-level defaults), with `ttl`/`version`; global `CAESIUM_CACHE_*` env vars.

The conceptual and architectural sections below remain the design of record for this shipped system.

### Goals (as built)

1. Cache-hit tasks skip execution entirely — outputs replayed from the previous successful run.
2. Restart-from-failure — on retry, only the failed task and downstream descendants re-execute.
3. Opt-in per task (`cache: true`); side-effectful tasks are never cached by default.
4. Correctness over speed — a cache miss is always safe; a false hit is a bug. When in doubt, re-execute.
5. Works in both local and distributed execution modes.

Non-goals: caching container images; content-addressable storage for large output *artifacts* (this caches task metadata + structured outputs); cross-job cache sharing (a future extension).

---

## Concepts

### Task Identity Hash

The cache key — uniquely identifies "what this task would do" from its deterministic inputs (`internal/cache/hash.go`):

```
TaskIdentityHash = SHA-256(
    job_alias, task_name, image, command, env, workdir, mounts,
    predecessor_hashes,       # sorted; transitive upstream invalidation
    predecessor_outputs,      # sorted by step, then by key
    run_params,               # sorted CAESIUM_PARAM_* values
    cache_version,            # user-settable, forces invalidation
)
```

Key decisions: **image tags are literal** — `etl:latest` re-hashes only when the tag string changes; use digest refs (`etl@sha256:…`) for content-addressed correctness (resolving digests at hash time would add network latency to every check). **Predecessor hashes are included**, so any upstream change transitively invalidates downstream even if the upstream output is identical. **Run params are included** (`date=2026-03-20` ≠ `date=2026-03-21`). **Only step-defined env** is hashed; system vars (`CAESIUM_RUN_ID`, etc.) are excluded.

### Chain mode (`cache.chain`)

"Predecessor hashes are included" above is the right default and the wrong one for a shared upstream step whose identity churns without changing what it produces. The hash is computed **before** execution — that is what decides whether to execute — so a checkout step's key can only contain its *inputs*, including the git ref, which moves on every commit. That churn propagates through `predecessor_hashes` to every downstream step in the repo.

There is no upstream fix: the checkout cannot hash its own output tree into its own key, because the key must exist before the step runs. The chain has to be broken downstream, which is what `cache.chain` does:

| | `transitive` (default) | `values` |
|---|---|---|
| `predecessor_hashes` | hashed | **excluded** |
| `predecessor_outputs` | hashed | hashed |
| everything else | hashed | hashed |

`values` means *"my key is what I consume, not my predecessors' internal churn."* It is sufficient — rather than merely convenient — because `predecessor_outputs` is already **direct-edge only**: a step sees the outputs of its immediate predecessors, so excluding the transitive hash chain does not silently drop a real input, it drops exactly the transitive one.

Implementation (`internal/cache/hash.go`): in values mode `Compute()` skips the `pred_hash:` lines and writes one framed `cache_chain:values` line instead. Transitive mode writes **nothing new**, so every existing cache entry survives — `TestCompute_GoldenTransitiveChainUnchanged` pins that digest as a string literal. The marker is unconditional in values mode because the mode itself is part of the identity: a step must not share a key with its transitive-mode self, whose key means something different. Values mode also folds the predecessor OUTPUTS in as one canonical-JSON `pred_outputs:` record rather than the transitive `pred_output:<step>:<key>=<value>` lines: output values are arbitrary producer text and may contain newlines, so the line form is not injective, and with the predecessor identity hashes excluded those outputs are the entire upstream identity — an aliasing collision there is a stale downstream cache hit (`TestCompute_ValuesChainPredecessorOutputFramingIsUnambiguous`, golden `TestCompute_GoldenValuesChainFraming`). The transitive line form is frozen unchanged for cache compatibility.

The resolved mode is snapshot on `TaskRun.cache_chain` and on the task execution descriptor's `cache.chain`, so the local executor, the distributed worker and replay all rebuild the same key rather than re-resolving from a mutable job definition.

`caesium why` must render the exclusion or a values-mode skip is unexplainable: the persisted `HashInputBlob` carries `chain`, the diff emits a `predecessorHashes` entry of kind `excluded` instead of a phantom add/remove, and `BlobDiff.Notes` carries `predecessor hashes excluded (chain: values)` for the summary line, the CLI table and the Console.

**Relationship to the value-verified short-circuit.** The two overlap but neither subsumes the other. `EquivalentPriorHash` stops a cascade *after the fact*, only when a re-executed step is **proven** to have published byte-identical output — so it does nothing for a step that emits no output at all (guard 2: silence is not proof of equality; the `warm` role is exactly that shape), and nothing for a consumer that cares about only part of what its predecessor publishes. `chain: values` decides *before* execution and is a declaration rather than a proof, so it covers those cases — and, unlike the short-circuit, it costs nothing when the upstream step would have re-run anyway. Use the short-circuit's automatic behaviour where outputs are stable; reach for `chain: values` when the upstream churns for reasons it does not publish.

**Sharp edge.** An upstream change that alters behaviour without altering its declared outputs leaves consumers cached. That is the intent and the hazard; the mitigation is the `why` rendering above plus the documentation callout in `job-definitions.md`.

### Cache entry & modes

A `task_cache` row associates a hash with its result, structured output, branch selections, originating run/task-run IDs, `created_at`, and optional `expires_at`. The `cache` field controls behaviour per step: `false` (default, always run), `true` (default TTL via `CAESIUM_CACHE_TTL`, 24h), `{ttl: "7d"}`, `{ttl: "never"}` (null `expires_at` — no wall-clock expiry at all, for a step keyed on a content fingerprint), `{version: 2}` (bump to force invalidation), `{chain: "values"}`, or any combination. Step-level `cache` overrides the `metadata.cache` job-level default; `cache: false` on a step opts out even when the job default is on. `chain` and `ttl` layer job → step exactly like `pinDigests`.

---

## Architecture

```
Job executor (internal/job/job.go)         Cache store (internal/cache/store.go)
  per task in DAG order:                      Get(hash) / Put(entry)
   1. compute TaskIdentityHash       ───▶     Invalidate(jobID, taskName) / InvalidateJob(jobID)
   2. CacheStore.Get(hash)                     Prune(olderThan)
   3a. HIT  → inject output, skip exec        backed by task_cache (dqlite/postgres)
   3b. MISS → execute, then Put(result)
```

`task_cache` (PostgreSQL types; dqlite uses TEXT/JSON via GORM AutoMigrate): `hash TEXT PK, job_id, task_name, result, output JSONB, branch_selections JSONB, run_id, task_run_id, created_at, expires_at`, indexed on `job_id` and `expires_at`. The Go model uses `uuid.UUID` + `datatypes.JSON`, mapping to both backends.

## Execution flow

### Local mode (`internal/job/job.go`)

In `runTask`: if cache-eligible, compute the identity hash (image/command/env/mounts from the atom; predecessor hashes from the in-memory map; predecessor outputs from `taskOutputs`; run params; `cache.version`), then `CacheStore.Get(hash)`. On HIT (not expired): mark the task `cached`, inject cached output + branch selections, emit `task_cached`, record the hash, return success — no container created. On MISS: execute normally, then `CacheStore.Put(...)` on success and record the computed hash.

### Distributed mode (`internal/worker/runtime_executor.go`)

The scheduler is the single source of truth for predecessor context in both modes. When a task becomes ready, the scheduler writes `PredecessorCacheHashes` and `PredecessorCacheOutputs` onto the `TaskRun` before dispatch; the worker computes the identity hash from those pre-computed values (no independent DB reconstruction) and on HIT calls `CacheHitTaskClaimed` (no container created), writing the computed hash back for downstream propagation.

### Restart-from-failure (`RetryFromFailure`)

`Store.RetryFromFailure` resets a failed run so previously-`succeeded` and `cached` tasks are preserved and only `failed`/`skipped` (and their downstream) reset to `pending`; DAG traversal then skips terminal-success tasks and, for newly-pending tasks whose predecessors succeeded, the cache check applies (skip on hit). It is gated behind the explicit `POST /v1/jobs/{id}/runs/{run_id}/retry` endpoint rather than the trigger path, so a re-trigger doesn't accidentally resume a stale failed run. The cache compounds this by also skipping unchanged tasks downstream of the failure (e.g. a parallel branch never reached).

### `cached` status & trigger-rule compatibility

`TaskStatusCached` is a terminal-success status, surfaced distinctly in the UI (cache icon), metrics (hit rate), and debugging. The `IsTerminalSuccess(status)` helper returns true for both `succeeded` and `cached` and is used consistently in `satisfiesTriggerRule`/`collectPredecessorStatuses`, indegree propagation, and the DAG-traversal `processed` set — so downstream `all_success`/`one_success` tasks don't block when a predecessor is served from cache. `CacheHitTask`/`CacheHitTaskClaimed` set the `cached` status, set result/output, decrement successor indegree, emit `task_cached`, and create no atom/runtime.

## Invalidation & pruning

- **Automatic**: TTL expiry (`Get` treats expired entries as misses; `ttl: never` writes a null `expires_at`, which never expires); transitive invalidation via predecessor hashing (no explicit walk needed, and deliberately opted out of by `chain: values`); job-definition changes re-hash changed steps on the next run (unchanged steps still hit).
- **Manual**: `DELETE /v1/jobs/{id}/cache`, `DELETE …/cache/{task_name}`, `POST /v1/cache/prune`; `caesium cache invalidate`/`prune`. Bump `cache.version` to force a single task's re-execution.
- **Pruning**: `cache.StartPruner` runs on `CACHE_PRUNE_INTERVAL` (default 1h), deleting expired entries.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CAESIUM_CACHE_ENABLED` | `false` | Global kill switch |
| `CAESIUM_CACHE_TTL` | `24h` | Default entry TTL |
| `CAESIUM_CACHE_PRUNE_INTERVAL` | `1h` | Pruner cadence |
| `CAESIUM_CACHE_MAX_ENTRIES` | `10000` | Max entries (LRU eviction) — see Phase 6 |

Precedence: step `cache` > job `metadata.cache` > env defaults > built-in (disabled).

## Metrics & events

Metrics: `caesium_task_cache_hits_total{job_alias,task_name}`, `…_misses_total`, `caesium_task_cache_entries` (gauge), `…_prune_total`, `caesium_run_cached_duration_saved_seconds` (histogram). Event: `task_cached` (`{run_id, task_id, task_name, cache_hash, original_run_id, original_completed_at}`) — the UI renders cached tasks with a distinct treatment and a "cached from run X" tooltip.

---

## Remaining work

Phases 1–5 (core cache, local + distributed integration, restart-from-failure, API/CLI, metrics/pruner, UI) are shipped. What remains:

### Phase 6 — polish

- **LRU eviction at `CAESIUM_CACHE_MAX_ENTRIES`** — bound the table by count, not just TTL, evicting least-recently-used entries.
- **Auto-invalidation on `caesium job apply`** — proactively invalidate entries for steps whose config changed, rather than relying on the next run's hash mismatch.
- **Cache hit-rate dashboard** on the job detail page (rolling hit rate, per-task).
- **Dry-run cache prediction** — `caesium job run --dry-run` showing per-task `CACHE HIT`/`CACHE MISS (reason)`/`SKIP (not cacheable)`.

### Future extensions

- **Cross-job cache sharing** — tasks with identical hashes across jobs share entries (requires namespace-aware keys).
- **Content-addressed image hashing** — resolve image tags to digests at hash time for stricter correctness (opt-in; latency cost).
- **Artifact caching** — store actual output files (not just structured outputs) in object storage keyed by identity hash — turns Caesium into a full build system for data pipelines.
- **Cache warming** — pre-compute hashes for scheduled runs and warm the cache off-peak.
- **Cache analytics** — surface highest hit-rate and most-invalidated tasks to help users optimize pipelines.

---

## Risks & mitigations

| Risk | Mitigation |
|------|------------|
| False cache hits (stale data served) | Conservative hashing (all inputs included); opt-in only; `cache.version` escape hatch. |
| Image tag mutation (`latest` changes, hash doesn't) | Document digest refs for cached tasks; TTL bounds staleness. |
| Cache table grows unbounded | TTL expiry + periodic pruning + (Phase 6) max-entries LRU. |
| Distributed hash divergence | Canonical (sorted, deterministic) serialization; scheduler-propagated predecessor context so both modes hash identically. |
| Hashing adds dispatch latency | O(input size), typically <1ms + one indexed lookup; net savings dwarf overhead. |
| Side-effectful tasks accidentally cached | Off by default; explicit per-task/per-job opt-in. |
