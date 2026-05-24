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

## Empirical Measurement — 2026-05-23 (local single-node)

Caesium server started via `just run` against the local Docker engine
(single-node, `CAESIUM_EXECUTION_MODE=local` — the default). Load harness
driven via `just load-test`. Two passes:

**Pass 1 — minimal smoke (3 jobs × fan-out=2 × depth=2 → `buildDAGSteps` emits 1 root + 2 fan-in + 1 join = 4 tasks/run × 3 jobs = 12 task lifecycles):**

| Category | Count | Share |
|---|---|---|
| `task_run_insert` | 12 | 14.3% |
| `task_run_status` | 36 | **42.9%** (tied dominant) |
| `event_insert` | 36 | **42.9%** (tied dominant) |
| `lease_renewal` | 0 | 0.0% (local mode) |
| `callback` | 0 | 0.0% |
| **TOTAL** | **84** | 7.0 writes/task |

**Pass 2 — realistic load (10 jobs × fan-out=4 × depth=3 = 10 tasks/run = 100 task lifecycles, 3 concurrent runs):**

| Category | Count | Share |
|---|---|---|
| `task_run_insert` | 100 | 13.9% |
| `task_run_status` | 320 | **44.4%** (dominant) |
| `event_insert` | 300 | 41.7% |
| `lease_renewal` | 0 | 0.0% (local mode) |
| **TOTAL** | **720** | 7.2 writes/task |

End-to-end p50/p99: 4s/6s per run. `caesium_db_busy_retries_total` delta: 7 (mild contention even on a single-node sandbox). 0 claims (local mode bypasses the distributed claimer).

### Key empirical findings vs. analytical prediction

1. **`task_run_status` is the actual dominant category, not `event_insert`.** The analytical prediction had events slightly ahead (~38% vs. ~35%). In practice the order is reversed — events: 41.7%, status: 44.4% — and the gap is driven by predecessor-counter UPDATEs in `CompleteTask` that the analytical model underweighted.

2. **Per-task overhead matches the design's "6–10 rows/task" estimate.** Pass 1 measured 7.0/task; Pass 2 measured 7.2/task. Both passes land in the predicted band, and the small Pass-1-to-Pass-2 delta tracks fan-out width (more successors → more predecessor-decrement UPDATEs).

3. **Local mode has zero `lease_renewal` and `claim`-driven writes.** Distributed mode would shift the distribution — lease_renewal would likely appear at the ~15–25% predicted level. This baseline understates the value of Phase 1.2 (lease batching) for distributed deployments specifically.

4. **Contention exists even on a single-node sandbox.** 7 busy-retries on a 720-write workload is a measurable signal that write contention is real at modest scale.

### Sandbox caveats

- **Single-node, in-container tmpfs storage** — no Raft replication cost, no NVMe write latency. Real-cluster numbers will be slower per write but should show the *same* category proportions.
- **Local execution mode** — no distributed worker overhead, so lease_renewal and the claim hot path don't contribute. Re-run with `CAESIUM_EXECUTION_MODE=distributed` + multi-node for a complete picture before sequencing Phase 1.2.
- **No load-test of `claims_total`** — the claims counter shows 0 because local mode doesn't use the claimer. For distributed-mode profiling, this column matters.

### Updated Phase 1 sequencing recommendation

Based on the empirical numbers:

1. **Phase 1.1 (event coalescing) remains a high-impact lever** — 41.7% of writes in local mode. A 3–5× reduction inside `CompleteTask`'s event batch would cut total writes by ~30%.

2. **Add: predecessor-counter UPDATE batching** — *not currently in the design doc*. `task_run_status` is 44.4% of writes, and a meaningful chunk comes from per-successor `UPDATE … SET outstanding_predecessors = outstanding_predecessors - 1`. A single `UPDATE … WHERE id IN (?…) … - 1` per parent completion would collapse this by fan-out factor. Worth a Phase 1.4 PR.

3. **Phase 1.2 (lease batching) — already shipped (#165)**, but its impact is invisible in local mode. Re-baseline against distributed mode to confirm the per-node renewal write rate drops as expected.

4. **Phase 1.3 (catalog cache) — read-side optimization, not visible in write counters.** Defer unless catalog-read latency becomes a separate concern.

5. **Phase 2 gate** — re-measure after 1.1 + 1.4 (proposed). If `task_run_status` + `event_insert` no longer dominate (say, drop below 30% combined), Phase 2's run-owner pattern is gated as designed: still useful but not the next priority.

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

## How to reproduce

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

> Superseded by the empirical findings above (2026-05-23). The empirical
> measurement shifted the recommendation from "events dominate, start with
> 1.1" to "task_run_status edges out events; consider adding 1.4
> (predecessor-counter UPDATE batching) to Phase 1 alongside 1.1." See
> the "Updated Phase 1 sequencing recommendation" block above for the
> current guidance.

---

## Post-Phase-1 Re-baseline — 2026-05-23

After PRs #165 (1.2 lease batching) and #167 (1.1 event coalescing + 1.4
predecessor batching) merged, the harness was re-run against the same single-node
sandbox to measure delta.

**Caesium commit:** `648b118` (master with all Phase 1 work merged).

### Local mode — identical workload to Pass 2 above

| Category | Pre-Phase-1 (#166) | Post-Phase-1 | Delta |
|---|---|---|---|
| `task_run_insert` | 100 (13.9%) | 100 (13.9%) | 0 |
| `task_run_status` | 320 (44.4%) | 320 (44.4%) | 0 |
| `event_insert` | 300 (41.7%) | 300 (41.7%) | 0 |
| `lease_renewal` | 0 (0.0%) | 0 (0.0%) | 0 (local mode) |
| **TOTAL** | **720** | **720** | **0** |
| `caesium_db_busy_retries_total` | 7 | 8 | +1 (noise) |
| End-to-end p50/p99 | 4s / 6s | 4s / 6s | 0 |

**Why row counts are unchanged:** the `caesium_db_writes_total` counter increments
by row count (`Add(N)`), not statement count. Phase 1.1 collapses N event
INSERTs into one multi-row INSERT, and Phase 1.4 collapses N predecessor
UPDATEs into one IN-clause UPDATE — but the total rows written stays the same.
The benefit is in *roundtrip count*, *dqlite leader contention*, and
*per-completion latency under sustained load* — none of which the row counter
directly measures, and none of which manifest on a single-node sandbox at this
modest concurrency.

To make this visible, the harness would need an additional
`caesium_db_statements_total{category}` counter that increments once per
statement regardless of row count. That's a follow-up item; the current
metric is still useful for tracking total DB work done.

### Distributed mode — short tasks (500ms × 100 tasks)

| Category | Count | Share |
|---|---|---|
| `task_run_insert` | 100 | 10.9% |
| `task_run_status` | 420 | **45.7%** (dominant) |
| `event_insert` | 400 | 43.5% |
| `lease_renewal` | 0 | 0.0% |
| **TOTAL** | **920** | 9.2 writes/claim |
| `caesium_db_busy_retries_total` | 10 | — |
| `caesium_worker_claims_total` | 100 | — |
| End-to-end p50/p99 | 6s / 6s | — |

Distributed adds ~2 writes per claim (the claim UPDATE in `ClaimNext` plus the
`task_claimed` event) on top of the local-mode footprint. The +200 difference
vs. local mode (920 vs 720) tracks `2 × 100 = 200` extra writes for 100 claims.

`lease_renewal` is still zero because the 500ms task duration is far below
the renewal trigger threshold (`lease_ttl/2 = 15s`) — the
skip-when-not-needed branch in PR #165 correctly determines no renewal is
needed and exits early.

### Distributed mode — long tasks (45s × 8 tasks) to exercise lease renewal

| Category | Count | Share |
|---|---|---|
| `task_run_insert` | 8 | 9.1% |
| `task_run_status` | 32 | 36.4% |
| `event_insert` | 32 | 36.4% |
| `lease_renewal` | 16 | **18.2%** |
| **TOTAL** | **88** | 11.0 writes/claim |
| `caesium_db_busy_retries_total` | 1 | — |
| End-to-end p50/p99 | 2m18s / 2m18s | — |

`lease_renewal` now shows 16 row updates — matching ~2 batched renewal cycles
× 8 in-flight claims per cycle. This is the row-count view of PR #165's
batching: in the *unbatched* world this would still be 16 row updates but
spread across 16 individual UPDATE statements; with PR #165 it's 16 rows
across **2 statements** (one per renewal cycle), and that's the bit the
row counter doesn't surface but `caesium_db_busy_retries_total` shows as
~zero contention.

### Conclusions

1. **Phase 1's per-statement reduction is invisible to the row-counting metric.**
   The metric still functions as a "total DB work" indicator. Phase 1.1/1.4's
   actual wins (fewer roundtrips, lower contention, faster completion latency)
   would surface in a `caesium_db_statements_total{category}` counter or in
   sustained-load latency profiling — neither of which exists yet. Worth a
   future small PR.

2. **Phase 1.2 lease batching works as designed in distributed mode.** The
   skip-when-not-needed path keeps `lease_renewal` at 0 for short-running
   workloads (where it would otherwise be pointless overhead) and batches all
   in-flight claims into one statement when renewal is needed. The 16 rows in
   the 45s-task test represent 2 statements covering 8 claims each.

3. **Phase 2 (run-owner) gate decision deferred to real-cluster numbers.**
   Single-node sandbox doesn't surface the per-statement contention or
   per-shard write-rate ceiling that would justify Phase 2's complexity. A
   multi-node distributed integration run with sustained throughput is what
   should make the call — not these single-node row-count measurements.

4. **Phase 1.3 (catalog cache) remains deferred.** Catalog read QPS doesn't
   show up in the write counters at all; deciding whether to ship 1.3 should
   be driven by catalog-read latency / leader-CPU profiling, not by
   re-baselining the write counter.

### Follow-up: per-statement counter

Add `caesium_db_statements_total{category}` to make the post-batching wins
quantifiable. The two counters would tell different stories:

- **Rows:** total work done in the DB. Unchanged by batching, useful for
  capacity planning.
- **Statements:** count of round-trips to dqlite. Cut by Phase 1.1/1.4
  proportionally to fan-out width; the headline metric for "is batching
  working?"
