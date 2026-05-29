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

### Cache entry & modes

A `task_cache` row associates a hash with its result, structured output, branch selections, originating run/task-run IDs, `created_at`, and optional `expires_at`. The `cache` field controls behaviour per step: `false` (default, always run), `true` (default TTL via `CAESIUM_CACHE_TTL`, 24h), `{ttl: "7d"}`, `{version: 2}` (bump to force invalidation), or both. Step-level `cache` overrides the `metadata.cache` job-level default; `cache: false` on a step opts out even when the job default is on.

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

- **Automatic**: TTL expiry (`Get` treats expired entries as misses); transitive invalidation via predecessor hashing (no explicit walk needed); job-definition changes re-hash changed steps on the next run (unchanged steps still hit).
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
