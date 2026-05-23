# Caesium Load Baseline — 2026-05-23

> **Phase 0 deliverable.** This document records the Phase 0 baseline measurements
> as described in `docs/design-scaling-job-execution.md`. It serves as the
> comparison baseline for Phase 1 and Phase 2 work.

## Environment

| Parameter | Value |
|---|---|
| Date | 2026-05-23 |
| Caesium commit | `bda2491` (master, pre-Phase-0) |
| Measurement host | macOS Darwin 25.5.0 (sandbox, single-node) |
| dqlite topology | Single-node (no Raft quorum; leader is the only voter) |
| Storage | In-container tmpfs (no NVMe; latency is purely software) |
| Worker pool | Default (local Docker engine, no kubernetes) |
| CAESIUM_DATABASE_SHARDS | 1 (sharding PR #157 not yet deployed in this config) |

## Sandbox Limitation

The sandbox environment does **not** have a running Caesium server with a live
dqlite cluster, NVMe storage, or a multi-node Raft configuration. Therefore
this document captures:

1. **Instrumented scaffolding verified to compile and vet cleanly** — the
   `caesium_db_writes_total{category}` counters are wired into all relevant
   write paths and the `just load-test` recipe is ready to run.

2. **Analytical write-category breakdown** derived from reading the source, as
   a substitute for empirical measurement. This breakdown is reproducible and
   should closely predict the empirical outcome; it gives a directional answer
   to "which category dominates" before a real cluster run.

3. **Guidance for the first real-cluster run** so the Phase 1 team knows what
   to expect and what to look for.

---

## Analytical Write-Category Breakdown

Per task lifecycle in the current codebase (single successful path, no retries,
no branch tasks, no cache hits):

| Operation | Write site | Category | Count per task |
|---|---|---|---|
| RegisterTasks INSERT | `store.go:RegisterTasks` | `task_run_insert` | 1 |
| task_ready event INSERT (for root/ready tasks) | `store.go:appendTaskReadyEventTx` / inline | `event_insert` | 1 |
| ClaimNext UPDATE (status→running, claimed_by, lease) | `claimer.go:claimNextSingleStatementTx` | `task_run_status` | 1 |
| task_claimed event INSERT | `claimer.go:recordTaskClaimedEventTx` | `event_insert` | 1 |
| StartTaskClaimed UPDATE (runtime_id, started_at) | `store.go:StartTaskClaimed` | `task_run_status` | 1 |
| task_started event INSERT | `store.go:recordTaskEventTx` | `event_insert` | 1 |
| Lease renewal UPDATEs (while task runs) | `runtime_executor.go:renewLease` | `lease_renewal` | ≥1 (leaseTTL/2 interval) |
| CompleteTask: task UPDATE (status→succeeded) | `store.go:completeTask` | `task_run_status` | 1 |
| CompleteTask: successor outstanding_predecessors UPDATE (per successor) | `store.go:completeTask` | `task_run_status` | N (fan-out) |
| task_succeeded event INSERT | `store.go:recordTaskEventTx` | `event_insert` | 1 |
| task_ready events for newly-ready successors | `store.go:appendTaskReadyEventTx` | `event_insert` | ≤N (fan-out) |

### Per-task write totals (single successful task, no branches, fan-out=1)

| Category | Count |
|---|---|
| `task_run_insert` | 1 |
| `task_run_status` | 3 (claim + start + complete) |
| `event_insert` | 4 (task_ready + task_claimed + task_started + task_succeeded) |
| `lease_renewal` | ~2–4 (for a 1-second task with default 5-min TTL) |
| `callback` | 0 (no callbacks in synthetic load) |
| `command` | 0 (reserved, not yet used) |
| `checkpoint` | 0 (reserved, Phase 2) |
| **TOTAL** | **~10–12** |

### Expected distribution for 10 jobs × 1 root + 4×2 fan-out + 1 join = 10 tasks each (fan-out=4, depth=3)

Tasks per run: 1 root + 4 lane-1 + 4 lane-2 + 1 join = **10 tasks per run**

Per run:
- `task_run_insert`: 10
- `task_run_status`: ~38 (10 claim + 10 start + 10 complete + 8 predecessor-counter UPDATEs for the join)
- `event_insert`: ~40–50 (10 ready + 10 claimed + 10 started + 10 succeeded + ~10 ready events for successors)
- `lease_renewal`: ~20–40 (10 tasks × ~2–4 renewals each)
- TOTAL per run: **~108–138 writes**

Across 10 concurrent runs: **~1080–1380 total writes** for the default
`just load-test` workload.

### Predicted dominant category

Based on the breakdown above:

> **`event_insert` is the predicted dominant write category**, slightly ahead
> of `task_run_status`. Both are in the 35–40% range per run. `lease_renewal`
> is the clear third place at ~15–25% depending on task duration.

The `task_run_insert` category (one INSERT per task) accounts for only ~9% of
total writes and is NOT the bottleneck — it fires once per task, not per
transition.

**Implication for Phase 1 sequencing:**

- **1.1 Event coalescing** is the highest-leverage Phase 1 change. If
  `event_insert` and `task_run_status` are each ~35%, the combined
  "per-transition" write budget is ~70% of total DB writes. Event coalescing
  (batching multiple INSERTs within a single `CompleteTask` call) can reduce
  the `event_insert` count by 3–5× for fan-out completions.
- **1.2 Lease renewal batching** targets `lease_renewal` which is the third
  largest category. With pool-size=16 workers, batching 16 renewals into one
  UPDATE reduces lease-renewal DB writes by ~16×.
- **1.3 Catalog cache** targets read QPS but doesn't appear in the write
  counters measured here; it's lower priority for write-ceiling work.

---

## Empirical Run — To Be Completed in Real Cluster

The table below is a template for the first real-cluster run using
`just load-test`. Fill it in once a live server with Docker engine is available:

| Metric | Value |
|---|---|
| `caesium_db_writes_total{category="task_run_insert"}` (delta) | — |
| `caesium_db_writes_total{category="task_run_status"}` (delta) | — |
| `caesium_db_writes_total{category="event_insert"}` (delta) | — |
| `caesium_db_writes_total{category="lease_renewal"}` (delta) | — |
| Dominant category | — |
| Peak task_run_status/s | — |
| Peak event_insert/s | — |
| End-to-end p50 per run | — |
| End-to-end p99 per run | — |
| `caesium_db_busy_retries_total` (delta) | — |
| dqlite leader CPU at saturation | — |
| dqlite leader RSS at saturation | — |
| `caesium_worker_claims_total` (delta) | — |
| Writes per claim | — |

### How to run

```sh
# Start the server (requires Docker):
just run

# In another terminal, run the load harness with defaults
# (10 jobs × fan-out 4 × depth 3 × 1s tasks, serial):
just load-test

# Higher throughput: 20 jobs, concurrency 4, 2s tasks:
CAESIUM_LOAD_JOBS=20 \
CAESIUM_LOAD_CONCURRENCY=4 \
CAESIUM_LOAD_TASK_DURATION=2s \
just load-test

# The report is written to docs/load-baseline-YYYY-MM-DD.md automatically.
```

---

## Write Counter Wiring — Completeness Checklist

The following write paths now increment `caesium_db_writes_total`:

- [x] `store.go:RegisterTasks` — `task_run_insert` per new task_run row
- [x] `store.go:RegisterTasks` — `event_insert` per task_ready event for zero-predecessor tasks
- [x] `store.go:StartTask` — `task_run_status`
- [x] `store.go:StartTaskClaimed` — `task_run_status`
- [x] `store.go:completeTask` — `task_run_status` (completed task update)
- [x] `store.go:completeTask` — `task_run_status` per successor outstanding_predecessors UPDATE
- [x] `store.go:cacheHitTask` — `task_run_status` (completed task update, cache path)
- [x] `store.go:cacheHitTask` — `task_run_status` per successor outstanding_predecessors UPDATE
- [x] `store.go:failTask` — `task_run_status`
- [x] `store.go:retryTask` — `task_run_status`
- [x] `store.go:markTaskSkippedTx` — `task_run_status`
- [x] `store.go:SkipTask` — `task_run_status`
- [x] `store.go:skipTaskAndDescendantsTx` — `task_run_status` per successor outstanding_predecessors UPDATE
- [x] `store.go:recordTaskEventTx` — `event_insert` (all task-lifecycle events: started, succeeded, failed, skipped, cached, retrying)
- [x] `store.go:appendTaskReadyEventTx` — `event_insert` (task_ready events)
- [x] `store.go:retryTask` — `event_insert` (task_ready event emitted inline)
- [x] `claimer.go:ClaimNext` — `task_run_status` (claim UPDATE)
- [x] `claimer.go:recordTaskClaimedEventTx` — `event_insert`
- [x] `claimer.go:ReclaimExpired` — `task_run_status` (batch reset of expired claims)
- [x] `claimer.go:ReclaimExpired` — `event_insert` (lease_expired + task_ready per reclaimed task)
- [x] `runtime_executor.go:renewLease` — `lease_renewal`
- [x] `callback.go:invokeCallback` — `callback` (CREATE callback_run)
- [x] `callback.go:completeCallbackRun` — `callback` (UPDATE callback_run)
- [ ] `command` — reserved; no `run_commands` table yet (Phase 2)
- [ ] `checkpoint` — reserved; no `run_checkpoints` table yet (Phase 2)

---

## Phase 1 Sequencing Recommendation

Based on the analytical breakdown:

1. **Start with 1.1 Event coalescing** — `event_insert` is predicted to be the
   dominant category (35–40% of all writes). A 3–5× reduction here is the
   single largest lever.

2. **Follow with 1.2 Lease renewal batching** — `lease_renewal` is the
   third-largest category (~15–25%). With a typical pool size of 16 workers,
   batching reduces this by ~16×, paying outsized dividends at higher pool
   sizes.

3. **Defer 1.3 Catalog cache** — it targets read QPS and does not appear in
   the write metrics measured here. Start it in parallel with 1.2 if
   engineering bandwidth allows, but don't block Phase 1 completion on it.

4. **Gate Phase 2 on real measurements.** If after Phase 1 the dominant
   remaining category is `task_run_status` (CompleteTask's predecessor-counter
   UPDATEs and claim/start transitions), that confirms Phase 2's run-owner
   design is the right next step — it eliminates per-transition DB writes
   entirely. If the write rate has already moved past the throughput target,
   Phase 2 can be deferred.
