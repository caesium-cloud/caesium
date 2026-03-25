# Design: Smart Incremental Execution

> Status: Proposed only. This feature is not implemented in the current repo; use this file as a design draft rather than a statement of shipped behavior.

## Status

Draft (proposed).

## Overview

Smart incremental execution gives Caesium a build-cache for pipelines. When a DAG is re-run, tasks whose inputs haven't changed since the last successful execution are skipped and their cached outputs are reused. Failed re-runs restart from the point of failure, not from scratch. The combination dramatically reduces execution time, resource consumption, and cost for iterative and scheduled workloads.

No other mainstream job scheduler (Airflow, Dagster, Prefect, Argo Workflows) offers this as a first-class, declarative feature at the scheduler level.

### Goals

1. **Cache-hit tasks skip execution entirely** — no container is created, outputs are replayed from the previous successful run.
2. **Restart-from-failure** — on re-trigger after a failed run, only the failed task and its downstream descendants are re-executed.
3. **Opt-in per task** — tasks must explicitly declare `cache: true` to participate. Side-effectful tasks (sending emails, writing to external DBs) are never cached by default.
4. **Correctness over speed** — a cache miss is always safe. A false cache hit is a correctness bug. When in doubt, re-execute.
5. **Works in both local and distributed execution modes.**

### Non-Goals

- Caching container images (that's the job of the registry/runtime).
- Content-addressable storage for large output artifacts (e.g., storing actual data files). This design caches *task metadata and structured outputs*, not arbitrary filesystem state.
- Cross-job cache sharing (future extension).

---

## Concepts

### Task Identity Hash

The cache key for a task execution. It uniquely identifies "what this task would do" based on its deterministic inputs:

```
TaskIdentityHash = SHA-256(
    job_alias,
    task_name,
    image,                    # container image reference (with tag/digest)
    command,                  # serialized command array
    env,                      # sorted key=value pairs from step definition
    workdir,
    mounts,                   # sorted serialized mount specs
    predecessor_hashes,       # sorted list of predecessor task identity hashes
    predecessor_outputs,      # sorted by step name, then sorted key=value within each step
    run_params,               # sorted CAESIUM_PARAM_* values
    cache_version,            # user-settable version to force invalidation
)
```

Key design decisions:
- **Image tags are included literally.** If you use `etl:latest`, the hash changes only when the tag string changes, not when the underlying image changes. Users who want content-addressed caching should use digest references (`etl@sha256:abc123`). This is intentional: resolving digests at hash time would require pulling manifests, adding latency and network dependency to every cache check.
- **Predecessor hashes are included.** This means a change anywhere upstream automatically invalidates all downstream caches (transitive invalidation), even if the upstream's *output* happens to be identical.
- **Run parameters are included.** A run with `date=2026-03-20` has a different hash than `date=2026-03-21`.
- **Environment variables from the step definition only.** System-injected vars (`CAESIUM_RUN_ID`, `CAESIUM_JOB_ALIAS`) are excluded since they change every run.

### Cache Entry

A stored record associating a task identity hash with its execution result:

```
CacheEntry {
    hash:           string       // TaskIdentityHash
    job_id:         UUID
    task_name:      string
    result:         string       // "success", "failure", etc.
    output:         JSON         // structured task output (from WS8 markers)
    branch_selections: []string  // branch selections (for branch-type tasks)
    created_at:     timestamp
    expires_at:     timestamp    // optional TTL
    run_id:         UUID         // the run that produced this entry
    task_run_id:    UUID         // the specific task run
}
```

### Cache Modes

Users control caching behavior per-task via the `cache` field on a step:

| Mode | Behavior |
|------|----------|
| `false` (default) | No caching. Task always executes. |
| `true` | Cache enabled with default TTL (configurable globally via `CAESIUM_CACHE_TTL`, default 24h). |
| `{ttl: "7d"}` | Cache enabled with explicit TTL. |
| `{version: 2}` | Cache enabled; bumping version forces invalidation without changing the YAML otherwise. |
| `{ttl: "7d", version: 2}` | Both TTL and version. |

---

## YAML Schema Changes

### Step Definition

```yaml
steps:
  - name: extract
    image: etl:latest
    command: ["extract.sh"]
    cache: true                    # opt-in to caching

  - name: transform
    image: etl:latest
    command: ["transform.sh"]
    dependsOn: [extract]
    cache:
      ttl: 12h                    # cache valid for 12 hours
      version: 3                  # bump to force re-execution

  - name: notify
    image: slack-notify:v1
    command: ["notify.sh"]
    dependsOn: [transform]
    # cache: false (default) — side-effectful, always runs
```

### Metadata (Job-Level Defaults)

```yaml
metadata:
  alias: my-pipeline
  cache:
    enabled: true                  # enable caching for all steps by default
    ttl: 24h                       # default TTL for all cached steps
```

Step-level `cache` overrides job-level defaults. `cache: false` on a step disables caching even if the job default is enabled.

---

## Architecture

### New Components

```
┌──────────────────────────────────────────────────────┐
│                   Job Executor                        │
│                  (internal/job/job.go)                │
│                                                       │
│  For each task in DAG traversal order:               │
│                                                       │
│    1. Compute TaskIdentityHash                       │
│    2. Query CacheStore for matching entry            │
│    3a. HIT  → inject cached output, skip execution   │
│    3b. MISS → execute normally, store result in cache │
│                                                       │
└───────────────┬──────────────────────────────────────┘
                │
                ▼
┌──────────────────────────────────────────────────────┐
│                  Cache Store                          │
│              (internal/cache/store.go)                │
│                                                       │
│  - Get(hash) → *CacheEntry, error                    │
│  - Put(entry) → error                                │
│  - Invalidate(jobID, taskName) → error               │
│  - InvalidateJob(jobID) → error                      │
│  - Prune(olderThan) → (int, error)                   │
│                                                       │
│  Backed by: task_cache table in dqlite/postgres      │
└──────────────────────────────────────────────────────┘
```

### Cache Store

A new `internal/cache` package with a `Store` backed by a `task_cache` database table:

PostgreSQL schema (dqlite equivalent uses TEXT for UUID columns):

```sql
CREATE TABLE task_cache (
    hash              TEXT PRIMARY KEY,
    job_id            UUID NOT NULL,
    task_name         TEXT NOT NULL,
    result            TEXT NOT NULL,
    output            JSONB,
    branch_selections JSONB,
    run_id            UUID NOT NULL,
    task_run_id       UUID NOT NULL,
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL,
    expires_at        TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_task_cache_job ON task_cache (job_id);
CREATE INDEX idx_task_cache_expires ON task_cache (expires_at);
```

For dqlite, GORM's AutoMigrate handles the dialect differences (UUID → TEXT, JSONB → TEXT, TIMESTAMPTZ → TIMESTAMP). The Go model uses `uuid.UUID` and `datatypes.JSON` which map correctly to both backends.

The store interface:

```go
package cache

type Store struct {
    db *gorm.DB
}

type Entry struct {
    Hash             string
    JobID            uuid.UUID
    TaskName         string
    Result           string
    Output           map[string]string
    BranchSelections []string
    RunID            uuid.UUID
    TaskRunID        uuid.UUID
    CreatedAt        time.Time
    ExpiresAt        *time.Time
}

func (s *Store) Get(hash string) (*Entry, bool, error)
func (s *Store) Put(entry *Entry) error
func (s *Store) Invalidate(jobID uuid.UUID, taskName string) error
func (s *Store) InvalidateJob(jobID uuid.UUID) error
func (s *Store) Prune(olderThan time.Time) (int, error)
```

### Task Identity Hash Computation

A new `internal/cache/hash.go`:

```go
package cache

type HashInput struct {
    JobAlias           string
    TaskName           string
    Image              string
    Command            []string
    Env                map[string]string
    WorkDir            string
    Mounts             []container.Mount
    PredecessorHashes  []string                       // sorted
    PredecessorOutputs map[string]map[string]string   // keyed by step name, preserving namespace
    RunParams          map[string]string
    CacheVersion       int
}

// Compute returns the SHA-256 hex digest for the given inputs.
func (h *HashInput) Compute() string
```

The hash is computed by serializing all fields into a deterministic canonical form (sorted keys, JSON encoding) and feeding them into SHA-256.

---

## Execution Flow Changes

### Local Mode (`internal/job/job.go`)

The main execution loop changes in the `runTask` function. Before executing an atom, check the cache:

```
runTask(taskID):
    if task is not cache-eligible:
        → execute normally (existing path)

    compute TaskIdentityHash:
        - gather image, command, env, mounts from atom model
        - gather predecessor hashes from in-memory map (already computed)
        - gather predecessor outputs from taskOutputs map
        - gather run params
        - include cache version from step definition

    query CacheStore.Get(hash):
        if HIT and not expired:
            → mark task as "cached" (new status) in run store
            → inject cached output into taskOutputs map
            → inject cached branch_selections
            → emit task_cached event (new event type)
            → record predecessor hash in in-memory map
            → return success (no container created)

        if MISS:
            → execute normally (existing path)
            → on success: CacheStore.Put(hash, result, output, branch_selections)
            → record computed hash in in-memory map
```

### Distributed Mode (`internal/worker/runtime_executor.go`)

In distributed mode, the scheduler propagates predecessor context to workers via the `TaskRun` model. This avoids a circular dependency where the worker would need to independently reconstruct predecessor hashes.

**Model changes**: Add two columns to `TaskRun`:

```go
// PredecessorCacheHashes stores the identity hashes of immediate
// predecessors, written by the scheduler when it dispatches the task.
PredecessorCacheHashes datatypes.JSON `gorm:"type:json" json:"predecessor_cache_hashes,omitempty"`

// PredecessorCacheOutputs stores the namespaced outputs of immediate
// predecessors (from cache or live execution), written by the scheduler.
PredecessorCacheOutputs datatypes.JSON `gorm:"type:json" json:"predecessor_cache_outputs,omitempty"`
```

**Scheduler-side** (`internal/job/job.go` or the distributed task registration path): When a task becomes ready for dispatch (indegree reaches 0), the scheduler writes the predecessor hashes and outputs into these fields before the worker claims the task.

**Worker-side** (`internal/worker/runtime_executor.go`): The cache check uses the pre-computed values from the task run record:

```
executeTask(taskRun):
    if task is not cache-eligible:
        → execute normally (existing path)

    compute TaskIdentityHash:
        - use taskRun fields (image, command)
        - predecessor hashes: read from taskRun.PredecessorCacheHashes (set by scheduler)
        - predecessor outputs: read from taskRun.PredecessorCacheOutputs (set by scheduler)

    query CacheStore.Get(hash):
        if HIT and not expired:
            → call store.CacheHitTaskClaimed(runID, taskID, cachedResult, cachedOutput, claimedBy)
            → return (no container created)

        if MISS:
            → execute normally
            → on success: CacheStore.Put(...)
            → store computed hash back to taskRun for downstream propagation
```

This mirrors the local execution flow where hashes propagate along the DAG during traversal — the scheduler is the single source of truth for predecessor context in both modes.

### Restart-from-Failure

**Important**: The current codebase does NOT support restart-from-failure out of the box. `resolveRun()` only looks for runs with status `running` (via `FindRunning`), and `ResetInFlightTasks()` only resets tasks that are still `running` — it does not touch tasks with status `failed`. A re-trigger after a fully failed run creates a brand-new run.

This design requires new resumption infrastructure as a prerequisite:

1. **New**: `resolveRun()` must also look for the most recent failed run for the job (not just running runs). Add `FindLastFailed(jobID)` to the run store.
2. **New**: `ResetFailedTasks(runID)` method that resets tasks with status `failed` back to `pending`, preserving succeeded/cached/skipped tasks.
3. The DAG traversal then skips already-succeeded/cached tasks (existing behavior via `processed` map).
4. **New**: For pending tasks whose predecessors all succeeded, compute the identity hash. If cached, skip. If not, execute.

This resumption logic should be gated behind an explicit re-run API (`POST /v1/jobs/{id}/runs/{run_id}/retry`) rather than implicit in the trigger path, to avoid accidentally resuming a stale failed run when the user intended a fresh start.

The cache adds value on top of restart-from-failure by also skipping tasks that are downstream of the failure point but whose inputs haven't actually changed (e.g., a parallel branch that was never reached).

---

## New Task Status: `cached`

Add a new `TaskStatus`:

```go
const TaskStatusCached TaskStatus = "cached"
```

This is a terminal success status (like `succeeded`) but indicates the task was served from cache. This distinction matters for:
- **UI**: Show a cache icon on cached tasks in the DAG view
- **Metrics**: Track cache hit rate (`caesium_task_cache_hits_total`, `caesium_task_cache_misses_total`)
- **Debugging**: Users can see which tasks actually ran vs. were cached

### Trigger-Rule Compatibility

The `cached` status **must be treated as equivalent to `succeeded`** in all trigger-rule evaluation paths. Without this, downstream tasks behind `all_success` or `one_success` rules will stay blocked when a predecessor is served from cache.

Required changes:

1. **`internal/job/failure_policy.go`**: Update `collectPredecessorStatuses()` and `satisfiesTriggerRule()` to treat `cached` the same as `succeeded`. Specifically, in any predicate that checks for `TaskStatusSucceeded`, also match `TaskStatusCached`.
2. **`internal/run/store.go`**: The distributed-mode trigger-rule evaluation in `CompleteTaskClaimed` / `CacheHitTaskClaimed` must also count `cached` as a success when decrementing indegree and evaluating downstream readiness.
3. **`IsSuccessfulTaskResult()`**: Not affected — cache hits don't go through result parsing; they use the stored status directly. But `CacheHitTask` must explicitly set the task outcome to a success-equivalent.

A clean way to implement this: add a helper `IsTerminalSuccess(status TaskStatus) bool` that returns `true` for both `succeeded` and `cached`, and use it consistently in trigger-rule evaluation, indegree propagation, and the `processed` map checks in the DAG traversal loop.

### Run Store Changes

New methods on `run.Store`:

```go
// CacheHitTask marks a task as completed via cache hit, without executing it.
func (s *Store) CacheHitTask(runID, taskID uuid.UUID, result string, output map[string]string, branchSelections []string) error
```

This method:
1. Updates the task run status to `cached`
2. Sets the result and output fields
3. Decrements successor indegree counts (same as normal completion)
4. Emits a `task_cached` event
5. Does NOT create an atom or set a runtime ID

---

## Cache Invalidation

### Automatic Invalidation

1. **TTL expiry**: Entries expire after their TTL. The `Get()` method checks `expires_at` and treats expired entries as misses.
2. **Transitive invalidation via hashing**: Because predecessor hashes are included in the identity hash, any change upstream automatically produces a different hash downstream. No explicit invalidation walk is needed.
3. **Job definition change**: When `caesium job apply` updates a job, any changed steps produce different hashes on the next run. Unchanged steps still cache-hit.

### Manual Invalidation

New API endpoints and CLI commands:

```
# Invalidate all cache entries for a job
DELETE /v1/jobs/{id}/cache
caesium job cache clear <alias>

# Invalidate cache for a specific task in a job
DELETE /v1/jobs/{id}/cache/{task_name}
caesium job cache clear <alias> --task <task_name>

# Invalidate all cache entries (admin)
DELETE /v1/cache
caesium cache clear
```

### Cache Version Bump

The simplest way to force re-execution of a single task: bump its `cache.version` in the YAML. This changes the identity hash without changing any actual execution parameters.

---

## Periodic Pruning

A background goroutine in the scheduler prunes expired cache entries:

```go
func (s *Store) StartPruner(ctx context.Context, interval time.Duration) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            pruned, err := s.Prune(time.Now())
            if err != nil {
                log.Error("cache prune failed", "error", err)
                continue
            }
            if pruned > 0 {
                log.Info("pruned expired cache entries", "count", pruned)
            }
        }
    }
}
```

Default prune interval: 1 hour. Configurable via `CAESIUM_CACHE_PRUNE_INTERVAL`.

---

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CAESIUM_CACHE_ENABLED` | `false` | Global kill switch for caching |
| `CAESIUM_CACHE_TTL` | `24h` | Default TTL for cache entries |
| `CAESIUM_CACHE_PRUNE_INTERVAL` | `1h` | How often to prune expired entries |
| `CAESIUM_CACHE_MAX_ENTRIES` | `10000` | Maximum cache entries (LRU eviction when exceeded) |

### Precedence

1. Step-level `cache` field (highest priority)
2. Job-level `metadata.cache` defaults
3. Environment variable defaults
4. Built-in defaults (disabled)

---

## Metrics

New Prometheus metrics:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `caesium_task_cache_hits_total` | Counter | `job_alias`, `task_name` | Tasks skipped due to cache hit |
| `caesium_task_cache_misses_total` | Counter | `job_alias`, `task_name` | Tasks executed due to cache miss |
| `caesium_task_cache_entries` | Gauge | — | Current number of cache entries |
| `caesium_task_cache_prune_total` | Counter | — | Total entries pruned |
| `caesium_run_cached_duration_saved_seconds` | Histogram | `job_alias` | Estimated wall-clock time saved by cache hits (sum of historical durations of cached tasks) |

---

## Events

New SSE event type:

```go
const TypeTaskCached event.Type = "task_cached"
```

Payload:

```json
{
    "type": "task_cached",
    "run_id": "...",
    "task_id": "...",
    "task_name": "extract",
    "cache_hash": "abc123...",
    "original_run_id": "...",
    "original_completed_at": "2026-03-20T02:15:00Z"
}
```

The UI uses this to render cached tasks with a distinct visual treatment (e.g., dashed border, cache icon, "cached from run X" tooltip).

---

## UI Changes

### DAG View

- **Cached tasks**: Rendered with a dashed border and a small cache icon overlay on the `TaskNode` component
- **Tooltip**: "Cached — result from run {run_id} at {timestamp}"
- **Color**: Use a muted version of the success color (e.g., lighter green or gray-green)

### Run Detail

- **Task status badge**: New "Cached" badge alongside Succeeded/Failed/Skipped/Running
- **Cache info panel**: In the task slide-over drawer, show cache hash, original run ID, time saved
- **Run summary**: "5/8 tasks cached (saved ~3m 20s)"

### Job Detail

- **Cache hit rate**: Show rolling cache hit rate on the job detail page
- **"Clear Cache" button**: Manual invalidation from the UI

---

## CLI Changes

```
# View cache status for a job
caesium job cache status <alias>
  extract:     cached (hash: abc123, expires: 2026-03-21T02:00:00Z)
  transform:   cached (hash: def456, expires: 2026-03-21T02:00:00Z)
  notify:      not cacheable

# Clear cache
caesium job cache clear <alias>
caesium job cache clear <alias> --task extract

# Dry-run to preview what would be cached
caesium job run <alias> --dry-run
  extract:     CACHE HIT (from run abc-123)
  transform:   CACHE MISS (env changed)
  notify:      SKIP (not cacheable)
```

---

## Implementation Plan

### Phase 1: Core Cache Infrastructure (P0)

1. **Cache store** (`internal/cache/store.go`): `Get`, `Put`, `Invalidate`, `Prune`
2. **Hash computation** (`internal/cache/hash.go`): `HashInput.Compute()` with namespaced predecessor outputs (`map[string]map[string]string`)
3. **Database migration**: `task_cache` table (PostgreSQL-native types, separate index statements)
4. **Step definition changes** (`pkg/jobdef/definition.go`): Parse `cache` field
5. **TaskStatus addition**: `cached` status in `internal/run`
6. **`IsTerminalSuccess()` helper**: Returns true for both `succeeded` and `cached`; update `satisfiesTriggerRule()`, `collectPredecessorStatuses()`, and all trigger-rule evaluation paths to use it
7. **`CacheHitTask` method** on `run.Store`

### Phase 2: Local Execution Integration (P0)

8. **Cache check in `runTask`** (`internal/job/job.go`): Hash computation, cache lookup, skip-on-hit
9. **Cache write on success**: Store result after successful execution
10. **Predecessor hash propagation**: In-memory map of task → hash during DAG traversal
11. **Event emission**: `task_cached` event type

### Phase 3: Distributed Execution Integration (P1)

12. **Predecessor context propagation**: Add `PredecessorCacheHashes` and `PredecessorCacheOutputs` columns to `TaskRun`; scheduler writes these when dispatching tasks
13. **Cache check in `executeTask`** (`internal/worker/runtime_executor.go`): Use propagated predecessor context (not independent DB queries)
14. **`CacheHitTaskClaimed`** method for distributed claim semantics
15. **Hash write-back**: Worker stores computed hash on `TaskRun` for downstream propagation

### Phase 3.5: Restart-from-Failure Infrastructure (P1)

16. **`FindLastFailed(jobID)`** on run store: Find most recent failed run for a job
17. **`ResetFailedTasks(runID)`** on run store: Reset `failed` tasks to `pending`, preserving `succeeded`/`cached`/`skipped`
18. **Retry API**: `POST /v1/jobs/{id}/runs/{run_id}/retry` endpoint (explicit, not implicit in trigger path)
19. **CLI**: `caesium job retry <alias> [--run-id <id>]`

### Phase 4: API & CLI (P1)

14. **Cache invalidation endpoints**: `DELETE /v1/jobs/{id}/cache`
15. **CLI commands**: `caesium job cache status`, `caesium job cache clear`
16. **Dry-run mode**: `caesium job run --dry-run` with cache prediction

### Phase 5: UI & Observability (P1)

17. **DAG view**: Cached task rendering (dashed border, icon, tooltip)
18. **Run detail**: Cache info in task drawer, run summary
19. **Prometheus metrics**: Hit/miss counters, entries gauge, time-saved histogram
20. **Pruner goroutine**: Background TTL cleanup

### Phase 6: Polish (P2)

21. **Job-level cache defaults** in metadata
22. **`CAESIUM_CACHE_MAX_ENTRIES`** with LRU eviction
23. **Job definition change detection**: Auto-invalidate on `caesium job apply` when step config changes
24. **Cache hit rate dashboard** on job detail page

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| False cache hits (stale data served) | Correctness bug | Conservative hashing (include all inputs). Opt-in only. `cache.version` escape hatch. |
| Image tag mutation (`latest` changes but hash doesn't) | Stale execution | Document that digest references are recommended for cached tasks. Cache TTL provides a time bound. |
| Cache table grows unbounded | Disk/performance | TTL expiry + periodic pruning + max entries with LRU. |
| Distributed mode hash divergence | Cache miss when it should hit | Canonical hash computation (sorted, deterministic serialization). Same code path in both modes. |
| Cache adds latency to every task dispatch | Performance regression | Hash computation is O(input size), typically < 1ms. DB lookup is a single indexed query. Net savings dwarf overhead. |
| Side-effectful tasks accidentally cached | Incorrect behavior | Off by default. Users must explicitly opt in per-task or per-job. |

---

## Future Extensions

- **Cross-job cache sharing**: Tasks with identical hashes across different jobs could share cache entries. Requires namespace-aware cache keys.
- **Content-addressed image hashing**: Resolve image tags to digests at hash time for stricter cache correctness. Opt-in due to latency cost.
- **Artifact caching**: Store actual output files (not just structured outputs) in object storage, keyed by identity hash. Turns Caesium into a full build system for data pipelines.
- **Cache warming**: Pre-compute hashes for scheduled runs and warm the cache during off-peak hours.
- **Cache analytics**: Which tasks have the highest hit rates? Which have the most invalidations? Surface this in the UI to help users optimize their pipelines.
